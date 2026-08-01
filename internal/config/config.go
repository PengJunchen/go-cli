// Package config provides configuration loading and management.
package config

import (
	"fmt"
	"os"
)

// Config holds the application configuration.
type Config struct {
	verbose bool
}

// Verbose returns whether verbose output is enabled.
func (c *Config) Verbose() bool { return c.verbose }

// Load reads configuration from environment variables and defaults.
func Load() (*Config, error) {
	cfg := &Config{
		verbose: os.Getenv("GO_CLI_VERBOSE") == "1",
	}

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("config validation: %w", err)
	}

	return cfg, nil
}

// validate checks the configuration for errors.
func (c *Config) validate() error {
	return nil
}
