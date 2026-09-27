// Package policy defines update policies and resolves which image tag to pull.
package policy

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
)

const (
	Digest Policy = "digest"
	Patch  Policy = "patch"
	Minor  Policy = "minor"
	Major  Policy = "major"
)

var (
	// Splits e.g. "v1.25.3-alpine" into prefix, up to three numbers and suffix.
	versionRe    = regexp.MustCompile(`^(v?)(\d+)(?:\.(\d+))?(?:\.(\d+))?([-+].*)?$`)
	prereleaseRe = regexp.MustCompile(`(?i)^-(alpha|beta|pre|preview|rc)([.-]?\d+)*$`)
)

// Policy defines how aggressively to update container images.
type Policy string

// version is a parsed tag plus the shape it was written in.
type version struct {
	*semver.Version

	prefix string
	parts  int
	digits int // of the major component
	suffix string
}

func (p Policy) String() string { return string(p) }

func (p Policy) IsValid() bool {
	switch p {
	case Digest, Patch, Minor, Major:
		return true
	}
	return false
}

func Parse(raw string) (Policy, error) {
	if p := Policy(strings.ToLower(strings.TrimSpace(raw))); p.IsValid() {
		return p, nil
	}
	return "", fmt.Errorf("unknown policy %q (want digest, patch, minor or major)", raw)
}

func ParseOr(raw string, fallback Policy) Policy {
	p, err := Parse(raw)
	if err != nil {
		slog.Warn("Unknown container policy, using default", "policy", raw, "fallback", fallback)
		return fallback
	}
	return p
}

// FindUpdateTarget resolves the best available tag for a policy.
func FindUpdateTarget(ctx context.Context, image string, policy Policy) (string, error) {
	if !policy.IsValid() || policy == Digest {
		return image, nil
	}

	repo, tag, err := ParseImage(image)
	if err != nil {
		return "", err
	}

	current, ok := parseVersion(tag)
	if !ok {
		return image, nil
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	tags, err := crane.ListTags(
		repo,
		crane.WithContext(ctx),
		crane.WithAuthFromKeychain(authn.DefaultKeychain),
	)
	if err != nil {
		return "", err
	}

	return findBestVersion(repo, tags, current, policy), nil
}

// ParseImage splits an image reference into repository and tag, dropping any
// digest. The repository keeps its familiar spelling ("nginx", not
// "index.docker.io/library/nginx").
func ParseImage(img string) (repo, tag string, err error) {
	baseImg, _, _ := strings.Cut(img, "@")

	ref, err := name.ParseReference(baseImg, name.WeakValidation)
	if err != nil {
		return "", "", err
	}

	repo = familiarName(ref.Context())
	if t, ok := ref.(name.Tag); ok {
		return repo, t.TagStr(), nil
	}
	return repo, "latest", nil
}

func familiarName(r name.Repository) string {
	if r.RegistryStr() == name.DefaultRegistry {
		return strings.TrimPrefix(r.RepositoryStr(), "library/")
	}
	return r.RegistryStr() + "/" + r.RepositoryStr()
}

func findBestVersion(repo string, tags []string, current version, policy Policy) string {
	best := current
	for _, tag := range tags {
		v, ok := parseVersion(tag)
		if ok && isAllowed(v, current, policy) && v.GreaterThan(best.Version) {
			best = v
		}
	}
	return repo + ":" + best.Original()
}

func isAllowed(v, current version, policy Policy) bool {
	if !v.sameShape(current) || !v.GreaterThan(current.Version) {
		return false
	}

	switch policy {
	case Patch:
		return v.Major() == current.Major() && v.Minor() == current.Minor()
	case Minor:
		return v.Major() == current.Major()
	default:
		return true
	}
}

func parseVersion(tag string) (version, bool) {
	m := versionRe.FindStringSubmatch(tag)
	if m == nil {
		return version{}, false
	}
	v, err := semver.NewVersion(tag)
	if err != nil {
		return version{}, false
	}

	parts := 1
	for _, p := range m[3:5] {
		if p != "" {
			parts++
		}
	}
	return version{Version: v, prefix: m[1], parts: parts, digits: len(m[2]), suffix: m[5]}, true
}

// sameShape keeps updates within the same tag style, so "16.2-alpine" never
// becomes "16.4-bookworm", "1.25" never "1.25.3" and "3" never "20240101".
func (v version) sameShape(current version) bool {
	if v.prefix != current.prefix || v.parts != current.parts || v.digits > current.digits+1 {
		return false
	}
	if v.suffix == current.suffix {
		return true
	}
	return current.isPrerelease() && (v.suffix == "" || v.isPrerelease())
}

func (v version) isPrerelease() bool { return prereleaseRe.MatchString(v.suffix) }
