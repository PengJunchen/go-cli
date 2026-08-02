// Package config provides configuration loading and management.
package config

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

// defaultConfigPaths lists the file paths probed by Load in order. The first
// file that exists is used as the file configuration layer; if none exists the
// loader falls back to environment variables and built-in defaults only.
var defaultConfigPaths = []string{
	".go-cli.yaml",
	".go-cli.yml",
	".go-cli.json",
	"go-cli.yaml",
	"go-cli.yml",
	"go-cli.json",
	filepath.Join(os.Getenv("HOME"), ".go-cli", "config.yaml"),
	filepath.Join(os.Getenv("HOME"), ".go-cli", "config.yml"),
	filepath.Join(os.Getenv("HOME"), ".go-cli", "config.json"),
}

// Load reads configuration from defaults, an optional config file, and
// environment variables. It probes the standard config paths (CWD then $HOME)
// and uses the first file found as the file layer. When no file exists the
// loader falls back to env + defaults so bare invocations (go-cli version/help)
// still work without a config file. For fully controlled layered loading use
// Loader directly.
func Load() (*Config, error) {
	filePath := probeConfigFile()
	if filePath != "" {
		slog.Info("config_load",
			"op", "config_load",
			"source", "file+env",
			"file_path", filePath,
		)
	} else {
		slog.Info("config_load",
			"op", "config_load",
			"source", "env",
			"file_path", "",
		)
	}

	loader := NewLoader()
	if filePath != "" {
		loader = loader.WithFile(filePath)
	}

	cfg, err := loader.Load(context.Background())
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

// probeConfigFile returns the first path in defaultConfigPaths that refers to
// an existing regular file, or "" when none is found.
func probeConfigFile() string {
	for _, p := range defaultConfigPaths {
		if p == "" {
			continue
		}
		info, err := os.Stat(p)
		if err == nil && !info.IsDir() {
			return p
		}
	}
	return ""
}
