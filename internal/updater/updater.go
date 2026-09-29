// Package updater monitors and updates containers.
package updater

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"sync"

	"github.com/docker/go-sdk/client"
	"github.com/moby/moby/api/types/swarm"
	dockerclient "github.com/moby/moby/client"
	"github.com/robfig/cron/v3"
	"github.com/urfave/cli/v3"

	"github.com/mizuchilabs/kata/buildinfo"

	"github.com/mizuchilabs/orbitd/internal/policy"
)

const maxConcurrentUpdates = 3

var (
	containerIDRe = regexp.MustCompile(`/containers/([0-9a-f]{64})/`)
	shortIDRe     = regexp.MustCompile(`^[0-9a-f]{12}$`)
)

type Updater struct {
	Policy       policy.Policy
	Schedule     string
	Cleanup      bool
	RequireLabel bool
	selfID       string
	cli          client.SDKClient
	pull         func(ctx context.Context, image string) error
}

// run caches registry lookups and pulls for a single update pass.
// A nil run disables caching.
type run struct {
	targets onceMap[string]
	pulls   onceMap[struct{}]
	digests onceMap[string]
}

type onceMap[V any] struct {
	mu sync.Mutex
	m  map[string]func() (V, error)
}

func New(ctx context.Context, cmd *cli.Command) error {
	pol, err := policy.Parse(cmd.String("policy"))
	if err != nil {
		return err
	}

	cli, err := client.New(ctx)
	if err != nil {
		return fmt.Errorf("failed to create docker client: %w", err)
	}
	defer func() { _ = cli.Close() }()

	updater := &Updater{
		Policy:       pol,
		Schedule:     cmd.String("schedule"),
		Cleanup:      cmd.Bool("cleanup"),
		RequireLabel: cmd.Bool("require-label"),
		selfID:       selfContainerID(),
		cli:          cli,
	}
	return updater.Start(ctx)
}

func (u *Updater) Start(ctx context.Context) error {
	c := cron.New(cron.WithChain(cron.SkipIfStillRunning(cron.DiscardLogger)))
	if _, err := c.AddFunc(u.Schedule, func() { u.check(ctx) }); err != nil {
		return fmt.Errorf("invalid schedule: %w", err)
	}

	slog.Info(
		"Starting orbitd",
		"version",
		buildinfo.Version,
		"schedule",
		u.Schedule,
		"policy",
		u.Policy,
	)

	u.check(ctx)

	c.Start()
	<-ctx.Done()
	<-c.Stop().Done()
	return nil
}

func (u *Updater) check(ctx context.Context) {
	info, err := u.cli.Info(ctx, dockerclient.InfoOptions{})
	if err != nil {
		slog.Error("Failed to query docker daemon", "error", err)
		return
	}

	s := info.Info.Swarm
	if s.LocalNodeState == swarm.LocalNodeStateActive && s.ControlAvailable {
		slog.Debug("Checking for updates", "mode", "swarm")
		u.checkSwarm(ctx)
		return
	}
	slog.Debug("Checking for updates", "mode", "standalone")
	u.checkDocker(ctx)
}

func (u *Updater) filters() dockerclient.Filters {
	filters := dockerclient.Filters{}
	if u.RequireLabel {
		filters.Add("label", "orbitd.enable=true")
	}
	return filters
}

func updateAll[T any](ctx context.Context, items []T, fn func(context.Context, T)) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, maxConcurrentUpdates)
	for _, item := range items {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case sem <- struct{}{}:
		}
		wg.Go(func() {
			defer func() { <-sem }()
			fn(ctx, item)
		})
	}
	wg.Wait()
}

// selfContainerID returns the ID of the container orbitd runs in, or "" on
// the host.
func selfContainerID() string {
	if data, err := os.ReadFile("/proc/self/mountinfo"); err == nil {
		if id := containerIDFromMountinfo(data); id != "" {
			return id
		}
	}
	if h, err := os.Hostname(); err == nil && shortIDRe.MatchString(h) {
		return h
	}
	return ""
}

// containerIDFromMountinfo reads the ID from the /etc/hostname bind mount.
// Other paths are ignored, since the host lists mounts of every container.
func containerIDFromMountinfo(data []byte) string {
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 || fields[4] != "/etc/hostname" {
			continue
		}
		if m := containerIDRe.FindStringSubmatch(fields[3]); m != nil {
			return m[1]
		}
	}
	return ""
}

func (r *run) target(key string, fn func() (string, error)) (string, error) {
	if r == nil {
		return fn()
	}
	return r.targets.do(key, fn)
}

func (r *run) pull(key string, fn func() error) error {
	if r == nil {
		return fn()
	}
	_, err := r.pulls.do(key, func() (struct{}, error) { return struct{}{}, fn() })
	return err
}

func (r *run) digest(key string, fn func() (string, error)) (string, error) {
	if r == nil {
		return fn()
	}
	return r.digests.do(key, fn)
}

func (o *onceMap[V]) do(key string, fn func() (V, error)) (V, error) {
	o.mu.Lock()
	f, ok := o.m[key]
	if !ok {
		if o.m == nil {
			o.m = make(map[string]func() (V, error))
		}
		f = sync.OnceValues(fn)
		o.m[key] = f
	}
	o.mu.Unlock()
	return f()
}
