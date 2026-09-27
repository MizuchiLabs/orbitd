package updater

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/docker/go-units"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/moby/moby/api/types/swarm"
	dockerclient "github.com/moby/moby/client"
)

const digestTimeout = 30 * time.Second

func (u *Updater) checkSwarm(ctx context.Context) {
	res, err := u.cli.ServiceList(ctx, dockerclient.ServiceListOptions{Filters: u.filters()})
	if err != nil {
		slog.Error("Failed to list services", "error", err)
		return
	}

	slog.Debug("Found services", "count", len(res.Items))

	r := &run{}
	updateAll(ctx, res.Items, func(ctx context.Context, s swarm.Service) {
		u.updateSwarm(ctx, r, s)
	})

	if ctx.Err() == nil {
		u.pruneImagesSwarm(ctx)
	}
}

func (u *Updater) updateSwarm(ctx context.Context, r *run, s swarm.Service) {
	if s.Spec.TaskTemplate.ContainerSpec == nil {
		return
	}

	imageRef := s.Spec.TaskTemplate.ContainerSpec.Image
	if !hasNamedImage(imageRef) {
		return
	}

	resolved, err := u.resolveTargetImage(ctx, r, imageRef, s.Spec.Labels)
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

	digest, err := r.digest(resolved.target, func() (string, error) {
		ctx, cancel := context.WithTimeout(ctx, digestTimeout)
		defer cancel()
		return crane.Digest(resolved.target,
			crane.WithContext(ctx),
			crane.WithAuthFromKeychain(authn.DefaultKeychain),
		)
	})
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

	s.Spec.TaskTemplate.ContainerSpec.Image = newImage
	_, err = u.cli.ServiceUpdate(ctx, s.ID, dockerclient.ServiceUpdateOptions{
		Version:          s.Version,
		Spec:             s.Spec,
		RegistryAuthFrom: swarm.RegistryAuthFromPreviousSpec,
	})
	if err != nil {
		slog.Error("Failed to update service", "service", s.Spec.Name, "error", err)
	}
}

// pruneImagesSwarm removes dangling images on this node only. Swarm pulls by
// digest, so images of replaced tasks end up untagged.
func (u *Updater) pruneImagesSwarm(ctx context.Context) {
	if !u.Cleanup {
		return
	}

	filters := dockerclient.Filters{}
	filters.Add("dangling", "true")
	res, err := u.cli.ImagePrune(ctx, dockerclient.ImagePruneOptions{Filters: filters})
	if err != nil {
		slog.Warn("Image cleanup failed", "error", err)
		return
	}

	if len(res.Report.ImagesDeleted) > 0 {
		slog.Info("Cleaned up old images",
			"count", len(res.Report.ImagesDeleted),
			"reclaimed", units.HumanSize(float64(res.Report.SpaceReclaimed)),
		)
	}
}

func isNewSwarmImage(current, target string) bool {
	_, digest, ok := strings.Cut(current, "@")
	if !ok || digest == "" || target == "" {
		return true
	}
	return digest != target
}
