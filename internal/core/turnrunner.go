package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// errTurnUnknown reports that a turn id is not managed by the runner.
var errTurnUnknown = errors.New("core: unknown or inactive turn")

// EinoTurnRunner is the default TurnRunner. It manages the lifecycle of each
// turn it executes, exposing Cancel, Steer and FollowUp to act on a running
// turn. It drives the injected AgentLoop sequentially per turn and is safe for
// concurrent use.
type EinoTurnRunner struct {
	loop    AgentLoop
	mu      sync.Mutex
	turns   map[string]*Turn
	running map[string]context.CancelFunc
	idSeq   atomic.Uint64
}

var _ TurnRunner = (*EinoTurnRunner)(nil)

// NewEinoTurnRunner builds a TurnRunner bound to the given AgentLoop. A nil
// loop causes the first RunTurn to fail with errNilModel rather than panic only
// if the loop is never set; callers should always pass a real loop.
func NewEinoTurnRunner(loop AgentLoop) *EinoTurnRunner {
	r := &EinoTurnRunner{
		loop:    loop,
		turns:   make(map[string]*Turn),
		running: make(map[string]context.CancelFunc),
	}
	slog.Info("core.turn.runner.new", "loop_set", loop != nil)
	return r
}

// newID generates a unique, monotonic turn identifier.
func (r *EinoTurnRunner) newID() string {
	return fmt.Sprintf("turn-%d", r.idSeq.Add(1))
}

// RunTurn executes one turn from the submission and returns its result. It
// creates a cancellable turn, drives the loop, and records the terminal state.
func (r *EinoTurnRunner) RunTurn(ctx context.Context, submission Submission) (Result, error) {
	if r.loop == nil {
		return Result{}, errNilModel
	}

	turnCtx, cancel := context.WithCancel(ctx)
	defer cancel() // release the turn's context resources when the turn ends
	id := r.newID()
	turn := &Turn{
		ID:         id,
		Submission: submission,
		Status:     TurnRunning,
		StartTime:  time.Now(),
	}

	span, spanCtx := tracing.SpanFromContext(turnCtx, "turn.start", tracing.SpanKindInternal)
	span.SetAttributes(tracing.Attribute{Key: "turn_id", Value: id}, tracing.Attribute{Key: "type", Value: submission.Type.String()})
	span.End()
	logger := tracing.NewTraceLogger(span, nil)
	slog.Info("core.turn.runner.start", "id", id, "type", submission.Type)

	r.mu.Lock()
	r.turns[id] = turn
	r.running[id] = cancel
	r.mu.Unlock()

	events, runErr := r.loop.Run(spanCtx, submission)
	result := Result{Message: lastMessageEvent(events), Success: runErr == nil}

	r.mu.Lock()
	delete(r.running, id)
	turn.Result = result
	turn.Err = runErr
	turn.EndTime = time.Now()
	switch {
	case turn.Canceled:
		turn.Status = TurnCanceled
	case runErr != nil:
		turn.Status = TurnFailed
	default:
		turn.Status = TurnCompleted
	}
	r.mu.Unlock()

	endSpan, _ := tracing.SpanFromContext(ctx, "turn.end", tracing.SpanKindInternal)
	endSpan.SetAttributes(tracing.Attribute{Key: "turn_id", Value: id}, tracing.Attribute{Key: "status", Value: turn.Status.String()})
	endSpan.End()

	slog.Info("core.turn.runner.end",
		"id", id,
		"status", turn.Status.String(),
		"canceled", turn.Canceled,
		"error", runErr != nil,
	)
	logger.Info("turn.end", "id", id, "status", turn.Status.String())
	return result, runErr
}

// Cancel marks a running turn as canceled and cancels its underlying context.
// It returns an error if the turn is unknown or not currently running.
func (r *EinoTurnRunner) Cancel(ctx context.Context, id string) error {
	span, _ := tracing.SpanFromContext(ctx, "turn.cancel", tracing.SpanKindInternal)
	defer span.End()
	span.SetAttributes(
		tracing.Attribute{Key: "turn_id", Value: id},
	)

	r.mu.Lock()
	turn, ok := r.turns[id]
	cancel, running := r.running[id]
	r.mu.Unlock()

	if !ok || !running {
		span.SetStatus(tracing.SpanStatusError, errTurnUnknown.Error())
		return errTurnUnknown
	}

	cancel()

	r.mu.Lock()
	turn.Canceled = true
	if turn.Status == TurnRunning {
		turn.Status = TurnCanceled
	}
	r.mu.Unlock()

	span.SetStatus(tracing.SpanStatusOK, "")
	slog.Info("core.turn.runner.cancel", "id", id)
	return nil
}

// Steer injects a steering submission into a running turn. The steering is
// recorded on the turn and surfaced via Get so the runtime can observe it; it
// returns an error if the turn is unknown or not running.
func (r *EinoTurnRunner) Steer(_ context.Context, id, instruction string) error {
	return r.inject(id, Submission{Type: SubmissionSteering, Content: instruction}, "steer")
}

// FollowUp appends a follow-up user message to a running turn. It is recorded
// on the turn and surfaced via Get; it returns an error if the turn is unknown
// or not running.
func (r *EinoTurnRunner) FollowUp(_ context.Context, id, content string) error {
	return r.inject(id, Submission{Type: SubmissionFollowUp, Content: content}, "followup")
}

// inject appends a submission of the given kind to a running turn's
// corresponding slice.
func (r *EinoTurnRunner) inject(id string, submission Submission, kind string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	turn, ok := r.turns[id]
	if !ok || turn.Status != TurnRunning {
		slog.Info("core.turn.runner.inject.skip", "id", id, "kind", kind)
		return errTurnUnknown
	}
	switch kind {
	case "steer":
		turn.Steerings = append(turn.Steerings, submission)
	case "followup":
		turn.FollowUps = append(turn.FollowUps, submission)
	}
	slog.Info("core.turn.runner.inject", "id", id, "kind", kind, "content", submission.Content)
	return nil
}

// Get returns a copy of the turn with the given id, or an error if unknown.
func (r *EinoTurnRunner) Get(_ context.Context, id string) (Turn, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.turns[id]
	if !ok {
		return Turn{}, errTurnUnknown
	}
	turn := *t
	turn.Steerings = append([]Submission{}, t.Steerings...)
	turn.FollowUps = append([]Submission{}, t.FollowUps...)
	return turn, nil
}
