package core

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/pengjunchen/go-cli/internal/tools"
)

const defaultRunCleanupTimeout = 250 * time.Millisecond

var errRunCleanupPending = errors.New("core: run cleanup continues after caller deadline")

type runCallbackPanicError struct{}

func (runCallbackPanicError) Error() string        { return "core: run callback panicked" }
func (runCallbackPanicError) OutcomeUnknown() bool { return true }

type runWorkerTracker interface {
	RegisterWorker(toolCallID, toolName string) (done func())
	RegisterPendingResource(toolCallID, toolName string, executionDone <-chan struct{})
}

type runCompletionTaskTracker interface {
	RegisterCompletionTask(ActionIntent, tools.PreparedCompletionSignal) uint64
	HandoffCompletionTask(uint64)
}

type runCompletionTaskState string

const (
	runCompletionTaskHandoffPending runCompletionTaskState = "handoff_pending"
	runCompletionTaskNoProof        runCompletionTaskState = "no_proof"
	runCompletionTaskResourceOpen   runCompletionTaskState = "pending"
	runCompletionTaskReady          runCompletionTaskState = "ready"
)

type runCompletionTask struct {
	intent ActionIntent
	signal tools.PreparedCompletionSignal
	state  runCompletionTaskState
	done   <-chan struct{}
	proof  tools.PreparedCompletionProof
	read   func() tools.PreparedCompletionProof
}

type runCompletionTaskHandoff struct {
	id    uint64
	done  <-chan struct{}
	proof func() tools.PreparedCompletionProof
}

type runOutcomeUnknown interface {
	OutcomeUnknown() bool
}

type runOwnerContextKey struct{}

type runOwner struct {
	mu                 sync.Mutex
	guard              RunSlotGuard
	claim              RunClaim
	ownerGeneration    string
	ctx                context.Context
	callbackCtx        context.Context
	cleanupBaseCtx     context.Context
	cleanupCommitCtx   context.Context
	cleanupCommitStop  context.CancelFunc
	cancel             context.CancelFunc
	done               chan struct{}
	changed            chan struct{}
	completionChanged  chan struct{}
	cleanup            chan struct{}
	backpressureSignal chan struct{}
	cleanupOnce        sync.Once

	status               RunStatus
	result               Result
	runErr               error
	callbackFinished     bool
	transcriptCommitted  bool
	runCommitBound       bool
	runCommitBinding     RunCommitBinding
	terminalSyncRequired bool
	terminalSyncSuccess  bool
	runCommitLease       RunCommitRunLease
	leaseCloseAttempted  bool
	workers              int
	resources            int
	completionTasks      map[uint64]*runCompletionTask
	nextCompletionTask   uint64
	expiredBackpressure  int
	cleanupTimeout       time.Duration
	unknown              bool
	cancelRequested      bool
	timedOut             bool
	released             bool
	onTimeout            func(Result, error)
	eventGapSource       interface{ GapCount() uint64 }
}

func newRunOwner(ctx context.Context, guard RunSlotGuard, claim RunClaim, timeout func(Result, error)) *runOwner {
	if guard == nil {
		guard = defaultRunSlotGuard
	}
	ownerCtx, cancel := context.WithCancel(ctx)
	return &runOwner{
		guard:              guard,
		claim:              claim,
		ownerGeneration:    "owner-" + newClaimID(),
		ctx:                ownerCtx,
		cancel:             cancel,
		done:               make(chan struct{}),
		changed:            make(chan struct{}, 1),
		completionChanged:  make(chan struct{}, 1),
		cleanup:            make(chan struct{}),
		backpressureSignal: make(chan struct{}, 1),
		status: RunStatus{
			RunID:          claim.ID,
			ExecutionState: RunExecutionRunning,
		},
		onTimeout:           timeout,
		terminalSyncSuccess: true,
		completionTasks:     make(map[uint64]*runCompletionTask),
	}
}

func withRunWorkerTracker(ctx context.Context, tracker runWorkerTracker) context.Context {
	if tracker == nil {
		return ctx
	}
	return context.WithValue(ctx, runOwnerContextKey{}, tracker)
}

func registerRunWorker(ctx context.Context, toolCallID, toolName string) func() {
	if ctx == nil {
		return func() {}
	}
	tracker, ok := ctx.Value(runOwnerContextKey{}).(runWorkerTracker)
	if !ok || tracker == nil {
		return func() {}
	}
	return tracker.RegisterWorker(toolCallID, toolName)
}

func registerRunPendingResource(ctx context.Context, toolCallID, toolName string, done <-chan struct{}) {
	if ctx == nil {
		return
	}
	tracker, ok := ctx.Value(runOwnerContextKey{}).(runWorkerTracker)
	if ok && tracker != nil {
		tracker.RegisterPendingResource(toolCallID, toolName, done)
	}
}

func registerRunCompletionTask(ctx context.Context, intent ActionIntent, signal tools.PreparedCompletionSignal) uint64 {
	if ctx == nil {
		return 0
	}
	tracker, ok := ctx.Value(runOwnerContextKey{}).(runCompletionTaskTracker)
	if !ok || tracker == nil {
		return 0
	}
	return tracker.RegisterCompletionTask(intent, signal)
}

func handoffRunCompletionTask(ctx context.Context, taskID uint64) {
	if ctx == nil || taskID == 0 {
		return
	}
	tracker, ok := ctx.Value(runOwnerContextKey{}).(runCompletionTaskTracker)
	if ok && tracker != nil {
		tracker.HandoffCompletionTask(taskID)
	}
}

func markRunOutcomeUnknown(ctx context.Context) {
	if ctx == nil {
		return
	}
	tracker, ok := ctx.Value(runOwnerContextKey{}).(interface{ MarkOutcomeUnknown() })
	if ok && tracker != nil {
		tracker.MarkOutcomeUnknown()
	}
}

func markRunCommitTerminalSynced(ctx context.Context) {
	if ctx == nil {
		return
	}
	if owner, ok := ctx.Value(runOwnerContextKey{}).(*runOwner); ok && owner != nil {
		owner.markRunCommitTerminalSynced()
	}
}

func (o *runOwner) RegisterWorker(_, _ string) func() {
	o.mu.Lock()
	if o.released {
		o.mu.Unlock()
		return func() {}
	}
	o.workers++
	o.refreshPendingLocked()
	o.signalLocked()
	o.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			o.mu.Lock()
			if o.workers > 0 {
				o.workers--
			}
			o.refreshPendingLocked()
			o.signalLocked()
			o.mu.Unlock()
		})
	}
}

func (o *runOwner) RegisterPendingResource(_, _ string, executionDone <-chan struct{}) {
	o.mu.Lock()
	if o.released {
		o.mu.Unlock()
		return
	}
	o.resources++
	o.refreshPendingLocked()
	o.signalLocked()
	o.mu.Unlock()
	if executionDone == nil {
		return
	}
	go func() {
		<-executionDone
		o.mu.Lock()
		if o.resources > 0 {
			o.resources--
		}
		o.refreshPendingLocked()
		o.signalLocked()
		o.mu.Unlock()
	}()
}

// RegisterCompletionTask reserves run-owner ownership before Dispatch can
// attach Done to the prepared signal. The placeholder is counted as a worker
// until HandoffCompletionTask atomically transfers it to the final channel.
func (o *runOwner) RegisterCompletionTask(intent ActionIntent, signal tools.PreparedCompletionSignal) uint64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.released || o.callbackFinished || intent.RunID != o.claim.ID || intent.OwnerGeneration != o.ownerGeneration {
		return 0
	}
	if o.runCommitBound && (intent.SessionID != o.runCommitBinding.SessionID || intent.WorkspaceID != o.runCommitBinding.WorkspaceID) {
		return 0
	}
	o.nextCompletionTask++
	id := o.nextCompletionTask
	o.completionTasks[id] = &runCompletionTask{
		intent: intent,
		signal: signal,
		state:  runCompletionTaskHandoffPending,
		read:   signal.Proof,
	}
	o.workers++
	o.refreshPendingLocked()
	o.signalLocked()
	o.signalCompletionLocked()
	return id
}

// HandoffCompletionTask observes the final Done attached to the same signal
// after Dispatch returns. The worker remains counted until either the final
// channel is adopted as a resource or the task is classified as proofless.
func (o *runOwner) HandoffCompletionTask(id uint64) {
	o.mu.Lock()
	task := o.completionTasks[id]
	if task == nil || task.state != runCompletionTaskHandoffPending {
		o.mu.Unlock()
		return
	}
	signal := task.signal
	readProof := task.read
	o.mu.Unlock()
	o.handoffCompletionTask(id, signal.Done(), readProof)
}

// handoffCompletionTask is separated from the public handoff so race tests can
// drive a controlled final channel while preserving the production state
// transition. A nil channel means this invocation has no proof; a closed
// non-nil channel means the resource joined even if its proof value is empty.
func (o *runOwner) finishCompletionTaskHandoff(id uint64, done <-chan struct{}, proofReady bool, proof tools.PreparedCompletionProof) {
	readProof := func() tools.PreparedCompletionProof {
		if proofReady {
			return proof
		}
		return ""
	}
	o.handoffCompletionTask(id, done, readProof)
}

func (o *runOwner) handoffCompletionTask(id uint64, done <-chan struct{}, readProof func() tools.PreparedCompletionProof) {
	if done == nil {
		o.mu.Lock()
		if task := o.completionTasks[id]; task != nil && task.state == runCompletionTaskHandoffPending {
			task.state = runCompletionTaskNoProof
			task.read = nil
			o.workers--
			o.refreshPendingLocked()
			o.signalLocked()
			o.signalCompletionLocked()
		}
		o.mu.Unlock()
		return
	}

	select {
	case <-done:
		proof := tools.PreparedCompletionProof("")
		if readProof != nil {
			proof = readProof()
		}
		o.mu.Lock()
		if task := o.completionTasks[id]; task != nil && task.state == runCompletionTaskHandoffPending {
			task.state = runCompletionTaskReady
			task.done = done
			task.proof = proof
			task.read = nil
			o.workers--
			o.refreshPendingLocked()
			o.signalLocked()
			o.signalCompletionLocked()
		}
		o.mu.Unlock()
		return
	default:
	}

	o.mu.Lock()
	task := o.completionTasks[id]
	if task == nil || task.state != runCompletionTaskHandoffPending {
		o.mu.Unlock()
		return
	}
	task.state = runCompletionTaskResourceOpen
	task.done = done
	task.read = readProof
	// Increment the resource before releasing placeholder worker ownership.
	o.resources++
	o.workers--
	o.refreshPendingLocked()
	o.signalLocked()
	o.signalCompletionLocked()
	o.mu.Unlock()

	go func() {
		<-done
		proof := tools.PreparedCompletionProof("")
		if readProof != nil {
			proof = readProof()
		}
		o.finishCompletionTaskResource(id, done, proof)
	}()
}

func (o *runOwner) finishCompletionTaskResource(id uint64, done <-chan struct{}, proof tools.PreparedCompletionProof) {
	o.mu.Lock()
	defer o.mu.Unlock()
	task := o.completionTasks[id]
	if task == nil || task.state != runCompletionTaskResourceOpen || task.done != done {
		return
	}
	task.state = runCompletionTaskReady
	task.proof = proof
	task.read = nil
	if o.resources > 0 {
		o.resources--
	}
	o.refreshPendingLocked()
	o.signalLocked()
	o.signalCompletionLocked()
}

func (o *runOwner) completionTaskSnapshot(id uint64) (runCompletionTaskState, tools.PreparedCompletionProof, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	task := o.completionTasks[id]
	if task == nil {
		return "", "", false
	}
	return task.state, task.proof, true
}

// waitForCompletionTasks is a bounded, non-worker-path wait on the exact
// channels handed to this run owner. On cancellation/timeout it rechecks each
// same channel before returning so an already-closed Done wins the select race.
func (o *runOwner) waitForCompletionTasks(ctx context.Context, ids []uint64) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	timeout := o.cleanupTimeout
	if timeout <= 0 {
		timeout = defaultRunCleanupTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		ready, terminal, err := o.completionTasksReady(ids)
		if err != nil || terminal {
			return ready, err
		}
		if ctx.Err() != nil {
			o.refreshClosedCompletionTasks(ids)
			ready, terminal, err = o.completionTasksReady(ids)
			if err != nil || terminal {
				return ready, err
			}
			return false, ctx.Err()
		}
		select {
		case <-o.completionChanged:
		case <-ctx.Done():
			o.refreshClosedCompletionTasks(ids)
			ready, terminal, err = o.completionTasksReady(ids)
			if err != nil || terminal {
				return ready, err
			}
			return false, ctx.Err()
		case <-timer.C:
			o.refreshClosedCompletionTasks(ids)
			ready, terminal, err = o.completionTasksReady(ids)
			if err != nil || terminal {
				return ready, err
			}
			return false, context.DeadlineExceeded
		}
	}
}

func (o *runOwner) completionTasksReady(ids []uint64) (ready, terminal bool, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	ready = true
	for _, id := range ids {
		task := o.completionTasks[id]
		if task == nil {
			return false, true, errors.New("core: unknown run completion task")
		}
		switch task.state {
		case runCompletionTaskNoProof:
			return false, true, nil
		case runCompletionTaskReady:
			if task.proof == "" {
				return false, true, nil
			}
		default:
			ready = false
		}
	}
	return ready, ready, nil
}

func (o *runOwner) refreshClosedCompletionTasks(ids []uint64) {
	o.mu.Lock()
	handoffs := make([]runCompletionTaskHandoff, 0, len(ids))
	for _, id := range ids {
		if task := o.completionTasks[id]; task != nil && task.state == runCompletionTaskResourceOpen {
			handoffs = append(handoffs, runCompletionTaskHandoff{id: id, done: task.done, proof: task.read})
		}
	}
	o.mu.Unlock()
	for _, handoff := range handoffs {
		if handoff.done == nil {
			continue
		}
		select {
		case <-handoff.done:
			proof := tools.PreparedCompletionProof("")
			if handoff.proof != nil {
				proof = handoff.proof()
			}
			o.finishCompletionTaskResource(handoff.id, handoff.done, proof)
		default:
		}
	}
}

func (o *runOwner) signalCompletionLocked() {
	select {
	case o.completionChanged <- struct{}{}:
	default:
	}
}

func (o *runOwner) MarkOutcomeUnknown() {
	o.mu.Lock()
	o.unknown = true
	o.status.Outcome = RunOutcomeUnknown
	o.signalLocked()
	o.mu.Unlock()
}

// CleanupCommitContext returns the callback context during normal execution.
// After cancellation or cleanup starts, it returns one cached cancellation-free
// context with the same run and sink binding values and one shared deadline.
// Callers must use it only for already-known durable bookkeeping, never for
// provider calls or tool dispatch.
func (o *runOwner) CleanupCommitContext(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = o.callbackCtx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	o.mu.Lock()
	cleanupNeeded := o.cancelRequested || o.timedOut || ctx.Err() != nil
	if !cleanupNeeded {
		o.mu.Unlock()
		return ctx
	}
	cleanupCtx := o.startCleanupCommitContextLocked(ctx)
	o.mu.Unlock()
	return cleanupCtx
}

func (o *runOwner) startCleanupCommitContextLocked(ctx context.Context) context.Context {
	if o.cleanupCommitCtx != nil {
		return o.cleanupCommitCtx
	}
	base := o.cleanupBaseCtx
	if base == nil {
		if ctx == nil {
			ctx = context.Background()
		}
		base = context.WithoutCancel(ctx)
		o.cleanupBaseCtx = base
	}
	timeout := o.cleanupTimeout
	if timeout <= 0 {
		timeout = defaultRunCleanupTimeout
	}
	o.cleanupCommitCtx, o.cleanupCommitStop = context.WithTimeout(base, timeout)
	return o.cleanupCommitCtx
}

func (o *runOwner) markRunCommitTerminalSynced() {
	o.mu.Lock()
	o.terminalSyncSuccess = true
	o.signalLocked()
	o.mu.Unlock()
}

func (o *runOwner) runCommitTerminalSynced() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return !o.terminalSyncRequired || o.terminalSyncSuccess
}

func (o *runOwner) MarkCancelRequested() {
	o.mu.Lock()
	if !o.released && !o.timedOut && o.status.ExecutionState != RunExecutionQuiescent && o.status.ExecutionState != RunExecutionQuarantined {
		o.cancelRequested = true
		o.status.ExecutionState = RunExecutionCancelRequested
		o.startCleanupCommitContextLocked(o.callbackCtx)
		o.startCleanupLocked()
		o.signalLocked()
	}
	o.mu.Unlock()
}

func (o *runOwner) Snapshot() RunStatus {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.refreshPendingLocked()
	return o.status
}

func (o *runOwner) SetEventGapSource(source interface{ GapCount() uint64 }) {
	o.mu.Lock()
	o.eventGapSource = source
	o.refreshPendingLocked()
	o.mu.Unlock()
}

func (o *runOwner) BeginBackpressure() func() {
	o.mu.Lock()
	if o.released || o.timedOut || o.callbackFinished {
		o.mu.Unlock()
		return func() {}
	}
	timeout := o.cleanupTimeout
	if timeout <= 0 {
		timeout = defaultRunCleanupTimeout
	}
	lease := &runBackpressureLease{active: true}
	lease.timer = time.AfterFunc(timeout, func() {
		o.mu.Lock()
		if lease.active && !o.released && !o.timedOut && !o.callbackFinished {
			lease.expired = true
			o.expiredBackpressure++
			select {
			case o.backpressureSignal <- struct{}{}:
			default:
			}
		}
		o.mu.Unlock()
	})
	o.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			o.mu.Lock()
			if lease.active {
				lease.active = false
				if lease.expired && o.expiredBackpressure > 0 {
					o.expiredBackpressure--
				}
				lease.timer.Stop()
			}
			o.mu.Unlock()
		})
	}
}

type runBackpressureLease struct {
	timer   *time.Timer
	active  bool
	expired bool
}

func (o *runOwner) run(timeout time.Duration, fn func(context.Context) (Result, error)) (Result, error) {
	if timeout <= 0 {
		timeout = defaultRunCleanupTimeout
	}
	o.mu.Lock()
	o.cleanupTimeout = timeout
	o.mu.Unlock()
	callbackCtx := withRunID(withRunWorkerTracker(o.ctx, o), o.claim.ID)
	var bindingErr error
	if sink, binding, ok := RunCommitFromContext(callbackCtx); ok {
		o.mu.Lock()
		o.runCommitBound = true
		o.terminalSyncSuccess = false
		if lease, ok := sink.(RunCommitRunLease); ok {
			o.runCommitLease = lease
		}
		o.mu.Unlock()
		binding.OwnerGeneration = o.ownerGeneration
		bindingErr = binding.Validate()
		if bindingErr == nil {
			callbackCtx = WithRunCommit(callbackCtx, sink, binding)
			o.mu.Lock()
			o.runCommitBinding = binding
			o.mu.Unlock()
		}
	}
	o.mu.Lock()
	o.callbackCtx = callbackCtx
	o.cleanupBaseCtx = context.WithoutCancel(callbackCtx)
	if o.cancelRequested || o.timedOut {
		o.startCleanupCommitContextLocked(callbackCtx)
	}
	o.mu.Unlock()
	go o.supervise()
	if bindingErr != nil {
		o.finishUndispatched(bindingErr)
	} else if ctxErr := callbackCtx.Err(); ctxErr != nil {
		o.finishUndispatched(ctxErr)
	} else {
		o.mu.Lock()
		o.terminalSyncRequired = o.runCommitBound
		o.mu.Unlock()
		go func() {
			result, panicked, err := invokeRunCallback(callbackCtx, fn)
			o.mu.Lock()
			o.callbackFinished = true
			commitFailure := errors.Is(err, ErrCanonicalTranscriptInvalid) || errors.Is(err, ErrCanonicalTranscriptRequired)
			o.transcriptCommitted = !panicked && !commitFailure
			o.result = result
			o.runErr = err
			durableCommitFailure := o.terminalSyncRequired && !o.terminalSyncSuccess
			if durableCommitFailure && o.runErr == nil {
				o.runErr = ErrRunCommitTerminalNotSynced
			}
			if panicked || commitFailure || durableCommitFailure {
				o.unknown = true
			}
			var marker runOutcomeUnknown
			if errors.As(o.runErr, &marker) && marker.OutcomeUnknown() {
				o.unknown = true
			}
			switch {
			case o.unknown || result.Outcome == RunOutcomeUnknown:
				o.unknown = true
				o.status.Outcome = RunOutcomeUnknown
			case result.Outcome != "":
				o.status.Outcome = result.Outcome
			case o.runErr != nil && o.ctx.Err() != nil && errors.Is(o.runErr, o.ctx.Err()):
				o.status.Outcome = RunOutcomeCancelled
			case o.runErr != nil:
				o.status.Outcome = RunOutcomeError
			default:
				o.status.Outcome = RunOutcomeSuccess
			}
			if panicked || commitFailure || durableCommitFailure {
				o.status.ExecutionState = RunExecutionQuarantined
				o.startCleanupLocked()
			} else if o.status.ExecutionState != RunExecutionCleanupPending && o.status.ExecutionState != RunExecutionQuarantined {
				o.status.ExecutionState = RunExecutionDraining
			}
			o.refreshPendingLocked()
			if o.workers > 0 || o.resources > 0 {
				o.startCleanupLocked()
			}
			o.signalLocked()
			o.mu.Unlock()
		}()
		go func() {
			select {
			case <-o.ctx.Done():
				o.MarkCancelRequested()
			case <-o.done:
			}
		}()
	}

	for {
		select {
		case <-o.done:
			return o.returnSnapshot()
		case <-o.cleanup:
			timer := time.NewTimer(timeout)
			defer timer.Stop()
			select {
			case <-o.done:
				return o.returnSnapshot()
			case <-timer.C:
				select {
				case <-o.done:
					return o.returnSnapshot()
				default:
				}
				return o.cleanupPending()
			}
		case <-o.backpressureSignal:
			if o.backpressureIsStillExpired() {
				return o.cleanupPending()
			}
		}
	}
}

func (o *runOwner) finishUndispatched(err error) {
	o.mu.Lock()
	o.callbackFinished = true
	o.transcriptCommitted = true
	o.runErr = err
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		o.cancelRequested = true
		o.status.Outcome = RunOutcomeCancelled
	} else {
		o.status.Outcome = RunOutcomeError
	}
	if o.terminalSyncRequired && !o.terminalSyncSuccess {
		o.unknown = true
		o.status.Outcome = RunOutcomeUnknown
		o.status.ExecutionState = RunExecutionQuarantined
	} else {
		o.status.ExecutionState = RunExecutionDraining
	}
	o.signalLocked()
	o.mu.Unlock()
}

func (o *runOwner) backpressureIsStillExpired() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.expiredBackpressure > 0 && !o.released && !o.timedOut
}

func invokeRunCallback(ctx context.Context, fn func(context.Context) (Result, error)) (result Result, panicked bool, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			_ = recovered
			err = runCallbackPanicError{}
			panicked = true
		}
	}()
	result, err = fn(ctx)
	return result, false, err
}

func (o *runOwner) supervise() {
	for {
		<-o.changed
		o.mu.Lock()
		if o.timedOut && !o.localResourcesJoinedLocked() {
			o.status.ExecutionState = RunExecutionQuarantined
		}
		if !o.localResourcesJoinedLocked() {
			o.mu.Unlock()
			continue
		}
		if o.released {
			o.mu.Unlock()
			return
		}
		o.released = true
		lease := o.runCommitLease
		closeLease := lease != nil && !o.leaseCloseAttempted
		if closeLease {
			o.leaseCloseAttempted = true
		}
		claim := o.claim
		guard := o.guard
		o.mu.Unlock()

		leasePanicked := false
		if closeLease {
			leasePanicked = closeRunCommitLease(lease)
		}
		guard.Release(claim)

		o.mu.Lock()
		if leasePanicked {
			o.runErr = errors.Join(o.runErr, ErrRunCommitLeaseClosePanicked)
			o.unknown = true
			o.status.Outcome = RunOutcomeUnknown
		}
		if leasePanicked || !o.durableCommitSucceededLocked() {
			o.status.ExecutionState = RunExecutionQuarantined
		} else {
			o.status.ExecutionState = RunExecutionQuiescent
		}
		o.status.PendingWorkers = 0
		cleanupCommitStop := o.cleanupCommitStop
		o.cleanupCommitStop = nil
		o.cancel()
		close(o.done)
		o.mu.Unlock()
		if cleanupCommitStop != nil {
			cleanupCommitStop()
		}
		return
	}
}

func closeRunCommitLease(lease RunCommitRunLease) (panicked bool) {
	defer func() {
		if recover() != nil {
			panicked = true
		}
	}()
	lease.CloseRunCommit()
	return false
}

func (o *runOwner) localResourcesJoinedLocked() bool {
	return o.callbackFinished && o.workers == 0 && o.resources == 0
}

func (o *runOwner) durableCommitSucceededLocked() bool {
	return o.transcriptCommitted && (!o.terminalSyncRequired || o.terminalSyncSuccess)
}

func (o *runOwner) cleanupPending() (Result, error) {
	o.mu.Lock()
	if o.released || o.localResourcesJoinedLocked() {
		result := o.resultSnapshotLocked()
		runErr := o.runErr
		o.mu.Unlock()
		return result, runErr
	}
	o.timedOut = true
	o.startCleanupCommitContextLocked(o.callbackCtx)
	// Return the caller's bounded-wait observation consistently. The supervisor
	// may move the live status to Quarantined when the callback has not yet
	// committed a transcript, but this immutable return snapshot records that
	// cleanup remains pending at the point the caller stopped waiting.
	o.status.ExecutionState = RunExecutionCleanupPending
	if o.workers > 0 || !o.callbackFinished {
		o.unknown = true
		o.status.Outcome = RunOutcomeUnknown
	} else if o.status.Outcome == "" && o.cancelRequested {
		o.status.Outcome = RunOutcomeCancelled
	} else if o.status.Outcome == "" {
		o.status.Outcome = RunOutcomeError
	}
	o.refreshPendingLocked()
	o.signalLocked()
	result := o.resultSnapshotLocked()
	runErr := o.runErr
	o.cancel()
	o.mu.Unlock()
	if runErr == nil {
		runErr = errRunCleanupPending
	}
	if o.onTimeout != nil {
		o.onTimeout(result, runErr)
	}
	return result, runErr
}

func (o *runOwner) returnSnapshot() (Result, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.resultSnapshotLocked(), o.runErr
}

func (o *runOwner) resultSnapshotLocked() Result {
	result := o.result
	result.RunID = o.status.RunID
	result.Outcome = o.status.Outcome
	result.ExecutionState = o.status.ExecutionState
	result.StatusHandle = o
	result.Success = result.Outcome == RunOutcomeSuccess
	return result
}

func (o *runOwner) refreshPendingLocked() {
	o.status.PendingWorkers = o.workers + o.resources
	if o.eventGapSource != nil {
		o.status.EventGapCount = o.eventGapSource.GapCount()
	}
}

func (o *runOwner) startCleanupLocked() {
	o.cleanupOnce.Do(func() { close(o.cleanup) })
}

func (o *runOwner) signalLocked() {
	select {
	case o.changed <- struct{}{}:
	default:
	}
}
