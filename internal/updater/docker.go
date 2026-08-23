package updater

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/docker/go-sdk/container"
	"github.com/docker/go-sdk/image"
	"github.com/docker/go-units"
	dockercontainer "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	dockerclient "github.com/moby/moby/client"
)

func (u *Updater) checkDocker(ctx context.Context) {
	res, err := u.cli.ContainerList(ctx, dockerclient.ContainerListOptions{Filters: u.filters()})
	if err != nil {
		slog.Error("Failed to list containers", "error", err)
		return
	}

	slog.Debug("Found containers", "count", len(res.Items))

	updateAll(ctx, len(res.Items), func(ctx context.Context, i int) {
		u.updateDocker(ctx, res.Items[i])
	})

	if ctx.Err() == nil {
		u.pruneImagesDocker(ctx)
	}
}

func (u *Updater) updateDocker(ctx context.Context, c dockercontainer.Summary) {
	imageRef, ok := u.namedImage(ctx, c)
	if !ok {
		return
	}

	res, err := u.resolveTargetImage(ctx, imageRef, c.Labels)
	if err != nil {
		slog.Warn("Could not resolve target image", "image", imageRef, "error", err)
		return
	}
	if res.target != res.current {
		slog.Info("Update found", "from", res.current, "to", res.target, "policy", res.policy)
	}

	if err := u.pullImage(ctx, res.target); err != nil {
		slog.Warn("Pull failed", "image", res.target, "error", err)
		return
	}

	targetImage, err := u.cli.ImageInspect(ctx, res.target)
	if err != nil {
		slog.Warn("Could not verify pulled image", "image", res.target, "error", err)
		return
	}

	// Compare against the image backing the running container
	if !isNewDockerImage(c.ImageID, targetImage.ID) {
		slog.Debug("Already up to date", "image", res.target)
		return
	}

	// Skip recreation
	if u.isSelfDocker(c) {
		slog.Info("Update available for orbitd, restart to apply")
		return
	}

	slog.Info("Updating container", "image", res.target)
	u.recreateDocker(ctx, res.target, c.ID)
}

// namedImage returns the container's image reference. The list API reports a
// bare sha256:... id once the image becomes dangling (for example after an
// interrupted update), so it falls back to the Config.Image the container was
// originally created with.
func (u *Updater) namedImage(ctx context.Context, c dockercontainer.Summary) (string, bool) {
	if hasNamedImage(c.Image) {
		return c.Image, true
	}

	info, err := u.cli.ContainerInspect(ctx, c.ID, dockerclient.ContainerInspectOptions{})
	if err != nil || info.Container.Config == nil || !hasNamedImage(info.Container.Config.Image) {
		return "", false
	}
	return info.Container.Config.Image, true
}

func (u *Updater) recreateDocker(ctx context.Context, image, id string) {
	oldC, err := container.FromID(ctx, u.cli, id)
	if err != nil {
		slog.Error("Failed to inspect container", "id", id, "error", err)
		return
	}
	info, err := oldC.Inspect(ctx)
	if err != nil {
		slog.Error("Failed to inspect container", "id", id, "error", err)
		return
	}
	if info.Container.Config == nil || info.Container.HostConfig == nil {
		slog.Error("Container metadata incomplete", "container", id)
		return
	}
	if info.Container.HostConfig.AutoRemove {
		slog.Warn("Skipping container with AutoRemove (--rm) enabled", "container", id)
		return
	}

	name := strings.TrimPrefix(info.Container.Name, "/")
	if !oldC.IsRunning() {
		slog.Debug("Skipping container, not running", "container", name)
		return
	}

	// Stop old container (keep for rollback)
	if err := oldC.Stop(ctx); err != nil {
		slog.Error("Failed to stop container", "container", name, "error", err)
		return
	}

	// Free up container name
	backupName := name + "-orbitd-old-" + shortID(id)
	if _, err := u.cli.ContainerRename(
		ctx,
		id,
		dockerclient.ContainerRenameOptions{NewName: backupName},
	); err != nil {
		slog.Error("Failed to rename container", "container", name, "error", err)
		if err := oldC.Start(ctx); err != nil {
			slog.Error(
				"Failed to restart container, may need manual intervention",
				"container",
				name,
				"error",
				err,
			)
		}
		return
	}

	newConfig := *info.Container.Config
	newConfig.Image = image

	// Clear auto-generated hostnames
	if newConfig.Hostname == shortID(info.Container.ID) {
		newConfig.Hostname = ""
	}

	// Preserve anonymous volumes to prevent data loss
	for _, m := range info.Container.Mounts {
		if string(m.Type) == "volume" && m.Name != "" {
			isAnonymous := true
			for _, b := range info.Container.HostConfig.Binds {
				if strings.HasPrefix(b, m.Name+":") {
					isAnonymous = false
					break
				}
			}
			if isAnonymous {
				info.Container.HostConfig.Binds = append(
					info.Container.HostConfig.Binds,
					fmt.Sprintf("%s:%s", m.Name, m.Destination),
				)
			}
		}
	}

	// Strip operational network data to prevent conflicts
	endpointsConfig := make(map[string]*network.EndpointSettings)
	if info.Container.NetworkSettings != nil {
		for name, n := range info.Container.NetworkSettings.Networks {
			endpointsConfig[name] = &network.EndpointSettings{
				IPAMConfig: n.IPAMConfig,
				Links:      n.Links,
				Aliases:    n.Aliases,
				GwPriority: n.GwPriority,
				DriverOpts: n.DriverOpts,
			}
		}
	}

	resp, err := u.cli.ContainerCreate(ctx, dockerclient.ContainerCreateOptions{
		Name:       name,
		Config:     &newConfig,
		HostConfig: info.Container.HostConfig,
		NetworkingConfig: &network.NetworkingConfig{
			EndpointsConfig: endpointsConfig,
		},
	})
	if err == nil {
		_, err = u.cli.ContainerStart(ctx, resp.ID, dockerclient.ContainerStartOptions{})
	}

	if err != nil {
		slog.Error("Update failed", "container", name, "error", err)

		// Cleanup failed new container
		if resp.ID != "" {
			_, _ = u.cli.ContainerRemove(
				ctx,
				resp.ID,
				dockerclient.ContainerRemoveOptions{Force: true},
			)
		}

		// Rollback: rename backup back and restart
		if _, err := u.cli.ContainerRename(ctx, id, dockerclient.ContainerRenameOptions{
			NewName: name,
		}); err != nil {
			slog.Error(
				"Rollback failed, container may need manual intervention",
				"container",
				name,
				"error",
				err,
			)
			return
		}

		if err := oldC.Start(ctx); err != nil {
			slog.Error("Rollback failed, container is down", "container", name, "error", err)
			return
		}

		slog.Info("Rolled back to previous version", "container", name)
		return
	}

	slog.Info("Updated successfully", "container", name)

	// Cleanup old container
	if err := oldC.Terminate(ctx); err != nil {
		slog.Warn("Failed to remove old container", "container", backupName, "error", err)
	}
}

func (u *Updater) pullImage(ctx context.Context, img string) error {
	if u.pull != nil {
		return u.pull(ctx, img)
	}
	return u.pullDocker(ctx, img)
}

func (u *Updater) pullDocker(ctx context.Context, img string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()

	return image.Pull(ctx, img,
		image.WithPullClient(u.cli),
		image.WithPullHandler(func(r io.ReadCloser) error {
			_, err := io.Copy(io.Discard, r)
			return err
		}),
	)
}

func (u *Updater) pruneImagesDocker(ctx context.Context) {
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

func isNewDockerImage(current, target string) bool {
	if current == "" || target == "" {
		return false
	}
	return current != target
}

// shortID truncates a container ID to its first 12 characters, guarding
// against short IDs.
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// isSelfDocker checks whether this container is the orbitd instance itself
// by comparing the container ID prefix against our hostname (Docker sets
// the hostname to the first 12 chars of the container ID by default).
func (u *Updater) isSelfDocker(c dockercontainer.Summary) bool {
	if u.hostname != "" && strings.HasPrefix(c.ID, u.hostname) {
		return true
	}
	// Fallback check
	for _, name := range c.Names {
		if strings.Contains(name, "orbitd") {
			return true
		}
	}
	return false
}
