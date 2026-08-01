// Package config provides configuration loading and management.
package config

import (
	"context"
	"fmt"
	"log/slog"
)

// Load reads configuration from environment variables and defaults, returning
// the default+env merged Config. It is the backward-compatible entry point
// used by cmd/cli/main.go so bare invocations (go-cli version/help) still
// work without a config file. For layered loading use Loader.
func Load() (*Config, error) {
	slog.Info("config_load",
		"op", "config_load",
		"source", "env",
		"file_path", "",
	)

	cfg, err := NewLoader().Load(context.Background())
	if err != nil {
		slog.Error("config_error",
			"op", "config_error",
			"error_type", "validation_failed",
			"error", err,
		)
		return nil, fmt.Errorf("config validation: %w", err)
	}
	return cfg, nil
}
