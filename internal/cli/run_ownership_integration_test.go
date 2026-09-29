package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/tools"
)

const assemblyPendingMutationToolName = "assembly_barrier_mutation"

var errAssemblyPendingMutation = errors.New("assembly mutation continues behind test barrier")

type assemblyPendingExecutionError struct {
	done <-chan struct{}
}

func (e assemblyPendingExecutionError) Error() string { return errAssemblyPendingMutation.Error() }
func (e assemblyPendingExecutionError) OutcomeUnknown() bool {
	return true
}
func (e assemblyPendingExecutionError) ExecutionDone() <-chan struct{} { return e.done }
func (e assemblyPendingExecutionError) Unwrap() error                  { return errAssemblyPendingMutation }

type assemblyBarrierMutationTool struct {
	release <-chan struct{}

	mu          sync.Mutex
	dispatched  map[string]int
	completed   map[string]int
	countSignal chan struct{}
}

func newAssemblyBarrierMutationTool(release <-chan struct{}) *assemblyBarrierMutationTool {
	return &assemblyBarrierMutationTool{
		release:     release,
		dispatched:  make(map[string]int),
		completed:   make(map[string]int),
		countSignal: make(chan struct{}, 1),
	}
}

func (*assemblyBarrierMutationTool) Name() string { return assemblyPendingMutationToolName }

func (*assemblyBarrierMutationTool) Description() string {
	return "Apply a controlled test mutation and wait for its execution barrier."
}

func (*assemblyBarrierMutationTool) Parameters() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"label": map[string]any{"type": "string"},
		},
		"required": []string{"label"},
	}
}

func (tool *assemblyBarrierMutationTool) Execute(_ context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
	label, _ := call.Args["label"].(string)
	if label == "" {
		return nil, errors.New("assembly test mutation requires a label")
	}
	tool.mu.Lock()
	tool.dispatched[label]++
	tool.mu.Unlock()
	select {
	case tool.countSignal <- struct{}{}:
	default:
	}

	done := make(chan struct{})
	go func() {
		<-tool.release
		tool.mu.Lock()
		tool.completed[label]++
		tool.mu.Unlock()
		close(done)
		select {
		case tool.countSignal <- struct{}{}:
		default:
		}
	}()
	return nil, assemblyPendingExecutionError{done: done}
}

func (tool *assemblyBarrierMutationTool) count(label string) (dispatched, completed int) {
	tool.mu.Lock()
	defer tool.mu.Unlock()
	return tool.dispatched[label], tool.completed[label]
}

func (tool *assemblyBarrierMutationTool) waitForCount(t *testing.T, label string, wantDispatched, wantCompleted int) {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		dispatched, completed := tool.count(label)
		if dispatched >= wantDispatched && completed >= wantCompleted {
			return
		}
		select {
		case <-tool.countSignal:
		case <-ticker.C:
		case <-deadline.C:
			t.Fatalf("tool %q counts=(dispatched=%d, completed=%d), want at least (%d, %d)", label, dispatched, completed, wantDispatched, wantCompleted)
		}
	}
}

type assemblyRequestMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type assemblyToolCallFixture struct {
	id    string
	label string
}

type assemblyOwnershipFixture struct {
	streamFixture *assemblyStreamFixture
	mu            sync.Mutex
	requests      map[string]int
}

func newAssemblyOwnershipFixture(t *testing.T, parallel bool) *assemblyOwnershipFixture {
	t.Helper()
	fixture := &assemblyOwnershipFixture{requests: make(map[string]int)}
	fixture.streamFixture = newAssemblyStreamFixture(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read request body", http.StatusBadRequest)
			return
		}
		var request struct {
			Stream   bool                     `json:"stream"`
			Messages []assemblyRequestMessage `json:"messages"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			http.Error(w, "decode request body", http.StatusBadRequest)
			return
		}
		prompt := lastAssemblyUserMessage(request.Messages)
		fixture.mu.Lock()
		fixture.requests[prompt]++
		attempt := fixture.requests[prompt]
		fixture.mu.Unlock()

		var calls []assemblyToolCallFixture
		switch prompt {
		case "primary":
			if attempt == 1 {
				calls = []assemblyToolCallFixture{{id: "primary-a", label: "primary"}}
				if parallel {
					calls = append(calls, assemblyToolCallFixture{id: "primary-b", label: "primary"})
				}
			}
		case "blocked":
			if attempt == 1 {
				calls = []assemblyToolCallFixture{{id: "blocked-a", label: "blocked"}}
			}
		case "after-release":
			if attempt == 1 {
				calls = []assemblyToolCallFixture{{id: "after-release-a", label: "after-release"}}
			}
		}
		if len(calls) > 0 {
			writeAssemblyToolCallResponse(w, request.Stream, calls)
			return
		}
		writeAssemblyFinalResponse(w, request.Stream, "complete")
	})
	return fixture
}

func lastAssemblyUserMessage(messages []assemblyRequestMessage) string {
	var last string
	for _, message := range messages {
		if message.Role != "user" {
			continue
		}
		var content string
		if err := json.Unmarshal(message.Content, &content); err == nil {
			last = content
		}
	}
	return last
}

func (fixture *assemblyOwnershipFixture) requestCount(prompt string) int {
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	return fixture.requests[prompt]
}

func writeAssemblyToolCallResponse(w http.ResponseWriter, streaming bool, calls []assemblyToolCallFixture) {
	toolCalls := make([]map[string]any, 0, len(calls))
	for index, call := range calls {
		args, _ := json.Marshal(map[string]string{"label": call.label})
		toolCalls = append(toolCalls, map[string]any{
			"index": index,
			"id":    call.id,
			"type":  "function",
			"function": map[string]any{
				"name":      assemblyPendingMutationToolName,
				"arguments": string(args),
			},
		})
	}
	if streaming {
		writeAssemblyStreamHeaders(w)
		writeAssemblyChunk(w, map[string]any{"tool_calls": toolCalls}, nil)
		writeAssemblyChunk(w, map[string]any{}, "tool_calls")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []any{map[string]any{
			"message":       map[string]any{"role": "assistant", "tool_calls": toolCalls},
			"finish_reason": "tool_calls",
		}},
	})
}

func writeAssemblyFinalResponse(w http.ResponseWriter, streaming bool, content string) {
	if streaming {
		writeAssemblyTextResponse(w, []string{content}, "stop")
		return
	}
	writeAssemblyGenerateTextResponse(w, content, "stop")
}

func TestAssemblySharesRunSlotGuardAcrossHarnessAndTurnRunner(t *testing.T) {
	cases := []struct {
		name           string
		parallel       bool
		firstIsHarness bool
	}{
		{name: "sequential_harness_to_turn_runner", firstIsHarness: true},
		{name: "parallel_harness_to_turn_runner", parallel: true, firstIsHarness: true},
		{name: "sequential_turn_runner_to_harness"},
		{name: "parallel_turn_runner_to_harness", parallel: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			previousLogger := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
			t.Cleanup(func() { slog.SetDefault(previousLogger) })

			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			t.Cleanup(cancel)
			fixture := newAssemblyOwnershipFixture(t, tc.parallel)
			config := newE2ETestConfig(t, fixture.streamFixture.server.URL)
			config.Git.WorkDir = t.TempDir()
			parallel := tc.parallel
			config.Tools.Parallel = &parallel
			assembly, err := AssembleAgent(ctx, config, config.Provider.Name, config.Provider.Model, io.Discard, WithApproveMode(ApproveAuto))
			require.NoError(t, err)
			t.Cleanup(assembly.Cleanup)

			releaseBarrier := make(chan struct{})
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(releaseBarrier) }) }
			t.Cleanup(release)
			mutationTool := newAssemblyBarrierMutationTool(releaseBarrier)
			require.NoError(t, assembly.ToolRegistry.Register(ctx, mutationTool))

			wantPrimaryCalls := 1
			if tc.parallel {
				wantPrimaryCalls = 2
			}
			var firstStream core.EventStream
			var firstResult core.Result
			var firstErr error
			firstTurnDone := make(chan struct{})
			if tc.firstIsHarness {
				firstStream, err = assembly.Harness.Submit(ctx, "primary")
				require.NoError(t, err)
			} else {
				go func() {
					defer close(firstTurnDone)
					firstResult, firstErr = assembly.TurnRunner.RunTurn(ctx, core.Submission{Content: "primary"})
				}()
			}
			mutationTool.waitForCount(t, "primary", wantPrimaryCalls, 0)

			var firstHandle core.RunStatusHandle
			if tc.firstIsHarness {
				for range firstStream.Events() {
				}
				_, streamErr := firstStream.Result()
				assert.Error(t, streamErr, "the pending action must not be reported as successful")
				statusSource, ok := firstStream.(core.RunStatusSource)
				require.True(t, ok)
				firstHandle = statusSource.RunStatusHandle()
			} else {
				select {
				case <-firstTurnDone:
				case <-ctx.Done():
					t.Fatalf("TurnRunner did not return its bounded cleanup snapshot: %v", ctx.Err())
				}
				require.Error(t, firstErr)
				assert.Equal(t, core.RunExecutionCleanupPending, firstResult.ExecutionState)
				firstHandle = firstResult.StatusHandle
			}
			require.NotNil(t, firstHandle)
			firstStatus := waitForAssemblyPendingStatus(t, firstHandle)
			assert.Equal(t, core.RunOutcomeUnknown, firstStatus.Outcome)
			assert.GreaterOrEqual(t, firstStatus.PendingWorkers, wantPrimaryCalls)
			assertCanonicalAssemblyMutationReceipts(t, assembly.Agent, "primary", wantPrimaryCalls)

			blockedCtx, blockedCancel := context.WithTimeout(ctx, 40*time.Millisecond)
			if tc.firstIsHarness {
				_, blockedErr := assembly.TurnRunner.RunTurn(blockedCtx, core.Submission{Content: "blocked"})
				assert.ErrorIs(t, blockedErr, context.DeadlineExceeded)
			} else {
				blockedStream, blockedErr := assembly.Harness.Submit(blockedCtx, "blocked")
				assert.ErrorIs(t, blockedErr, context.DeadlineExceeded)
				assert.Nil(t, blockedStream)
			}
			blockedCancel()
			assert.Equal(t, 0, fixture.requestCount("blocked"), "the rejected run must not reach the provider")
			blockedCalls, _ := mutationTool.count("blocked")
			assert.Zero(t, blockedCalls, "the rejected run must not dispatch a mutation")

			release()
			mutationTool.waitForCount(t, "primary", wantPrimaryCalls, wantPrimaryCalls)
			waitForAssemblyRunState(t, firstHandle, core.RunExecutionQuiescent)
			assert.Equal(t, 0, firstHandle.Snapshot().PendingWorkers)
			assert.Equal(t, core.RunOutcomeUnknown, firstHandle.Snapshot().Outcome, "quiescence does not rewrite the unknown action receipt")

			if tc.firstIsHarness {
				secondResult, secondErr := assembly.TurnRunner.RunTurn(ctx, core.Submission{Content: "after-release"})
				require.Error(t, secondErr)
				assert.Equal(t, core.RunExecutionQuiescent, secondResult.ExecutionState)
				assert.Equal(t, core.RunOutcomeUnknown, secondResult.Outcome)
				waitForAssemblyRunState(t, secondResult.StatusHandle, core.RunExecutionQuiescent)
			} else {
				secondStream, secondErr := assembly.Harness.Submit(ctx, "after-release")
				require.NoError(t, secondErr)
				for range secondStream.Events() {
				}
				_, resultErr := secondStream.Result()
				require.Error(t, resultErr)
				statusSource, ok := secondStream.(core.RunStatusSource)
				require.True(t, ok)
				waitForAssemblyRunState(t, statusSource.RunStatusHandle(), core.RunExecutionQuiescent)
				assert.Equal(t, core.RunOutcomeUnknown, statusSource.RunStatusHandle().Snapshot().Outcome)
			}
			mutationTool.waitForCount(t, "after-release", 1, 1)
			blockedCalls, _ = mutationTool.count("blocked")
			assert.Zero(t, blockedCalls)
		})
	}
}

func waitForAssemblyPendingStatus(t *testing.T, handle core.RunStatusHandle) core.RunStatus {
	t.Helper()
	var status core.RunStatus
	require.Eventually(t, func() bool {
		status = handle.Snapshot()
		return status.ExecutionState == core.RunExecutionCleanupPending || status.ExecutionState == core.RunExecutionQuarantined
	}, 2*time.Second, time.Millisecond, "run should return a pending or quarantined status")
	return status
}

func waitForAssemblyRunState(t *testing.T, handle core.RunStatusHandle, want core.RunExecutionState) {
	t.Helper()
	require.Eventually(t, func() bool {
		return handle.Snapshot().ExecutionState == want
	}, 2*time.Second, time.Millisecond, "run should reach %s", want)
}

func assertCanonicalAssemblyMutationReceipts(t *testing.T, agent *core.AgentImpl, label string, want int) {
	t.Helper()
	messages := agent.Messages()
	calls := make(map[string]struct{})
	receipts := make(map[string]string)
	matchedUserSubmission := false
	for _, message := range messages {
		if message.Role == "user" && message.Content == label {
			matchedUserSubmission = true
			assert.Equal(t, core.InputKindUser, message.InputKind, "the real Assembly compaction hook must preserve input metadata")
		}
		if message.Role == "assistant" {
			for _, call := range message.ToolCalls {
				if call.Name == assemblyPendingMutationToolName && assemblyToolCallLabel(call.Args) == label {
					calls[call.ID] = struct{}{}
				}
			}
		}
		if message.Role == "tool" && message.ToolName == assemblyPendingMutationToolName {
			if _, primaryCall := calls[message.ToolCallID]; primaryCall {
				receipts[message.ToolCallID] = message.ReceiptOutcome
			}
		}
	}
	assert.True(t, matchedUserSubmission, "the submitted user input must be committed to canonical history")
	require.Len(t, calls, want, "the canonical transcript must commit each dispatched call")
	require.Len(t, receipts, want, "the canonical transcript must commit one matching receipt per call")
	for callID := range calls {
		assert.Equal(t, core.ReceiptOutcomeUnknown, receipts[callID], "tool receipt %q remains unknown until the barrier completes", callID)
	}
}

func assemblyToolCallLabel(args any) string {
	values, _ := args.(map[string]any)
	label, _ := values["label"].(string)
	return label
}
