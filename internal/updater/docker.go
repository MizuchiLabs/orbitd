package updater

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/docker/go-sdk/image"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	dockercontainer "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	dockerclient "github.com/moby/moby/client"
)

const (
	swarmServiceLabel = "com.docker.swarm.service.id"
	composeImageLabel = "com.docker.compose.image"

	pullTimeout     = 15 * time.Minute
	recreateTimeout = 10 * time.Minute
)

func (u *Updater) checkDocker(ctx context.Context) {
	r := &run{}

	// Dependents (network_mode: container:...) go last, as updating their
	// parent recreates them.
	for _, dependents := range []bool{false, true} {
		items, err := u.listDocker(ctx, dependents)
		if err != nil {
			slog.Error("Failed to list containers", "error", err)
			return
		}
		updateAll(ctx, items, func(ctx context.Context, c dockercontainer.Summary) {
			u.updateDocker(ctx, r, c)
		})
		if ctx.Err() != nil {
			return
		}
	}
}

func (u *Updater) listDocker(
	ctx context.Context,
	dependents bool,
) ([]dockercontainer.Summary, error) {
	res, err := u.cli.ContainerList(ctx, dockerclient.ContainerListOptions{Filters: u.filters()})
	if err != nil {
		return nil, err
	}

	var items []dockercontainer.Summary
	for _, c := range res.Items {
		// Swarm tasks belong to their service
		if c.Labels[swarmServiceLabel] != "" {
			continue
		}
		if isContainerNetwork(c.HostConfig.NetworkMode) == dependents {
			items = append(items, c)
		}
	}

	slog.Debug("Found containers", "count", len(items), "dependents", dependents)
	return items, nil
}

func (u *Updater) updateDocker(ctx context.Context, r *run, c dockercontainer.Summary) {
	imageRef, ok := u.namedImage(ctx, c)
	if !ok {
		return
	}

	name := containerName(c)
	if strings.Contains(imageRef, "@") {
		slog.Debug("Skipping container pinned to a digest", "container", name, "image", imageRef)
		return
	}

	res, err := u.resolveTargetImage(ctx, r, imageRef, c.Labels)
	if err != nil {
		slog.Warn("Could not resolve target image", "image", imageRef, "error", err)
		return
	}
	if res.target != res.current {
		slog.Info("Update found", "from", res.current, "to", res.target, "policy", res.policy)
	}

	if err := r.pull(res.target, func() error { return u.pullImage(ctx, res.target) }); err != nil {
		slog.Warn("Pull failed", "image", res.target, "error", err)
		return
	}

	targetImage, err := u.cli.ImageInspect(ctx, res.target)
	if err != nil {
		slog.Warn("Could not verify pulled image", "image", res.target, "error", err)
		return
	}

	if !isNewDockerImage(c.ImageID, targetImage.ID) {
		slog.Debug("Already up to date", "container", name, "image", res.target)
		return
	}

	if u.isSelfDocker(c) {
		slog.Info("Update available for orbitd, restart to apply")
		return
	}

	slog.Info("Updating container", "container", name, "image", res.target)
	if err := u.recreateDocker(ctx, c.ID, res.target, targetImage.ID); err != nil {
		slog.Error("Update failed", "container", name, "error", err)
		return
	}
	slog.Info("Updated successfully", "container", name)

	u.removeImage(ctx, c.ImageID)
}

// namedImage falls back to the configured image when the list API reports a
// bare sha256 ID, which happens once the image is dangling.
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

func (u *Updater) recreateDocker(ctx context.Context, id, image, imageID string) error {
	// Never abandon a swap halfway, even on shutdown.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recreateTimeout)
	defer cancel()

	info, err := u.cli.ContainerInspect(ctx, id, dockerclient.ContainerInspectOptions{})
	if err != nil {
		return fmt.Errorf("inspect container: %w", err)
	}
	old := info.Container
	if old.Config == nil || old.HostConfig == nil {
		return errors.New("container metadata incomplete")
	}
	if old.HostConfig.AutoRemove {
		return errors.New("container has AutoRemove (--rm) enabled")
	}

	oldImage, err := u.cli.ImageInspect(ctx, old.Image)
	if err != nil {
		return fmt.Errorf("inspect current image: %w", err)
	}

	config := stripImageDefaults(*old.Config, oldImage.Config)
	config.Image = image
	if config.Hostname == shortID(old.ID) {
		config.Hostname = ""
	}
	// Compose recreates containers whose label differs from the local image.
	if _, ok := config.Labels[composeImageLabel]; ok {
		config.Labels[composeImageLabel] = imageID
	}

	newID, err := u.replaceDocker(ctx, old, dockerclient.ContainerCreateOptions{
		Config:           &config,
		HostConfig:       hostConfigFor(old),
		NetworkingConfig: networkingConfigFor(old),
	})
	if err != nil {
		return err
	}

	u.repointDependents(ctx, old, newID)
	return nil
}

// replaceDocker swaps old for a new container under the same name, restoring
// old if the new one fails.
func (u *Updater) replaceDocker(
	ctx context.Context,
	old dockercontainer.InspectResponse,
	opts dockerclient.ContainerCreateOptions,
) (string, error) {
	name := strings.TrimPrefix(old.Name, "/")
	running := old.State != nil && old.State.Running

	// No timeout, so the daemon uses the container's own stop timeout
	if running {
		if _, err := u.cli.ContainerStop(ctx, old.ID, dockerclient.ContainerStopOptions{}); err != nil {
			return "", fmt.Errorf("stop: %w", err)
		}
	}

	backupName := name + "-orbitd-old-" + shortID(old.ID)
	if _, err := u.cli.ContainerRename(
		ctx,
		old.ID,
		dockerclient.ContainerRenameOptions{NewName: backupName},
	); err != nil {
		return "", u.restore(ctx, old.ID, "", running, fmt.Errorf("rename: %w", err))
	}

	opts.Name = name
	resp, err := u.cli.ContainerCreate(ctx, opts)
	if err == nil && running {
		_, err = u.cli.ContainerStart(ctx, resp.ID, dockerclient.ContainerStartOptions{})
	}
	if err != nil {
		if resp.ID != "" {
			_, _ = u.cli.ContainerRemove(
				ctx,
				resp.ID,
				dockerclient.ContainerRemoveOptions{Force: true},
			)
		}
		return "", u.restore(ctx, old.ID, name, running, err)
	}

	// Keep volumes, they now belong to the new container
	if _, err := u.cli.ContainerRemove(
		ctx,
		old.ID,
		dockerclient.ContainerRemoveOptions{Force: true},
	); err != nil {
		slog.Warn("Failed to remove old container", "container", backupName, "error", err)
	}
	return resp.ID, nil
}

// restore renames the old container back (unless name is empty) and restarts
// it, returning cause annotated with the outcome.
func (u *Updater) restore(ctx context.Context, id, name string, running bool, cause error) error {
	if name != "" {
		if _, err := u.cli.ContainerRename(
			ctx,
			id,
			dockerclient.ContainerRenameOptions{NewName: name},
		); err != nil {
			return fmt.Errorf("%w; rollback failed, rename back to %s: %w", cause, name, err)
		}
	}
	if running {
		if _, err := u.cli.ContainerStart(ctx, id, dockerclient.ContainerStartOptions{}); err != nil {
			return fmt.Errorf("%w; rollback failed, container is down: %w", cause, err)
		}
	}
	return fmt.Errorf("%w (rolled back to previous container)", cause)
}

// repointDependents recreates containers sharing old's network namespace so
// they join newID instead of a removed container.
func (u *Updater) repointDependents(
	ctx context.Context,
	old dockercontainer.InspectResponse,
	newID string,
) {
	res, err := u.cli.ContainerList(ctx, dockerclient.ContainerListOptions{All: true})
	if err != nil {
		slog.Warn("Failed to list dependent containers", "error", err)
		return
	}

	name := strings.TrimPrefix(old.Name, "/")
	for _, c := range res.Items {
		mode := c.HostConfig.NetworkMode
		if mode != "container:"+old.ID && mode != "container:"+name {
			continue
		}

		depName := containerName(c)
		info, err := u.cli.ContainerInspect(ctx, c.ID, dockerclient.ContainerInspectOptions{})
		if err != nil || info.Container.Config == nil || info.Container.HostConfig == nil {
			slog.Error("Failed to inspect dependent container", "container", depName, "error", err)
			continue
		}

		dep := info.Container
		config := *dep.Config
		// Shared namespaces cannot set their own hostname
		config.Hostname, config.Domainname = "", ""
		hostConfig := hostConfigFor(dep)
		hostConfig.NetworkMode = dockercontainer.NetworkMode("container:" + newID)

		if _, err := u.replaceDocker(ctx, dep, dockerclient.ContainerCreateOptions{
			Config:     &config,
			HostConfig: hostConfig,
		}); err != nil {
			slog.Error("Failed to reattach dependent container", "container", depName, "error", err)
			continue
		}
		slog.Info("Reattached dependent container", "container", depName, "network", name)
	}
}

// stripImageDefaults drops values inherited from the image, so the new
// image's defaults apply.
func stripImageDefaults(
	c dockercontainer.Config,
	img *dockerspec.DockerOCIImageConfig,
) dockercontainer.Config {
	c.Labels = maps.Clone(c.Labels)
	if img == nil {
		return c
	}

	c.Env = slices.DeleteFunc(slices.Clone(c.Env), func(e string) bool {
		return slices.Contains(img.Env, e)
	})
	maps.DeleteFunc(c.Labels, func(k, v string) bool {
		iv, ok := img.Labels[k]
		return ok && iv == v
	})

	// A custom entrypoint means Cmd was set explicitly too
	if slices.Equal(c.Entrypoint, img.Entrypoint) {
		c.Entrypoint = nil
		if slices.Equal(c.Cmd, img.Cmd) {
			c.Cmd = nil
		}
	}

	if c.WorkingDir == img.WorkingDir {
		c.WorkingDir = ""
	}
	if c.User == img.User {
		c.User = ""
	}
	if c.StopSignal == img.StopSignal {
		c.StopSignal = ""
	}
	if slices.Equal(c.Shell, img.Shell) {
		c.Shell = nil
	}
	if reflect.DeepEqual(c.Healthcheck, img.Healthcheck) {
		c.Healthcheck = nil
	}

	c.ExposedPorts = maps.Clone(c.ExposedPorts)
	maps.DeleteFunc(c.ExposedPorts, func(p network.Port, _ struct{}) bool {
		_, ok := img.ExposedPorts[p.String()]
		return ok
	})
	c.Volumes = maps.Clone(c.Volumes)
	maps.DeleteFunc(c.Volumes, func(v string, _ struct{}) bool {
		_, ok := img.Volumes[v]
		return ok
	})
	return c
}

// hostConfigFor copies the host config and pins anonymous volumes by name, so
// the new container reuses their data.
func hostConfigFor(c dockercontainer.InspectResponse) *dockercontainer.HostConfig {
	hc := *c.HostConfig
	hc.Binds = slices.Clone(hc.Binds)
	hc.Mounts = slices.Clone(hc.Mounts)

	volumes := make(map[string]string) // destination -> name
	for _, m := range c.Mounts {
		if m.Type == mount.TypeVolume && m.Name != "" {
			volumes[m.Destination] = m.Name
		}
	}

	covered := make(map[string]bool)
	for _, b := range hc.Binds {
		if _, dst, ok := strings.Cut(b, ":"); ok {
			dst, _, _ = strings.Cut(dst, ":")
			covered[dst] = true
		}
	}
	for i, m := range hc.Mounts {
		covered[m.Target] = true
		if m.Type == mount.TypeVolume && m.Source == "" {
			hc.Mounts[i].Source = volumes[m.Target]
		}
	}

	for _, m := range c.Mounts {
		if name, ok := volumes[m.Destination]; ok && !covered[m.Destination] {
			hc.Binds = append(hc.Binds, name+":"+m.Destination)
		}
	}
	return &hc
}

// networkingConfigFor copies network attachments without runtime state like
// IPs and endpoint IDs.
func networkingConfigFor(c dockercontainer.InspectResponse) *network.NetworkingConfig {
	endpoints := make(map[string]*network.EndpointSettings)
	if c.NetworkSettings != nil {
		for name, n := range c.NetworkSettings.Networks {
			endpoints[name] = &network.EndpointSettings{
				IPAMConfig: n.IPAMConfig,
				Links:      n.Links,
				Aliases:    n.Aliases,
				GwPriority: n.GwPriority,
				DriverOpts: n.DriverOpts,
			}
		}
	}
	return &network.NetworkingConfig{EndpointsConfig: endpoints}
}

func (u *Updater) pullImage(ctx context.Context, img string) error {
	if u.pull != nil {
		return u.pull(ctx, img)
	}
	return u.pullDocker(ctx, img)
}

func (u *Updater) pullDocker(ctx context.Context, img string) error {
	ctx, cancel := context.WithTimeout(ctx, pullTimeout)
	defer cancel()

	return image.Pull(ctx, img,
		image.WithPullClient(u.cli),
		image.WithPullHandler(func(r io.ReadCloser) error {
			_, err := io.Copy(io.Discard, r)
			return err
		}),
	)
}

// removeImage deletes the replaced image. The daemon refuses while other
// containers still use it, which is fine.
func (u *Updater) removeImage(ctx context.Context, id string) {
	if !u.Cleanup || id == "" {
		return
	}

	if _, err := u.cli.ImageRemove(
		ctx,
		id,
		dockerclient.ImageRemoveOptions{PruneChildren: true},
	); err != nil {
		slog.Debug("Keeping old image", "image", id, "error", err)
		return
	}
	slog.Info("Removed old image", "image", id)
}

func (u *Updater) isSelfDocker(c dockercontainer.Summary) bool {
	return u.selfID != "" && strings.HasPrefix(c.ID, u.selfID)
}

func isNewDockerImage(current, target string) bool {
	return current != "" && target != "" && current != target
}

func isContainerNetwork(mode string) bool {
	return strings.HasPrefix(mode, "container:")
}

func containerName(c dockercontainer.Summary) string {
	if len(c.Names) > 0 {
		return strings.TrimPrefix(c.Names[0], "/")
	}
	return shortID(c.ID)
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
