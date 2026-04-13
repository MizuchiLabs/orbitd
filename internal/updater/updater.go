// Package updater monitors and updates containers.
package updater

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/docker/go-sdk/client"
	"github.com/mizuchilabs/orbitd/internal/policy"
	"github.com/moby/moby/api/types/swarm"
	dockerclient "github.com/moby/moby/client"
	"github.com/urfave/cli/v3"
)

type Updater struct {
	Policy       policy.Policy // Update policy (patch, minor, major, digest)
	Interval     time.Duration // Update check interval
	Cleanup      bool          // Prune old images
	RequireLabel bool          // Only monitor orbitd.enable=true
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
		Interval:     max(5*time.Minute, cmd.Duration("interval")),
		Cleanup:      cmd.Bool("cleanup"),
		RequireLabel: cmd.Bool("require-label"),
		hostname:     hostname,
		cli:          cli,
	}

	// Handle shutdown
	go func() {
		<-ctx.Done()
		slog.Info("Shutting down")
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

	slog.Info("Starting orbitd", "mode", mode, "interval", u.Interval, "policy", u.Policy)

	ticker := time.NewTicker(u.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if isSwarm {
				u.checkSwarm(ctx)
			} else {
				u.checkDocker(ctx)
			}
		}
	}
}
