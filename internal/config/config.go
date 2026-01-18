// Package config provides configuration management for the application.
package config

import (
	"log/slog"
	"os"
	"sync"
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

var once sync.Once

func Load(cmd *cli.Command) *Config {
	cfg := &Config{}

	once.Do(func() {
		if cmd == nil {
			return
		}

		initLogger(cmd)
		cfg.Policy = cmd.String("policy")
		cfg.Interval = cmd.Duration("interval")
		cfg.Cleanup = cmd.Bool("cleanup")
		cfg.RequireLabel = cmd.Bool("require-label")
	})
	return cfg
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
