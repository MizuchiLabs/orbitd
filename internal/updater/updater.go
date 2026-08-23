// Package updater monitors and updates containers.
package updater

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"

	"github.com/docker/go-sdk/client"
	"github.com/mizuchilabs/kata/buildinfo"
	"github.com/mizuchilabs/orbitd/internal/policy"
	"github.com/moby/moby/api/types/swarm"
	dockerclient "github.com/moby/moby/client"
	"github.com/robfig/cron/v3"
	"github.com/urfave/cli/v3"
)

type Updater struct {
	Policy       policy.Policy // Update policy (patch, minor, major, digest)
	Schedule     string        // Cron schedule
	Cleanup      bool          // Prune old images
	RequireLabel bool          // Only monitor orbitd.enable=true
	hostname     string
	cli          client.SDKClient
	pull         func(ctx context.Context, image string) error
}

func New(ctx context.Context, cmd *cli.Command) error {
	cli, err := client.New(ctx)
	if err != nil {
		return fmt.Errorf("failed to create docker client: %w", err)
	}

	hostname, _ := os.Hostname()
	updater := &Updater{
		Policy:       policy.Parse(cmd.String("policy")),
		Schedule:     cmd.String("schedule"),
		Cleanup:      cmd.Bool("cleanup"),
		RequireLabel: cmd.Bool("require-label"),
		hostname:     hostname,
		cli:          cli,
	}

	// Handle shutdown
	go func() {
		<-ctx.Done()
		_ = cli.Close()
	}()
	return updater.Start(ctx)
}

func (u *Updater) Start(ctx context.Context) error {
	info, err := u.cli.Info(ctx, dockerclient.InfoOptions{})
	isSwarm := false
	if err == nil {
		isSwarm = info.Info.Swarm.LocalNodeState == swarm.LocalNodeStateActive &&
			info.Info.Swarm.ControlAvailable
	}

	mode := "standalone"
	if isSwarm {
		mode = "swarm"
	}

	slog.Info(
		"Starting orbitd",
		"version",
		buildinfo.Version,
		"schedule",
		u.Schedule,
		"mode",
		mode,
		"policy",
		u.Policy,
	)

	// Initial check
	if isSwarm {
		u.checkSwarm(ctx)
	} else {
		u.checkDocker(ctx)
	}

	c := cron.New()
	_, err = c.AddFunc(u.Schedule, func() {
		if isSwarm {
			u.checkSwarm(ctx)
		} else {
			u.checkDocker(ctx)
		}
	})
	if err != nil {
		return fmt.Errorf("invalid schedule: %w", err)
	}
	c.Start()
	<-ctx.Done()
	<-c.Stop().Done()
	return nil
}

// filters returns the list filters honoring the require-label opt-in.
func (u *Updater) filters() dockerclient.Filters {
	filters := dockerclient.Filters{}
	if u.RequireLabel {
		filters.Add("label", "orbitd.enable=true")
	}
	return filters
}

// updateAll runs fn for each index concurrently, limited to three in flight.
func updateAll(ctx context.Context, n int, fn func(context.Context, int)) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, 3)
	for i := range n {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case sem <- struct{}{}:
		}
		wg.Go(func() {
			defer func() { <-sem }()
			fn(ctx, i)
		})
	}
	wg.Wait()
}
