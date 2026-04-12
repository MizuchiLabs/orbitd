// Package updater monitors and updates containers.
package updater

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/docker/go-sdk/client"
	"github.com/docker/go-sdk/container"
	"github.com/docker/go-sdk/image"
	"github.com/docker/go-units"
	"github.com/mizuchilabs/orbitd/internal/policy"
	dockercontainer "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	dockerclient "github.com/moby/moby/client"
	"github.com/urfave/cli/v3"
)

type Updater struct {
	Policy       policy.Policy // Update policy (patch, minor, major, digest)
	Interval     time.Duration // Update check interval
	Cleanup      bool          // Prune old images
	RequireLabel bool          // Only monitor orbitd.enable=true
	hostname     string
	docker       client.SDKClient
}

func New(ctx context.Context, cmd *cli.Command) error {
	cli, err := client.New(ctx)
	if err != nil {
		return fmt.Errorf("failed to create docker client: %w", err)
	}

	hostname, _ := os.Hostname()
	updater := &Updater{
		Policy:       policy.Parse(cmd.String("policy")),
		Interval:     max(5*time.Minute, cmd.Duration("interval")),
		Cleanup:      cmd.Bool("cleanup"),
		RequireLabel: cmd.Bool("require-label"),
		hostname:     hostname,
		docker:       cli,
	}

	// Handle shutdown
	go func() {
		<-ctx.Done()
		slog.Info("Shutting down")
		_ = cli.Close()
	}()
	return updater.Start(ctx)
}

func (u *Updater) Start(ctx context.Context) error {
	slog.Info("Starting orbitd", "interval", u.Interval, "policy", u.Policy)

	ticker := time.NewTicker(u.Interval)
	defer ticker.Stop()

	if err := u.check(ctx); err != nil {
		slog.Error("Update check failed", "error", err)
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := u.check(ctx); err != nil {
				slog.Error("Update check failed", "error", err)
			}
		}
	}
}

func (u *Updater) check(ctx context.Context) error {
	filters := dockerclient.Filters{}
	if u.RequireLabel {
		filters.Add("label", "orbitd.enable=true")
	}
	res, err := u.docker.ContainerList(ctx, dockerclient.ContainerListOptions{Filters: filters})
	if err != nil {
		return fmt.Errorf("failed to list containers: %w", err)
	}

	for _, c := range res.Items {
		u.update(ctx, c)
	}

	u.pruneImages(ctx) // run once per cycle
	return nil
}

func (u *Updater) update(ctx context.Context, c dockercontainer.Summary) {
	name := "unknown"
	if len(c.Names) > 0 {
		name = strings.TrimPrefix(c.Names[0], "/")
	}

	if strings.HasPrefix(c.Image, "sha256:") {
		slog.Debug(
			"Skipping container, image referenced by digest only",
			"container",
			name,
			"image",
			c.Image,
		)
		return
	}

	targetImg := c.Image
	pol := u.Policy

	// Check for container-specific policy override
	if raw, ok := c.Labels["orbitd.policy"]; ok {
		pol = policy.ParseOr(raw, u.Policy)
	}

	// Resolve semver target
	if pol != policy.Digest {
		target, err := policy.FindUpdateTarget(ctx, c.Image, pol)
		if err != nil {
			slog.Warn(
				"Could not resolve update target",
				"container",
				name,
				"image",
				c.Image,
				"error",
				err,
			)
			return
		}
		if target == "" {
			slog.Debug("No update available", "container", name, "image", c.Image, "policy", pol)
			return
		}
		if target != c.Image {
			slog.Info(
				"Update found (new version)",
				"container",
				name,
				"from",
				c.Image,
				"to",
				target,
				"policy",
				pol,
			)
			targetImg = target
		}
	}

	localDigestBefore, _ := u.getImageDigest(ctx, targetImg)
	if err := u.pull(ctx, targetImg); err != nil {
		slog.Warn("Pull failed", "container", name, "image", targetImg, "error", err)
		return
	}

	localDigestAfter, err := u.getImageDigest(ctx, targetImg)
	if err != nil {
		slog.Warn(
			"Could not verify pulled image digest",
			"container",
			name,
			"image",
			targetImg,
			"error",
			err,
		)
		return
	}

	// If the tag hasn't changed, and the digest hasn't changed, we're up to date
	if targetImg == c.Image && localDigestBefore != "" && localDigestBefore == localDigestAfter {
		slog.Debug("Already up to date", "container", name, "image", targetImg)
		return
	}

	if targetImg == c.Image {
		slog.Info("Update found (new digest)", "container", name, "image", targetImg)
	}

	// Defer self-updates to avoid crashing mid-run
	if u.isSelf(c) {
		slog.Info("Update available for orbitd itself, restart to apply", "container", name)
		return
	}

	slog.Info("Updating container", "container", name, "image", targetImg)
	u.recreate(ctx, targetImg, c.ID)
}

func (u *Updater) recreate(ctx context.Context, image, id string) {
	oldC, err := container.FromID(ctx, u.docker, id)
	if err != nil {
		slog.Error("Failed to inspect container", "id", id, "error", err)
		return
	}
	info, err := oldC.Inspect(ctx)
	if err != nil {
		slog.Error("Failed to inspect container", "id", id, "error", err)
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
	backupName := name + "-orbitd-old"
	if _, err := u.docker.ContainerRename(
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
	if len(info.Container.ID) >= 12 && newConfig.Hostname == info.Container.ID[:12] {
		newConfig.Hostname = ""
	}

	// Preserve anonymous volumes to prevent data loss
	for _, m := range info.Container.Mounts {
		if string(m.Type) == "volume" &&
			len(m.Name) == 64 { // Docker assigns 64-char hex strings for anonymous volumes
			info.Container.HostConfig.Binds = append(
				info.Container.HostConfig.Binds,
				fmt.Sprintf("%s:%s", m.Name, m.Destination),
			)
		}
	}

	// Strip operational network data to prevent conflicts
	endpointsConfig := make(map[string]*network.EndpointSettings)
	for name, n := range info.Container.NetworkSettings.Networks {
		endpointsConfig[name] = &network.EndpointSettings{
			IPAMConfig: n.IPAMConfig,
			Links:      n.Links,
			Aliases:    n.Aliases,
			GwPriority: n.GwPriority,
			DriverOpts: n.DriverOpts,
		}
	}

	resp, err := u.docker.ContainerCreate(ctx, dockerclient.ContainerCreateOptions{
		Name:       name,
		Config:     &newConfig,
		HostConfig: info.Container.HostConfig,
		NetworkingConfig: &network.NetworkingConfig{
			EndpointsConfig: endpointsConfig,
		},
	})
	if err == nil {
		_, err = u.docker.ContainerStart(ctx, resp.ID, dockerclient.ContainerStartOptions{})
	}

	if err != nil {
		slog.Error("Update failed", "container", name, "error", err)

		// Cleanup failed new container
		if resp.ID != "" {
			_, _ = u.docker.ContainerRemove(
				ctx,
				resp.ID,
				dockerclient.ContainerRemoveOptions{Force: true},
			)
		}

		// Rollback: rename backup back and restart
		if _, err := u.docker.ContainerRename(ctx, id, dockerclient.ContainerRenameOptions{
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

func (u *Updater) pull(ctx context.Context, img string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()

	return image.Pull(ctx, img,
		image.WithPullClient(u.docker),
		image.WithPullHandler(func(r io.ReadCloser) error {
			_, err := io.Copy(io.Discard, r)
			return err
		}),
	)
}

func (u *Updater) pruneImages(ctx context.Context) {
	if !u.Cleanup {
		return
	}

	res, err := u.docker.ImagePrune(ctx, dockerclient.ImagePruneOptions{
		Filters: dockerclient.Filters{}.Add("dangling", "true"),
	})
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

func (u *Updater) getImageDigest(ctx context.Context, image string) (string, error) {
	info, err := u.docker.ImageInspect(ctx, image)
	if err != nil {
		return "", err
	}

	// Prefer RepoDigests
	if len(info.RepoDigests) > 0 {
		return info.RepoDigests[0], nil
	}
	return info.ID, nil
}

// isSelf checks whether this container is the orbitd instance itself
// by comparing the container ID prefix against our hostname (Docker sets
// the hostname to the first 12 chars of the container ID by default).
func (u *Updater) isSelf(c dockercontainer.Summary) bool {
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
