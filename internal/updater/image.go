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

func (u *Updater) resolveTargetImage(
	ctx context.Context,
	r *run,
	imgRef string,
	labels map[string]string,
) (resolvedImage, error) {
	current, _, _ := strings.Cut(imgRef, "@")

	pol := u.Policy
	if raw := strings.TrimSpace(labels["orbitd.policy"]); raw != "" {
		pol = policy.ParseOr(raw, u.Policy)
	}

	target := current
	if pol != policy.Digest {
		var err error
		target, err = r.target(current+"|"+pol.String(), func() (string, error) {
			return policy.FindUpdateTarget(ctx, current, pol)
		})
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

func hasNamedImage(imageRef string) bool {
	return imageRef != "" && !strings.HasPrefix(imageRef, "sha256:")
}

func pinImageDigest(imageRef, digest string) string {
	return imageRef + "@" + digest
}
