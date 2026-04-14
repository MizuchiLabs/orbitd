package updater

import (
	"context"
	"log/slog"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/mizuchilabs/orbitd/internal/policy"
	"github.com/moby/moby/api/types/swarm"
	dockerclient "github.com/moby/moby/client"
)

func (u *Updater) checkSwarm(ctx context.Context) {
	filters := dockerclient.Filters{}
	if u.RequireLabel {
		filters.Add("label", "orbitd.enable=true")
	}

	res, err := u.cli.ServiceList(ctx, dockerclient.ServiceListOptions{Filters: filters})
	if err != nil {
		slog.Error("Failed to list services", "error", err)
		return
	}

	slog.Debug("Found services", "count", len(res.Items))
	for _, s := range res.Items {
		u.updateSwarm(ctx, s)
	}
}

func (u *Updater) updateSwarm(ctx context.Context, s swarm.Service) {
	if s.Spec.TaskTemplate.ContainerSpec == nil {
		return
	}

	imageRef := s.Spec.TaskTemplate.ContainerSpec.Image
	if imageRef == "" || strings.HasPrefix(imageRef, "sha256:") {
		return
	}

	repo, tag, _, err := policy.ParseImage(imageRef)
	if err != nil {
		slog.Warn("Failed to parse image reference", "image", imageRef, "error", err)
		return
	}

	baseImage := repo + ":" + tag
	targetImg := baseImage

	// Check for service-specific policy override
	pol := u.Policy
	if raw, ok := s.Spec.Labels["orbitd.policy"]; ok {
		pol = policy.ParseOr(raw, u.Policy)
	}

	// Resolve semver target if not using standard digest policy
	if pol != policy.Digest {
		target, err := policy.FindUpdateTarget(ctx, baseImage, pol)
		if err != nil {
			slog.Warn("Could not resolve update target", "image", baseImage, "error", err)
			return
		}
		if target == "" {
			slog.Debug("No update available", "image", baseImage, "policy", pol)
			return
		}
		if target != baseImage {
			slog.Info("Update found", "from", baseImage, "to", target, "policy", pol)
			targetImg = target
		}
	}

	// Resolve remote digest using crane to see if the underlying image changed
	digest, err := crane.Digest(targetImg,
		crane.WithContext(ctx),
		crane.WithAuthFromKeychain(authn.DefaultKeychain),
	)
	if err != nil {
		slog.Warn("Could not resolve remote digest", "image", targetImg, "error", err)
		return
	}

	newImage := targetImg + "@" + digest
	if newImage == imageRef {
		slog.Debug("Already up to date", "image", newImage)
		return
	}

	slog.Info("Updating service", "service", s.Spec.Name, "image", newImage)

	// Apply the new digest and force an update
	s.Spec.TaskTemplate.ContainerSpec.Image = newImage
	s.Spec.TaskTemplate.ForceUpdate++
	_, err = u.cli.ServiceUpdate(ctx, s.ID, dockerclient.ServiceUpdateOptions{
		Version:          s.Version,
		Spec:             s.Spec,
		QueryRegistry:    true,
		RegistryAuthFrom: swarm.RegistryAuthFromPreviousSpec,
	})
	if err != nil {
		slog.Error("Failed to update service", "service", s.Spec.Name, "error", err)
		return
	}
}
