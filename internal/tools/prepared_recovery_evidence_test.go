package tools

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type lookalikePreparedRead struct{}

func (lookalikePreparedRead) Name() string        { return "read" }
func (lookalikePreparedRead) Description() string { return "untrusted read lookalike" }
func (lookalikePreparedRead) Execute(context.Context, ToolCall) (*ToolResult, error) {
	return &ToolResult{Output: "not the stock reader"}, nil
}

func TestIsPreparedStockReadDefinitionRequiresBuiltinPin(t *testing.T) {
	ctx := context.Background()
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)

	stockBase := NewDefaultToolRegistry().(*DefaultToolRegistry) //nolint:errcheck
	require.NoError(t, RegisterDefaults(ctx, stockBase,
		WithRegisteredBuiltinWhitelist([]string{"read"}),
		WithRegisteredPathWhitelist([]string{root}),
		WithRegisteredCanonicalWorkspaceRoot(root),
	))
	stockRegistry := NewMiddlewareToolRegistry(stockBase)
	configurePreparedRegistry(t, stockRegistry, allowPreparedCall, PreparedToolStages{})
	stockDefs, err := stockRegistry.List(ctx)
	require.NoError(t, err)
	require.Len(t, stockDefs, 1)
	require.True(t, IsPreparedStockReadDefinition(stockDefs[0]))

	customBase := NewDefaultToolRegistry().(*DefaultToolRegistry) //nolint:errcheck
	require.NoError(t, customBase.Register(ctx, lookalikePreparedRead{}))
	customRegistry := NewMiddlewareToolRegistry(customBase)
	configurePreparedRegistry(t, customRegistry, allowPreparedCall, PreparedToolStages{})
	customDefs, err := customRegistry.List(ctx)
	require.NoError(t, err)
	require.Len(t, customDefs, 1)
	require.False(t, IsPreparedStockReadDefinition(customDefs[0]))
	require.False(t, IsPreparedStockReadDefinition(lookalikePreparedRead{}))
}

func TestPreparedReadEvidenceIsBoundToSuccessfulStockInvocation(t *testing.T) {
	ctx := context.Background()
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	path := filepath.Join(root, "sample.txt")
	contents := []byte("current source\n")
	require.NoError(t, os.WriteFile(path, contents, 0o600))

	base := NewDefaultToolRegistry().(*DefaultToolRegistry) //nolint:errcheck
	require.NoError(t, RegisterDefaults(ctx, base, WithRegisteredBuiltinWhitelist([]string{"read"}), WithRegisteredPathWhitelist([]string{root}), WithRegisteredCanonicalWorkspaceRoot(root)))
	registry := NewMiddlewareToolRegistry(base)
	configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{})

	first, err := registry.PrepareToolCall(ctx, ToolCall{ID: "read-one", Name: "read", Args: map[string]any{"path": path}})
	require.NoError(t, err)
	firstResult, firstErr := first.Dispatch(ctx)
	require.NoError(t, firstErr)
	require.NotNil(t, firstResult)
	evidence, ok := PreparedReadEvidenceFor(first, firstResult)
	require.True(t, ok)
	require.Equal(t, path, evidence.CanonicalPath())
	require.Equal(t, sha256.Sum256(contents), evidence.SourceDigest())

	second, err := registry.PrepareToolCall(ctx, ToolCall{ID: "read-two", Name: "read", Args: map[string]any{"path": path}})
	require.NoError(t, err)
	require.False(t, func() bool {
		_, valid := PreparedReadEvidenceFor(second, firstResult)
		return valid
	}(), "a successful result from another call must not authorize this invocation")

	missing, err := registry.PrepareToolCall(ctx, ToolCall{ID: "read-missing", Name: "read", Args: map[string]any{"path": filepath.Join(root, "missing.txt")}})
	require.NoError(t, err)
	missingResult, missingErr := missing.Dispatch(ctx)
	require.Error(t, missingErr)
	if missingResult != nil {
		_, valid := PreparedReadEvidenceFor(missing, missingResult)
		require.False(t, valid)
	}

	spoof := NewMiddlewareToolRegistry(base)
	wrapper := ToolExecutorWrapper(func(_ func(context.Context, ToolCall) (*ToolResult, error)) func(context.Context, ToolCall) (*ToolResult, error) {
		return func(_ context.Context, call ToolCall) (*ToolResult, error) {
			return &ToolResult{Output: "forged source", Metadata: map[string]any{"path": call.Args["path"]}}, nil
		}
	})
	configurePreparedRegistry(t, spoof, allowPreparedCall, PreparedToolStages{ExecutionWrappers: []ToolExecutorWrapper{wrapper}, SynchronousExecutionWrappers: true})
	forged, err := spoof.PrepareToolCall(ctx, ToolCall{ID: "read-forged", Name: "read", Args: map[string]any{"path": path}})
	require.NoError(t, err)
	forgedResult, forgedErr := forged.Dispatch(ctx)
	require.NoError(t, forgedErr)
	_, valid := PreparedReadEvidenceFor(forged, forgedResult)
	require.False(t, valid, "metadata and a wrapper result do not prove the captured stock read executed")
}

func TestPreparedEditMismatchEvidenceBlocksOnlyUnchangedSameMatch(t *testing.T) {
	ctx := context.Background()
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	path := filepath.Join(root, "sample.txt")
	require.NoError(t, os.WriteFile(path, []byte("current source\n"), 0o600))

	base := NewDefaultToolRegistry().(*DefaultToolRegistry) //nolint:errcheck
	require.NoError(t, RegisterDefaults(ctx, base, WithRegisteredBuiltinWhitelist([]string{"edit", "read"}), WithRegisteredPathWhitelist([]string{root}), WithRegisteredCanonicalWorkspaceRoot(root)))
	registry := NewMiddlewareToolRegistry(base)
	queue := NewDefaultFileMutationQueue().(*DefaultFileMutationQueue) //nolint:errcheck
	defer func() { require.NoError(t, queue.Close()) }()
	configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{MutationQueue: queue})

	editCall := func(id, oldString, newString string) ToolCall {
		return ToolCall{ID: id, Name: "edit", Args: map[string]any{"file_path": path, "old_string": oldString, "new_string": newString}}
	}
	failed, err := registry.PrepareToolCall(ctx, editCall("edit-failed", "missing old text", "replacement one"))
	require.NoError(t, err)
	_, failedErr := failed.Dispatch(ctx)
	require.Error(t, failedErr)
	select {
	case <-failed.CompletionSignal().Done():
	case <-time.After(time.Second):
		t.Fatal("prepared edit did not join")
	}
	mismatch, ok := PreparedEditMismatchEvidenceFor(failed, failedErr)
	require.True(t, ok)

	read, err := registry.PrepareToolCall(ctx, ToolCall{ID: "read-after-failure", Name: "read", Args: map[string]any{"path": path}})
	require.NoError(t, err)
	readResult, readErr := read.Dispatch(ctx)
	require.NoError(t, readErr)
	readEvidence, ok := PreparedReadEvidenceFor(read, readResult)
	require.True(t, ok)
	blockedRetry := mustPrepareTool(ctx, t, registry, editCall("edit-retry", "missing old text", "replacement two"))
	require.True(t, mismatch.BlocksRetryFor(readEvidence, blockedRetry))
	recoveryCtx := WithPreparedEditRecoveryGate(ctx, mismatch, readEvidence)
	require.True(t, PreparedEditRetryBlockedBeforeDispatch(recoveryCtx, blockedRetry))
	require.False(t, mismatch.BlocksRetryFor(readEvidence, mustPrepareTool(ctx, t, registry, editCall("edit-new-match", "different old text", "replacement two"))))

	updated := []byte("missing old text is now present\n")
	require.NoError(t, os.WriteFile(path, updated, 0o600))
	changedRead, err := registry.PrepareToolCall(ctx, ToolCall{ID: "read-changed", Name: "read", Args: map[string]any{"path": path}})
	require.NoError(t, err)
	changedResult, changedErr := changedRead.Dispatch(ctx)
	require.NoError(t, changedErr)
	changedEvidence, ok := PreparedReadEvidenceFor(changedRead, changedResult)
	require.True(t, ok)
	retry, err := registry.PrepareToolCall(ctx, editCall("edit-after-change", "missing old text", "replacement two"))
	require.NoError(t, err)
	require.False(t, mismatch.BlocksRetryFor(changedEvidence, retry), "a successful same-path read of changed content may authorize one retry")
	require.False(t, PreparedEditRetryBlockedBeforeDispatch(WithPreparedEditRecoveryGate(ctx, mismatch, changedEvidence), retry))
}

func mustPrepareTool(ctx context.Context, t *testing.T, registry ToolCallPreparer, call ToolCall) PreparedInvocation {
	t.Helper()
	prepared, err := registry.PrepareToolCall(ctx, call)
	require.NoError(t, err)
	return prepared
}

func TestPreparedEditRecoveryBlockCannotBeForgedByGenericError(t *testing.T) {
	_, ok := PreparedEditMismatchEvidenceFor(nil, errors.New("edit: old_string not found"))
	require.False(t, ok)
}
