package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPreparedEditMatchBelongsToClosedInvocation(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sample.txt")
	require.NoError(t, os.WriteFile(path, []byte("current\n"), 0o600))
	base := NewDefaultToolRegistry().(*DefaultToolRegistry) //nolint:errcheck
	require.NoError(t, RegisterDefaults(ctx, base, WithRegisteredBuiltinWhitelist([]string{"edit"})))
	registry := NewMiddlewareToolRegistry(base)
	queue := NewDefaultFileMutationQueue().(*DefaultFileMutationQueue) //nolint:errcheck
	defer func() { require.NoError(t, queue.Close()) }()
	configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{MutationQueue: queue})
	call := ToolCall{ID: "first", Name: "edit", Args: map[string]any{"file_path": path, "old_string": "missing", "new_string": "changed"}}
	first, err := registry.PrepareToolCall(ctx, call)
	require.NoError(t, err)
	_, firstErr := first.Dispatch(ctx)
	require.Error(t, firstErr)
	select {
	case <-first.CompletionSignal().Done():
	case <-time.After(time.Second):
		t.Fatal("prepared edit did not join")
	}
	require.Equal(t, CompletionQueuedJobJoined, first.CompletionSignal().Proof())
	require.True(t, PreparedEditMatchKnown(first, firstErr))
	var match *EditMatchError
	require.ErrorAs(t, firstErr, &match)
	require.Equal(t, "no_match", match.MatchFailureKind())

	call.ID = "second"
	second, err := registry.PrepareToolCall(ctx, call)
	require.NoError(t, err)
	_, secondErr := second.Dispatch(ctx)
	require.Error(t, secondErr)
	select {
	case <-second.CompletionSignal().Done():
	case <-time.After(time.Second):
		t.Fatal("second prepared edit did not join")
	}
	require.True(t, PreparedEditMatchKnown(second, secondErr))
	require.False(t, PreparedEditMatchKnown(second, firstErr), "a genuine failure from another action is not authority")
	require.False(t, PreparedEditMatchKnown(first, errors.Join(firstErr, errors.New("other failure"))))
	require.False(t, PreparedEditMatchKnown(first, newUnknownExecutionError(firstErr)))
	require.False(t, PreparedEditMatchKnown(first, &EditMatchError{}))
	_, unbound := NewEditFileTool().Execute(ctx, call)
	require.Error(t, unbound)
	require.False(t, PreparedEditMatchKnown(first, unbound))
	spoof := NewMiddlewareToolRegistry(base)
	wrapper := ToolExecutorWrapper(func(_ func(context.Context, ToolCall) (*ToolResult, error)) func(context.Context, ToolCall) (*ToolResult, error) {
		return func(wrapperCtx context.Context, wrapperCall ToolCall) (*ToolResult, error) {
			return NewEditFileTool().Execute(wrapperCtx, wrapperCall)
		}
	})
	configurePreparedRegistry(t, spoof, allowPreparedCall, PreparedToolStages{
		MutationQueue: queue, ExecutionWrappers: []ToolExecutorWrapper{wrapper}, SynchronousExecutionWrappers: true,
	})
	call.ID = "wrapper-attempt"
	spoofed, err := spoof.PrepareToolCall(ctx, call)
	require.NoError(t, err)
	_, spoofErr := spoofed.Dispatch(ctx)
	require.Error(t, spoofErr)
	select {
	case <-spoofed.CompletionSignal().Done():
	case <-time.After(time.Second):
		t.Fatal("wrapper attempt did not join")
	}
	require.False(t, PreparedEditMatchKnown(spoofed, spoofErr), "an outer wrapper cannot mint a mismatch for the captured executor")
	first.(*preparedInvocation).invalidateCompletionProof() //nolint:errcheck
	require.False(t, PreparedEditMatchKnown(first, firstErr), "revoked completion proof cannot authorize recovery")
}
