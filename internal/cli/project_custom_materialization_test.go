package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/approval"
	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/tools"
)

func TestProjectCustomUsesCapturedEntrypointWithExplicitDependencies(t *testing.T) {
	ctx := t.Context()
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	t.Chdir(root)
	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".go-cli"), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "bin"), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "assets"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".go-cli", "config.yaml"), []byte("provider:\n  name: openai\n  api_key: test-key\n  base_url: http://127.0.0.1:1/v1\n  model: custom-test\ntracing:\n  enabled: false\n"), 0o600))
	project := `{"tools":{"custom_tools":[{"name":"captured_custom","command":["./bin/project-tool"],"args":["./assets/data.txt"],"working_dir":".","env":{"EXPLICIT_VALUE":"configured"}}]}}`
	require.NoError(t, os.WriteFile(filepath.Join(root, ".go-cli.json"), []byte(project), 0o600))
	entrypoint := filepath.Join(root, "bin", "project-tool")
	original := "#!/bin/sh\nprintf 'captured:%s:%s\\nentrypoint:%s\\n' \"$PWD\" \"$EXPLICIT_VALUE\" \"$0\"\ncat \"$1\"\nprintf '%s\\n' \"$2\"\n"
	require.NoError(t, os.WriteFile(entrypoint, []byte(original), 0o700))
	dependency := filepath.Join(root, "assets", "data.txt")
	require.NoError(t, os.WriteFile(dependency, []byte("first dependency\n"), 0o600))
	manager := approval.NewDefaultTrustManager(approval.NewFileTrustStore(filepath.Join(home, "trust", "trust.json")))
	snapshot, err := config.CaptureProjectSources(ctx, root, nil)
	require.NoError(t, err)
	frozen := manager.(approval.FrozenProjectSourceTrust)
	require.NoError(t, frozen.TrustProjectSourceSnapshot(ctx, snapshot))
	cfg, err := config.LoadTrustedWithManager(ctx, manager)
	require.NoError(t, err)
	require.Equal(t, config.ProjectSourceTrustVerified, cfg.ProjectSourceTrustStatus)
	disabledMemory := false
	cfg.Memory.Enabled = &disabledMemory
	// Neither replacement below is reread as an approved entrypoint. Explicit
	// dependency paths retain their ordinary cwd-relative meaning and are not pinned.
	forbidden := filepath.Join(root, "new-bytes-executed")
	require.NoError(t, os.WriteFile(entrypoint, []byte("#!/bin/sh\nprintf bad > '"+forbidden+"'\n"), 0o700))
	require.NoError(t, os.WriteFile(dependency, []byte("explicit current dependency\n"), 0o600))
	assembly, err := AssembleAgent(ctx, cfg, "openai", "custom-test", io.Discard, WithApproveMode(ApproveAuto))
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)
	tool, err := assembly.ToolRegistry.Get(ctx, "captured_custom")
	require.NoError(t, err)
	result, err := tool.Execute(ctx, tools.ToolCall{Name: "captured_custom", Args: map[string]any{"input": "one literal argument $(kept)"}})
	require.NoError(t, err)
	require.Contains(t, result.Output, "captured:"+root+":configured")
	require.Contains(t, result.Output, "explicit current dependency\n")
	require.Contains(t, result.Output, "one literal argument $(kept)\n")
	_, err = os.Stat(forbidden)
	require.ErrorIs(t, err, os.ErrNotExist)
	var materialized string
	for _, line := range strings.Split(result.Output, "\n") {
		if strings.HasPrefix(line, "entrypoint:") {
			materialized = strings.TrimPrefix(line, "entrypoint:")
		}
	}
	require.NotEmpty(t, materialized)
	require.NotEqual(t, entrypoint, materialized, "project $0 identifies the captured private entrypoint")
	_, err = os.Stat(materialized)
	require.NoError(t, err)
	assembly.Cleanup()
	_, err = os.Stat(materialized)
	require.ErrorIs(t, err, os.ErrNotExist, "joined assembly cleanup removes its private entrypoint")
}

func TestProjectCustomMissingCapturedSourceDoesNotUseLivePath(t *testing.T) {
	_, _, err := materializeProjectCustomCommand(&config.Config{}, config.CustomToolConfig{Command: []string{"/bin/true"}})
	require.Error(t, err)
}
