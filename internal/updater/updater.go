// Package updater monitors and updates containers.
package updater

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/docker/go-sdk/client"
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
	version      string
	hostname     string
	cli          client.SDKClient
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
		version:      cmd.Root().Version,
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

	slog.Info("Starting orbitd", "version", u.version, "schedule", u.Schedule, "mode", mode, "policy", u.Policy)

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
