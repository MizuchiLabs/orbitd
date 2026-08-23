package updater

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	sdkclient "github.com/docker/go-sdk/client"
	dockercontainer "github.com/moby/moby/api/types/container"
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

	lastCreateOpts dockerclient.ContainerCreateOptions
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
		State:  &dockercontainer.State{Running: true},
		Config: &dockercontainer.Config{Image: image},
		HostConfig: &dockercontainer.HostConfig{
			Binds: []string{},
		},
	}
	f.inspects[id] = res
}

// addImage registers an image ID for a reference.
func (f *fakeClient) addImage(id string) {
	res := dockerclient.ImageInspectResult{}
	res.ID = id
	f.images["nginx:1.25"] = res
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
	_ dockerclient.ContainerStopOptions,
) (dockerclient.ContainerStopResult, error) {
	f.stopped = append(f.stopped, id)
	return dockerclient.ContainerStopResult{}, f.stopErr
}

func (f *fakeClient) ContainerRemove(
	_ context.Context,
	id string,
	_ dockerclient.ContainerRemoveOptions,
) (dockerclient.ContainerRemoveResult, error) {
	f.removed = append(f.removed, id)
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
		Policy: policy.Digest,
		cli:    f,
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
	u.updateDocker(context.Background(), c)

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
	u.updateDocker(context.Background(), c)

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
	u.updateDocker(context.Background(), c)

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
	u.updateDocker(context.Background(), c)

	assert.Empty(t, f.created)
	assert.Empty(t, f.started)
}

func TestUpdateDockerSelfSkip(t *testing.T) {
	f := newFakeClient()
	f.addImage("sha256:new")
	u := newTestUpdater(f)
	u.hostname = "c1"

	c := dockercontainer.Summary{
		ID:      "c1",
		Names:   []string{"/orbitd"},
		Image:   "nginx:1.25",
		ImageID: "sha256:old",
	}
	u.updateDocker(context.Background(), c)

	assert.Equal(t, []string{"nginx:1.25"}, f.pulls)
	assert.Empty(t, f.created)
	assert.Empty(t, f.started)
}

func TestRecreateDockerSuccess(t *testing.T) {
	f := newFakeClient()
	f.addContainer()
	u := newTestUpdater(f)

	u.recreateDocker(context.Background(), "nginx:1.25", "c1")

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

	u.recreateDocker(context.Background(), "nginx:1.25", "c1")

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

	u.recreateDocker(context.Background(), "nginx:1.25", "c1")

	assert.Contains(t, f.started, "c1")
	assert.Empty(t, f.created)
}

func TestRecreateDockerSkipAutoRemove(t *testing.T) {
	f := newFakeClient()
	f.addContainer()
	f.inspects["c1"].Container.HostConfig.AutoRemove = true
	u := newTestUpdater(f)

	u.recreateDocker(context.Background(), "nginx:1.25", "c1")

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
	u.recreateDocker(context.Background(), "nginx:1.25", "c1")

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
	u.updateDocker(context.Background(), c)

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
