package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/approval"
	"github.com/pengjunchen/go-cli/internal/config"
)

func TestAssembleUsesLoadedTrustManagerForProjectCustomTools(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	canonicalRoot, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	root = canonicalRoot
	t.Chdir(root)
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, dir := range []string{
		filepath.Join(home, ".go-cli"),
		filepath.Join(root, "bin"),
	} {
		require.NoError(t, os.MkdirAll(dir, 0o700))
	}
	globalConfig := "provider:\n  name: openai\n  api_key: test-key\n  base_url: http://127.0.0.1:1/v1\n  model: trust-test\nmodel:\n  name: trust-test\ntracing:\n  enabled: false\n"
	require.NoError(t, os.WriteFile(filepath.Join(home, ".go-cli", "config.yaml"), []byte(globalConfig), 0o600))
	projectConfig := "tools:\n  custom_tools:\n    - name: project_custom\n      description: Project-scoped custom tool\n      command:\n        - ./bin/project-tool\n"
	require.NoError(t, os.WriteFile(filepath.Join(root, ".go-cli.yaml"), []byte(projectConfig), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "bin", "project-tool"), []byte("#!/bin/sh\nprintf 'project custom ran\\n'\n"), 0o700))

	manager := approval.NewDefaultTrustManager(approval.NewFileTrustStore(filepath.Join(home, ".config", "go-cli", "trust.json")))
	snapshot, err := config.CaptureProjectSources(ctx, root, nil)
	require.NoError(t, err)
	frozen, ok := manager.(approval.FrozenProjectSourceTrust)
	require.True(t, ok)
	require.NoError(t, frozen.TrustProjectSourceSnapshot(ctx, snapshot))

	loaded, err := config.LoadTrustedWithManager(ctx, manager)
	require.NoError(t, err)
	require.Equal(t, config.ProjectSourceTrustVerified, loaded.ProjectSourceTrustStatus)
	require.Same(t, manager, loaded.ProjectTrustManager())
	require.Len(t, loaded.Tools.CustomTools, 1)

	assembly, err := AssembleAgent(ctx, loaded, "openai", "trust-test", io.Discard, WithApproveMode(ApproveAuto))
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)
	definitions, err := assembly.ToolRegistry.List(ctx)
	require.NoError(t, err)
	toolNames := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		toolNames = append(toolNames, definition.Name())
	}
	require.Contains(t, toolNames, "project_custom")

	require.NoError(t, manager.RevokeTrust(ctx, snapshot.CanonicalRoot()))
	revokedConfig, err := config.LoadTrustedWithManager(ctx, manager)
	require.NoError(t, err)
	require.Empty(t, revokedConfig.Tools.CustomTools)
	revokedAssembly, err := AssembleAgent(ctx, revokedConfig, "openai", "trust-test", io.Discard, WithApproveMode(ApproveAuto))
	require.NoError(t, err)
	t.Cleanup(revokedAssembly.Cleanup)
	revokedDefinitions, err := revokedAssembly.ToolRegistry.List(ctx)
	require.NoError(t, err)
	revokedToolNames := make([]string, 0, len(revokedDefinitions))
	for _, definition := range revokedDefinitions {
		revokedToolNames = append(revokedToolNames, definition.Name())
	}
	require.NotContains(t, revokedToolNames, "project_custom")
}
