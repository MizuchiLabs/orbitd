// Package policy defines update policies and resolves which image tag to pull.
package policy

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
)

// Policy defines how aggressively to update container images.
type Policy string

const (
	Digest Policy = "digest"
	Patch  Policy = "patch"
	Minor  Policy = "minor"
	Major  Policy = "major"
)

func (p Policy) String() string { return string(p) }

func (p Policy) IsValid() bool {
	switch p {
	case Digest, Patch, Minor, Major:
		return true
	}
	return false
}

// Parse normalizes and validates a policy string, falling back to Digest.
func Parse(raw string) Policy {
	if p := Policy(strings.ToLower(strings.TrimSpace(raw))); p.IsValid() {
		return p
	}
	slog.Warn("Unknown policy, defaulting to digest", "policy", raw)
	return Digest
}

// ParseOr normalizes and validates a policy string, falling back to the given default.
func ParseOr(raw string, fallback Policy) Policy {
	if p := Policy(strings.ToLower(strings.TrimSpace(raw))); p.IsValid() {
		return p
	}
	slog.Warn("Unknown container policy, using default", "policy", raw, "fallback", fallback)
	return fallback
}

// FindUpdateTarget resolves the best available tag for a policy.
func FindUpdateTarget(ctx context.Context, image string, policy Policy) (string, error) {
	if !policy.IsValid() || policy == Digest {
		return image, nil // Keep current tag
	}

	repo, tag, err := parseImage(image)
	if err != nil {
		return "", err
	}
	if tag == "" {
		return image, nil // Cannot semver match digest/tagless
	}

	currentVer, err := semver.NewVersion(tag)
	if err != nil {
		return image, nil // Fallback to digest if not semver
	}

	listCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	tags, err := crane.ListTags(
		repo,
		crane.WithContext(listCtx),
		crane.WithAuthFromKeychain(authn.DefaultKeychain),
	)
	if err != nil {
		return "", err
	}

	return findBestVersion(repo, tags, currentVer, policy)
}

func findBestVersion(
	repo string,
	tags []string,
	current *semver.Version,
	policy Policy,
) (string, error) {
	var best *semver.Version
	var pullTag string

	for _, tag := range tags {
		v, err := semver.NewVersion(tag)
		if err != nil {
			continue
		}

		if !isAllowed(v, current, policy) {
			continue
		}

		if best == nil || v.GreaterThan(best) {
			best = v
			pullTag = tag
		}
	}

	if best == nil {
		return "", nil
	}
	return repo + ":" + pullTag, nil
}

func isAllowed(v, current *semver.Version, policy Policy) bool {
	// Prevent updating to prerelease if current is not prerelease
	if v.Prerelease() != "" && current.Prerelease() == "" {
		return false
	}

	if !v.GreaterThan(current) {
		return false
	}

	switch policy {
	case Patch:
		return v.Major() == current.Major() && v.Minor() == current.Minor()
	case Minor:
		return v.Major() == current.Major()
	default: // Major
		return true
	}
}

func parseImage(img string) (repo, tag string, err error) {
	ref, err := name.ParseReference(img, name.WeakValidation)
	if err != nil {
		return "", "", err
	}

	// Strip tag/digest from repo
	repo = ref.Context().String()

	if t, ok := ref.(name.Tag); ok {
		return repo, t.TagStr(), nil
	}

	// Digest or tagless
	return repo, "", nil
}
