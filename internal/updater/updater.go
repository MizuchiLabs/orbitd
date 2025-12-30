// Package updater provides container update functionality by monitoring Docker
// containers and automatically updating them to the latest image versions.
package updater

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
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

	slog.Info("Starting orbitd", "interval", cfg.Interval)
	updater := &Updater{cfg: cfg, docker: cli}
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
		u.updateContainer(ctx, c)

		// Small delay between container updates to avoid overwhelming the Docker API
		time.Sleep(1 * time.Second)
	}
	return nil
}

// updateContainer checks based on the policy if an update is available and applies it if needed.
func (u *Updater) updateContainer(ctx context.Context, c dockercontainer.Summary) {
	// Check if the image is a digest without a tag
	if strings.HasPrefix(c.Image, "sha256:") {
		slog.Warn("Container running with untagged digest, skipping update", "image", c.Image)
		return
	}

	policy := u.getPolicy(c.Labels)
	targetImage := c.Image

	// For semver policies, find the target version
	if policy != PolicyDigest {
		target, err := FindUpdateTarget(c.Image, policy)
		if err != nil {
			slog.Warn("Failed to find update target, skipping", "image", c.Image, "error", err)
			return
		}
		if target == "" {
			slog.Debug("No update available", "image", c.Image, "policy", policy)
			return
		}
		if target != c.Image {
			slog.Info("Found update", "from", c.Image, "to", target, "policy", policy)
			targetImage = target
		}
	}

	// Get digest before pull for comparison
	oldDigest, _ := u.getImageDigest(ctx, targetImage)

	// Pull the image with timeout protection
	pullCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()

	// Pull the image
	if err := image.Pull(
		pullCtx,
		targetImage,
		image.WithPullClient(u.docker),
		image.WithPullHandler(func(r io.ReadCloser) error {
			_, err := io.Copy(io.Discard, r)
			return err
		}),
	); err != nil {
		slog.Warn("Failed to pull image, will retry next cycle", "image", targetImage, "error", err)
		return
	}

	// Skip self-updates
	if u.isSelf(c) {
		slog.Info("Self-update detected, skipping...")
		return
	}

	// Check if update is needed
	newDigest, err := u.getImageDigest(ctx, targetImage)
	if err != nil {
		slog.Warn("Failed to get new image digest", "image", targetImage, "error", err)
		return
	}

	if targetImage == c.Image && oldDigest != "" && oldDigest == newDigest {
		slog.Debug("Image already up to date", "image", targetImage)
		return
	}

	u.recreateContainer(ctx, targetImage, c.ImageID, c.ID)
}

func (u *Updater) recreateContainer(
	ctx context.Context,
	imageName, oldImageID, containerID string,
) {
	oldContainer, err := container.FromID(ctx, u.docker, containerID)
	if err != nil {
		slog.Error("Failed to get container", "image", imageName, "error", err)
		return
	}
	ins, err := oldContainer.Inspect(ctx)
	if err != nil {
		slog.Error("Failed to inspect container", "image", imageName, "error", err)
		return
	}

	containerName := strings.TrimPrefix(ins.Container.Name, "/")

	if !oldContainer.IsRunning() {
		slog.Debug("Container stopped, ignoring restart", "container", containerName)
		return
	}

	// Stop the container but don't remove it yet (for rollback)
	if err := oldContainer.Stop(ctx); err != nil {
		slog.Error("Failed to stop container", "container", containerName, "error", err)
		return
	}

	// Rename old container to free up the name
	backupName := containerName + "-orbitd-old"
	if _, err := u.docker.ContainerRename(
		ctx,
		containerID,
		dockerclient.ContainerRenameOptions{NewName: backupName},
	); err != nil {
		slog.Error("Failed to rename old container", "container", containerName, "error", err)
		// Try to restart old container with original name
		if startErr := oldContainer.Start(ctx); startErr != nil {
			slog.Error("Failed to restart container after rename failure",
				"container", containerName, "error", startErr)
		}
		return
	}

	_, err = container.Run(
		ctx,
		container.WithClient(u.docker),
		container.WithImage(imageName),
		container.WithName(containerName),
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
		slog.Error("Failed to start new container", "container", containerName, "error", err)

		// Rollback: rename old container back and restart it
		if _, renameErr := u.docker.ContainerRename(ctx, containerID, dockerclient.ContainerRenameOptions{
			NewName: containerName,
		}); renameErr != nil {
			slog.Error(
				"Failed to rename container back during rollback",
				"container",
				containerName,
				"error",
				renameErr,
			)
			return
		}

		if startErr := oldContainer.Start(ctx); startErr != nil {
			slog.Error(
				"Rollback failed, container is DOWN",
				"container",
				containerName,
				"error",
				startErr,
			)
			return
		}

		slog.Info("Successfully rolled back container", "container", containerName)
		return
	}

	slog.Info("Successfully updated container", "container", containerName)

	// Remove old container
	if err := oldContainer.Terminate(ctx); err != nil {
		slog.Warn("Failed to remove old container", "container", backupName, "error", err)
	}

	u.cleanupImage(ctx, oldImageID)
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

func (u *Updater) isSelf(c dockercontainer.Summary) bool {
	// Read our own container ID from /proc/self/cgroup or hostname
	hostname, err := os.Hostname()
	if err != nil {
		return false
	}

	// Docker sets hostname to container ID by default (first 12 chars)
	// Check if our hostname matches the container ID prefix
	return strings.HasPrefix(c.ID, hostname)
}
