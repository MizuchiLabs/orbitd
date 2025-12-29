// Package updater provides container update functionality by monitoring Docker
// containers and automatically updating them to the latest image versions.
package updater

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"strings"
	"time"

	"github.com/docker/go-sdk/client"
	"github.com/docker/go-sdk/container"
	"github.com/docker/go-sdk/image"
	"github.com/mizuchilabs/orbitd/internal/config"
	dockercontainer "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	dockerclient "github.com/moby/moby/client"
)

// Updater manages the container update process.
type Updater struct {
	cfg    *config.Config
	docker client.SDKClient
}

// New creates a new Updater instance and starts the update daemon.
// It establishes a connection to the Docker daemon and begins monitoring containers.
func New(ctx context.Context, cfg *config.Config) error {
	cli, err := client.New(context.Background())
	if err != nil {
		return fmt.Errorf("failed to create docker client: %w", err)
	}

	updater := &Updater{cfg: cfg, docker: cli}
	slog.Info("Starting orbitd", "interval", cfg.Interval)
	return updater.Start(ctx)
}

// Start begins the update daemon's main loop, checking for updates at the configured interval.
// It runs an immediate check on startup, then continues on the ticker schedule.
// The function blocks until the context is cancelled.
func (u *Updater) Start(ctx context.Context) error {
	ticker := time.NewTicker(u.cfg.Interval)
	defer ticker.Stop()

	// Run immediately on start
	if err := u.RunOnce(ctx); err != nil {
		slog.Error("Error during update check", "error", err)
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := u.RunOnce(ctx); err != nil {
				slog.Error("Error during update check", "error", err)
			}
		}
	}
}

// RunOnce performs a single check and update cycle for all running containers.
// It iterates through all containers, checking each for available updates.
func (u *Updater) RunOnce(ctx context.Context) error {
	containers, err := u.docker.ContainerList(ctx, dockerclient.ContainerListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list containers: %w", err)
	}

	for _, c := range containers.Items {
		containerName := strings.Split(c.Names[0], "/")[1]
		slog.Debug("Checking container", "container", containerName)
		if err := u.updateContainer(ctx, c.Image, c.ImageID, c.ID); err != nil {
			slog.Error("Failed to update container", "container", containerName, "error", err)
		}

		// Small delay between container updates to avoid overwhelming the Docker API
		time.Sleep(1 * time.Second)
	}
	return nil
}

// updateContainer handles the update process for a single container.
// It pulls the latest image, compares digests, and recreates the container if an update is available.
// All container configuration, networks, volumes, and labels are preserved during the update.
func (u *Updater) updateContainer(
	ctx context.Context,
	imageName, imageID, containerID string,
) error {
	// Get current image digest before pull to detect if update is needed
	oldDigest, err := u.getImageDigest(ctx, imageName)
	if err != nil {
		slog.Debug("Could not get old image digest", "image", imageName, "error", err)
	}

	// Pull latest image from registry
	if err := image.Pull(
		ctx,
		imageName,
		image.WithPullHandler(func(r io.ReadCloser) error {
			_, err := io.Copy(io.Discard, r)
			return err
		}),
	); err != nil {
		return fmt.Errorf("failed to pull image: %w", err)
	}

	// Get new image digest after pull to compare with old
	newDigest, err := u.getImageDigest(ctx, imageName)
	if err != nil {
		return fmt.Errorf("failed to get new image digest: %w", err)
	}

	// Skip update if image hasn't changed
	if oldDigest != "" && oldDigest == newDigest {
		slog.Debug("Image already up to date", "image", imageName)
		return nil
	}

	// Inspect the old container to preserve its configuration
	oldContainer, err := container.FromID(ctx, u.docker, containerID)
	if err != nil {
		return fmt.Errorf("failed to get container from ID: %w", err)
	}
	ins, err := oldContainer.Inspect(ctx)
	if err != nil {
		return fmt.Errorf("failed to inspect container: %w", err)
	}

	// Only update running containers - stopped containers are left alone
	if oldContainer.IsRunning() {
		if err := oldContainer.Terminate(ctx); err != nil {
			return fmt.Errorf("failed to terminate container: %w", err)
		}
	} else {
		slog.Debug("Container stopped, ignoring restart", "id", containerID)
		return nil
	}

	// Recreate container with updated image while preserving all configuration
	ctr, err := container.Run(
		ctx,
		container.WithImage(imageName),
		container.WithName(ins.Container.Name),
		container.WithConfigModifier(
			func(config *dockercontainer.Config) {
				*config = *ins.Container.Config
				config.Image = imageName
			},
		),
		container.WithHostConfigModifier(
			func(hostConfig *dockercontainer.HostConfig) {
				*hostConfig = *ins.Container.HostConfig
			},
		),
		container.WithEndpointSettingsModifier(
			func(endpointsConfig map[string]*network.EndpointSettings) {
				maps.Copy(endpointsConfig, ins.Container.NetworkSettings.Networks)
			},
		),
	)
	if err != nil {
		return fmt.Errorf("failed to run container: %w", err)
	}

	slog.Info("Successfully updated container", "image", ctr.Image())

	// Cleanup old image if configured to free up disk space
	if u.cfg.Cleanup {
		res, err := image.Remove(
			ctx,
			imageID,
			image.WithRemoveOptions(dockerclient.ImageRemoveOptions{
				Force:         false,
				PruneChildren: false,
			}),
		)
		if err != nil {
			slog.Warn("Failed to remove old image", "error", err)
		}
		for _, r := range res.Items {
			slog.Debug("Removed old image", "id", r.Deleted)
		}
	}

	return nil
}

// getImageDigest retrieves the digest or ID of an image for comparison.
// It prefers RepoDigests for registry-tracked images, falling back to the local ID.
func (u *Updater) getImageDigest(ctx context.Context, imageName string) (string, error) {
	inspect, err := u.docker.ImageInspect(ctx, imageName)
	if err != nil {
		return "", err
	}

	// Use RepoDigests if available (more reliable for updates)
	if len(inspect.RepoDigests) > 0 {
		return inspect.RepoDigests[0], nil
	}

	// Fallback to ID
	return inspect.ID, nil
}
