package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// errTurnUnknown reports that a turn id is not managed by the runner.
var errTurnUnknown = errors.New("core: unknown or inactive turn")
var errCanonicalTranscriptUnavailable = errors.New("core: loop cannot provide a canonical transcript for tool calls")
var errMessageSnapshotPanic = errors.New("core: agent message snapshot panicked")
var errTurnFinalizerPanic = errors.New("core: turn wrapper finalizer panicked")

// maxTurnsHistory bounds the number of completed turns retained in the turns
// map. Once the count of completed turns exceeds this value, the oldest ones
// are pruned to prevent unbounded memory growth in long sessions. Turns that
// are still running are never pruned.
const maxTurnsHistory = 100

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
	loop               AgentLoop
	agent              Agent
	stream             EventStream
	lifetimeCtx        context.Context
	generationProvider RunGenerationProvider
	steerCh            chan string
	followUpCh         chan string
	hookChain          *HookChain
	runSlot            RunSlotGuard
	cleanupTimeout     time.Duration
	mu                 sync.Mutex
	turns              map[string]*Turn
	running            map[string]context.CancelFunc
	owners             map[string]*runOwner
	idSeq              atomic.Uint64
}

var _ TurnRunner = (*EinoTurnRunner)(nil)

// NewEinoTurnRunner builds a TurnRunner bound to the given AgentLoop. A nil
// loop causes the first RunTurn to fail with errNilModel rather than panic only
// if the loop is never set; callers should always pass a real loop.
func NewEinoTurnRunner(loop AgentLoop) *EinoTurnRunner {
	r := &EinoTurnRunner{
		loop:           loop,
		turns:          make(map[string]*Turn),
		running:        make(map[string]context.CancelFunc),
		owners:         make(map[string]*runOwner),
		cleanupTimeout: defaultRunCleanupTimeout,
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
func (r *EinoTurnRunner) RunTurn(ctx context.Context, submission Submission) (result Result, returnErr error) {
	leaseGuard, leaseCapturePanic := captureRunCommitLeaseGuard(ctx)
	leaseTransferred := false
	defer func() {
		if !leaseTransferred && leaseGuard.close() {
			returnErr = errors.Join(returnErr, ErrRunCommitLeaseClosePanicked)
		}
	}()
	if r.loop == nil {
		return Result{}, errNilModel
	}
	submission = cloneSubmission(submission)
	id := r.newID()
	turn := &Turn{
		ID:         id,
		Submission: cloneSubmission(submission),
		Status:     TurnRunning,
	}

	r.mu.Lock()
	hookChain := r.hookChain
	runSlot := r.runSlot
	cleanupTimeout := r.cleanupTimeout
	agent := r.agent
	stream := r.stream
	lifetimeCtx := r.lifetimeCtx
	generationProvider := r.generationProvider
	r.mu.Unlock()

	if runSlot == nil {
		runSlot = defaultRunSlotGuard
	}
	turnCtx, cancel, stopLifetime := linkRunLifetime(ctx, lifetimeCtx)
	var contextStopOnce sync.Once
	stopTurnContext := func() {
		contextStopOnce.Do(func() {
			stopLifetime()
			cancel()
		})
	}
	contextTransferred := false
	defer func() {
		if !contextTransferred {
			stopTurnContext()
		}
	}()
	claimCtx, claimCancel := context.WithTimeout(turnCtx, 200*time.Millisecond)
	claim, claimErr := runSlot.ClaimRun(claimCtx)
	claimCancel()
	if claimErr != nil {
		result := Result{Outcome: RunOutcomeError, ExecutionState: RunExecutionQuiescent}
		turn.Result = result
		turn.Err = claimErr
		turn.EndTime = time.Now()
		turn.Status = TurnFailed
		r.mu.Lock()
		r.turns[id] = turn
		r.pruneCompletedTurns()
		r.mu.Unlock()
		return result, claimErr
	}
	turn.StartTime = time.Now()
	ownerStarted := false
	var wrapperDone func()
	var span tracing.TraceSpan
	completionTransferred := false
	var completionOnce sync.Once
	completeWrapper := func() {
		completionOnce.Do(func() {
			if wrapperDone != nil {
				defer callTurnWrapperCleanup("wrapper", wrapperDone)
			}
			defer stopTurnContext()
			if !ownerStarted {
				defer callTurnWrapperCleanup("claim release", func() { runSlot.Release(claim) })
			}
			if span != nil {
				defer callTurnWrapperCleanup("span end", span.End)
			}
		})
	}
	defer func() {
		if !completionTransferred {
			completeWrapper()
		}
	}()
	// Install claim cleanup before invoking the optional wrapper registrar.
	// A custom registrar may panic before returning its completion callback.
	wrapperDone, wrapperErr := registerRunWrapper(runSlot, claim, nil)
	if wrapperErr != nil {
		return Result{Outcome: RunOutcomeError, ExecutionState: RunExecutionQuiescent}, wrapperErr
	}
	if leaseCapturePanic != nil {
		panic(leaseCapturePanic)
	}

	var generationLease RunGenerationLease
	if generationProvider != nil {
		var acquireErr error
		turnCtx, generationLease, acquireErr = generationProvider.AcquireRun(turnCtx)
		if acquireErr != nil {
			return Result{Outcome: RunOutcomeError, ExecutionState: RunExecutionQuiescent}, acquireErr
		}
	}
	defer func() {
		if !ownerStarted {
			returnErr = errors.Join(returnErr, releaseGeneration(generationLease))
		}
	}()
	hookChain = runHookChain(turnCtx, hookChain)

	// Claim before borrowing tracing or invoking any hook/message provider so
	// Assembly.Close cannot observe idle while accepted entrypoint work runs.
	var spanCtx context.Context
	span, spanCtx = tracing.SpanFromContext(turnCtx, "turn.start", tracing.SpanKindInternal)
	span.SetAttributes(tracing.Attribute{Key: "turn_id", Value: id}, tracing.Attribute{Key: "type", Value: submission.Type.String()})
	logger := tracing.NewTraceLogger(span, nil)
	slog.Info("core.turn.runner.start", "id", id, "type", submission.Type)

	var beforeMsgCount int
	var beforeMsgCountCaptured bool
	timeoutClose := func(result Result, err error) {
		if stream != nil {
			if eventStream, ok := stream.(*EventStreamImpl); ok {
				eventStream.SetResult(AgentMessage{Role: "assistant", Content: result.Message}, err)
				if err != nil {
					_ = eventStream.SendTerminal(errEvent(err))
				} else {
					_ = eventStream.SendTerminal(AgentEvent{Kind: "done", Content: result.Message, Timestamp: time.Now()})
				}
			}
			stream.Close()
		}
	}
	owner := newRunOwner(spanCtx, runSlot, claim, timeoutClose)
	owner.setCleanupTimeout(cleanupTimeout)
	if gapSource, ok := stream.(interface{ GapCount() uint64 }); ok {
		owner.SetEventGapSource(gapSource)
	}
	if eventStream, ok := stream.(*EventStreamImpl); ok {
		eventStream.SetRunStatusHandle(owner)
	}
	wrapperErr = setRunWrapperStatus(runSlot, claim, owner)
	if wrapperErr != nil {
		result := Result{Outcome: RunOutcomeError, ExecutionState: RunExecutionQuiescent}
		return result, wrapperErr
	}

	r.mu.Lock()
	r.turns[id] = turn
	r.running[id] = cancel
	r.owners[id] = owner
	r.mu.Unlock()

	owner.generationLease = generationLease
	owner.adoptRunCommitLease(leaseGuard)
	leaseTransferred = true
	ownerStarted = true
	var runErr error
	result, runErr = owner.run(cleanupTimeout, func(runCtx context.Context) (Result, error) {
		if hookChain != nil {
			if herr := hookChain.OnTurnStart(runCtx, id); herr != nil {
				slog.Warn("core.turn.runner.hook.turn_start", "id", id, "err", herr)
			}
		}
		if ctxErr := runCtx.Err(); ctxErr != nil {
			return Result{}, ctxErr
		}
		if ms, ok := agent.(messageSource); ok {
			beforeMsgCount = len(ms.Messages())
			beforeMsgCountCaptured = true
		}
		if ctxErr := runCtx.Err(); ctxErr != nil {
			return Result{}, ctxErr
		}
		if agent != nil {
			if stream != nil {
				return agent.Run(runCtx, submission, stream)
			}
			return agent.Run(runCtx, submission)
		}
		if canonical, ok := r.loop.(CanonicalLoop); ok {
			var canonicalResult CanonicalRunResult
			var err error
			if stream != nil {
				canonicalResult, err = canonical.RunCanonical(runCtx, submission, stream)
			} else {
				canonicalResult, err = canonical.RunCanonical(runCtx, submission)
			}
			return Result{Message: lastMessageEvent(canonicalResult.Events)}, err
		}
		var events []AgentEvent
		var err error
		if stream != nil {
			events, err = r.loop.Run(runCtx, submission, stream)
		} else {
			events, err = r.loop.Run(runCtx, submission)
		}
		if hasToolCallEvent(events) {
			markRunOutcomeUnknown(runCtx)
			if err == nil {
				err = errCanonicalTranscriptUnavailable
			}
		}
		return Result{Message: lastMessageEvent(events)}, err
	})

	r.mu.Lock()
	delete(r.running, id)
	turn.Result = result
	turn.Err = runErr
	turn.EndTime = time.Now()
	turn.RunID = result.RunID
	turn.Outcome = result.Outcome
	turn.ExecutionState = result.ExecutionState
	switch {
	case result.Outcome == RunOutcomeCancelled:
		turn.Status = TurnCanceled
	case runErr != nil || result.Outcome == RunOutcomeError || result.Outcome == RunOutcomeUnknown:
		turn.Status = TurnFailed
	default:
		turn.Status = TurnCompleted
	}
	// Prune completed turns beyond maxTurnsHistory to prevent unbounded
	// memory growth. Only terminal turns are eligible; running turns are
	// always retained.
	r.pruneCompletedTurns()
	turnSnapshot := cloneTurn(*turn)
	r.mu.Unlock()

	finishWrapper := func() {
		defer completeWrapper()
		defer func() {
			if recovered := recover(); recovered != nil {
				logTurnFinalizerPanic("entrypoint")
			}
		}()
		// Callback-local values and the message source are only inspected after
		// owner.done establishes that the callback and its resources have joined.
		if hookChain != nil {
			if runErr != nil {
				callTurnPostHook("on_error", func() error { return hookChain.OnError(spanCtx, id, runErr) })
			}
			if beforeMsgCountCaptured {
				if ms, ok := agent.(messageSource); ok {
					afterMsgCount, snapshotErr := safeMessageCount(ms)
					if snapshotErr != nil {
						safeTurnLog("core.turn.runner.message_snapshot", "id", id, "err", snapshotErr)
					} else if afterMsgCount < beforeMsgCount {
						callTurnPostHook("compaction", func() error {
							return hookChain.OnCompaction(spanCtx, beforeMsgCount, afterMsgCount)
						})
					}
				}
			}
			callTurnPostHook("turn_end", func() error {
				return hookChain.OnTurnEnd(spanCtx, id, turnSnapshot.Result, runErr)
			})
		}

		endSpan, _ := tracing.SpanFromContext(spanCtx, "turn.end", tracing.SpanKindInternal)
		if endSpan != nil {
			defer callTurnWrapperCleanup("end span", endSpan.End)
			endSpan.SetAttributes(tracing.Attribute{Key: "turn_id", Value: id}, tracing.Attribute{Key: "status", Value: turnSnapshot.Status.String()})
		}

		endArgs := []any{
			"id", id,
			"status", turnSnapshot.Status.String(),
			"canceled", turnSnapshot.Canceled,
			"error", runErr != nil,
		}
		if runErr == nil && turnSnapshot.Status == TurnCompleted {
			safeTurnInfo("core.turn.runner.end", endArgs...)
		} else {
			safeTurnLog("core.turn.runner.end", endArgs...)
		}
		logger.Info("turn.end", "id", id, "status", turnSnapshot.Status.String())
	}
	select {
	case <-owner.done:
		finishWrapper()
	default:
		// RunTurn may return its bounded cleanup_pending snapshot while the
		// callback is still active. Transfer the existing wrapper lease, span,
		// and context lifetime to one continuation, which waits for that same
		// owner before touching callback state or running posthooks.
		completionTransferred = true
		contextTransferred = true
		go func() {
			<-owner.done
			finishWrapper()
		}()
	}
	return result, runErr
}

func callTurnWrapperCleanup(stage string, cleanup func()) {
	defer func() {
		if recover() != nil {
			logTurnFinalizerPanic(stage)
		}
	}()
	cleanup()
}

func callTurnPostHook(stage string, callback func() error) {
	defer func() {
		if recover() != nil {
			logTurnFinalizerPanic(stage)
		}
	}()
	if err := callback(); err != nil {
		safeTurnLog("core.turn.runner.hook_failed", "stage", stage, "err", err)
	}
}

func logTurnFinalizerPanic(stage string) {
	defer func() { _ = recover() }()
	slog.Error("core.turn.runner.finalizer_panic", "stage", stage, "err", errTurnFinalizerPanic)
}

func safeTurnLog(message string, args ...any) {
	defer func() { _ = recover() }()
	slog.Warn(message, args...)
}

func safeTurnInfo(message string, args ...any) {
	defer func() { _ = recover() }()
	slog.Info(message, args...)
}

func safeMessageCount(source messageSource) (count int, err error) {
	defer func() {
		if recover() != nil {
			count = 0
			err = errMessageSnapshotPanic
		}
	}()
	return len(source.Messages()), nil
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
	owner := r.owners[id]
	var turnSnapshot Turn
	if ok && running {
		turn.Canceled = true
		turnSnapshot = cloneTurn(*turn)
	}
	r.mu.Unlock()

	if !ok || !running {
		span.SetStatus(tracing.SpanStatusError, errTurnUnknown.Error())
		return errTurnUnknown
	}

	if owner != nil {
		owner.MarkCancelRequested()
	}
	cancel()

	span.SetStatus(tracing.SpanStatusOK, "")
	slog.Info("core.turn.runner.cancel", "id", turnSnapshot.ID)
	return nil
}

// Steer injects a steering submission into a running turn. The steering is
// recorded on the turn and surfaced via Get so the runtime can observe it.
// When a steering channel is set, the instruction is also sent to that
// channel so the running loop picks it up between LLM iterations. It returns
// an error if the turn is unknown or not running.
func (r *EinoTurnRunner) Steer(ctx context.Context, id, instruction string) error {
	r.mu.Lock()
	turn, ok := r.turns[id]
	if !ok || turn.Status != TurnRunning {
		r.mu.Unlock()
		return errTurnUnknown
	}
	steerCh := r.steerCh
	if steerCh == nil {
		r.mu.Unlock()
		return fmt.Errorf("core: steering delivery channel is not configured")
	}
	sent, sendErr := tryQueueSend(steerCh, instruction)
	if sendErr != nil {
		r.mu.Unlock()
		return fmt.Errorf("core: steering delivery failed: %w", sendErr)
	}
	if !sent {
		r.mu.Unlock()
		return fmt.Errorf("core: steering channel full")
	}
	turn.Steerings = append(turn.Steerings, Submission{Type: SubmissionSteering, Content: instruction})
	r.mu.Unlock()
	slog.Info("core.turn.runner.steer.sent", "id", id, "instruction", instruction)
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

// SetRunLifetimeContext links future turns to an Assembly-owned lifetime.
// Canceling the lifetime stops run input; each run owner continues supervising
// resources until they are actually quiescent.
func (r *EinoTurnRunner) SetRunLifetimeContext(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lifetimeCtx = ctx
}

// SetRunCleanupTimeout sets the bounded time RunTurn waits for owned workers
// after its top-level callback returns or cancellation is requested.
func (r *EinoTurnRunner) SetRunCleanupTimeout(timeout time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if timeout > 0 {
		r.cleanupTimeout = timeout
	}
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
	for id := range r.running {
		return id
	}
	return ""
}

// FollowUp appends a follow-up user message to a running turn. It is recorded
// on the turn and surfaced via Get. When a follow-up channel is set, the
// instruction is also sent to that channel so the running loop picks it up
// between LLM iterations. It returns an error if the turn is unknown or not
// running.
func (r *EinoTurnRunner) FollowUp(_ context.Context, id, content string) error {
	r.mu.Lock()
	ch := r.followUpCh
	turn, ok := r.turns[id]
	if !ok || turn.Status != TurnRunning {
		r.mu.Unlock()
		return errTurnUnknown
	}
	if ch == nil {
		r.mu.Unlock()
		return fmt.Errorf("core: follow-up delivery channel is not configured")
	}
	sent, sendErr := tryQueueSend(ch, content)
	if sendErr != nil {
		r.mu.Unlock()
		return fmt.Errorf("core: follow-up delivery failed: %w", sendErr)
	}
	if !sent {
		r.mu.Unlock()
		return fmt.Errorf("core: followup channel full")
	}
	turn.FollowUps = append(turn.FollowUps, Submission{Type: SubmissionFollowUp, Content: content})
	r.mu.Unlock()
	slog.Info("core.turn.runner.followup.sent", "id", id, "content_len", len(content))
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
	turn := cloneTurn(*t)
	if owner := r.owners[id]; owner != nil {
		status := owner.Snapshot()
		turn.RunID = status.RunID
		turn.Outcome = status.Outcome
		turn.ExecutionState = status.ExecutionState
		turn.Result.RunID = status.RunID
		turn.Result.Outcome = status.Outcome
		turn.Result.ExecutionState = status.ExecutionState
		turn.Result.Success = status.Outcome == RunOutcomeSuccess
		turn.Result.StatusHandle = owner
	}
	return turn, nil
}

func tryQueueSend(ch chan string, content string) (sent bool, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("channel send panicked: %v", recovered)
		}
	}()
	select {
	case ch <- content:
		return true, nil
	default:
		return false, nil
	}
}

// pruneCompletedTurns removes the oldest completed turns when the turns map
// exceeds maxTurnsHistory. It must be called with r.mu held. Only turns in a
// terminal state (completed, canceled, failed) are eligible for removal;
// running turns are always retained. The most recent maxTurnsHistory
// completed turns are kept.
func (r *EinoTurnRunner) pruneCompletedTurns() {
	if len(r.turns) <= maxTurnsHistory {
		return
	}

	// Collect completed turns as pruning candidates.
	type turnEntry struct {
		id        string
		startTime time.Time
	}
	var completed []turnEntry
	for id, t := range r.turns {
		if owner := r.owners[id]; owner != nil {
			status := owner.Snapshot()
			t.RunID = status.RunID
			t.Outcome = status.Outcome
			t.ExecutionState = status.ExecutionState
			t.Result.RunID = status.RunID
			t.Result.Outcome = status.Outcome
			t.Result.ExecutionState = status.ExecutionState
			t.Result.Success = status.Outcome == RunOutcomeSuccess
		}
		if t.Done() && t.ExecutionState == RunExecutionQuiescent {
			completed = append(completed, turnEntry{id: id, startTime: t.StartTime})
		}
	}
	if len(completed) <= maxTurnsHistory {
		return
	}

	// Sort by StartTime ascending (oldest first) and remove the excess.
	sort.Slice(completed, func(i, j int) bool {
		return completed[i].startTime.Before(completed[j].startTime)
	})
	excess := len(completed) - maxTurnsHistory
	for i := 0; i < excess; i++ {
		delete(r.turns, completed[i].id)
		delete(r.owners, completed[i].id)
	}
}

func hasToolCallEvent(events []AgentEvent) bool {
	for _, event := range events {
		if event.Kind == "tool_call" {
			return true
		}
	}
	return false
}
