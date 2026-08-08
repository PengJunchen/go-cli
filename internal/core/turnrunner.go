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

// messageSource is satisfied by Agent implementations that expose their
// accumulated message history. The TurnRunner uses it to detect compaction
// (a decrease in message count) during a turn.
type messageSource interface {
	Messages() []AgentMessage
}

// EinoTurnRunner is the default TurnRunner. It manages the lifecycle of each
// turn it executes, exposing Cancel, Steer and FollowUp to act on a running
// turn. It drives the injected AgentLoop sequentially per turn and is safe for
// concurrent use.
//
// When an Agent is set via SetAgent, RunTurn delegates to agent.Run (which
// includes history management) instead of calling the loop directly. When a
// steering channel is set via SetSteerChannel, Steer sends the instruction to
// that channel so the running loop picks it up between LLM iterations.
type EinoTurnRunner struct {
	loop       AgentLoop
	agent      Agent
	stream     EventStream
	steerCh    chan string
	followUpCh chan string
	hookChain  *HookChain
	runSlot    RunSlotGuard
	mu         sync.Mutex
	turns      map[string]*Turn
	running    map[string]context.CancelFunc
	idSeq      atomic.Uint64
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
	hookChain := r.hookChain
	runSlot := r.runSlot
	r.mu.Unlock()

	if runSlot == nil {
		runSlot = defaultRunSlotGuard
	}

	// OnTurnStart lifecycle hook (invoked before the run begins).
	if hookChain != nil {
		if herr := hookChain.OnTurnStart(spanCtx, id); herr != nil {
			slog.Warn("core.turn.runner.hook.turn_start", "id", id, "err", herr)
		}
	}

	// Capture message count before the run for compaction detection.
	var beforeMsgCount int
	if ms, ok := r.agent.(messageSource); ok {
		beforeMsgCount = len(ms.Messages())
	}

	var result Result
	var runErr error

	// Claim the run slot before executing the turn so concurrent turns
	// fail fast when a run is already in progress.
	claimCtx, claimCancel := context.WithTimeout(spanCtx, 200*time.Millisecond)
	claim, claimErr := runSlot.ClaimRun(claimCtx)
	claimCancel()
	if claimErr != nil {
		runErr = claimErr
	} else {
		runErr = runSlot.ExecuteClaimedRun(claim, func() error {
			var e error
			if r.agent != nil {
				// Delegate to the agent (includes history management). Pass the
				// stream if one is set so events are streamed in real time.
				if r.stream != nil {
					result, e = r.agent.Run(spanCtx, submission, r.stream)
				} else {
					result, e = r.agent.Run(spanCtx, submission)
				}
			} else if r.stream != nil {
				var events []AgentEvent
				events, e = r.loop.Run(spanCtx, submission, r.stream)
				result = Result{Message: lastMessageEvent(events), Success: e == nil}
			} else {
				var events []AgentEvent
				events, e = r.loop.Run(spanCtx, submission)
				result = Result{Message: lastMessageEvent(events), Success: e == nil}
			}
			return e
		})
	}

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

	// Post-run lifecycle hooks. OnError fires when the run failed; OnCompaction
	// fires when the agent's message count decreased during the turn; OnTurnEnd
	// always fires to signal the turn is complete.
	if hookChain != nil {
		if runErr != nil {
			if herr := hookChain.OnError(spanCtx, id, runErr); herr != nil {
				slog.Warn("core.turn.runner.hook.on_error", "id", id, "err", herr)
			}
		}
		if ms, ok := r.agent.(messageSource); ok {
			afterMsgCount := len(ms.Messages())
			if afterMsgCount < beforeMsgCount {
				if herr := hookChain.OnCompaction(spanCtx, beforeMsgCount, afterMsgCount); herr != nil {
					slog.Warn("core.turn.runner.hook.compaction", "id", id, "err", herr)
				}
			}
		}
		if herr := hookChain.OnTurnEnd(spanCtx, id, result, runErr); herr != nil {
			slog.Warn("core.turn.runner.hook.turn_end", "id", id, "err", herr)
		}
	}

	endSpan, _ := tracing.SpanFromContext(spanCtx, "turn.end", tracing.SpanKindInternal)
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
// recorded on the turn and surfaced via Get so the runtime can observe it.
// When a steering channel is set, the instruction is also sent to that
// channel so the running loop picks it up between LLM iterations. It returns
// an error if the turn is unknown or not running.
func (r *EinoTurnRunner) Steer(ctx context.Context, id, instruction string) error {
	if err := r.inject(id, Submission{Type: SubmissionSteering, Content: instruction}, "steer"); err != nil {
		return err
	}
	// Send to the steering channel so the running loop drains it between
	// LLM iterations. The send is non-blocking: if the channel is full,
	// the instruction is still recorded on the Turn (above) and the loop
	// will pick up whatever is in the buffer.
	r.mu.Lock()
	steerCh := r.steerCh
	r.mu.Unlock()
	if steerCh != nil {
		select {
		case steerCh <- instruction:
			slog.Info("core.turn.runner.steer.sent", "id", id, "instruction", instruction)
		default:
			slog.Warn("core.turn.runner.steer.channel_full", "id", id)
			return fmt.Errorf("steering channel full")
		}
	}
	return nil
}

// SetSteerChannel sets the channel used to deliver steering instructions to
// the running loop. It must be called before RunTurn starts the turn.
func (r *EinoTurnRunner) SetSteerChannel(ch chan string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.steerCh = ch
}

// SetFollowUpChannel sets the channel used to deliver follow-up user messages
// to the running loop. It must be called before RunTurn starts the turn.
func (r *EinoTurnRunner) SetFollowUpChannel(ch chan string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.followUpCh = ch
}

// SetAgent sets the Agent that RunTurn delegates to when non-nil. When set,
// RunTurn calls agent.Run (which includes history management) instead of
// calling the loop directly.
func (r *EinoTurnRunner) SetAgent(a Agent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.agent = a
}

// SetHookChain sets the HookChain used to dispatch lifecycle events
// (OnTurnStart, OnTurnEnd, OnError, OnCompaction) during RunTurn. Set to nil
// to disable lifecycle hook dispatch.
func (r *EinoTurnRunner) SetHookChain(chain *HookChain) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hookChain = chain
}

// SetRunSlotGuard sets the RunSlotGuard used to enforce single-run
// exclusivity. When nil (or not set), the runner uses the noop default so
// concurrent turns are allowed.
func (r *EinoTurnRunner) SetRunSlotGuard(g RunSlotGuard) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runSlot = g
}

// SetStream sets the EventStream that RunTurn passes to the agent or loop so
// events are streamed in real time. Set to nil to disable streaming.
func (r *EinoTurnRunner) SetStream(s EventStream) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stream = s
}

// RunningTurnID returns the ID of the currently running turn, or the empty
// string when no turn is running.
func (r *EinoTurnRunner) RunningTurnID() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, turn := range r.turns {
		if turn.Status == TurnRunning {
			return id
		}
	}
	return ""
}

// FollowUp appends a follow-up user message to a running turn. It is recorded
// on the turn and surfaced via Get. When a follow-up channel is set, the
// instruction is also sent to that channel so the running loop picks it up
// between LLM iterations. It returns an error if the turn is unknown or not
// running.
func (r *EinoTurnRunner) FollowUp(_ context.Context, id, content string) error {
	if err := r.inject(id, Submission{Type: SubmissionFollowUp, Content: content}, "followup"); err != nil {
		return err
	}
	// Send to the follow-up channel so the running loop drains it between
	// LLM iterations. The send is non-blocking: if the channel is full,
	// the message is still recorded on the Turn (above) and the loop will
	// pick up whatever is in the buffer.
	r.mu.Lock()
	ch := r.followUpCh
	r.mu.Unlock()
	if ch != nil {
		select {
		case ch <- content:
			slog.Info("core.turn.runner.followup.sent", "id", id, "content_len", len(content))
		default:
			slog.Warn("core.turn.runner.followup.channel_full", "id", id)
			return fmt.Errorf("followup channel full")
		}
	}
	return nil
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
