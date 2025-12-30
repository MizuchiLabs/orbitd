package updater

import (
	"context"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/google/go-containerregistry/pkg/crane"
)

type UpdatePolicy string

const (
	PolicyDigest UpdatePolicy = "digest" // Default: same tag, new digest
	PolicyPatch  UpdatePolicy = "patch"  // ~1.2.0 -> 1.2.x
	PolicyMinor  UpdatePolicy = "minor"  // ^1.2.0 -> 1.x.x
	PolicyMajor  UpdatePolicy = "major"  // Any newer version
)

// FindUpdateTarget returns the best available tag given a policy.
// Returns empty string if no update is available.
func FindUpdateTarget(currentImage string, policy UpdatePolicy) (string, error) {
	if !policy.IsValid() {
		policy = PolicyDigest
	}

	if policy == PolicyDigest {
		return currentImage, nil // Just re-pull same tag
	}

	repo, tag := parseImage(currentImage)
	currentVer, err := semver.NewVersion(tag)
	if err != nil {
		return currentImage, nil // Not semver, fall back to digest
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tags, err := crane.ListTags(repo, crane.WithContext(ctx))
	if err != nil {
		return "", err
	}

	constraint := buildConstraint(currentVer, policy)
	return findBestVersion(repo, tags, currentVer, constraint)
}

func buildConstraint(current *semver.Version, policy UpdatePolicy) *semver.Constraints {
	var c string
	switch policy {
	case PolicyPatch:
		c = "~" + current.String()
	case PolicyMinor:
		c = "^" + current.String()
	case PolicyMajor:
		c = ">= " + current.String()
	}
	constraint, _ := semver.NewConstraint(c)
	return constraint
}

func findBestVersion(
	repo string,
	tags []string,
	current *semver.Version,
	constraint *semver.Constraints,
) (string, error) {
	var best *semver.Version
	for _, tag := range tags {
		v, err := semver.NewVersion(tag)
		if err != nil {
			continue // Skip non-semver tags
		}
		if constraint.Check(v) && v.GreaterThan(current) {
			if best == nil || v.GreaterThan(best) {
				best = v
			}
		}
	}
	if best == nil {
		return "", nil
	}
	return repo + ":" + best.String(), nil
}

func parseImage(image string) (repo, tag string) {
	if i := strings.LastIndex(image, ":"); i != -1 {
		return image[:i], image[i+1:]
	}
	return image, "latest"
}

func (p UpdatePolicy) IsValid() bool {
	switch p {
	case PolicyDigest, PolicyPatch, PolicyMinor, PolicyMajor:
		return true
	}
	return false
}
