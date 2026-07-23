package updater

import (
	"context"
	"strings"

	"github.com/mizuchilabs/orbitd/internal/policy"
)

type resolvedImage struct {
	current string
	target  string
	policy  policy.Policy
}

func hasNamedImage(imageRef string) bool {
	return imageRef != "" && !strings.HasPrefix(imageRef, "sha256:")
}

func (u *Updater) resolveTargetImage(
	ctx context.Context,
	imageRef string,
	labels map[string]string,
) (resolvedImage, error) {
	repo, tag, err := policy.ParseImage(imageRef)
	if err != nil {
		return resolvedImage{}, err
	}

	current := repo + ":" + tag
	pol := u.Policy
	if raw := strings.TrimSpace(labels["orbitd.policy"]); raw != "" {
		pol = policy.ParseOr(raw, u.Policy)
	}

	target := current
	if pol != policy.Digest {
		target, err = policy.FindUpdateTarget(ctx, current, pol)
		if err != nil {
			return resolvedImage{}, err
		}
	}

	return resolvedImage{
		current: current,
		target:  target,
		policy:  pol,
	}, nil
}

func imageDisplayRef(currentRef, targetRef string) string {
	_, targetTag, err := policy.ParseImage(targetRef)
	if err != nil {
		return targetRef
	}

	_, currentTag, err := policy.ParseImage(currentRef)
	if err == nil && currentTag == targetTag {
		return currentRef
	}

	name := currentRef
	if idx := strings.LastIndex(currentRef, ":"); idx >= 0 {
		rest := currentRef[idx+1:]
		if !strings.Contains(rest, "/") {
			name = currentRef[:idx]
		}
	}

	if targetTag == "" {
		targetTag = "latest"
	}

	return name + ":" + targetTag
}

func imageDigest(imageRef string) string {
	_, digest, ok := strings.Cut(imageRef, "@")
	if !ok {
		return ""
	}
	return digest
}

func pinImageDigest(imageRef, digest string) string {
	return imageRef + "@" + digest
}
