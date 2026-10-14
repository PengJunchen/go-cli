package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/approval"
	"github.com/pengjunchen/go-cli/internal/config"
)

func TestAssembleSkipsProjectShellHooksWithoutVerifiedSources(t *testing.T) {
	ctx := context.Background()
	root, _, marker := newProjectShellHookFixture(t, "untrusted")
	manager := approval.NewDefaultTrustManager(approval.NewInMemoryTrustStore())
	rc, err := config.LoadTrustedWithManager(ctx, manager)
	require.NoError(t, err)
	require.Nil(t, rc.VerifiedProjectSources())

	assembly := assembleProjectShellHookTestRuntime(ctx, t, root, rc)
	require.NoError(t, assembly.HookChain.OnTurnStart(ctx, "untrusted-turn"))
	_, err = os.Stat(marker)
	require.ErrorIs(t, err, os.ErrNotExist, "untrusted project hook must not execute")
}

func TestAssembleProjectShellHooksUseCapturedBytesAndTrustGeneration(t *testing.T) {
	ctx := context.Background()
	root, hooksPath, capturedMarker := newProjectShellHookFixture(t, "captured")
	liveMarker := filepath.Join(root, "live-hook-ran")
	store := approval.NewInMemoryTrustStore()
	manager := approval.NewDefaultTrustManager(store)
	frozen, ok := manager.(approval.FrozenProjectSourceTrust)
	require.True(t, ok)

	snapshot, err := config.CaptureProjectSources(ctx, root, nil)
	require.NoError(t, err)
	require.NoError(t, frozen.TrustProjectSourceSnapshot(ctx, snapshot))
	rc, err := config.LoadTrustedWithManager(ctx, manager)
	require.NoError(t, err)
	require.NotNil(t, rc.VerifiedProjectSources())

	// The verified capability must keep the captured command authoritative even
	// when the project file changes after LoadTrustedWithManager returns.
	require.NoError(t, os.WriteFile(hooksPath, projectShellHookYAML(t, shellAppendCommand(liveMarker)), 0o600))
	assembly := assembleProjectShellHookTestRuntime(ctx, t, root, rc)
	require.NotNil(t, assembly.HookChain)
	require.NotEmpty(t, assembly.HookChain.Hooks())
	require.NoError(t, assembly.HookChain.OnTurnStart(ctx, "captured-turn"))
	assertFileContents(t, capturedMarker, "ran")
	_, err = os.Stat(liveMarker)
	require.ErrorIs(t, err, os.ErrNotExist, "post-capture bytes must not replace the verified command")

	// Revocation invalidates the already-assembled hook before it can start a
	// new process; a later grant creates a new generation and cannot revive the
	// stale capability retained by this Assembly.
	require.NoError(t, manager.RevokeTrust(ctx, snapshot.CanonicalRoot()))
	require.NoError(t, assembly.HookChain.OnTurnStart(ctx, "revoked-turn"))
	assertFileContents(t, capturedMarker, "ran")

	require.NoError(t, frozen.TrustProjectSourceSnapshot(ctx, snapshot))
	require.NoError(t, assembly.HookChain.OnTurnStart(ctx, "regranted-old-assembly-turn"))
	assertFileContents(t, capturedMarker, "ran")
	_, err = os.Stat(liveMarker)
	require.ErrorIs(t, err, os.ErrNotExist)

	freshRevokedManager := approval.NewDefaultTrustManager(store)
	require.NoError(t, freshRevokedManager.RevokeTrust(ctx, snapshot.CanonicalRoot()))
	freshRevoked, err := config.LoadTrustedWithManager(ctx, freshRevokedManager)
	require.NoError(t, err)
	freshAssembly := assembleProjectShellHookTestRuntime(ctx, t, root, freshRevoked)
	require.NoError(t, freshAssembly.HookChain.OnTurnStart(ctx, "fresh-revoked-turn"))
	assertFileContents(t, capturedMarker, "ran")
	_, err = os.Stat(liveMarker)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestAssembleProjectShellHookStartRespectsCanceledRunContext(t *testing.T) {
	root, _, marker := newProjectShellHookFixture(t, "cancelled")
	ctx := context.Background()
	manager := approval.NewDefaultTrustManager(approval.NewInMemoryTrustStore())
	grantProjectShellHooks(ctx, t, root, manager)
	rc, err := config.LoadTrustedWithManager(ctx, manager)
	require.NoError(t, err)
	assembly := assembleProjectShellHookTestRuntime(ctx, t, root, rc)

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	require.NoError(t, assembly.HookChain.OnTurnStart(canceled, "cancelled-turn"))
	_, err = os.Stat(marker)
	require.ErrorIs(t, err, os.ErrNotExist, "a canceled event context must not start a project shell process")
}

func TestAssembleProjectShellHooksRequireVerifiedRootMatch(t *testing.T) {
	ctx := context.Background()
	root, _, marker := newProjectShellHookFixture(t, "root-mismatch")
	manager := approval.NewDefaultTrustManager(approval.NewInMemoryTrustStore())
	grantProjectShellHooks(ctx, t, root, manager)
	rc, err := config.LoadTrustedWithManager(ctx, manager)
	require.NoError(t, err)
	require.NotNil(t, rc.VerifiedProjectSources())
	rc.Git.WorkDir = t.TempDir()

	assembly, err := AssembleAgent(ctx, rc, "openai", "trust-test", io.Discard, WithApproveMode(ApproveAuto))
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)
	require.NoError(t, assembly.HookChain.OnTurnStart(ctx, "root-mismatch-turn"))
	_, err = os.Stat(marker)
	require.ErrorIs(t, err, os.ErrNotExist, "a verified source capability cannot authorize hooks under a different work root")
}

func newProjectShellHookFixture(t *testing.T, markerName string) (root, hooksPath, marker string) {
	t.Helper()
	root = t.TempDir()
	canonicalRoot, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	root = canonicalRoot
	t.Chdir(root)
	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".go-cli"), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".go-cli"), 0o700))
	globalConfig := "provider:\n  name: openai\n  api_key: test-key\n  base_url: http://127.0.0.1:1/v1\n  model: trust-test\nmodel:\n  name: trust-test\ntracing:\n  enabled: false\n"
	require.NoError(t, os.WriteFile(filepath.Join(home, ".go-cli", "config.yaml"), []byte(globalConfig), 0o600))
	marker = filepath.Join(root, markerName+"-hook-ran")
	hooksPath = filepath.Join(root, ".go-cli", "hooks.yaml")
	require.NoError(t, os.WriteFile(hooksPath, projectShellHookYAML(t, shellAppendCommand(marker)), 0o600))
	return root, hooksPath, marker
}

func projectShellHookYAML(t *testing.T, command string) []byte {
	t.Helper()
	return []byte("hooks:\n  - event: session_start\n    command: " + strconv.Quote(command) + "\n    timeout: 2s\n")
}

func shellAppendCommand(path string) string {
	return "printf ran >> " + shellSingleQuote(path)
}

func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func grantProjectShellHooks(ctx context.Context, t *testing.T, root string, manager approval.TrustManager) {
	t.Helper()
	snapshot, err := config.CaptureProjectSources(ctx, root, nil)
	require.NoError(t, err)
	frozen, ok := manager.(approval.FrozenProjectSourceTrust)
	require.True(t, ok)
	require.NoError(t, frozen.TrustProjectSourceSnapshot(ctx, snapshot))
}

func assembleProjectShellHookTestRuntime(ctx context.Context, t *testing.T, root string, rc *config.Config) *AgentAssembly {
	t.Helper()
	require.NotNil(t, rc)
	rc.Git.WorkDir = root
	assembly, err := AssembleAgent(ctx, rc, "openai", "trust-test", io.Discard, WithApproveMode(ApproveAuto))
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)
	return assembly
}

func assertFileContents(t *testing.T, path, expected string) {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, expected, string(data))
}
