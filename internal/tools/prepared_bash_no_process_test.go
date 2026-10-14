package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPreparedBashPolicyRejectionProvesNoProcessStarted(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		name := "bash"
		if streaming {
			name = "streaming bash"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			workdir := t.TempDir()
			marker := filepath.Join(workdir, "must-not-exist")
			command := fmt.Sprintf("python3 -c 'from pathlib import Path; Path(%q).write_text(\"started\")'", marker)
			sandbox := NewDefaultBashSandbox(WithAllowedPaths([]string{workdir}))

			base := NewDefaultToolRegistry().(*DefaultToolRegistry) //nolint:errcheck
			var definition ToolDefinition = NewBashTool(WithBashWorkdir(workdir), WithBashSandbox(sandbox))
			if streaming {
				definition = NewStreamingBashTool(WithBashWorkdir(workdir), WithBashSandbox(sandbox))
			}
			require.NoError(t, base.registerTrustedLocalProcess(ctx, definition))
			registry := NewMiddlewareToolRegistry(base)
			configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{})

			var processStarts atomic.Int32
			executionCtx := WithProcessExecutionIdentityObserver(ctx, func(context.Context, ProcessExecutionIdentity) error {
				processStarts.Add(1)
				return nil
			})
			call := ToolCall{Name: "bash", Args: map[string]any{"command": command}}
			var invocation PreparedInvocation
			if streaming {
				preparer, ok := any(registry).(StreamingToolCallPreparer)
				require.True(t, ok)
				sink, err := AttestSynchronousStreamSink(&mockStreamSink{})
				require.NoError(t, err)
				invocation, err = preparer.PrepareStreamingToolCall(ctx, call, sink)
				require.NoError(t, err)
				_, err = invocation.(PreparedStreamingInvocation).DispatchStreaming(executionCtx, sink)
				require.Error(t, err)
				assert.Contains(t, err.Error(), "command blocked")
			} else {
				var err error
				invocation, err = registry.PrepareToolCall(ctx, call)
				require.NoError(t, err)
				_, err = invocation.Dispatch(executionCtx)
				require.Error(t, err)
				assert.Contains(t, err.Error(), "command blocked")
			}

			signal := invocation.CompletionSignal()
			require.NotNil(t, signal.Done(), "the explicit sandbox rejection must close the prepared local completion signal")
			select {
			case <-signal.Done():
			default:
				t.Fatal("dispatch returned before recording that validation rejected the command before process start")
			}
			assert.Equal(t, CompletionInlinePipelineJoined, signal.Proof(), "the concrete pre-start denial should attest only the synchronous no-process path")
			assert.Zero(t, processStarts.Load(), "the identity observer runs only after a process has started")
			_, statErr := os.Stat(marker)
			assert.ErrorIs(t, statErr, os.ErrNotExist, "the denied interpreter must not create its marker")
		})
	}
}

func TestPreparedBashInvalidWorkdirProvesNoProcessStarted(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	allowed := filepath.Join(root, "allowed")
	denied := filepath.Join(root, "denied")
	require.NoError(t, os.Mkdir(allowed, 0o700))
	require.NoError(t, os.Mkdir(denied, 0o700))
	marker := filepath.Join(denied, "must-not-exist")
	command := fmt.Sprintf("printf started > %q", marker)

	base := NewDefaultToolRegistry().(*DefaultToolRegistry) //nolint:errcheck
	require.NoError(t, base.registerTrustedLocalProcess(ctx, NewBashTool(
		WithBashWorkdir(denied),
		WithBashSandbox(NewDefaultBashSandbox(WithAllowedPaths([]string{allowed}))),
	)))
	registry := NewMiddlewareToolRegistry(base)
	configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{})

	var processStarts atomic.Int32
	executionCtx := WithProcessExecutionIdentityObserver(ctx, func(context.Context, ProcessExecutionIdentity) error {
		processStarts.Add(1)
		return nil
	})
	invocation, err := registry.PrepareToolCall(ctx, ToolCall{Name: "bash", Args: map[string]any{"command": command}})
	require.NoError(t, err)
	_, err = invocation.Dispatch(executionCtx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is not in the path whitelist")

	signal := invocation.CompletionSignal()
	require.NotNil(t, signal.Done())
	select {
	case <-signal.Done():
	default:
		t.Fatal("workdir validation returned before recording the pre-start path")
	}
	assert.Equal(t, CompletionInlinePipelineJoined, signal.Proof())
	assert.Zero(t, processStarts.Load())
	_, statErr := os.Stat(marker)
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestPreparedLocalBashCompletionRequiresOneConcreteAttestation(t *testing.T) {
	t.Run("missing report does not default to no-process", func(t *testing.T) {
		state := newPreparedCompletionState()
		done := state.begin(completionLocalProcessOrPrestartNoProcessJoined)
		require.NotNil(t, done)
		state.finish(done, true)
		select {
		case <-state.signal().Done():
		default:
			t.Fatal("the synchronous executor returned, so its local signal must close")
		}
		assert.Empty(t, state.signal().Proof(), "no producer attestation must remain unproven")
	})

	t.Run("dispatch mode cannot be reported as a final proof", func(t *testing.T) {
		state := newPreparedCompletionState()
		done := state.begin(completionLocalProcessOrPrestartNoProcessJoined)
		require.NotNil(t, done)
		assert.False(t, validPreparedCompletionProof(completionLocalProcessOrPrestartNoProcessJoined))
		assert.False(t, state.attach(completionLocalProcessOrPrestartNoProcessJoined, closedCompletionChannel()))
		assert.False(t, state.attach(PreparedCompletionProof("unknown-completion-proof"), closedCompletionChannel()))
		state.finish(done, true)
		assert.Empty(t, state.signal().Proof())
	})

	t.Run("prestart report cannot be upgraded to a process report", func(t *testing.T) {
		state := newPreparedCompletionState()
		done := state.begin(completionLocalProcessOrPrestartNoProcessJoined)
		require.NotNil(t, done)
		require.True(t, state.attach(CompletionInlinePipelineJoined, closedCompletionChannel()))
		assert.False(t, state.attach(CompletionOwnedProcessGroupJoined, closedCompletionChannel()))
		state.finish(done, true)
		assert.Equal(t, CompletionInlinePipelineJoined, state.signal().Proof())
	})

	t.Run("process report cannot be replaced by a prestart report", func(t *testing.T) {
		state := newPreparedCompletionState()
		done := state.begin(completionLocalProcessOrPrestartNoProcessJoined)
		require.NotNil(t, done)
		processDone := closedCompletionChannel()
		require.True(t, state.attach(CompletionOwnedProcessGroupJoined, processDone))
		assert.False(t, state.attach(CompletionInlinePipelineJoined, closedCompletionChannel()))
		assert.False(t, state.attach(CompletionOwnedProcessGroupJoined, closedCompletionChannel()), "a second process report cannot replace the first")
		state.finish(done, true)
		assert.Equal(t, CompletionOwnedProcessGroupJoined, state.signal().Proof())
	})

	t.Run("pending owned process keeps the signal open until join", func(t *testing.T) {
		state := newPreparedCompletionState()
		done := state.begin(completionLocalProcessOrPrestartNoProcessJoined)
		require.NotNil(t, done)
		processDone := make(chan struct{})
		require.True(t, state.attach(CompletionOwnedProcessGroupJoined, processDone))
		state.finish(done, true)
		select {
		case <-state.signal().Done():
			t.Fatal("an observed process must remain fenced until its owned join signal closes")
		default:
		}
		close(processDone)
		select {
		case <-state.signal().Done():
		case <-time.After(time.Second):
			t.Fatal("completion did not follow the owned process join")
		}
		assert.Equal(t, CompletionOwnedProcessGroupJoined, state.signal().Proof())
	})

	t.Run("forced pipe close remains unknown without an owned join", func(t *testing.T) {
		state := newPreparedCompletionState()
		done := state.begin(completionLocalProcessOrPrestartNoProcessJoined)
		require.NotNil(t, done)
		ctx := withPreparedCompletionReporter(context.Background(), func(proof PreparedCompletionProof, observedDone <-chan struct{}) {
			assert.True(t, state.attach(proof, observedDone))
		})
		executionDone := closedCompletionChannel()
		reportPreparedLocalProcessCompletion(ctx, managedLocalResult{forcedPipeClose: true, executionDone: executionDone})
		state.finish(done, true)
		select {
		case <-state.signal().Done():
		default:
			t.Fatal("local executor returned without a proven containment signal")
		}
		assert.Empty(t, state.signal().Proof(), "forced reader closure cannot fall back to a synchronous no-process proof")
		assert.False(t, state.attach(CompletionInlinePipelineJoined, closedCompletionChannel()), "finalized unknown completion cannot be rehabilitated later")
	})
}
