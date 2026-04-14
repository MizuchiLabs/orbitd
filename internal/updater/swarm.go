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
	name := s.Spec.Name
	if s.Spec.TaskTemplate.ContainerSpec == nil {
		return
	}

	imageRef := s.Spec.TaskTemplate.ContainerSpec.Image
	if imageRef == "" || strings.HasPrefix(imageRef, "sha256:") {
		return
	}

	repo, tag, shortName, err := policy.ParseImage(imageRef)
	if err != nil {
		slog.Warn(
			"Failed to parse image reference",
			"service",
			name,
			"image",
			imageRef,
			"error",
			err,
		)
		return
	}

	if shortName == "orbitd" {
		slog.Debug("Processing self-update for orbitd service", "service", name)
	}

	baseImage := repo + ":" + tag
	targetImg := baseImage
	pol := u.Policy

	// Check for service-specific policy override
	if raw, ok := s.Spec.Labels["orbitd.policy"]; ok {
		pol = policy.ParseOr(raw, u.Policy)
	}

	// Resolve semver target if not using standard digest policy
	if pol != policy.Digest {
		target, err := policy.FindUpdateTarget(ctx, baseImage, pol)
		if err != nil {
			slog.Warn(
				"Could not resolve update target",
				"service",
				name,
				"image",
				baseImage,
				"error",
				err,
			)
			return
		}
		if target == "" {
			slog.Debug("No update available", "service", name, "image", baseImage, "policy", pol)
			return
		}
		if target != baseImage {
			slog.Info(
				"Update found (new version)",
				"service",
				name,
				"from",
				baseImage,
				"to",
				target,
				"policy",
				pol,
			)
			targetImg = target
		}
	}

	// Resolve remote digest using crane to see if the underlying image changed
	digest, err := crane.Digest(targetImg,
		crane.WithContext(ctx),
		crane.WithAuthFromKeychain(authn.DefaultKeychain),
	)
	if err != nil {
		slog.Warn(
			"Could not resolve remote digest",
			"service",
			name,
			"image",
			targetImg,
			"error",
			err,
		)
		return
	}

	newImage := targetImg + "@" + digest
	if newImage == imageRef {
		slog.Debug("Already up to date", "service", name, "image", newImage)
		return
	}

	if targetImg == baseImage {
		slog.Info("Update found (new digest)", "service", name, "image", newImage)
	} else {
		slog.Info("Updating service", "service", name, "image", newImage)
	}

	// Apply the new digest
	s.Spec.TaskTemplate.ContainerSpec.Image = newImage
	_, err = u.cli.ServiceUpdate(ctx, s.ID, dockerclient.ServiceUpdateOptions{
		Version:          s.Version,
		Spec:             s.Spec,
		QueryRegistry:    true,
		RegistryAuthFrom: swarm.RegistryAuthFromPreviousSpec,
	})
	if err != nil {
		slog.Error("Failed to update service", "service", name, "error", err)
		return
	}
	slog.Info("Service updated successfully", "service", name)
}
