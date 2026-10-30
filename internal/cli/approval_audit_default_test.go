package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/approval"
	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/tools"
)

func TestAssembleAgentApprovalAuditDefaultOutsideWorkspace(t *testing.T) {
	cfg := newApprovalAuditTestConfig(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	_, id, err := resolveWorkspaceIdentity(func() (string, error) { return cfg.Git.WorkDir, nil })
	require.NoError(t, err)
	expected := filepath.Join(home, ".go-cli", "audit", id)
	a, err := AssembleAgent(context.Background(), cfg, "openai", "test-model", io.Discard, WithApproveMode(ApproveAuto))
	require.NoError(t, err)
	t.Cleanup(a.Cleanup)
	tool := &approvalAuditAssemblyTool{name: "audit_readonly_contract", output: "read-only result"}
	require.NoError(t, a.ToolRegistry.Register(context.Background(), tool))
	invocation, err := a.ToolRegistry.(tools.ToolCallPreparer).PrepareToolCall(context.Background(), tools.ToolCall{ID: "audit-readonly", Name: tool.name})
	require.NoError(t, err)
	result, err := invocation.Dispatch(context.Background())
	require.NoError(t, err)
	require.Equal(t, "read-only result", result.Output)
	require.Equal(t, 1, tool.calls)
	require.NoDirExists(t, filepath.Join(cfg.Git.WorkDir, ".go-cli", "audit"))
	entries, err := approval.NewAuditTrail(expected).ReadEntries(time.Now())
	require.NoError(t, err)
	require.Len(t, entries, 3)
	require.Equal(t, "classifier_proposal", entries[0].Stage)
	require.Equal(t, "approval_resolution", entries[1].Stage)
	require.Equal(t, "tool_outcome", entries[2].Stage)
	info, err := os.Stat(expected)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), info.Mode().Perm())
}

func approvalAuditTestDir(t *testing.T, workspace string) string {
	t.Helper()
	dir, err := resolveApprovalAuditDir(os.UserHomeDir, func() (string, error) { return workspace, nil })
	require.NoError(t, err)
	return dir
}

func TestResolveApprovalAuditCanonicalNamespace(t *testing.T) {
	requireApprovalAuditFilesystem(t)
	base := t.TempDir()
	home := filepath.Join(base, "home")
	root := filepath.Join(base, "project")
	require.NoError(t, os.Mkdir(home, 0o755))
	require.NoError(t, os.Mkdir(root, 0o700))
	source := func() (string, error) { return home, nil }
	actual, err := resolveApprovalAuditDir(source, func() (string, error) { return root, nil })
	require.NoError(t, err)
	canonical, id, err := resolveWorkspaceIdentity(func() (string, error) { return root, nil })
	require.NoError(t, err)
	require.NotEmpty(t, canonical)
	canonicalHome, err := filepath.EvalSymlinks(home)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(canonicalHome, ".go-cli", "audit", id), actual)
	alias := filepath.Join(base, "alias")
	require.NoError(t, os.Symlink(root, alias))
	same, err := resolveApprovalAuditDir(source, func() (string, error) { return alias, nil })
	require.NoError(t, err)
	require.Equal(t, actual, same)
	sibling := root + "-sibling"
	require.NoError(t, os.Mkdir(sibling, 0o700))
	other, err := resolveApprovalAuditDir(source, func() (string, error) { return sibling, nil })
	require.NoError(t, err)
	require.NotEqual(t, actual, other)
	// A similarly prefixed HOME is outside, using component rather than string-prefix semantics.
	allowed, err := resolveApprovalAuditDir(func() (string, error) { return sibling, nil }, func() (string, error) { return root, nil })
	require.NoError(t, err)
	require.NotEmpty(t, allowed)
	info, err := os.Stat(home)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o755), info.Mode().Perm(), "do not chmod caller HOME")
	require.NoDirExists(t, filepath.Join(home, ".go-cli"), "resolution does not create a chain")
}

func TestResolveApprovalAuditRejectsUnsafeDefaults(t *testing.T) {
	requireApprovalAuditFilesystem(t)
	for _, name := range []string{"empty-home", "relative-home", "home-error", "workspace-error", "missing-home", "home-file", "workspace-file", "equal-home", "inside-home", "home-link-inside", "namespace-link-inside", "dangling-link", "default-file", "unsafe-mode", "unsafe-existing-parent", "inaccessible-ancestor"} {
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			home := filepath.Join(base, "home")
			root := filepath.Join(base, "repo")
			require.NoError(t, os.Mkdir(home, 0o700))
			require.NoError(t, os.Mkdir(root, 0o700))
			hs := func() (string, error) { return home, nil }
			ws := func() (string, error) { return root, nil }
			switch name {
			case "empty-home":
				hs = func() (string, error) { return "", nil }
			case "relative-home":
				hs = func() (string, error) { return "relative", nil }
			case "home-error":
				hs = func() (string, error) { return "", os.ErrPermission }
			case "workspace-error":
				ws = func() (string, error) { return "", os.ErrPermission }
			case "missing-home":
				hs = func() (string, error) { return filepath.Join(base, "missing"), nil }
			case "home-file":
				require.NoError(t, os.Remove(home))
				require.NoError(t, os.WriteFile(home, nil, 0o600))
			case "workspace-file":
				require.NoError(t, os.Remove(root))
				require.NoError(t, os.WriteFile(root, nil, 0o600))
			case "equal-home":
				hs = func() (string, error) { return root, nil }
			case "inside-home":
				home = filepath.Join(root, "home")
				require.NoError(t, os.Mkdir(home, 0o700))
			case "home-link-inside":
				require.NoError(t, os.Remove(home))
				require.NoError(t, os.Symlink(root, home))
			case "namespace-link-inside":
				require.NoError(t, os.Symlink(root, filepath.Join(home, ".go-cli")))
			case "dangling-link":
				require.NoError(t, os.Symlink(filepath.Join(base, "missing"), filepath.Join(home, ".go-cli")))
			case "default-file":
				require.NoError(t, os.WriteFile(filepath.Join(home, ".go-cli"), nil, 0o600))
			case "unsafe-mode":
				require.NoError(t, os.Mkdir(filepath.Join(home, ".go-cli"), 0o777))
				require.NoError(t, os.Chmod(filepath.Join(home, ".go-cli"), 0o777))
			case "unsafe-existing-parent":
				_, id, err := resolveWorkspaceIdentity(ws)
				require.NoError(t, err)
				require.NoError(t, os.MkdirAll(filepath.Join(home, ".go-cli", "audit", id), 0o700))
				require.NoError(t, os.Chmod(filepath.Join(home, ".go-cli"), 0o777))
			case "inaccessible-ancestor":
				require.NoError(t, os.Mkdir(filepath.Join(home, ".go-cli"), 0o700))
				require.NoError(t, os.Chmod(filepath.Join(home, ".go-cli"), 0))
				t.Cleanup(func() { _ = os.Chmod(filepath.Join(home, ".go-cli"), 0o700) })
			}
			dir, err := resolveApprovalAuditDir(hs, ws)
			require.Error(t, err)
			require.Empty(t, dir)
			if name == "home-error" || name == "workspace-error" {
				require.ErrorIs(t, err, os.ErrPermission)
			}
		})
	}
	dir, err := resolveApprovalAuditDir(nil, nil)
	require.Error(t, err)
	require.Empty(t, dir)
}

func TestAssembleApprovalAuditPreservesExplicitHistory(t *testing.T) {
	cfg := newApprovalAuditTestConfig(t)
	home := os.Getenv("HOME")
	old := filepath.Join(cfg.Git.WorkDir, ".go-cli", "audit")
	trail := approval.NewAuditTrail(old)
	require.NoError(t, trail.Record(approval.AuditEntry{Tool: "historical", Decision: "allow", Stage: "classifier_proposal"}))
	file := filepath.Join(old, time.Now().UTC().Format("2006-01-02")+".jsonl")
	before, err := os.ReadFile(file)
	require.NoError(t, err)
	info, err := os.Stat(file)
	require.NoError(t, err)
	a, err := AssembleAgent(context.Background(), cfg, "openai", "test-model", io.Discard, WithApproveMode(ApproveAuto))
	require.NoError(t, err)
	t.Cleanup(a.Cleanup)
	tool := &approvalAuditAssemblyTool{name: "audit_history_readonly", output: "ok"}
	require.NoError(t, a.ToolRegistry.Register(context.Background(), tool))
	i, err := a.ToolRegistry.(tools.ToolCallPreparer).PrepareToolCall(context.Background(), tools.ToolCall{ID: "new", Name: tool.name})
	require.NoError(t, err)
	_, err = i.Dispatch(context.Background())
	require.NoError(t, err)
	after, err := os.ReadFile(file)
	require.NoError(t, err)
	require.True(t, bytes.Equal(before, after))
	afterInfo, err := os.Stat(file)
	require.NoError(t, err)
	require.Equal(t, info.Mode(), afterInfo.Mode())
	entries, err := approval.NewAuditTrail(old).ReadEntries(time.Now())
	require.NoError(t, err)
	require.Len(t, entries, 1)
	entries, err = approval.NewAuditTrail(approvalAuditTestDir(t, cfg.Git.WorkDir)).ReadEntries(time.Now())
	require.NoError(t, err)
	require.Len(t, entries, 3)
	require.DirExists(t, filepath.Join(home, ".go-cli", "audit"))
	require.Equal(t, filepath.Join(cfg.Git.WorkDir, "audit.jsonl"), cfg.Production.Audit.Path, "independent production audit explicit path stays unchanged")
}

func TestAssembleApprovalAuditRejectsHomeBeforeDispatch(t *testing.T) {
	cfg := newApprovalAuditTestConfig(t)
	t.Setenv("HOME", cfg.Git.WorkDir)
	t.Setenv("USERPROFILE", cfg.Git.WorkDir)
	assembly, err := AssembleAgent(context.Background(), cfg, "openai", "test-model", io.Discard, WithApproveMode(ApproveAuto))
	require.Error(t, err)
	require.Nil(t, assembly)
	require.False(t, errors.Is(err, context.Canceled))
	require.NoDirExists(t, filepath.Join(cfg.Git.WorkDir, ".go-cli", "audit"))
}

func newApprovalAuditTestConfig(t *testing.T) *config.Config {
	t.Helper()
	requireApprovalAuditFilesystem(t)
	cfg := newAssembleTestConfig(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return cfg
}

func requireApprovalAuditFilesystem(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("approval default audit fixture requires supported Unix ownership and private-mode semantics")
	}
}
