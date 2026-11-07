package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

type serialContractDispatcher struct {
	mu           sync.Mutex
	calls        []string
	tasks        []SubagentTask
	parallel     int
	started      chan string
	firstRelease chan struct{}
	failID       string
	failErr      error
}

func (d *serialContractDispatcher) Dispatch(ctx context.Context, task SubagentTask) (SubagentResult, error) {
	d.mu.Lock()
	d.calls = append(d.calls, task.ID)
	d.tasks = append(d.tasks, task)
	d.mu.Unlock()
	if d.started != nil {
		d.started <- task.ID
	}
	result := SubagentResult{TaskID: task.ID, PhysicalSessionID: "session-" + task.ID, Content: "answer-" + task.ID}
	if task.ID == "a" && d.firstRelease != nil {
		select {
		case <-d.firstRelease:
		case <-ctx.Done():
			result.Error = ctx.Err()
			return result, ctx.Err()
		}
	}
	if task.ID == d.failID {
		result.Error = d.failErr
		return result, d.failErr
	}
	return result, nil
}
func (d *serialContractDispatcher) ParallelDispatch(_ context.Context, tasks []SubagentTask) ([]SubagentResult, error) {
	d.mu.Lock()
	d.parallel++
	d.mu.Unlock()
	out := make([]SubagentResult, len(tasks))
	for i, task := range tasks {
		out[i] = SubagentResult{TaskID: task.ID, PhysicalSessionID: "session-" + task.ID, Content: "parallel-" + task.ID}
	}
	return out, nil
}
func (*serialContractDispatcher) ListRunning() []SubagentTask { return nil }
func (d *serialContractDispatcher) snapshot() ([]string, int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.calls...), d.parallel
}

type serialCallResult struct {
	result *ToolResult
	err    error
}

type serialUnknownOutcome struct{ cause error }

func (e serialUnknownOutcome) Error() string      { return "unknown outcome: " + e.cause.Error() }
func (e serialUnknownOutcome) Unwrap() error      { return e.cause }
func (serialUnknownOutcome) OutcomeUnknown() bool { return true }

type serialExplicitOutcome struct {
	cause   error
	unknown bool
}

func (e *serialExplicitOutcome) Error() string        { return e.cause.Error() }
func (e *serialExplicitOutcome) Unwrap() error        { return e.cause }
func (e *serialExplicitOutcome) OutcomeUnknown() bool { return e.unknown }

type serialPendingOutcome struct {
	*serialExplicitOutcome
	done <-chan struct{}
}

func (e *serialPendingOutcome) ExecutionDone() <-chan struct{} { return e.done }

func serialCall(tasks ...any) ToolCall {
	return ToolCall{Args: map[string]any{"execution_mode": "sequential", "tasks": tasks}}
}
func serialTask(id string) map[string]any { return map[string]any{"id": id, "prompt": "task " + id} }
func serialReceive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(3 * time.Second):
		t.Fatal("fixture timed out waiting for channel")
		var zero T
		return zero
	}
}

func TestSequentialTaskBatchWaitsForFirstChildAndPreservesSessionIDs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := &serialContractDispatcher{started: make(chan string, 4), firstRelease: make(chan struct{})}
	done := make(chan serialCallResult, 1)
	go func() {
		r, e := NewSubagentTool(d).Execute(ctx, serialCall(serialTask("a"), serialTask("b")))
		done <- serialCallResult{r, e}
	}()
	var first string
	select {
	case first = <-d.started:
	case r := <-done:
		t.Fatalf("sequential batch returned before first child started: result=%+v error=%v", r.result, r.err)
	case <-time.After(3 * time.Second):
		t.Fatal("first child did not start")
	}
	if first != "a" {
		t.Fatalf("first started=%q, want a", first)
	}
	calls, parallel := d.snapshot()
	if fmt.Sprint(calls) != "[a]" || parallel != 0 {
		t.Fatalf("while child a is blocked: calls=%v parallel=%d; child b must not start", calls, parallel)
	}
	close(d.firstRelease)
	if next := serialReceive(t, d.started); next != "b" {
		t.Fatalf("second started=%q, want b", next)
	}
	r := serialReceive(t, done)
	if r.err != nil || r.result == nil {
		t.Fatalf("sequential result=%+v error=%v", r.result, r.err)
	}
	body := r.result.Output + fmt.Sprint(r.result.Metadata)
	for _, part := range []string{"answer-a", "answer-b", "session-a", "session-b"} {
		if !strings.Contains(body, part) {
			t.Errorf("aggregated result missing %q: %s", part, body)
		}
	}
}

func TestSequentialBatchValidatesEveryTaskBeforeAnyDispatch(t *testing.T) {
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"empty tasks", map[string]any{"execution_mode": "sequential", "tasks": []any{}}},
		{"bad final prompt", map[string]any{"execution_mode": "sequential", "tasks": []any{serialTask("a"), map[string]any{"id": "bad", "prompt": " "}}}},
		{"bad final object", map[string]any{"execution_mode": "sequential", "tasks": []any{serialTask("a"), 17}}},
		{"nil final object", map[string]any{"execution_mode": "sequential", "tasks": []any{serialTask("a"), nil}}},
		{"bad context", map[string]any{"execution_mode": "sequential", "tasks": []any{serialTask("a"), map[string]any{"id": "b", "prompt": "task b", "context_mode": "invalid"}}}},
		{"bad model type", map[string]any{"execution_mode": "sequential", "tasks": []any{serialTask("a"), map[string]any{"id": "b", "prompt": "task b", "model": 7}}}},
		{"bad role", map[string]any{"execution_mode": "sequential", "tasks": []any{serialTask("a"), map[string]any{"id": "b", "prompt": "task b", "role": "unknown"}}}},
		{"bad tools type", map[string]any{"execution_mode": "sequential", "tasks": []any{serialTask("a"), map[string]any{"id": "b", "prompt": "task b", "tools": []any{"read", 3}}}}},
		{"bad max turns", map[string]any{"execution_mode": "sequential", "tasks": []any{serialTask("a"), map[string]any{"id": "b", "prompt": "task b", "max_turns": "four"}}}},
		{"fractional max turns", map[string]any{"execution_mode": "sequential", "tasks": []any{serialTask("a"), map[string]any{"id": "b", "prompt": "task b", "max_turns": 2.5}}}},
		{"overflow max turns", map[string]any{"execution_mode": "sequential", "tasks": []any{serialTask("a"), map[string]any{"id": "b", "prompt": "task b", "max_turns": json.Number("9223372036854775808")}}}},
		{"bad system prompt", map[string]any{"execution_mode": "sequential", "tasks": []any{serialTask("a"), map[string]any{"id": "b", "prompt": "task b", "system_prompt": false}}}},
		{"invalid mode", map[string]any{"execution_mode": "bad-mode", "tasks": []any{serialTask("a")}}},
		{"empty mode", map[string]any{"execution_mode": "", "tasks": []any{serialTask("a")}}},
		{"nonstring mode", map[string]any{"execution_mode": true, "tasks": []any{serialTask("a")}}},
		{"nonbool parallel", map[string]any{"execution_mode": "sequential", "parallel": "false", "tasks": []any{serialTask("a")}}},
		{"conflicting parallel", map[string]any{"execution_mode": "sequential", "parallel": true, "tasks": []any{serialTask("a")}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &serialContractDispatcher{}
			_, err := NewSubagentTool(d).Execute(context.Background(), ToolCall{Args: tc.args})
			calls, parallel := d.snapshot()
			if err == nil || len(calls) != 0 || parallel != 0 {
				t.Fatalf("invalid batch must fail before any dispatch: err=%v sequential=%v parallel=%d", err, calls, parallel)
			}
		})
	}
}

func TestSequentialBatchStopsOnErrorWithPartialEvidence(t *testing.T) {
	sentinel := errors.New("fixture child unknown outcome")
	d := &serialContractDispatcher{failID: "b", failErr: serialUnknownOutcome{cause: sentinel}}
	result, err := NewSubagentTool(d).Execute(context.Background(), serialCall(serialTask("a"), serialTask("b"), serialTask("c")))
	calls, parallel := d.snapshot()
	if !errors.Is(err, sentinel) || fmt.Sprint(calls) != "[a b]" || parallel != 0 {
		t.Fatalf("error must stop before c and preserve cause: err=%v calls=%v parallel=%d", err, calls, parallel)
	}
	var unknown interface{ OutcomeUnknown() bool }
	if !errors.As(err, &unknown) || !unknown.OutcomeUnknown() {
		t.Fatal("unknown outcome classification must survive error wrapping")
	}
	if result == nil {
		t.Fatal("partial result and created child identities must be returned")
	}
	body := result.Output + fmt.Sprint(result.Metadata)
	for _, part := range []string{"answer-a", "answer-b", "session-a", "session-b", "not_started"} {
		if !strings.Contains(body, part) {
			t.Errorf("partial result missing %q: %s", part, body)
		}
		if !strings.Contains(err.Error(), part) {
			t.Errorf("partial failure error missing %q: %s", part, err)
		}
	}
	if strings.Contains(body, "session-c") || strings.Contains(err.Error(), "session-c") {
		t.Fatal("unstarted child must not have a fabricated physical session")
	}
}

func TestSequentialModeWithExplicitFalseRunsSequentially(t *testing.T) {
	d := &serialContractDispatcher{}
	call := serialCall(serialTask("a"), serialTask("b"))
	call.Args["parallel"] = false
	_, err := NewSubagentTool(d).Execute(context.Background(), call)
	calls, parallel := d.snapshot()
	if err != nil || fmt.Sprint(calls) != "[a b]" || parallel != 0 {
		t.Fatalf("explicit sequential overrides false parallel: err=%v calls=%v parallel=%d", err, calls, parallel)
	}
}

func TestSequentialBatchCancellationNeverStartsNextChild(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := &serialContractDispatcher{started: make(chan string, 4), firstRelease: make(chan struct{})}
	done := make(chan serialCallResult, 1)
	go func() {
		r, e := NewSubagentTool(d).Execute(ctx, serialCall(serialTask("a"), serialTask("b")))
		done <- serialCallResult{r, e}
	}()
	select {
	case id := <-d.started:
		if id != "a" {
			t.Fatalf("first child=%s", id)
		}
	case r := <-done:
		t.Fatalf("sequential batch returned before first child: %v", r.err)
	case <-time.After(3 * time.Second):
		t.Fatal("first child not started")
	}
	cancel()
	r := serialReceive(t, done)
	calls, parallel := d.snapshot()
	if !errors.Is(r.err, context.Canceled) || fmt.Sprint(calls) != "[a]" || parallel != 0 {
		t.Fatalf("cancellation launched later child or lost cause: err=%v calls=%v parallel=%d", r.err, calls, parallel)
	}
	if r.result == nil || !strings.Contains(r.result.Output+fmt.Sprint(r.result.Metadata), "session-a") {
		t.Fatal("cancellation must preserve created child ID")
	}
	body := r.result.Output + fmt.Sprint(r.result.Metadata)
	if !strings.Contains(body, "not_started") || strings.Contains(body, "session-b") ||
		!strings.Contains(r.err.Error(), "not_started") || !strings.Contains(r.err.Error(), "session-a") || strings.Contains(r.err.Error(), "session-b") {
		t.Fatal("canceled batch must mark the later child not started without fabricating a session")
	}
}

func TestSequentialCancellationOutcomePreservesExistingMarkers(t *testing.T) {
	done := make(chan struct{})
	markedFalse := &serialExplicitOutcome{cause: context.Canceled}
	markedTrue := &serialExplicitOutcome{cause: context.Canceled, unknown: true}
	pendingFalse := &serialPendingOutcome{serialExplicitOutcome: &serialExplicitOutcome{cause: context.Canceled}, done: done}
	pendingTrue := &serialPendingOutcome{serialExplicitOutcome: &serialExplicitOutcome{cause: context.DeadlineExceeded, unknown: true}, done: done}
	plainFailure := errors.New("ordinary failure")
	cases := []struct {
		name        string
		cause       error
		wantMarker  bool
		wantUnknown bool
	}{
		{"plain cancellation", context.Canceled, true, true},
		{"plain deadline", context.DeadlineExceeded, true, true},
		{"explicit false", markedFalse, true, false},
		{"explicit true", markedTrue, true, true},
		{"pending false", pendingFalse, true, false},
		{"pending true", pendingTrue, true, true},
		{"ordinary failure", plainFailure, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &serialContractDispatcher{failID: "a", failErr: tc.cause}
			_, err := NewSubagentTool(d).Execute(context.Background(), serialCall(serialTask("a"), serialTask("b")))
			if !errors.Is(err, tc.cause) || !strings.Contains(err.Error(), "not_started") {
				t.Fatalf("error must preserve cause and partial output: %v", err)
			}
			var marker interface{ OutcomeUnknown() bool }
			if found := errors.As(err, &marker); found != tc.wantMarker || (found && marker.OutcomeUnknown() != tc.wantUnknown) {
				t.Fatalf("outcome marker: found=%v err=%v", found, err)
			}
			if original, ok := tc.cause.(*serialExplicitOutcome); ok {
				var actual *serialExplicitOutcome
				if !errors.As(err, &actual) || actual != original {
					t.Fatal("explicit outcome identity must remain in error chain")
				}
			}
			if original, ok := tc.cause.(*serialPendingOutcome); ok {
				var actual PendingExecutionError
				if !errors.As(err, &actual) || actual != original || actual.ExecutionDone() != done {
					t.Fatal("pending execution identity and done receipt must remain intact")
				}
			}
		})
	}
}

func TestSequentialBatchKeepsSupportedIntegerAndContextValues(t *testing.T) {
	d := &serialContractDispatcher{}
	call := serialCall(
		map[string]any{"id": "a", "prompt": "first", "max_turns": json.Number("2"), "context_mode": "inherit"},
		map[string]any{"id": "b", "prompt": "second", "max_turns": -1, "tools": []any{"grep"}},
	)
	_, err := NewSubagentTool(d).Execute(context.Background(), call)
	if err != nil || len(d.tasks) != 2 || d.tasks[0].MaxTurns != 2 || d.tasks[0].ContextMode != "inherit" || d.tasks[1].MaxTurns != -1 || fmt.Sprint(d.tasks[1].Tools) != "[grep]" {
		t.Fatalf("valid task options were not preserved: err=%v tasks=%+v", err, d.tasks)
	}
}

func TestExplicitParallelAndLegacySinglePromptRemainUnchanged(t *testing.T) {
	t.Run("explicit parallel", func(t *testing.T) {
		d := &serialContractDispatcher{}
		_, err := NewSubagentTool(d).Execute(context.Background(), ToolCall{Args: map[string]any{"execution_mode": "parallel", "tasks": []any{serialTask("a"), serialTask("b")}}})
		calls, parallel := d.snapshot()
		if err != nil || len(calls) != 0 || parallel != 1 {
			t.Fatalf("explicit parallel must use ParallelDispatch: %v %v %d", err, calls, parallel)
		}
	})
	t.Run("legacy single", func(t *testing.T) {
		d := &serialContractDispatcher{}
		_, err := NewSubagentTool(d).Execute(context.Background(), ToolCall{Args: map[string]any{"id": "a", "prompt": "single"}})
		calls, parallel := d.snapshot()
		if err != nil || fmt.Sprint(calls) != "[a]" || parallel != 0 {
			t.Fatalf("legacy single must use Dispatch: %v %v %d", err, calls, parallel)
		}
	})
}

func TestLegacyTasksWithFalseParallelRemainParallel(t *testing.T) {
	d := &serialContractDispatcher{}
	_, err := NewSubagentTool(d).Execute(context.Background(), ToolCall{Args: map[string]any{"parallel": false, "tasks": []any{serialTask("a"), serialTask("b")}}})
	calls, parallel := d.snapshot()
	if err != nil || len(calls) != 0 || parallel != 1 {
		t.Fatalf("legacy tasks must remain parallel: err=%v calls=%v parallel=%d", err, calls, parallel)
	}
}

func TestSequentialModeIsDiscoverableInSchemaAndGuidance(t *testing.T) {
	tool := NewSubagentTool(&serialContractDispatcher{})
	schema := fmt.Sprint(tool.Parameters())
	guide := strings.Join(tool.PromptGuidelines(), " ") + tool.Description()
	if !strings.Contains(schema, "execution_mode") || !strings.Contains(schema, "sequential") || !strings.Contains(guide, "sequential") {
		t.Fatalf("sequential mode must be discoverable in schema and guidance: schema=%s guidance=%s", schema, guide)
	}
}
