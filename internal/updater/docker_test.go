package updater

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	sdkclient "github.com/docker/go-sdk/client"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	dockercontainer "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	dockerclient "github.com/moby/moby/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mizuchilabs/orbitd/internal/policy"
)

// fakeClient is a partial mock of the docker SDK client. It embeds the
// SDKClient interface so unimplemented methods fail loudly instead of being
// silently nil, and only overrides the methods the updater uses.
type fakeClient struct {
	sdkclient.SDKClient

	containers map[string]*dockercontainer.Summary
	inspects   map[string]dockerclient.ContainerInspectResult
	images     map[string]dockerclient.ImageInspectResult
	inspectErr error

	createID  string
	createErr error
	startErr  error
	stopErr   error
	removeErr error
	renameErr error

	pulls       []string
	created     []string
	createdName []string
	started     []string
	stopped     []string
	removed     []string
	renamed     []string

	imagesRemoved []string
	listed        []dockercontainer.Summary

	lastCreateOpts dockerclient.ContainerCreateOptions
	lastStopOpts   dockerclient.ContainerStopOptions
	removeOpts     []dockerclient.ContainerRemoveOptions
}

func newFakeClient() *fakeClient {
	return &fakeClient{
		containers: make(map[string]*dockercontainer.Summary),
		inspects:   make(map[string]dockerclient.ContainerInspectResult),
		images:     make(map[string]dockerclient.ImageInspectResult),
	}
}

// addContainer registers a running container and its inspect response.
func (f *fakeClient) addContainer() {
	id := "c1"
	name := "nginx"
	image := "nginx:1.25"
	f.containers[id] = &dockercontainer.Summary{
		ID:    id,
		Names: []string{"/" + name},
		Image: image,
		State: "running",
	}

	res := dockerclient.ContainerInspectResult{}
	res.Container = dockercontainer.InspectResponse{
		ID:     id,
		Name:   "/" + name,
		Image:  "sha256:old",
		State:  &dockercontainer.State{Running: true},
		Config: &dockercontainer.Config{Image: image},
		HostConfig: &dockercontainer.HostConfig{
			Binds: []string{},
		},
	}
	f.inspects[id] = res
	f.images["sha256:old"] = dockerclient.ImageInspectResult{ID: "sha256:old"}
}

// addImage registers an image ID for a reference.
func (f *fakeClient) addImage(id string) {
	res := dockerclient.ImageInspectResult{
		ID: id,
	}
	f.images["nginx:1.25"] = res
}

func (f *fakeClient) ContainerList(
	_ context.Context,
	_ dockerclient.ContainerListOptions,
) (dockerclient.ContainerListResult, error) {
	return dockerclient.ContainerListResult{Items: f.listed}, nil
}

func (f *fakeClient) ImageRemove(
	_ context.Context,
	id string,
	_ dockerclient.ImageRemoveOptions,
) (dockerclient.ImageRemoveResult, error) {
	f.imagesRemoved = append(f.imagesRemoved, id)
	return dockerclient.ImageRemoveResult{}, nil
}

func (f *fakeClient) Logger() *slog.Logger { return discardLogger() }
func (f *fakeClient) Close() error         { return nil }

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func (f *fakeClient) FindContainerByID(
	_ context.Context,
	id string,
) (*dockercontainer.Summary, error) {
	s, ok := f.containers[id]
	if !ok {
		return nil, errors.New("container not found")
	}
	return s, nil
}

func (f *fakeClient) ContainerInspect(
	_ context.Context,
	id string,
	_ dockerclient.ContainerInspectOptions,
) (dockerclient.ContainerInspectResult, error) {
	if f.inspectErr != nil {
		return dockerclient.ContainerInspectResult{}, f.inspectErr
	}
	res, ok := f.inspects[id]
	if !ok {
		return dockerclient.ContainerInspectResult{}, errors.New("container not found")
	}
	return res, nil
}

func (f *fakeClient) ImageInspect(
	_ context.Context,
	ref string,
	_ ...dockerclient.ImageInspectOption,
) (dockerclient.ImageInspectResult, error) {
	res, ok := f.images[ref]
	if !ok {
		return dockerclient.ImageInspectResult{}, errors.New("image not found")
	}
	return res, nil
}

func (f *fakeClient) ContainerCreate(
	_ context.Context,
	opts dockerclient.ContainerCreateOptions,
) (dockerclient.ContainerCreateResult, error) {
	if f.createErr != nil {
		return dockerclient.ContainerCreateResult{}, f.createErr
	}
	f.created = append(f.created, opts.Config.Image)
	f.createdName = append(f.createdName, opts.Name)
	f.lastCreateOpts = opts

	id := f.createID
	if id == "" {
		id = "newcontainerid"
	}
	return dockerclient.ContainerCreateResult{ID: id}, nil
}

func (f *fakeClient) ContainerStart(
	_ context.Context,
	id string,
	_ dockerclient.ContainerStartOptions,
) (dockerclient.ContainerStartResult, error) {
	f.started = append(f.started, id)
	return dockerclient.ContainerStartResult{}, f.startErr
}

func (f *fakeClient) ContainerStop(
	_ context.Context,
	id string,
	opts dockerclient.ContainerStopOptions,
) (dockerclient.ContainerStopResult, error) {
	f.stopped = append(f.stopped, id)
	f.lastStopOpts = opts
	return dockerclient.ContainerStopResult{}, f.stopErr
}

func (f *fakeClient) ContainerRemove(
	_ context.Context,
	id string,
	opts dockerclient.ContainerRemoveOptions,
) (dockerclient.ContainerRemoveResult, error) {
	f.removed = append(f.removed, id)
	f.removeOpts = append(f.removeOpts, opts)
	return dockerclient.ContainerRemoveResult{}, f.removeErr
}

func (f *fakeClient) ContainerRename(
	_ context.Context,
	_ string,
	opts dockerclient.ContainerRenameOptions,
) (dockerclient.ContainerRenameResult, error) {
	f.renamed = append(f.renamed, opts.NewName)
	return dockerclient.ContainerRenameResult{}, f.renameErr
}

// newTestUpdater builds an Updater wired to the fake client with a no-op pull.
func newTestUpdater(f *fakeClient) *Updater {
	u := &Updater{
		Policy:  policy.Digest,
		Cleanup: true,
		cli:     f,
	}
	u.pull = func(_ context.Context, img string) error {
		f.pulls = append(f.pulls, img)
		return nil
	}
	return u
}

func TestNamedImage(t *testing.T) {
	tests := []struct {
		name     string
		image    string
		config   string
		inspect  bool
		expected string
		ok       bool
	}{
		{"named summary", "nginx:1.25", "", false, "nginx:1.25", true},
		{"dangling recovers name", "sha256:deadbeef", "nginx:1.25", true, "nginx:1.25", true},
		{"digest only stays skipped", "sha256:deadbeef", "sha256:other", true, "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeClient()
			if tc.config != "" {
				info, ok := f.inspects["c1"]
				_ = info
				_ = ok
				res := dockerclient.ContainerInspectResult{}
				res.Container = dockercontainer.InspectResponse{
					Config: &dockercontainer.Config{Image: tc.config},
				}
				f.inspects["c1"] = res
			}

			c := dockercontainer.Summary{ID: "c1", Image: tc.image}
			got, ok := newTestUpdater(f).namedImage(context.Background(), c)
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.expected, got)
		})
	}
}

func TestUpdateDockerAlreadyUpToDate(t *testing.T) {
	f := newFakeClient()
	f.addImage("sha256:same")
	u := newTestUpdater(f)

	c := dockercontainer.Summary{
		ID:      "c1",
		Names:   []string{"/nginx"},
		Image:   "nginx:1.25",
		ImageID: "sha256:same",
	}
	u.updateDocker(context.Background(), nil, c)

	assert.Equal(t, []string{"nginx:1.25"}, f.pulls)
	assert.Empty(t, f.created)
	assert.Empty(t, f.started)
}

func TestUpdateDockerRecreates(t *testing.T) {
	f := newFakeClient()
	f.addImage("sha256:new")
	f.addContainer()
	u := newTestUpdater(f)

	c := dockercontainer.Summary{
		ID:      "c1",
		Names:   []string{"/nginx"},
		Image:   "nginx:1.25",
		ImageID: "sha256:old",
	}
	u.updateDocker(context.Background(), nil, c)

	assert.Equal(t, []string{"nginx:1.25"}, f.pulls)
	require.Len(t, f.created, 1)
	assert.Equal(t, "nginx:1.25", f.created[0])
	assert.Equal(t, "nginx", f.createdName[0])
	assert.Equal(t, []string{"newcontainerid"}, f.started)
	assert.Contains(t, f.removed, "c1")
}

func TestUpdateDockerRecoversDanglingImage(t *testing.T) {
	f := newFakeClient()
	f.addImage("sha256:new")
	f.addContainer()
	u := newTestUpdater(f)

	// The list API reports a bare image id because the image is dangling.
	c := dockercontainer.Summary{
		ID:      "c1",
		Names:   []string{"/nginx"},
		Image:   "sha256:deadbeef",
		ImageID: "sha256:old",
	}
	u.updateDocker(context.Background(), nil, c)

	assert.Equal(t, []string{"nginx:1.25"}, f.pulls)
	require.Len(t, f.created, 1)
	assert.Equal(t, "nginx:1.25", f.created[0])
}

func TestUpdateDockerPullError(t *testing.T) {
	f := newFakeClient()
	u := newTestUpdater(f)
	u.pull = func(context.Context, string) error { return errors.New("pull failed") }

	c := dockercontainer.Summary{
		ID:      "c1",
		Names:   []string{"/nginx"},
		Image:   "nginx:1.25",
		ImageID: "sha256:old",
	}
	u.updateDocker(context.Background(), nil, c)

	assert.Empty(t, f.created)
	assert.Empty(t, f.started)
}

func TestUpdateDockerSelfSkip(t *testing.T) {
	f := newFakeClient()
	f.addImage("sha256:new")
	u := newTestUpdater(f)
	u.selfID = "c1"

	c := dockercontainer.Summary{
		ID:      "c1",
		Names:   []string{"/orbitd"},
		Image:   "nginx:1.25",
		ImageID: "sha256:old",
	}
	u.updateDocker(context.Background(), nil, c)

	assert.Equal(t, []string{"nginx:1.25"}, f.pulls)
	assert.Empty(t, f.created)
	assert.Empty(t, f.started)
}

func TestRecreateDockerSuccess(t *testing.T) {
	f := newFakeClient()
	f.addContainer()
	u := newTestUpdater(f)

	_ = u.recreateDocker(context.Background(), "c1", "nginx:1.25", "sha256:new")

	assert.Equal(t, []string{"nginx-orbitd-old-c1"}, f.renamed)
	assert.Equal(t, []string{"nginx"}, f.createdName)
	assert.Equal(t, []string{"newcontainerid"}, f.started)
	assert.Contains(t, f.removed, "c1")
}

func TestRecreateDockerRollbackOnCreateError(t *testing.T) {
	f := newFakeClient()
	f.addContainer()
	f.createErr = errors.New("create failed")
	u := newTestUpdater(f)

	_ = u.recreateDocker(context.Background(), "c1", "nginx:1.25", "sha256:new")

	// Renamed away, then back on rollback.
	assert.Equal(t, []string{"nginx-orbitd-old-c1", "nginx"}, f.renamed)
	// Old container restarted.
	assert.Contains(t, f.started, "c1")
	assert.Empty(t, f.created)
}

func TestRecreateDockerRollbackOnRenameError(t *testing.T) {
	f := newFakeClient()
	f.addContainer()
	f.renameErr = errors.New("rename failed")
	u := newTestUpdater(f)

	_ = u.recreateDocker(context.Background(), "c1", "nginx:1.25", "sha256:new")

	assert.Contains(t, f.started, "c1")
	assert.Empty(t, f.created)
}

func TestRecreateDockerSkipAutoRemove(t *testing.T) {
	f := newFakeClient()
	f.addContainer()
	f.inspects["c1"].Container.HostConfig.AutoRemove = true
	u := newTestUpdater(f)

	_ = u.recreateDocker(context.Background(), "c1", "nginx:1.25", "sha256:new")

	assert.Empty(t, f.renamed)
	assert.Empty(t, f.created)
	assert.Empty(t, f.started)
}

func TestRecreateDockerPreservesVolumesAndNetworks(t *testing.T) {
	f := newFakeClient()
	f.addContainer()

	res := f.inspects["c1"]
	res.Container.ID = "c1"
	res.Container.Mounts = []dockercontainer.MountPoint{
		{Type: "volume", Name: "anonvol", Destination: "/data"},
		{Type: "volume", Name: "named", Destination: "/named"},
	}
	// "named" is already bound, so only the anonymous volume gets re-added.
	res.Container.HostConfig.Binds = []string{"named:/named"}
	res.Container.NetworkSettings = &dockercontainer.NetworkSettings{
		Networks: map[string]*network.EndpointSettings{
			"bridge": {Aliases: []string{"proxy"}},
		},
	}
	// Hostname equals shortID("c1") and must be cleared on recreate.
	res.Container.Config.Hostname = "c1"
	f.inspects["c1"] = res

	u := newTestUpdater(f)
	_ = u.recreateDocker(context.Background(), "c1", "nginx:1.25", "sha256:new")

	require.Len(t, f.created, 1)
	assert.Equal(t,
		[]string{"named:/named", "anonvol:/data"},
		f.lastCreateOpts.HostConfig.Binds,
	)
	assert.Equal(t,
		[]string{"proxy"},
		f.lastCreateOpts.NetworkingConfig.EndpointsConfig["bridge"].Aliases,
	)
	assert.Empty(t, f.lastCreateOpts.Config.Hostname)
}

func TestUpdateDockerVerifyError(t *testing.T) {
	f := newFakeClient()
	u := newTestUpdater(f)

	// Pull succeeds but the pulled image cannot be verified (not registered).
	c := dockercontainer.Summary{
		ID:      "c1",
		Names:   []string{"/nginx"},
		Image:   "nginx:1.25",
		ImageID: "sha256:old",
	}
	u.updateDocker(context.Background(), nil, c)

	assert.Equal(t, []string{"nginx:1.25"}, f.pulls)
	assert.Empty(t, f.created)
	assert.Empty(t, f.started)
}

func TestFilters(t *testing.T) {
	t.Run("require label", func(t *testing.T) {
		u := &Updater{RequireLabel: true}
		assert.True(t, u.filters()["label"]["orbitd.enable=true"])
	})

	t.Run("no label", func(t *testing.T) {
		u := &Updater{}
		assert.NotContains(t, u.filters(), "label")
	})
}

func TestRecreateDockerStopAndRemoveOptions(t *testing.T) {
	f := newFakeClient()
	f.addContainer()
	u := newTestUpdater(f)

	err := u.recreateDocker(context.Background(), "c1", "nginx:1.25", "sha256:new")
	require.NoError(t, err)

	// The daemon picks the container's own stop timeout.
	assert.Nil(t, f.lastStopOpts.Timeout)
	// Anonymous volumes moved to the new container and must survive.
	require.Len(t, f.removeOpts, 1)
	assert.False(t, f.removeOpts[0].RemoveVolumes)
}

func TestRecreateDockerSurvivesCancelledContext(t *testing.T) {
	f := newFakeClient()
	f.addContainer()
	u := newTestUpdater(f)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := u.recreateDocker(ctx, "c1", "nginx:1.25", "sha256:new")
	require.NoError(t, err)
}

func TestRecreateDockerStripsImageDefaults(t *testing.T) {
	f := newFakeClient()
	f.addContainer()

	res := f.inspects["c1"]
	res.Container.Config = &dockercontainer.Config{
		Image:      "nginx:1.25",
		Env:        []string{"PATH=/usr/bin", "NGINX_VERSION=1.25.0", "APP_MODE=prod"},
		Cmd:        []string{"nginx", "-g", "daemon off;"},
		Entrypoint: []string{"/docker-entrypoint.sh"},
		WorkingDir: "/srv",
		Labels: map[string]string{
			"org.opencontainers.image.version": "1.25.0",
			"com.docker.compose.image":         "sha256:old",
			"com.docker.compose.service":       "web",
		},
	}
	f.inspects["c1"] = res

	img := &dockerspec.DockerOCIImageConfig{}
	img.Env = []string{"PATH=/usr/bin", "NGINX_VERSION=1.25.0"}
	img.Cmd = []string{"nginx", "-g", "daemon off;"}
	img.Entrypoint = []string{"/docker-entrypoint.sh"}
	img.Labels = map[string]string{"org.opencontainers.image.version": "1.25.0"}
	f.images["sha256:old"] = dockerclient.ImageInspectResult{ID: "sha256:old", Config: img}

	u := newTestUpdater(f)
	err := u.recreateDocker(context.Background(), "c1", "nginx:1.25", "sha256:new")
	require.NoError(t, err)

	cfg := f.lastCreateOpts.Config
	assert.Equal(t, []string{"APP_MODE=prod"}, cfg.Env)
	assert.Nil(t, cfg.Cmd)
	assert.Nil(t, cfg.Entrypoint)
	assert.Equal(t, "/srv", cfg.WorkingDir)
	assert.Equal(t, map[string]string{
		"com.docker.compose.image":   "sha256:new",
		"com.docker.compose.service": "web",
	}, cfg.Labels)

	// The inspected config is left untouched.
	assert.Equal(t, "sha256:old", res.Container.Config.Labels["com.docker.compose.image"])
}

func TestStripImageDefaultsKeepsCmdWithCustomEntrypoint(t *testing.T) {
	img := &dockerspec.DockerOCIImageConfig{}
	img.Entrypoint = []string{"/entry.sh"}
	img.Cmd = []string{"serve"}

	// With a custom entrypoint, Docker never fills in the image Cmd, so a
	// matching Cmd was passed explicitly and must be kept.
	c := stripImageDefaults(dockercontainer.Config{
		Entrypoint: []string{"/bin/sh", "-c"},
		Cmd:        []string{"serve"},
	}, img)
	assert.Equal(t, []string{"/bin/sh", "-c"}, c.Entrypoint)
	assert.Equal(t, []string{"serve"}, c.Cmd)
}

func TestHostConfigForMountVolumes(t *testing.T) {
	c := dockercontainer.InspectResponse{
		HostConfig: &dockercontainer.HostConfig{
			Binds: []string{"/host:/bound:ro"},
			Mounts: []mount.Mount{
				{Type: mount.TypeVolume, Source: "named", Target: "/named"},
				{Type: mount.TypeVolume, Target: "/anon-mount"},
			},
		},
		Mounts: []dockercontainer.MountPoint{
			{Type: mount.TypeBind, Source: "/host", Destination: "/bound"},
			{Type: mount.TypeVolume, Name: "named", Destination: "/named"},
			{Type: mount.TypeVolume, Name: "abc123", Destination: "/anon-mount"},
			{Type: mount.TypeVolume, Name: "def456", Destination: "/image-volume"},
		},
	}

	hc := hostConfigFor(c)

	// Volumes from --mount are not duplicated into Binds; only the image's
	// anonymous volume is pinned there.
	assert.Equal(t, []string{"/host:/bound:ro", "def456:/image-volume"}, hc.Binds)
	// The anonymous --mount volume is pinned to its existing name.
	assert.Equal(t, "abc123", hc.Mounts[1].Source)
	assert.Empty(t, c.HostConfig.Mounts[1].Source, "original config mutated")
}

func TestUpdateDockerSkipsDigestPinned(t *testing.T) {
	f := newFakeClient()
	u := newTestUpdater(f)

	u.updateDocker(context.Background(), nil, dockercontainer.Summary{
		ID:      "c1",
		Image:   "nginx:1.25@sha256:abc",
		ImageID: "sha256:old",
	})

	assert.Empty(t, f.pulls)
}

func TestUpdateDockerRemovesOldImage(t *testing.T) {
	f := newFakeClient()
	f.addImage("sha256:new")
	f.addContainer()
	u := newTestUpdater(f)

	c := dockercontainer.Summary{ID: "c1", Image: "nginx:1.25", ImageID: "sha256:old"}
	u.updateDocker(context.Background(), nil, c)
	assert.Equal(t, []string{"sha256:old"}, f.imagesRemoved)

	f = newFakeClient()
	f.addImage("sha256:new")
	f.addContainer()
	u = newTestUpdater(f)
	u.Cleanup = false
	u.updateDocker(context.Background(), nil, c)
	assert.Empty(t, f.imagesRemoved)
}

func TestListDockerPartitions(t *testing.T) {
	f := newFakeClient()
	f.listed = []dockercontainer.Summary{
		{ID: "plain"},
		{ID: "task", Labels: map[string]string{swarmServiceLabel: "svc"}},
		{ID: "dependent"},
	}
	f.listed[2].HostConfig.NetworkMode = "container:plain"
	u := newTestUpdater(f)

	parents, err := u.listDocker(context.Background(), false)
	require.NoError(t, err)
	require.Len(t, parents, 1)
	assert.Equal(t, "plain", parents[0].ID)

	dependents, err := u.listDocker(context.Background(), true)
	require.NoError(t, err)
	require.Len(t, dependents, 1)
	assert.Equal(t, "dependent", dependents[0].ID)
}

func TestRecreateDockerRepointsDependents(t *testing.T) {
	f := newFakeClient()
	f.addContainer()

	dep := dockercontainer.Summary{ID: "d1", Names: []string{"/sidecar"}}
	dep.HostConfig.NetworkMode = "container:c1"
	f.listed = []dockercontainer.Summary{dep}

	res := dockerclient.ContainerInspectResult{}
	res.Container = dockercontainer.InspectResponse{
		ID:     "d1",
		Name:   "/sidecar",
		State:  &dockercontainer.State{Running: true},
		Config: &dockercontainer.Config{Image: "sidecar:1", Hostname: "c1"},
		HostConfig: &dockercontainer.HostConfig{
			NetworkMode: "container:c1",
		},
	}
	f.inspects["d1"] = res

	u := newTestUpdater(f)
	err := u.recreateDocker(context.Background(), "c1", "nginx:1.25", "sha256:new")
	require.NoError(t, err)

	assert.Equal(t, []string{"nginx", "sidecar"}, f.createdName)
	assert.Equal(t,
		dockercontainer.NetworkMode("container:newcontainerid"),
		f.lastCreateOpts.HostConfig.NetworkMode,
	)
	assert.Nil(t, f.lastCreateOpts.NetworkingConfig)
	assert.Empty(t, f.lastCreateOpts.Config.Hostname)
	assert.ElementsMatch(t, []string{"c1", "d1"}, f.removed)
}
