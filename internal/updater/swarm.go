package updater

import (
	"context"
	"log/slog"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
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
		if ctx.Err() != nil {
			return
		}
		u.updateSwarm(ctx, s)
	}
	u.pruneImagesDocker(ctx)
}

func (u *Updater) updateSwarm(ctx context.Context, s swarm.Service) {
	if s.Spec.TaskTemplate.ContainerSpec == nil {
		return
	}

	imageRef := s.Spec.TaskTemplate.ContainerSpec.Image
	if !hasNamedImage(imageRef) {
		return
	}

	resolved, err := u.resolveTargetImage(ctx, imageRef, s.Spec.Labels)
	if err != nil {
		slog.Warn("Could not resolve target image", "image", imageRef, "error", err)
		return
	}
	if resolved.target != resolved.current {
		slog.Info(
			"Update found",
			"from",
			resolved.current,
			"to",
			resolved.target,
			"policy",
			resolved.policy,
		)
	}

	// Resolve remote digest using crane to see if the underlying image changed
	digest, err := crane.Digest(resolved.target,
		crane.WithContext(ctx),
		crane.WithAuthFromKeychain(authn.DefaultKeychain),
	)
	if err != nil {
		slog.Warn("Could not resolve remote digest", "image", resolved.target, "error", err)
		return
	}

	if !isNewSwarmImage(imageRef, digest) {
		slog.Debug("Already up to date", "service", s.Spec.Name, "image", imageRef)
		return
	}

	newImage := pinImageDigest(resolved.target, digest)
	slog.Info("Updating service", "service", s.Spec.Name, "image", newImage)

	// Update service
	s.Spec.TaskTemplate.ContainerSpec.Image = newImage
	_, err = u.cli.ServiceUpdate(ctx, s.ID, dockerclient.ServiceUpdateOptions{
		Version:          s.Version,
		Spec:             s.Spec,
		RegistryAuthFrom: swarm.RegistryAuthFromPreviousSpec,
	})
	if err != nil {
		slog.Error("Failed to update service", "service", s.Spec.Name, "error", err)
		return
	}
}

func isNewSwarmImage(current, target string) bool {
	digest := imageDigest(current)
	if digest == "" || target == "" {
		return true
	}
	return digest != target
}
