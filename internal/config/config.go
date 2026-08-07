// Package config provides configuration loading and management.
package config

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/pengjunchen/go-cli/internal/tracing"
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

// probeGlobalConfigFile searches only global ($HOME-relative) config paths.
func probeGlobalConfigFile() string {
	paths := []string{
		filepath.Join(os.Getenv("HOME"), ".go-cli", "config.yaml"),
		filepath.Join(os.Getenv("HOME"), ".go-cli", "config.yml"),
		filepath.Join(os.Getenv("HOME"), ".go-cli", "config.json"),
	}
	for _, p := range paths {
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

// probeProjectConfigFile searches only project-level (CWD-relative) config paths.
func probeProjectConfigFile() string {
	paths := []string{
		".go-cli.yaml",
		".go-cli.yml",
		".go-cli.json",
		"go-cli.yaml",
		"go-cli.yml",
		"go-cli.json",
	}
	for _, p := range paths {
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

// LoadTrusted loads configuration in two phases: first global config (from
// $HOME), then project-level config (from CWD) only if the trustCheck
// callback approves the current working directory. When trustCheck is nil,
// it behaves identically to Load (backward compatible).
func LoadTrusted(trustCheck func(ctx context.Context, projectPath string) bool) (*Config, error) {
	ctx := context.Background()

	// Phase 1: Load global config only.
	globalPath := probeGlobalConfigFile()
	loader := NewLoader()
	if globalPath != "" {
		loader = loader.WithFile(globalPath)
		slog.Info("config_load_trusted",
			"op", "config.load.trusted",
			"phase", "global",
			"file_path", globalPath,
		)
	} else {
		slog.Info("config_load_trusted",
			"op", "config.load.trusted",
			"phase", "global",
			"file_path", "",
		)
	}

	cfg, err := loader.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("config validation: %w", err)
	}

	// Phase 2: If a project-level config exists, check trust before loading.
	projectPath := probeProjectConfigFile()
	if projectPath == "" {
		// No project config to load.
		return cfg, nil
	}

	cwd, _ := os.Getwd()
	trusted := true
	if trustCheck != nil {
		trusted = trustCheck(ctx, cwd)
	}

	if !trusted {
		slog.Warn("config_load_trusted",
			"op", "config.load.trusted",
			"phase", "project",
			"file_path", projectPath,
			"trusted", false,
			"reason", "project not trusted, skipping project config",
		)
		return cfg, nil
	}

	// Load and merge project-level config on top of global config.
	span, spanCtx := tracing.SpanFromContext(ctx, "config.load.trusted", tracing.SpanKindInternal)
	defer span.End()
	span.SetAttributes(
		tracing.Attribute{Key: "project_path", Value: projectPath},
		tracing.Attribute{Key: "trusted", Value: trusted},
	)

	projectLoader := NewLoader().WithFile(projectPath)
	projectCfg, err := projectLoader.Load(spanCtx)
	if err != nil {
		return nil, fmt.Errorf("project config validation: %w", err)
	}

	// Merge: project config overlays global config.
	cfg = mergeConfigs(cfg, projectCfg)

	slog.Info("config_load_trusted",
		"op", "config.load.trusted",
		"phase", "project",
		"file_path", projectPath,
		"trusted", true,
	)

	return cfg, nil
}
