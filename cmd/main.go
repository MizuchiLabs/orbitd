package main

import (
	"context"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mizuchilabs/orbitd/internal/config"
	"github.com/mizuchilabs/orbitd/internal/updater"
	"github.com/urfave/cli/v3"
)

var (
	Version = "debug"
	Commit  string
	Date    string
	Dirty   string
)

func main() {
	cmd := &cli.Command{
		EnableShellCompletion: true,
		Suggest:               true,
		Name:                  "orbitd",
		Version:               Version,
		Usage:                 "orbitd [command]",
		Description: `Orbitd is a lightweight container update daemon that automatically keeps your Docker containers up-to-date.

   It monitors running containers for new image versions and seamlessly recreates them with
   the latest digest while preserving all configuration, networks, volumes, and labels.
   Perfect for self-hosted services and Docker Compose setups.`,
		Before: func(ctx context.Context, cmd *cli.Command) (context.Context, error) {
			if _, err := os.Stat("/var/run/docker.sock"); err != nil {
				slog.Warn("Docker socket not found", "path", "/var/run/docker.sock")
			}
			if _, ok := os.LookupEnv("DOCKER_HOST"); !ok {
				_ = os.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")
			}

			// Create empty Docker config if it doesn't exist
			dockerConfigDir := os.ExpandEnv("$HOME/.docker")
			dockerConfigPath := dockerConfigDir + "/config.json"
			if _, err := os.Stat(dockerConfigPath); os.IsNotExist(err) {
				if err := os.MkdirAll(dockerConfigDir, 0o750); err == nil {
					_ = os.WriteFile(dockerConfigPath, []byte("{}"), 0o600)
					slog.Debug("Created empty Docker config", "path", dockerConfigPath)
				}
			}
			return ctx, nil
		},
		DefaultCommand: "start",
		Commands: []*cli.Command{
			{
				Name:    "start",
				Aliases: []string{"s"},
				Usage:   "Start the orbitd daemon to monitor and update containers",
				Action: func(ctx context.Context, cmd *cli.Command) error {
					cfg := config.Load(cmd)

					return updater.New(ctx, cfg)
				},
			},
		},
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:    "version",
				Aliases: []string{"v"},
				Usage:   "Print version information",
			},
			&cli.BoolFlag{
				Name:    "debug",
				Aliases: []string{"d"},
				Usage:   "Enable debug logging for detailed output",
				Sources: cli.EnvVars("ORBITD_DEBUG"),
			},
			&cli.StringFlag{
				Name:    "policy",
				Aliases: []string{"p"},
				Usage:   "Update policy (patch, minor, major, digest)",
				Value:   "digest",
				Sources: cli.EnvVars("ORBITD_POLICY"),
			},
			&cli.DurationFlag{
				Name:    "interval",
				Aliases: []string{"i"},
				Usage:   "Check for updates every interval (e.g., 5m, 1h, 12h)",
				Value:   12 * time.Hour,
				Sources: cli.EnvVars("ORBITD_INTERVAL"),
			},
			&cli.BoolFlag{
				Name:    "cleanup",
				Aliases: []string{"c"},
				Usage:   "Automatically remove old images after successful updates",
				Value:   true,
				Sources: cli.EnvVars("ORBITD_CLEANUP"),
			},
		},
	}

	// Graceful shutdown
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := cmd.Run(ctx, os.Args); err != nil {
		log.Fatal(err)
	}
}
