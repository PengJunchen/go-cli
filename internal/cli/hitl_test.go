package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/tui"
)

// hitlTestModel is a minimal tea.Model used to drive cliHITLEmitter in tests.
// When it receives a HITLMessage it records the message, invokes respond to
// compute a HITLResponse, and sends that response on the message's ResponseCh.
// It does not auto-quit so multiple events can be forwarded through a single
// program; callers stop the program with program.Quit().
type hitlTestModel struct {
	respond  func(HITLMessage) HITLResponse
	mu       sync.Mutex
	received []HITLMessage
}

func (m *hitlTestModel) Init() tea.Cmd { return nil }

func (m *hitlTestModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	hm, ok := msg.(HITLMessage)
	if !ok {
		return m, nil
	}
	m.mu.Lock()
	m.received = append(m.received, hm)
	m.mu.Unlock()
	resp := m.respond(hm)
	hm.ResponseCh <- resp
	return m, nil
}

func (m *hitlTestModel) View() string { return "" }

// receivedCopy returns a snapshot of the messages the model has seen so far.
// Tests must read the slice under the model's mutex because Update runs in the
// program's event-loop goroutine.
func (m *hitlTestModel) receivedCopy() []HITLMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]HITLMessage(nil), m.received...)
}

// newHitlProgram starts a real bubbletea program backed by model in a background
// goroutine. The program is configured without a renderer, input, or signal
// handler so it runs headless in a test process. The returned channel closes
// once Run has fully shut down, letting callers wait for cleanup.
func newHitlProgram(t *testing.T, model tea.Model) (*tea.Program, <-chan struct{}) {
	t.Helper()
	p := tea.NewProgram(model,
		tea.WithoutSignalHandler(),
		tea.WithoutRenderer(),
		tea.WithInput(nil),
		tea.WithOutput(io.Discard),
	)
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		_, _ = p.Run()
	}()
	return p, runDone
}

// stopHitlProgram gracefully shuts down a program started by newHitlProgram and
// waits for Run to return. It is safe to call from a defer.
func stopHitlProgram(t *testing.T, p *tea.Program, runDone <-chan struct{}) {
	t.Helper()
	p.Quit()
	select {
	case <-runDone:
	case <-time.After(3 * time.Second):
		t.Fatal("tea.Program did not shut down within timeout")
	}
}

// emitCtx returns a context with a generous timeout so a misbehaving program
// fails the test instead of hanging the whole suite.
func emitCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 3*time.Second)
}

// TestHITLEmitViaProgram verifies that when a program is wired, Emit forwards
// the question as a HITLMessage via program.Send, waits for the TUI's
// HITLResponse, and delivers the answer on event.ResponseCh with no error.
func TestHITLEmitViaProgram(t *testing.T) {
	model := &hitlTestModel{
		respond: func(_ HITLMessage) HITLResponse {
			return HITLResponse{Answer: "yes"}
		},
	}
	program, runDone := newHitlProgram(t, model)
	defer stopHitlProgram(t, program, runDone)

	emitter := &cliHITLEmitter{out: io.Discard, program: program}
	event := core.HITLQuestionEvent{
		QuestionID: "q1",
		Question:   "Proceed?",
		Options:    []string{"yes", "no"},
		ResponseCh: make(chan core.HITLAnswer, 1),
	}

	ctx, cancel := emitCtx()
	defer cancel()
	err := emitter.Emit(ctx, event)
	require.NoError(t, err)

	select {
	case ans := <-event.ResponseCh:
		assert.Equal(t, "q1", ans.QuestionID)
		assert.Equal(t, "yes", ans.Answer)
		assert.NoError(t, ans.Error)
	case <-time.After(time.Second):
		t.Fatal("did not receive answer on ResponseCh")
	}

	got := model.receivedCopy()
	require.Len(t, got, 1, "program must receive exactly one HITLMessage")
	assert.Equal(t, "q1", got[0].QuestionID)
	assert.Equal(t, "Proceed?", got[0].Question)
	assert.Equal(t, []string{"yes", "no"}, got[0].Options)
	assert.NotNil(t, got[0].ResponseCh)
}

// TestHITLFallbackNoProgram verifies that when program is nil (plain CLI mode),
// Emit writes the question directly to the output writer and reads the answer
// from stdin, delivering it on event.ResponseCh.
func TestHITLFallbackNoProgram(t *testing.T) {
	// Redirect stdin to a pipe pre-filled with the answer line. readLine reads
	// os.Stdin directly, so the global must be swapped for the fallback path.
	origStdin := os.Stdin
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = origStdin
		r.Close()
		w.Close()
	})
	// Pre-fill the pipe; ReadString returns once it sees the newline.
	_, err = w.Write([]byte("yes\n"))
	require.NoError(t, err)

	var out bytes.Buffer
	emitter := &cliHITLEmitter{out: &out, program: nil}
	event := core.HITLQuestionEvent{
		QuestionID: "q2",
		Question:   "Continue?",
		Options:    []string{"yes", "no"},
		ResponseCh: make(chan core.HITLAnswer, 1),
	}

	ctx, cancel := emitCtx()
	defer cancel()
	err = emitter.Emit(ctx, event)
	require.NoError(t, err)

	// The question must have been written directly to stdout (the out buffer).
	assert.Contains(t, out.String(), "[ask_user]")
	assert.Contains(t, out.String(), "Continue?")
	assert.Contains(t, out.String(), "1. yes")
	assert.Contains(t, out.String(), "2. no")

	select {
	case ans := <-event.ResponseCh:
		assert.Equal(t, "q2", ans.QuestionID)
		assert.Equal(t, "yes", ans.Answer)
	case <-time.After(time.Second):
		t.Fatal("did not receive answer on ResponseCh")
	}
}

// TestHITLNoDirectStdoutWithProgram verifies that when a program is wired, Emit
// must NOT write anything directly to the output writer; the question is routed
// exclusively through program.Send so the TUI display is not corrupted.
func TestHITLNoDirectStdoutWithProgram(t *testing.T) {
	var out bytes.Buffer
	model := &hitlTestModel{
		respond: func(_ HITLMessage) HITLResponse {
			return HITLResponse{Answer: "ok"}
		},
	}
	program, runDone := newHitlProgram(t, model)
	defer stopHitlProgram(t, program, runDone)

	emitter := &cliHITLEmitter{out: &out, program: program}
	event := core.HITLQuestionEvent{
		QuestionID: "q3",
		Question:   "Direct write?",
		ResponseCh: make(chan core.HITLAnswer, 1),
	}

	ctx, cancel := emitCtx()
	defer cancel()
	err := emitter.Emit(ctx, event)
	require.NoError(t, err)

	assert.Empty(t, out.String(), "program mode must not write directly to stdout")
}

// TestHITLMultipleEventsAllForwarded verifies that every Emit call forwards its
// HITLMessage through program.Send and that all responses are delivered. Because
// Emit is synchronous and the event loop processes messages one at a time, the
// model observes the questions in FIFO order.
func TestHITLMultipleEventsAllForwarded(t *testing.T) {
	const n = 5
	model := &hitlTestModel{
		respond: func(hm HITLMessage) HITLResponse {
			return HITLResponse{Answer: "ans-" + hm.QuestionID}
		},
	}
	program, runDone := newHitlProgram(t, model)
	defer stopHitlProgram(t, program, runDone)

	emitter := &cliHITLEmitter{out: io.Discard, program: program}

	for i := 0; i < n; i++ {
		id := fmt.Sprintf("q%d", i)
		event := core.HITLQuestionEvent{
			QuestionID: id,
			Question:   fmt.Sprintf("Question %d", i),
			ResponseCh: make(chan core.HITLAnswer, 1),
		}
		ctx, cancel := emitCtx()
		err := emitter.Emit(ctx, event)
		cancel()
		require.NoError(t, err)

		select {
		case ans := <-event.ResponseCh:
			assert.Equal(t, id, ans.QuestionID)
			assert.Equal(t, "ans-"+id, ans.Answer)
		case <-time.After(time.Second):
			t.Fatalf("event %d did not receive answer", i)
		}
	}

	got := model.receivedCopy()
	require.Len(t, got, n, "all events must be forwarded to the program")
	for i, hm := range got {
		assert.Equal(t, fmt.Sprintf("q%d", i), hm.QuestionID,
			"events must be forwarded in FIFO order")
	}
}

// TestHITLProgramReadyBeforeTurn verifies that BubbleteaApp.ProgramReady()
// channel is open before Run starts and closes once Run has stored the
// tea.Program. After the channel closes, Program() must return non-nil.
// This replaces the former polling loop that called Program() up to 100 times.
func TestHITLProgramReadyBeforeTurn(t *testing.T) {
	events := make(chan tui.AgentEvent)
	app := tui.NewBubbleteaApp(events)

	// Before Run, ProgramReady should be open (not closed).
	select {
	case <-app.ProgramReady():
		t.Fatal("ProgramReady should not be closed before Run")
	default:
	}

	runCtx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()

	go func() {
		_ = app.Run(runCtx)
	}()

	// Wait for ProgramReady to close (program stored).
	select {
	case <-app.ProgramReady():
	case <-time.After(3 * time.Second):
		t.Fatal("ProgramReady did not close within timeout")
	}

	// Program must now be available.
	prog := app.Program()
	require.NotNil(t, prog, "Program must be non-nil after ProgramReady closes")

	// Clean up: close events to make the app exit, then wait for Done.
	close(events)
	cancelRun()
	select {
	case <-app.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("app did not shut down within timeout")
	}
}

// TestHITLProgramReadyChannelSync verifies the full integration: after
// ProgramReady fires, the program can be wired to a cliHITLEmitter and
// Emit routes through it successfully. This validates the channel-based
// replacement for the polling loop in interactive.go.
func TestHITLProgramReadyChannelSync(t *testing.T) {
	events := make(chan tui.AgentEvent)
	app := tui.NewBubbleteaApp(events)

	runCtx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()

	go func() {
		_ = app.Run(runCtx)
	}()

	// Wait for program readiness via channel (no polling).
	select {
	case <-app.ProgramReady():
	case <-time.After(3 * time.Second):
		t.Fatal("ProgramReady did not close within timeout")
	}

	prog := app.Program()
	require.NotNil(t, prog)

	// Verify the emitter integration: wire a program to a HITL emitter
	// and confirm Emit routes through it, exactly as interactive.go does
	// after ProgramReady fires. We use a standalone program (not the
	// BubbleteaApp's program) because the app's program runs a teaModel
	// that cannot handle HITLMessage; the hitlTestModel can.
	model := &hitlTestModel{
		respond: func(_ HITLMessage) HITLResponse {
			return HITLResponse{Answer: "channel-sync"}
		},
	}
	// The hitlTestModel needs to be the model behind the program. Since
	// we're testing the emitter's SetProgram path, we use a standalone
	// program to receive the HITLMessage.
	hitlProg, hitlDone := newHitlProgram(t, model)
	defer stopHitlProgram(t, hitlProg, hitlDone)

	emitter := &cliHITLEmitter{out: io.Discard}
	emitter.SetProgram(hitlProg)

	event := core.HITLQuestionEvent{
		QuestionID: "sync-q",
		Question:   "Channel sync?",
		ResponseCh: make(chan core.HITLAnswer, 1),
	}
	ctx, cancel := emitCtx()
	defer cancel()
	err := emitter.Emit(ctx, event)
	require.NoError(t, err)

	select {
	case ans := <-event.ResponseCh:
		assert.Equal(t, "sync-q", ans.QuestionID)
		assert.Equal(t, "channel-sync", ans.Answer)
	case <-time.After(time.Second):
		t.Fatal("did not receive answer via channel-synced program")
	}

	// Clean up the BubbleteaApp.
	close(events)
	cancelRun()
	select {
	case <-app.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("app did not shut down within timeout")
	}
}

// TestHITLStaleProgramCleared verifies that after SetProgram(nil) is called
// (as interactive.go does at the end of each turn), Emit falls back to
// direct stdout output instead of attempting to use a stale program.
// This ensures no stale program reference leaks across turns.
func TestHITLStaleProgramCleared(t *testing.T) {
	model := &hitlTestModel{
		respond: func(_ HITLMessage) HITLResponse {
			return HITLResponse{Answer: "stale"}
		},
	}
	program, runDone := newHitlProgram(t, model)
	defer stopHitlProgram(t, program, runDone)

	var out bytes.Buffer
	emitter := &cliHITLEmitter{out: &out, program: program}

	// Verify program is initially wired and Emit routes through it.
	event1 := core.HITLQuestionEvent{
		QuestionID: "stale-q1",
		Question:   "Before clear?",
		ResponseCh: make(chan core.HITLAnswer, 1),
	}
	ctx1, cancel1 := emitCtx()
	defer cancel1()
	err := emitter.Emit(ctx1, event1)
	require.NoError(t, err)
	assert.Empty(t, out.String(), "should route through program, not stdout")

	// Clear the program (simulating end-of-turn cleanup).
	emitter.SetProgram(nil)

	// After clearing, Emit must fall back to direct stdout.
	origStdin := os.Stdin
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = origStdin
		r.Close()
		w.Close()
	})
	_, err = w.Write([]byte("fallback\n"))
	require.NoError(t, err)

	out.Reset()
	event2 := core.HITLQuestionEvent{
		QuestionID: "stale-q2",
		Question:   "After clear?",
		Options:    []string{"fallback", "no"},
		ResponseCh: make(chan core.HITLAnswer, 1),
	}
	ctx2, cancel2 := emitCtx()
	defer cancel2()
	err = emitter.Emit(ctx2, event2)
	require.NoError(t, err)

	// Must have fallen back to direct stdout output.
	assert.Contains(t, out.String(), "[ask_user]")
	assert.Contains(t, out.String(), "After clear?")

	select {
	case ans := <-event2.ResponseCh:
		assert.Equal(t, "stale-q2", ans.QuestionID)
		assert.Equal(t, "fallback", ans.Answer)
	case <-time.After(time.Second):
		t.Fatal("did not receive answer after stale program cleared")
	}
}
