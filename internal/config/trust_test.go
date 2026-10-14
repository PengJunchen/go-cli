package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeGlobalConfig creates a $HOME/.go-cli/config.json with the given JSON
// content inside homeDir and returns homeDir so the caller can set it as HOME.
func writeGlobalConfig(t *testing.T, content string) string {
	t.Helper()
	homeDir := t.TempDir()
	cfgDir := filepath.Join(homeDir, ".go-cli")
	require.NoError(t, os.MkdirAll(cfgDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(content), 0o600))
	return homeDir
}

// writeProjectConfig creates a project config file named filename with the
// given JSON content inside projectDir and returns projectDir.
func writeProjectConfig(t *testing.T, filename, content string) string {
	t.Helper()
	projectDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, filename), []byte(content), 0o600))
	return projectDir
}

// chdir changes the working directory to dir and restores it at test cleanup.
func chdir(t *testing.T, dir string) {
	t.Helper()
	origDir, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { _ = os.Chdir(origDir) })
}

// TestTrustedLoad verifies that the legacy path callback cannot grant
// executable project-source authority.
func TestTrustedLoad(t *testing.T) {
	homeDir := writeGlobalConfig(t, `{"provider":{"name":"global-provider"}}`)
	projectDir := writeProjectConfig(t, ".go-cli.json", `{"model":{"name":"project-model"}}`)

	t.Setenv("HOME", homeDir)
	chdir(t, projectDir)

	trustCheck := func(_ context.Context, _ string) bool { return true }

	cfg, err := LoadTrusted(trustCheck)
	require.NoError(t, err)
	assert.Equal(t, "global-provider", cfg.Provider.Name)
	assert.Equal(t, "", cfg.Model.Name)
	assert.Equal(t, ProjectSourceTrustUnsupported, cfg.ProjectSourceTrustStatus)
	assert.Nil(t, cfg.VerifiedProjectSources())
}

// TestGlobalOnlyBeforeTrust verifies that when the trust check denies the
// project, only the global config is loaded and the project config is skipped.
func TestGlobalOnlyBeforeTrust(t *testing.T) {
	homeDir := writeGlobalConfig(t, `{"provider":{"name":"global-provider"}}`)
	projectDir := writeProjectConfig(t, ".go-cli.json", `{"model":{"name":"project-model"}}`)

	t.Setenv("HOME", homeDir)
	chdir(t, projectDir)

	trustCheck := func(_ context.Context, _ string) bool { return false }

	cfg, err := LoadTrusted(trustCheck)
	require.NoError(t, err)
	assert.Equal(t, "global-provider", cfg.Provider.Name)
	// Project config was skipped, so model.name should be empty (default).
	assert.Equal(t, "", cfg.Model.Name)
}

// TestTrustedProjectLoads verifies that a path-only callback cannot authorize
// project config values even when it reports trusted.
func TestTrustedProjectLoads(t *testing.T) {
	homeDir := writeGlobalConfig(t, `{"provider":{"name":"global-provider"},"model":{"name":"global-model"}}`)
	projectDir := writeProjectConfig(t, ".go-cli.json", `{"model":{"name":"project-model"}}`)

	t.Setenv("HOME", homeDir)
	chdir(t, projectDir)

	trustCheck := func(_ context.Context, _ string) bool { return true }

	cfg, err := LoadTrusted(trustCheck)
	require.NoError(t, err)
	// Global value preserved (project file does not set provider.name).
	assert.Equal(t, "global-provider", cfg.Provider.Name)
	// The global value remains because the path-only callback cannot authorize a project overlay.
	assert.Equal(t, "global-model", cfg.Model.Name)
}

// TestUntrustedFallback verifies that when no global config exists and the
// project is untrusted, defaults are used and no project config is loaded.
func TestUntrustedFallback(t *testing.T) {
	// Empty HOME dir: no global config.
	emptyHome := t.TempDir()
	projectDir := writeProjectConfig(t, ".go-cli.json", `{"model":{"name":"project-model"}}`)

	t.Setenv("HOME", emptyHome)
	chdir(t, projectDir)

	trustCheck := func(_ context.Context, _ string) bool { return false }

	cfg, err := LoadTrusted(trustCheck)
	require.NoError(t, err)
	// No global config and project config skipped: model.name is default (empty).
	assert.Equal(t, "", cfg.Model.Name)
	// Defaults should still be present.
	assert.Equal(t, defaultMaxTokens, cfg.Model.MaxTokens)
	assert.Equal(t, defaultTracingLevel, cfg.Tracing.Level)
}
