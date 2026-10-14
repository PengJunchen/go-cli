package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/approval"
	"github.com/pengjunchen/go-cli/internal/config"
)

func TestUntrustedProjectACPCommandIsFilteredAndNeverExecuted(t *testing.T) {
	ctx, root, home, expectedMarker, _, manager := newACPProjectCommandTrustFixture(t)
	loaded, err := config.LoadTrustedWithManager(ctx, manager)
	require.NoError(t, err)
	require.Nil(t, loaded.VerifiedProjectSources())
	require.Empty(t, loaded.ACP.Command, "untrusted project ACP settings must not enter the effective config")
	require.Empty(t, loaded.ACP.Transport)

	assembly, err := AssembleAgent(ctx, loaded, "openai", "acp-project-trust", io.Discard)
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)
	_, err = os.Stat(expectedMarker)
	require.ErrorIs(t, err, os.ErrNotExist, "an untrusted project command must never start")
	_ = home
	_ = root
}

func TestTrustedProjectACPCommandUsesCapturedExecutableAndRejectsChangedPath(t *testing.T) {
	ctx, root, _, expectedMarker, commandPath, manager := newACPProjectCommandTrustFixture(t)
	snapshot, err := config.CaptureProjectSources(ctx, root, nil)
	require.NoError(t, err)
	var commandSourceCaptured bool
	for _, record := range snapshot.SourceRecords() {
		if record.ID == "acp-command/bin/acp-agent" && record.Present {
			commandSourceCaptured = true
			break
		}
	}
	require.True(t, commandSourceCaptured, "the configured ACP executable must be bound into the project manifest")
	frozen, ok := manager.(approval.FrozenProjectSourceTrust)
	require.True(t, ok)
	require.NoError(t, frozen.TrustProjectSourceSnapshot(ctx, snapshot))

	loaded, err := config.LoadTrustedWithManager(ctx, manager)
	require.NoError(t, err)
	require.Equal(t, config.ProjectSourceTrustVerified, loaded.ProjectSourceTrustStatus)
	require.NotEmpty(t, loaded.VerifiedProjectSources())
	require.Equal(t, "stdio", loaded.ACP.Transport, "trusted project ACP transport must reach the effective config")
	require.Equal(t, "./bin/acp-agent", loaded.ACP.Command, "trusted project executable must reach the effective config")
	assembly, err := AssembleAgent(ctx, loaded, "openai", "acp-project-trust", io.Discard)
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)
	markerContents := requireACPConnectHandshake(t, expectedMarker)
	closeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	closeErr := assembly.Close(closeCtx)
	cancel()
	require.NoError(t, closeErr)
	require.Contains(t, markerContents, `"type":"connect"`)

	changedMarker := filepath.Join(root, "attacker-started")
	changedCommand := "#!/bin/sh\nprintf 'attacker\\n' > " + shellQuote(changedMarker) + "\n"
	require.NoError(t, os.WriteFile(commandPath, []byte(changedCommand), 0o700))
	staleAssembly, err := AssembleAgent(ctx, loaded, "openai", "acp-project-trust", io.Discard)
	require.Error(t, err, "a changed project executable must fail fresh manifest verification")
	require.Nil(t, staleAssembly)
	_, err = os.Stat(changedMarker)
	require.ErrorIs(t, err, os.ErrNotExist, "the changed executable must not run")

	pathChangedMarker := filepath.Join(root, "attacker-path-started")
	pathChangedCommand := "#!/bin/sh\nprintf 'attacker-path\\n' > " + shellQuote(pathChangedMarker) + "\n"
	pathChangedExecutable := filepath.Join(root, "bin", "attacker-agent")
	require.NoError(t, os.WriteFile(pathChangedExecutable, []byte(pathChangedCommand), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".go-cli.yaml"), []byte("acp:\n  transport: stdio\n  command: ./bin/attacker-agent\n"), 0o600))
	pathChangedAssembly, err := AssembleAgent(ctx, loaded, "openai", "acp-project-trust", io.Discard)
	require.Error(t, err, "a changed project executable path must fail fresh manifest verification")
	require.Nil(t, pathChangedAssembly)
	_, err = os.Stat(pathChangedMarker)
	require.ErrorIs(t, err, os.ErrNotExist, "the new executable path must not run")
}

func TestRevokedProjectACPGenerationCannotStart(t *testing.T) {
	ctx, root, _, marker, _, manager := newACPProjectCommandTrustFixture(t)
	snapshot, err := config.CaptureProjectSources(ctx, root, nil)
	require.NoError(t, err)
	frozen, ok := manager.(approval.FrozenProjectSourceTrust)
	require.True(t, ok)
	require.NoError(t, frozen.TrustProjectSourceSnapshot(ctx, snapshot))
	loaded, err := config.LoadTrustedWithManager(ctx, manager)
	require.NoError(t, err)
	require.NotEmpty(t, loaded.VerifiedProjectSources())
	require.NoError(t, manager.RevokeTrust(ctx, root))

	assembly, err := AssembleAgent(ctx, loaded, "openai", "acp-project-trust", io.Discard)
	require.Error(t, err, "a revoked project generation must not start its configured child")
	require.Nil(t, assembly)
	_, err = os.Stat(marker)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestMissingTrustedProjectACPCommandCreatedAfterLoadCannotRun(t *testing.T) {
	ctx, root, _, marker, commandPath, manager := newACPProjectCommandTrustFixture(t)
	require.NoError(t, os.Remove(commandPath))
	snapshot, err := config.CaptureProjectSources(ctx, root, nil)
	require.NoError(t, err)
	frozen, ok := manager.(approval.FrozenProjectSourceTrust)
	require.True(t, ok)
	require.NoError(t, frozen.TrustProjectSourceSnapshot(ctx, snapshot))
	loaded, err := config.LoadTrustedWithManager(ctx, manager)
	require.NoError(t, err)
	require.Equal(t, config.ProjectSourceTrustVerified, loaded.ProjectSourceTrustStatus)

	attackerCommand := "#!/bin/sh\nprintf 'attacker\\n' > " + shellQuote(marker) + "\nwhile IFS= read -r line; do :; done\n"
	require.NoError(t, os.WriteFile(commandPath, []byte(attackerCommand), 0o700))
	assembly, err := AssembleAgent(ctx, loaded, "openai", "acp-project-trust", io.Discard)
	if assembly != nil {
		closeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		require.NoError(t, assembly.Close(closeCtx))
		cancel()
		t.Cleanup(assembly.Cleanup)
	}
	require.Error(t, err, "a command missing from the trusted snapshot must not fall back to a live path")
	require.Nil(t, assembly)
	_, err = os.Stat(marker)
	require.ErrorIs(t, err, os.ErrNotExist, "a newly created executable must not run under an absent-file receipt")
}

func newACPProjectCommandTrustFixture(t *testing.T) (context.Context, string, string, string, string, approval.TrustManager) {
	t.Helper()
	ctx := context.Background()
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	t.Chdir(root)
	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".go-cli"), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "bin"), 0o700))
	globalConfig := "provider:\n  name: openai\n  api_key: test-key\n  base_url: http://127.0.0.1:1/v1\n  model: acp-project-trust\nmodel:\n  name: acp-project-trust\ntracing:\n  enabled: false\n"
	require.NoError(t, os.WriteFile(filepath.Join(home, ".go-cli", "config.yaml"), []byte(globalConfig), 0o600))
	marker := filepath.Join(root, "trusted-command-started")
	commandPath := filepath.Join(root, "bin", "acp-agent")
	command := "#!/bin/sh\nprintf 'started\\n' > " + shellQuote(marker) + "\nIFS= read -r frame || exit 31\nprintf '%s\\n' \"$frame\" >> " + shellQuote(marker) + "\nwhile IFS= read -r line; do :; done\n"
	require.NoError(t, os.WriteFile(commandPath, []byte(command), 0o700))
	projectConfig := fmt.Sprintf("acp:\n  transport: stdio\n  command: ./bin/acp-agent\n")
	require.NoError(t, os.WriteFile(filepath.Join(root, ".go-cli.yaml"), []byte(projectConfig), 0o600))
	manager := approval.NewDefaultTrustManager(approval.NewInMemoryTrustStore())
	return ctx, root, home, marker, commandPath, manager
}

func requireACPConnectHandshake(t *testing.T, markerPath string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var lastContents string
	started := false
	for time.Now().Before(deadline) {
		contents, err := os.ReadFile(markerPath)
		if err == nil {
			lastContents = string(contents)
			started = strings.HasPrefix(lastContents, "started\n")
			if strings.Contains(lastContents, `"type":"connect"`) {
				return lastContents
			}
		} else if !os.IsNotExist(err) {
			t.Fatalf("read ACP fixture marker: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !started {
		t.Fatalf("configured ACP executable did not start within 10s; last marker=%q", lastContents)
	}
	t.Fatalf("configured ACP executable started but did not consume the connect handshake within 10s; last marker=%q", lastContents)
	return ""
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
