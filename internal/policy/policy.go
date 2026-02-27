// Package policy defines update policies and resolves which image tag to pull.
package policy

import (
	"context"
	"fmt"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// FindUpdateTarget returns the best available tag given a policy.
func FindUpdateTarget(
	ctx context.Context,
	currentImage string,
	updatePolicy Policy,
) (string, error) {
	if !updatePolicy.IsValid() || updatePolicy == Digest {
		return currentImage, nil // Just re-pull same tag
	}

	repo, tag, err := parseImage(currentImage)
	if err != nil {
		return "", err
	}
	if tag == "" {
		return currentImage, nil // digest reference or tagless reference -> can't semver match
	}

	currentVer, err := semver.NewVersion(tag)
	if err != nil {
		return currentImage, nil // Not semver, fall back to digest
	}

	listCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	tags, err := crane.ListTags(
		repo,
		crane.WithContext(listCtx),
		crane.WithTransport(remote.DefaultTransport),
	)
	if err != nil {
		return "", err
	}

	return findBestVersion(repo, tags, currentVer, updatePolicy)
}

func findBestVersion(
	repo string,
	tags []string,
	current *semver.Version,
	updatePolicy Policy,
) (string, error) {
	constraint, err := buildConstraint(current, updatePolicy)
	if err != nil {
		return "", err
	}

	var best *semver.Version
	var pullTag string
	for _, tag := range tags {
		v, err := semver.NewVersion(tag)
		if err != nil {
			continue
		}
		if !constraint.Check(v) {
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

func buildConstraint(
	current *semver.Version,
	updatePolicy Policy,
) (*semver.Constraints, error) {
	switch updatePolicy {
	case Patch:
		return semver.NewConstraint(fmt.Sprintf("~%s, > %s", current.String(), current.String()))
	case Minor:
		return semver.NewConstraint(fmt.Sprintf("^%s, > %s", current.String(), current.String()))
	default:
		return semver.NewConstraint("> " + current.String())
	}
}

func parseImage(image string) (repo, tag string, err error) {
	ref, err := name.ParseReference(image, name.WeakValidation)
	if err != nil {
		return "", "", err
	}

	// repo should be without tag/digest
	repo = ref.Context().String()

	if t, ok := ref.(name.Tag); ok {
		return repo, t.TagStr(), nil
	}

	// Digest or tagless reference
	return repo, "", nil
}
