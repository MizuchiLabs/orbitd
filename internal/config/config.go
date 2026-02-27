// Package config parses CLI flags into a runtime updater configuration.
package config

import (
	"time"

	"github.com/mizuchilabs/orbitd/internal/policy"
	"github.com/urfave/cli/v3"
)

type Config struct {
	Policy       policy.Policy // Update policy for semantic versioning
	Interval     time.Duration // How often to check for container updates
	Cleanup      bool          // Whether to remove old images after updates
	RequireLabel bool          // Only monitor containers with orbitd.enable=true
}

func Load(cmd *cli.Command) *Config {
	interval := max(5*time.Minute, cmd.Duration("interval"))

	return &Config{
		Policy:       policy.Parse(cmd.String("policy")),
		Interval:     interval,
		Cleanup:      cmd.Bool("cleanup"),
		RequireLabel: cmd.Bool("require-label"),
	}
}
