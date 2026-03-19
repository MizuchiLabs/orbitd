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
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
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
		slog.Info("Shutting down orbitd")
		_ = cli.Close()
	}()
	return updater.Start(ctx)
}

func (u *Updater) Start(ctx context.Context) error {
	slog.Info("Starting orbitd", "interval", u.Interval)

	ticker := time.NewTicker(u.Interval)
	defer ticker.Stop()

	if err := u.check(ctx); err != nil {
		slog.Error("Error during update check", "error", err)
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := u.check(ctx); err != nil {
				slog.Error("Error during update check", "error", err)
			}
		}
	}
}

func (u *Updater) check(ctx context.Context) error {
	containers, err := u.docker.ContainerList(ctx, dockerclient.ContainerListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list containers: %w", err)
	}

	for _, c := range containers.Items {
		if len(c.Names) == 0 {
			slog.Warn("Container has no names, skipping", "id", c.ID)
			continue
		}
		containerName := strings.TrimPrefix(c.Names[0], "/")

		if u.isEnabled(c, containerName) {
			u.update(ctx, c)
		}
	}

	u.pruneImages(ctx) // run once per cycle
	return nil
}

func (u *Updater) isEnabled(c dockercontainer.Summary, name string) bool {
	label := c.Labels["orbitd.enable"]

	if u.RequireLabel {
		// Opt-in mode
		if label != "true" {
			slog.Debug("Skipping (opt-in)", "container", name)
			return false
		}
	} else {
		// Opt-out mode
		if label == "false" {
			slog.Debug("Skipping (disabled)", "container", name)
			return false
		}
	}
	return true
}

func (u *Updater) update(ctx context.Context, c dockercontainer.Summary) {
	if strings.HasPrefix(c.Image, "sha256:") {
		slog.Debug("Skipping untagged digest", "image", c.Image)
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
			slog.Warn("Failed to resolve target", "image", c.Image, "error", err)
			return
		}
		if target == "" {
			slog.Debug("No update available", "image", c.Image, "policy", pol)
			return
		}
		if target != c.Image {
			slog.Info("Found update", "from", c.Image, "to", target, "policy", pol)
			targetImg = target
		}
	}

	// Store current digest
	oldDigest, _ := u.getImageDigest(ctx, targetImg)

	// Compare remote digest before pulling
	remoteDigest, err := crane.Digest(
		targetImg,
		crane.WithAuthFromKeychain(authn.DefaultKeychain),
	)
	if err == nil && oldDigest != "" {
		// Check if remote digest matches local
		if strings.HasSuffix(oldDigest, remoteDigest) {
			slog.Debug("Image up to date", "image", targetImg)
			return
		}
	}

	pullCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()

	if err := image.Pull(
		pullCtx,
		targetImg,
		image.WithPullClient(u.docker),
		image.WithPullHandler(func(r io.ReadCloser) error {
			_, err := io.Copy(io.Discard, r)
			return err
		}),
	); err != nil {
		slog.Warn("Failed to pull image", "image", targetImg, "error", err)
		return
	}

	// Get new digest for comparison
	newDigest, err := u.getImageDigest(ctx, targetImg)
	if err != nil {
		slog.Warn("Failed to get new digest", "image", targetImg, "error", err)
		return
	}

	if targetImg == c.Image && oldDigest != "" && oldDigest == newDigest {
		slog.Debug("Image up to date", "image", targetImg)
		return
	}

	// Defer self-updates to avoid crashing mid-run
	if u.isSelf(c) {
		slog.Info("Self-update available, restart orbitd manually to apply")
		return
	}

	u.recreate(ctx, targetImg, c.ID)
}

func (u *Updater) recreate(ctx context.Context, image, id string) {
	oldC, err := container.FromID(ctx, u.docker, id)
	if err != nil {
		slog.Error("Failed to get container", "image", image, "error", err)
		return
	}
	info, err := oldC.Inspect(ctx)
	if err != nil {
		slog.Error("Failed to inspect container", "image", image, "error", err)
		return
	}

	name := strings.TrimPrefix(info.Container.Name, "/")

	if !oldC.IsRunning() {
		slog.Debug("Skipping stopped container", "container", name)
		return
	}

	// Stop old container (keep for rollback)
	if err := oldC.Stop(ctx); err != nil {
		slog.Error("Failed to stop", "container", name, "error", err)
		return
	}

	// Free up container name
	backupName := name + "-orbitd-old"
	if _, err := u.docker.ContainerRename(
		ctx,
		id,
		dockerclient.ContainerRenameOptions{NewName: backupName},
	); err != nil {
		slog.Error("Failed to rename", "container", name, "error", err)
		// Rollback on rename failure
		if startErr := oldC.Start(ctx); startErr != nil {
			slog.Error("Failed to restart container", "container", name, "error", startErr)
		}
		return
	}

	newConfig := *info.Container.Config
	newConfig.Image = image

	// Clear auto-generated hostnames
	if len(info.Container.ID) >= 12 && newConfig.Hostname == info.Container.ID[:12] {
		newConfig.Hostname = ""
	}

	// Strip operational network data to prevent conflicts
	endpointsConfig := make(map[string]*network.EndpointSettings)
	for netName, epSettings := range info.Container.NetworkSettings.Networks {
		endpointsConfig[netName] = &network.EndpointSettings{
			IPAMConfig: epSettings.IPAMConfig,
			Links:      epSettings.Links,
			Aliases:    epSettings.Aliases,
			DriverOpts: epSettings.DriverOpts,
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
		slog.Error("Failed to start new container", "container", name, "error", err)

		// Cleanup failed new container
		if resp.ID != "" {
			_, _ = u.docker.ContainerRemove(ctx, resp.ID, dockerclient.ContainerRemoveOptions{Force: true})
		}

		// Execute rollback
		if _, renameErr := u.docker.ContainerRename(ctx, id, dockerclient.ContainerRenameOptions{
			NewName: name,
		}); renameErr != nil {
			slog.Error("Failed to rename during rollback", "container", name, "error", renameErr)
			return
		}

		if startErr := oldC.Start(ctx); startErr != nil {
			slog.Error("Rollback failed, container is DOWN", "container", name, "error", startErr)
			return
		}

		slog.Info("Successfully rolled back", "container", name)
		return
	}

	slog.Info("Successfully updated", "container", name)

	// Cleanup old container
	if err := oldC.Terminate(ctx); err != nil {
		slog.Warn("Failed to remove old container", "container", backupName, "error", err)
	}
}

func (u *Updater) pruneImages(ctx context.Context) {
	if !u.Cleanup {
		return
	}

	res, err := u.docker.ImagePrune(ctx, dockerclient.ImagePruneOptions{
		Filters: dockerclient.Filters{}.Add("dangling", "true"),
	})
	if err != nil {
		slog.Warn("Failed to prune images", "error", err)
		return
	}

	if len(res.Report.ImagesDeleted) > 0 {
		slog.Info(
			"Pruned dangling images",
			"count", len(res.Report.ImagesDeleted),
			"space_reclaimed", units.HumanSize(float64(res.Report.SpaceReclaimed)),
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

func (u *Updater) isSelf(c dockercontainer.Summary) bool {
	if u.hostname == "" {
		return false
	}
	// Check if container ID prefix matches our hostname
	return strings.HasPrefix(c.ID, u.hostname)
}
