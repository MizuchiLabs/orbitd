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
	if imageRef == "" {
		return
	}

	// Logic for resolving image and checking for updates
	baseImage := strings.Split(imageRef, "@")[0]
	if !strings.Contains(baseImage, ":") && strings.Contains(imageRef, "@") {
		slog.Debug(
			"Skipping service, image referenced by digest only",
			"service",
			name,
			"image",
			imageRef,
		)
		return
	}
	if strings.HasPrefix(imageRef, "sha256:") {
		return
	}

	targetImg := baseImage
	pol := u.Policy

	// Defer self-updates
	if strings.Contains(name, "orbitd") {
		slog.Info("Update available for orbitd service, apply manually or restart", "service", name)
		return
	}

	// Check for service-specific policy override
	if raw, ok := s.Spec.Labels["orbitd.policy"]; ok {
		pol = policy.ParseOr(raw, u.Policy)
	}

	// Resolve semver target
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

	// Resolve remote digest to pin it
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

	// If the image ref is already this digest, we are up to date
	if newImage == imageRef {
		slog.Debug("Already up to date", "service", name, "image", newImage)
		return
	}

	if targetImg == baseImage {
		slog.Info("Update found (new digest)", "service", name, "image", newImage)
	}

	slog.Info("Updating service", "service", name, "image", newImage)

	// Clone the spec and update the image
	spec := s.Spec
	spec.TaskTemplate.ContainerSpec.Image = newImage

	opts := dockerclient.ServiceUpdateOptions{
		Version:       s.Version,
		Spec:          spec,
		QueryRegistry: true,
	}

	_, err = u.cli.ServiceUpdate(ctx, s.ID, opts)
	if err != nil {
		slog.Error("Failed to update service", "service", name, "error", err)
		return
	}
	slog.Info("Service updated successfully", "service", name)
}
