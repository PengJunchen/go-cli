package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPreparedTerminalObserverReportsCallerVisibleResult(t *testing.T) {
	tests := []struct {
		name       string
		result     *ToolResult
		err        error
		wantClass  string
		wantResult bool
		wantError  bool
	}{
		{
			name:       "success",
			result:     &ToolResult{Output: "done"},
			wantClass:  PreparedTerminalErrorSuccess,
			wantResult: true,
		},
		{
			name:       "structured result error",
			result:     &ToolResult{Output: "failed", IsError: true},
			wantClass:  PreparedTerminalErrorResult,
			wantResult: true,
			wantError:  true,
		},
		{
			name:       "executor error",
			result:     &ToolResult{Output: "partial"},
			err:        errors.New("executor failed"),
			wantClass:  PreparedTerminalErrorExecutor,
			wantResult: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var observed atomic.Int32
			var outcome PreparedTerminalOutcome
			fence := NewDispatchFence()
			invocation := newPreparedInvocation(preparedInvocationConfig{
				fence:       fence,
				generations: fence.Snapshot(),
				call:        mustFreezeToolCall(t, ToolCall{ID: "call-1", Name: "capture"}),
				execute: func(context.Context, ToolCall) (*ToolResult, error) {
					return test.result, test.err
				},
				terminalObserver: func(got PreparedTerminalOutcome) error {
					observed.Add(1)
					outcome = got
					return nil
				},
			})

			result, err := invocation.Dispatch(context.Background())
			if test.err != nil {
				require.ErrorIs(t, err, test.err)
			} else {
				require.NoError(t, err)
			}
			assert.Same(t, test.result, result)
			assert.EqualValues(t, 1, observed.Load())
			assert.Equal(t, terminalFingerprint("call-1"), outcome.ToolCallID)
			assert.Equal(t, "capture", outcome.ToolName)
			assert.True(t, outcome.DispatchCommitted)
			assert.Equal(t, test.wantResult, outcome.ResultPresent)
			assert.Equal(t, test.wantError, outcome.ResultIsError)
			assert.Equal(t, test.wantClass, outcome.ErrorClass)
		})
	}
}

func TestPreparedTerminalObserverReportsPendingCleanupWithoutWaiting(t *testing.T) {
	processDone := make(chan struct{})
	var observed atomic.Int32
	var outcome PreparedTerminalOutcome
	fence := NewDispatchFence()
	invocation := newPreparedInvocation(preparedInvocationConfig{
		fence:           fence,
		generations:     fence.Snapshot(),
		call:            mustFreezeToolCall(t, ToolCall{ID: "call-pending", Name: "capture"}),
		completionProof: CompletionOwnedProcessGroupJoined,
		execute: func(ctx context.Context, _ ToolCall) (*ToolResult, error) {
			reportPreparedCompletion(ctx, CompletionOwnedProcessGroupJoined, processDone)
			return &ToolResult{Output: "started"}, pendingProcessErrorWithDone(errors.New("still running"), true, processDone)
		},
		terminalObserver: func(got PreparedTerminalOutcome) error {
			observed.Add(1)
			outcome = got
			return nil
		},
	})

	started := time.Now()
	result, err := invocation.Dispatch(context.Background())
	require.Error(t, err)
	assert.Less(t, time.Since(started), time.Second, "the caller-visible result must not wait for process-group join")
	assert.Equal(t, "started", result.Output)
	assert.EqualValues(t, 1, observed.Load())
	assert.True(t, outcome.DispatchCommitted)
	assert.True(t, outcome.ResultPresent)
	assert.Equal(t, PreparedTerminalErrorCleanupPending, outcome.ErrorClass)
	select {
	case <-invocation.CompletionSignal().Done():
		t.Fatal("generation retirement signal closed before the process joined")
	default:
	}

	close(processDone)
	select {
	case <-invocation.CompletionSignal().Done():
	case <-time.After(time.Second):
		t.Fatal("process completion did not release the prepared completion")
	}
	assert.EqualValues(t, 1, observed.Load(), "cleanup completion must not produce a second outcome")
}

func TestPreparedTerminalObserverFailureDoesNotRewriteToolResult(t *testing.T) {
	secret := errors.New("credential-bearing callback failure")
	fence := NewDispatchFence()
	invocation := newPreparedInvocation(preparedInvocationConfig{
		fence:       fence,
		generations: fence.Snapshot(),
		call:        mustFreezeToolCall(t, ToolCall{ID: "call-2", Name: "capture"}),
		execute: func(context.Context, ToolCall) (*ToolResult, error) {
			return &ToolResult{Output: "completed"}, nil
		},
		terminalObserver: func(PreparedTerminalOutcome) error { return secret },
	})

	result, err := invocation.Dispatch(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "completed", result.Output)
}

func TestPreparedTerminalObserverIsCapturedFromPolicyDecision(t *testing.T) {
	ctx := context.Background()
	base := NewDefaultToolRegistry().(*DefaultToolRegistry) //nolint:errcheck
	tool := &preparedCaptureTool{name: "capture", schema: map[string]any{
		"type": "object", "properties": map[string]any{}, "additionalProperties": false,
	}}
	require.NoError(t, base.Register(ctx, tool))
	registry := NewMiddlewareToolRegistry(base)
	var observed atomic.Int32
	configurePreparedRegistry(t, registry, func(_ context.Context, request PreparedPolicyRequest) (PreparedPolicyDecision, error) {
		return PreparedPolicyDecision{
			Allowed: true, Generations: request.Generations,
			TerminalObserver: func(outcome PreparedTerminalOutcome) error {
				observed.Add(1)
				assert.Equal(t, terminalFingerprint("call-3"), outcome.ToolCallID)
				assert.Equal(t, "capture", outcome.ToolName)
				return nil
			},
		}, nil
	}, PreparedToolStages{})

	invocation, err := registry.PrepareToolCall(ctx, ToolCall{ID: "call-3", Name: "capture", Args: map[string]any{}})
	require.NoError(t, err)
	_, err = invocation.Dispatch(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 1, observed.Load())
}

func TestPreparedTerminalObserverRunsAfterQueuedResult(t *testing.T) {
	ctx := context.Background()
	base := NewDefaultToolRegistry().(*DefaultToolRegistry) //nolint:errcheck
	require.NoError(t, RegisterDefaults(ctx, base, WithRegisteredBuiltinWhitelist([]string{"write"})))
	registry := NewMiddlewareToolRegistry(base)
	queue := NewDefaultFileMutationQueue().(*DefaultFileMutationQueue) //nolint:errcheck
	defer queue.Close()
	var observed atomic.Int32
	var outcome PreparedTerminalOutcome
	configurePreparedRegistry(t, registry, func(_ context.Context, request PreparedPolicyRequest) (PreparedPolicyDecision, error) {
		return PreparedPolicyDecision{
			Allowed: true, Generations: request.Generations,
			TerminalObserver: func(got PreparedTerminalOutcome) error {
				observed.Add(1)
				outcome = got
				return nil
			},
		}, nil
	}, PreparedToolStages{MutationQueue: queue})

	path := filepath.Join(t.TempDir(), "queued.txt")
	invocation, err := registry.PrepareToolCall(ctx, ToolCall{ID: "call-queued", Name: "write", Args: map[string]any{"path": path, "content": "queued"}})
	require.NoError(t, err)
	result, err := invocation.Dispatch(ctx)
	require.NoError(t, err)
	contents, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "queued", string(contents))
	assert.Contains(t, result.Output, "wrote 6 bytes")
	assert.EqualValues(t, 1, observed.Load())
	assert.Equal(t, terminalFingerprint("call-queued"), outcome.ToolCallID)
	assert.Equal(t, "write", outcome.ToolName)
	assert.True(t, outcome.DispatchCommitted)
	assert.True(t, outcome.ResultPresent)
	assert.Equal(t, PreparedTerminalErrorSuccess, outcome.ErrorClass)
}

func TestPreparedTerminalObserverRunsAfterStreamingResult(t *testing.T) {
	var observed atomic.Int32
	var outcome PreparedTerminalOutcome
	fence := NewDispatchFence()
	invocation := newPreparedStreamingInvocation(preparedInvocationConfig{
		fence:       fence,
		generations: fence.Snapshot(),
		call:        mustFreezeToolCall(t, ToolCall{ID: "call-stream", Name: "bash"}),
		terminalObserver: func(got PreparedTerminalOutcome) error {
			observed.Add(1)
			outcome = got
			return nil
		},
	}, func(_ context.Context, _ ToolCall, sink StreamSink) (*ToolResult, error) {
		if err := sink.Send("line", "call-stream", "stdout"); err != nil {
			return nil, err
		}
		return &ToolResult{Output: "streamed"}, nil
	})
	sink := &preparedCaptureSink{}

	result, err := invocation.DispatchStreaming(context.Background(), sink)
	require.NoError(t, err)
	assert.Equal(t, "streamed", result.Output)
	assert.Equal(t, "line", sink.content)
	assert.EqualValues(t, 1, observed.Load())
	assert.Equal(t, terminalFingerprint("call-stream"), outcome.ToolCallID)
	assert.Equal(t, "bash", outcome.ToolName)
	assert.True(t, outcome.DispatchCommitted)
	assert.Equal(t, PreparedTerminalErrorSuccess, outcome.ErrorClass)
}

func TestPreparedTerminalObserverReportsStalePreparedDispatchOnce(t *testing.T) {
	fence := NewDispatchFence()
	var observed atomic.Int32
	var outcome PreparedTerminalOutcome
	invocation := newPreparedInvocation(preparedInvocationConfig{
		fence:       fence,
		generations: fence.Snapshot(),
		call:        mustFreezeToolCall(t, ToolCall{ID: "call-stale", Name: "capture"}),
		execute:     func(context.Context, ToolCall) (*ToolResult, error) { return &ToolResult{Output: "unreachable"}, nil },
		terminalObserver: func(got PreparedTerminalOutcome) error {
			observed.Add(1)
			outcome = got
			return nil
		},
	})
	_, err := fence.Publish(DispatchFencePlan, nil, func() {})
	require.NoError(t, err)

	_, err = invocation.Dispatch(context.Background())
	require.ErrorIs(t, err, ErrDispatchFenceChanged)
	_, err = invocation.Dispatch(context.Background())
	require.ErrorIs(t, err, ErrPreparedInvocationConsumed)
	assert.EqualValues(t, 1, observed.Load())
	assert.Equal(t, terminalFingerprint("call-stale"), outcome.ToolCallID)
	assert.False(t, outcome.DispatchCommitted)
	assert.False(t, outcome.ResultPresent)
	assert.Equal(t, PreparedTerminalErrorDispatchReject, outcome.ErrorClass)
}

func TestPreparedTerminalOutcomeBoundsAndSanitizesUntrustedIdentity(t *testing.T) {
	tests := []struct {
		name         string
		callID       string
		toolName     string
		wantToolName string
	}{
		{
			name:         "oversized identity",
			callID:       strings.Repeat("provider-call-", 512),
			toolName:     strings.Repeat("tool-name-", 512),
			wantToolName: "[redacted:oversized]",
		},
		{
			name:         "control characters",
			callID:       "call\x00\tsecret\nvalue",
			toolName:     "read\nworkspace\tfiles",
			wantToolName: "read_workspace_files",
		},
		{
			name:         "credential-like identity",
			callID:       "api_key=sk-live-super-secret-value",
			toolName:     "fetch_token=private-credential-value",
			wantToolName: "[redacted]",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fence := NewDispatchFence()
			var outcome PreparedTerminalOutcome
			invocation := newPreparedInvocation(preparedInvocationConfig{
				fence:       fence,
				generations: fence.Snapshot(),
				identity:    TrustedToolIdentity{toolName: test.toolName},
				call:        mustFreezeToolCall(t, ToolCall{ID: test.callID, Name: test.toolName}),
				execute: func(context.Context, ToolCall) (*ToolResult, error) {
					return &ToolResult{Output: "done"}, nil
				},
				terminalObserver: func(got PreparedTerminalOutcome) error {
					outcome = got
					return nil
				},
			})

			_, err := invocation.Dispatch(context.Background())
			require.NoError(t, err)
			assert.Equal(t, terminalFingerprint(test.callID), outcome.ToolCallID)
			assert.Equal(t, test.wantToolName, outcome.ToolName)
			assert.LessOrEqual(t, len(outcome.ToolCallID), 128)
			assert.LessOrEqual(t, len(outcome.ToolName), 128)
			assert.NotContains(t, outcome.ToolCallID, test.callID)
			assert.NotContains(t, outcome.ToolName, test.toolName)
			for _, r := range outcome.ToolName {
				assert.False(t, r < 0x20 || r == 0x7f, "control rune leaked into tool name")
			}

			encoded, err := json.Marshal(outcome)
			require.NoError(t, err)
			assert.NotContains(t, string(encoded), test.callID)
			assert.NotContains(t, string(encoded), test.toolName)
			assert.NotContains(t, string(encoded), "sk-live-super-secret-value")
			assert.NotContains(t, string(encoded), "private-credential-value")
		})
	}
}

func terminalFingerprint(value string) string {
	digest := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(digest[:])
}

// An unbuffered result channel makes the queue's publication boundary explicit:
// ordinary completion must precede publication, while a pending resource must
// still publish its result before that resource joins.
func TestPreparedQueueCompletionPublicationOrder(t *testing.T) {
	for _, pending := range []bool{false, true} {
		name := "joined executor"
		if pending {
			name = "pending resource"
		}
		t.Run(name, func(t *testing.T) {
			queue, ok := NewDefaultFileMutationQueue().(*DefaultFileMutationQueue)
			require.True(t, ok)
			input := make(chan queuedMutation)
			results := make(chan FileMutationResult)
			done := make(chan struct{})
			resourceDone := make(chan struct{})
			var release sync.Once
			queue.startWorker(input)
			defer func() {
				release.Do(func() { close(resourceDone) })
				// Also releases a worker blocked publishing after a failed assertion.
				for range results {
				}
				close(input)
				require.NoError(t, queue.Close())
			}()
			fence := NewDispatchFence()
			invocation := newPreparedInvocation(preparedInvocationConfig{
				fence: fence, generations: fence.Snapshot(),
				call:            mustFreezeToolCall(t, ToolCall{ID: "queued-order", Name: "capture"}),
				completionProof: CompletionQueuedJobJoined,
				execute: func(context.Context, ToolCall) (*ToolResult, error) {
					result := &ToolResult{Output: "executed"}
					if pending {
						return result, pendingProcessErrorWithDone(errors.New("still running"), true, resourceDone)
					}
					return result, nil
				},
			})
			require.True(t, invocation.completion.attach(CompletionQueuedJobJoined, done))
			input <- queuedMutation{ctx: context.Background(), prepared: invocation, result: results, done: done}
			if !pending {
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Fatal("joined executor completion waited for result consumption")
				}
				require.Equal(t, CompletionQueuedJobJoined, invocation.CompletionSignal().Proof())
			}
			var result FileMutationResult
			select {
			case result = <-results:
			case <-time.After(time.Second):
				t.Fatal("pending resource prevented caller-visible result publication")
			}
			require.NotNil(t, result.ToolResult)
			require.Equal(t, "executed", result.ToolResult.Output)
			if pending {
				require.Error(t, result.Error)
				select {
				case <-done:
					t.Fatal("pending resource was reported joined before it completed")
				default:
				}
				require.Empty(t, invocation.CompletionSignal().Proof())
				release.Do(func() { close(resourceDone) })
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Fatal("resource join did not complete its queued job")
				}
				require.Equal(t, CompletionQueuedJobJoined, invocation.CompletionSignal().Proof())
			} else {
				require.NoError(t, result.Error)
			}
		})
	}
}
