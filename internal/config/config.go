// Package config provides configuration management for the application.
package config

import (
	"log/slog"
	"os"
	"time"

	"github.com/lmittmann/tint"
	"github.com/mattn/go-colorable"
	"github.com/mattn/go-isatty"
	"github.com/urfave/cli/v3"
)

type Config struct {
	Policy       string        // Update policy for semantic versioning
	Interval     time.Duration // How often to check for container updates
	Cleanup      bool          // Whether to remove old images after updates
	RequireLabel bool          // Only monitor containers with orbitd.enable=true
}

func Load(cmd *cli.Command) *Config {
	initLogger(cmd)
	return &Config{
		Policy:       cmd.String("policy"),
		Interval:     cmd.Duration("interval"),
		Cleanup:      cmd.Bool("cleanup"),
		RequireLabel: cmd.Bool("require-label"),
	}
}

func initLogger(cmd *cli.Command) {
	level := slog.LevelInfo
	if cmd.Bool("debug") {
		level = slog.LevelDebug
	}

	slog.SetDefault(slog.New(
		tint.NewHandler(colorable.NewColorable(os.Stderr), &tint.Options{
			Level:      level,
			TimeFormat: time.RFC3339,
			NoColor:    !isatty.IsTerminal(os.Stderr.Fd()),
		}),
	))
}
