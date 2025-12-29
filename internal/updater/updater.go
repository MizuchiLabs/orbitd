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
	cli, err := client.New(ctx)
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

		// Skip containers with disable label
		if c.Labels["orbitd.enable"] == "false" {
			slog.Debug("Skipping disabled container", "container", containerName)
			continue
		}

		slog.Debug("Checking container", "container", containerName)
		if err := u.updateContainer(ctx, c); err != nil {
			slog.Error("Failed to update container", "container", containerName, "error", err)
		}

		// Small delay between container updates to avoid overwhelming the Docker API
		time.Sleep(1 * time.Second)
	}
	return nil
}

// updateContainer checks based on the policy if an update is available and applies it if needed.
func (u *Updater) updateContainer(ctx context.Context, c dockercontainer.Summary) error {
	policy := u.getPolicy(c.Labels)
	targetImage := c.Image

	// For semver policies, find the target version
	if policy != PolicyDigest {
		target, err := FindUpdateTarget(c.Image, policy)
		if err != nil {
			return fmt.Errorf("failed to find update target: %w", err)
		}
		if target == "" {
			slog.Debug("No update available", "image", c.Image, "policy", policy)
			return nil
		}
		if target != c.Image {
			slog.Info("Found update", "from", c.Image, "to", target, "policy", policy)
			targetImage = target
		}
	}

	// Get digest before pull for comparison
	oldDigest, _ := u.getImageDigest(ctx, targetImage)

	// Pull the image
	if err := image.Pull(
		ctx,
		targetImage,
		image.WithPullClient(u.docker),
		image.WithPullHandler(func(r io.ReadCloser) error {
			_, err := io.Copy(io.Discard, r)
			return err
		}),
	); err != nil {
		return fmt.Errorf("failed to pull image: %w", err)
	}

	// Check if update is needed
	newDigest, err := u.getImageDigest(ctx, targetImage)
	if err != nil {
		return fmt.Errorf("failed to get new image digest: %w", err)
	}

	if targetImage == c.Image && oldDigest != "" && oldDigest == newDigest {
		slog.Debug("Image already up to date", "image", targetImage)
		return nil
	}

	return u.recreateContainer(ctx, targetImage, c.ImageID, c.ID)
}

func (u *Updater) recreateContainer(
	ctx context.Context,
	imageName, oldImageID, containerID string,
) error {
	oldContainer, err := container.FromID(ctx, u.docker, containerID)
	if err != nil {
		return fmt.Errorf("failed to get container from ID: %w", err)
	}
	ins, err := oldContainer.Inspect(ctx)
	if err != nil {
		return fmt.Errorf("failed to inspect container: %w", err)
	}

	if !oldContainer.IsRunning() {
		slog.Debug("Container stopped, ignoring restart", "id", containerID)
		return nil
	}

	if err := oldContainer.Terminate(ctx); err != nil {
		return fmt.Errorf("failed to terminate container: %w", err)
	}

	ctr, err := container.Run(
		ctx,
		container.WithClient(u.docker),
		container.WithImage(imageName),
		container.WithName(strings.TrimPrefix(ins.Container.Name, "/")),
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

	u.cleanupImage(ctx, oldImageID)
	return nil
}

func (u *Updater) cleanupImage(ctx context.Context, imageID string) {
	if !u.cfg.Cleanup {
		return
	}

	res, err := image.Remove(
		ctx,
		imageID,
		image.WithRemoveOptions(dockerclient.ImageRemoveOptions{
			Force:         false,
			PruneChildren: true,
		}),
	)
	if err != nil {
		slog.Warn("Failed to remove image", "error", err)
		return
	}

	// Only log actually deleted/untagged images
	for _, r := range res.Items {
		if r.Deleted != "" {
			slog.Debug("Removed image", "id", r.Deleted)
		}
		if r.Untagged != "" {
			slog.Debug("Untagged image", "id", r.Untagged)
		}
	}
}

// getPolicy returns the update policy for a container, falling back to the global policy if not set.
func (u *Updater) getPolicy(labels map[string]string) UpdatePolicy {
	if p, ok := labels["orbitd.policy"]; ok {
		return UpdatePolicy(p)
	}
	return UpdatePolicy(u.cfg.Policy)
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
