package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/approval"
	"github.com/pengjunchen/go-cli/internal/config"
)

func TestProjectRPCMaterializationPreservesLoaderSuffixAndCapturedCwd(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	t.Chdir(root)
	path := filepath.Join(root, "worker.rpc.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"command":["bin/worker"],"artifact_sha256":"`+strings.Repeat("a", 64)+`"}`), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".go-cli.json"), []byte(`{"extensions":{"enabled":true,"plugin_paths":["worker.rpc.json"]}}`), 0600))
	manager := approval.NewDefaultTrustManager(approval.NewInMemoryTrustStore())
	pending, err := config.LoadTrustedWithManager(context.Background(), manager)
	require.NoError(t, err)
	frozen := manager.(approval.FrozenProjectSourceTrust)
	require.NoError(t, frozen.TrustProjectSourceSnapshot(context.Background(), pending.CapturedProjectSourcesForGrant()))
	cfg, err := config.LoadTrustedWithManager(context.Background(), manager)
	require.NoError(t, err)
	paths, cleanup, err := materializeProjectExtensionPaths(cfg, cfg.Extensions.PluginPaths)
	require.NoError(t, err)
	defer func() { require.NoError(t, cleanup(context.Background())) }()
	require.Len(t, paths, 1)
	require.True(t, strings.HasSuffix(paths[0], ".rpc.json"))
	data, err := os.ReadFile(paths[0])
	require.NoError(t, err)
	var descriptor struct {
		Cwd     string   `json:"cwd"`
		Command []string `json:"command"`
	}
	require.NoError(t, json.Unmarshal(data, &descriptor))
	canonical, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	require.Equal(t, canonical, descriptor.Cwd)
	require.Equal(t, filepath.Join(canonical, "bin", "worker"), descriptor.Command[0])
}
