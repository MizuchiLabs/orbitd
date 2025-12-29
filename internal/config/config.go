// Package config provides configuration management and logging initialization
// for the application.
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

// Config holds the runtime configuration for the updater daemon.
type Config struct {
	Policy   string        // Update policy for semantic versioning
	Interval time.Duration // How often to check for container updates
	Cleanup  bool          // Whether to remove old images after updates
}

// once ensures logger initialization happens only once
var once sync.Once

// Load creates a new Config from CLI flags and initializes the global logger.
// It uses sync.Once to ensure logger initialization happens only once.
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
	})
	return cfg
}

// initLogger configures the global slog logger with colored output and appropriate log level.
// Output is colorized when stderr is a terminal, plain text otherwise.
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
