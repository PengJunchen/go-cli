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
	AttachPendingExecution(uint64, <-chan struct{}) bool
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
	intent                  ActionIntent
	signal                  tools.PreparedCompletionSignal
	state                   runCompletionTaskState
	resourceChannels        []<-chan struct{}
	proof                   tools.PreparedCompletionProof
	read                    func() tools.PreparedCompletionProof
	pendingExecutionDone    <-chan struct{}
	pendingExecutionSeen    bool
	pendingResourceUnproven bool
	receiptDurable          bool
	proofExecutionDurable   bool
	resolutionDurable       bool
}

type runCompletionTaskDetails struct {
	id                      uint64
	intent                  ActionIntent
	state                   runCompletionTaskState
	proof                   tools.PreparedCompletionProof
	pendingResourceUnproven bool
	receiptDurable          bool
	proofExecutionDurable   bool
	resolutionDurable       bool
}

type runCompletionTaskHandoff struct {
	id       uint64
	channels []<-chan struct{}
	proof    func() tools.PreparedCompletionProof
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
	cleanupDeadline    time.Time
	cleanupDeadlineSet bool
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
	releaseStarted       bool
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
		cleanupTimeout:      defaultRunCleanupTimeout,
		terminalSyncSuccess: true,
		completionTasks:     make(map[uint64]*runCompletionTask),
	}
}

func (o *runOwner) setCleanupTimeout(timeout time.Duration) {
	if timeout <= 0 {
		timeout = defaultRunCleanupTimeout
	}
	o.mu.Lock()
	if !o.cleanupDeadlineSet {
		o.cleanupTimeout = timeout
	}
	o.mu.Unlock()
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

func attachRunCompletionPendingExecution(ctx context.Context, taskID uint64, done <-chan struct{}) bool {
	if ctx == nil || taskID == 0 {
		return false
	}
	tracker, ok := ctx.Value(runOwnerContextKey{}).(runCompletionTaskTracker)
	return ok && tracker != nil && tracker.AttachPendingExecution(taskID, done)
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

func markRunCompletionReceiptDurable(ctx context.Context, taskID uint64) {
	if owner := runOwnerFromContext(ctx); owner != nil {
		owner.markCompletionReceiptDurable(taskID)
	}
}

func markRunCompletionProofExecutionDurable(ctx context.Context, taskID uint64) {
	if owner := runOwnerFromContext(ctx); owner != nil {
		owner.markCompletionProofExecutionDurable(taskID)
	}
}

func markRunCompletionResolved(ctx context.Context, taskID uint64) {
	if owner := runOwnerFromContext(ctx); owner != nil {
		owner.markCompletionResolved(taskID)
	}
}

func runCompletionDetails(ctx context.Context, taskID uint64) (runCompletionTaskDetails, bool) {
	if owner := runOwnerFromContext(ctx); owner != nil {
		return owner.completionDetails(taskID)
	}
	return runCompletionTaskDetails{}, false
}

func runOwnerFromContext(ctx context.Context) *runOwner {
	if ctx == nil {
		return nil
	}
	owner, _ := ctx.Value(runOwnerContextKey{}).(*runOwner)
	return owner
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
	if o.releaseStarted || o.released {
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
	if o.releaseStarted || o.released {
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
	if o.releaseStarted || o.released || o.callbackFinished || intent.RunID != o.claim.ID || intent.OwnerGeneration != o.ownerGeneration {
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

// AttachPendingExecution attaches the underlying resource channel reported by
// a PendingExecutionError while the prepared placeholder still owns the worker.
// Handoff folds this channel into the same action resource group as signal.Done.
func (o *runOwner) AttachPendingExecution(id uint64, done <-chan struct{}) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	task := o.completionTasks[id]
	if task == nil || task.state != runCompletionTaskHandoffPending || task.pendingExecutionSeen {
		return false
	}
	task.pendingExecutionSeen = true
	task.pendingExecutionDone = done
	task.pendingResourceUnproven = done == nil
	return true
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
	channels := make([]<-chan struct{}, 0, 2)
	if done != nil {
		channels = append(channels, done)
	}
	o.mu.Lock()
	task := o.completionTasks[id]
	if task == nil || task.state != runCompletionTaskHandoffPending {
		o.mu.Unlock()
		return
	}
	pendingDone := task.pendingExecutionDone
	if pendingDone != nil && pendingDone != done {
		channels = append(channels, pendingDone)
	}
	if task.pendingExecutionSeen && pendingDone == nil {
		task.pendingResourceUnproven = true
	}
	if len(channels) == 0 {
		task.state = runCompletionTaskNoProof
		task.read = nil
		o.workers--
		o.refreshPendingLocked()
		o.signalLocked()
		o.signalCompletionLocked()
		o.mu.Unlock()
		return
	}
	if completionChannelsClosed(channels) {
		proof := tools.PreparedCompletionProof("")
		if done != nil && readProof != nil {
			proof = readProof()
		}
		task.state = runCompletionTaskReady
		task.resourceChannels = channels
		task.proof = proof
		task.read = nil
		o.workers--
		o.refreshPendingLocked()
		o.signalLocked()
		o.signalCompletionLocked()
		o.mu.Unlock()
		return
	}
	task.state = runCompletionTaskResourceOpen
	task.resourceChannels = channels
	task.read = readProof
	// One action owns one resource group even when the prepared signal wraps a
	// distinct underlying PendingExecutionError channel.
	o.resources++
	o.workers--
	o.refreshPendingLocked()
	o.signalLocked()
	o.signalCompletionLocked()
	o.mu.Unlock()
	// One owner observer waits for every distinct resource channel attached to
	// this action. The worker count is transferred before this observer starts.
	go func() {
		for _, resourceDone := range channels {
			<-resourceDone
		}
		proof := tools.PreparedCompletionProof("")
		if done != nil && readProof != nil {
			proof = readProof()
		}
		o.finishCompletionTaskResource(id, proof)
	}()
}

func completionChannelsClosed(channels []<-chan struct{}) bool {
	for _, done := range channels {
		select {
		case <-done:
		default:
			return false
		}
	}
	return true
}

func (o *runOwner) finishCompletionTaskResource(id uint64, proof tools.PreparedCompletionProof) {
	o.mu.Lock()
	defer o.mu.Unlock()
	task := o.completionTasks[id]
	if task == nil || task.state != runCompletionTaskResourceOpen || !completionChannelsClosed(task.resourceChannels) {
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

func (o *runOwner) completionDetails(id uint64) (runCompletionTaskDetails, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	task := o.completionTasks[id]
	if task == nil {
		return runCompletionTaskDetails{}, false
	}
	return runCompletionTaskDetails{
		id:                      id,
		intent:                  task.intent,
		state:                   task.state,
		proof:                   task.proof,
		pendingResourceUnproven: task.pendingResourceUnproven,
		receiptDurable:          task.receiptDurable,
		proofExecutionDurable:   task.proofExecutionDurable,
		resolutionDurable:       task.resolutionDurable,
	}, true
}

func (o *runOwner) markCompletionReceiptDurable(id uint64) {
	o.mu.Lock()
	if task := o.completionTasks[id]; task != nil {
		task.receiptDurable = true
		o.signalLocked()
	}
	o.mu.Unlock()
}

func (o *runOwner) markCompletionProofExecutionDurable(id uint64) {
	o.mu.Lock()
	if task := o.completionTasks[id]; task != nil {
		task.proofExecutionDurable = true
		o.signalLocked()
	}
	o.mu.Unlock()
}

func (o *runOwner) markCompletionResolved(id uint64) {
	o.mu.Lock()
	if task := o.completionTasks[id]; task != nil {
		task.resolutionDurable = true
		o.signalLocked()
	}
	o.mu.Unlock()
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
			if task.pendingResourceUnproven || task.proof == "" {
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
			handoffs = append(handoffs, runCompletionTaskHandoff{
				id:       id,
				channels: append([]<-chan struct{}(nil), task.resourceChannels...),
				proof:    task.read,
			})
		}
	}
	o.mu.Unlock()
	for _, handoff := range handoffs {
		if completionChannelsClosed(handoff.channels) {
			proof := tools.PreparedCompletionProof("")
			if handoff.proof != nil {
				proof = handoff.proof()
			}
			o.finishCompletionTaskResource(handoff.id, proof)
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
	cleanupCtx := o.startCleanupCommitContextLocked()
	o.mu.Unlock()
	if cleanupCtx == nil {
		return ctx
	}
	return cleanupCtx
}

func (o *runOwner) startCleanupCommitContextLocked() context.Context {
	if o.cleanupCommitCtx != nil {
		return o.cleanupCommitCtx
	}
	if !o.cleanupDeadlineSet {
		timeout := o.cleanupTimeout
		if timeout <= 0 {
			timeout = defaultRunCleanupTimeout
		}
		o.cleanupDeadline = time.Now().Add(timeout)
		o.cleanupDeadlineSet = true
	}
	base := o.cleanupBaseCtx
	if base == nil {
		// Cancel can arrive after the owner is published but before run has
		// attached its RunID and validated sink binding. Keep only the first
		// absolute deadline until those legitimate callback values are ready.
		return nil
	}
	o.cleanupCommitCtx, o.cleanupCommitStop = context.WithDeadline(base, o.cleanupDeadline)
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
		o.startCleanupCommitContextLocked()
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
	if o.releaseStarted || o.released || o.timedOut || o.callbackFinished {
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
		if lease.active && !o.releaseStarted && !o.released && !o.timedOut && !o.callbackFinished {
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
	o.setCleanupTimeout(timeout)
	callbackCtx := withRunID(withRunWorkerTracker(o.ctx, o), o.claim.ID)
	var bindingErr error
	if sink, binding, ok := RunCommitFromContext(callbackCtx); ok {
		if lease, ok := sink.(RunCommitRunLease); ok {
			o.mu.Lock()
			o.runCommitLease = lease
			o.mu.Unlock()
		}
		binding.OwnerGeneration = o.ownerGeneration
		bindingErr = binding.Validate()
		if bindingErr == nil {
			callbackCtx = WithRunCommit(callbackCtx, sink, binding)
			o.mu.Lock()
			o.runCommitBound = true
			o.terminalSyncSuccess = false
			o.runCommitBinding = binding
			o.mu.Unlock()
		} else {
			// Invalid descriptors must fail before provider work and must not
			// leak a partial sink identity into cancellation bookkeeping.
			callbackCtx = WithRunCommit(callbackCtx, nil, RunCommitBinding{})
		}
	}
	o.mu.Lock()
	o.callbackCtx = callbackCtx
	o.cleanupBaseCtx = context.WithoutCancel(callbackCtx)
	if o.cancelRequested || o.timedOut {
		o.startCleanupCommitContextLocked()
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
	return o.expiredBackpressure > 0 && !o.releaseStarted && !o.released && !o.timedOut
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
		if o.releaseStarted {
			o.mu.Unlock()
			return
		}
		o.releaseStarted = true
		// Release may call user-provided lease code. Start the caller's existing
		// bounded cleanup wait before leaving the lock so it cannot look like a
		// successful run while release is still in flight.
		o.startCleanupLocked()
		lease := o.runCommitLease
		closeLease := lease != nil && !o.leaseCloseAttempted
		if closeLease {
			o.leaseCloseAttempted = true
		}
		claim := o.claim
		guard := o.guard
		terminalSynced := !o.terminalSyncRequired || o.terminalSyncSuccess
		o.mu.Unlock()

		var lateCommitErr error
		if terminalSynced {
			lateCommitErr = o.drainLateCompletionTasks()
		}
		leasePanicked := false
		if closeLease {
			leasePanicked = closeRunCommitLease(lease)
		}
		guard.Release(claim)

		o.mu.Lock()
		o.released = true
		if lateCommitErr != nil {
			o.runErr = errors.Join(o.runErr, lateCommitErr)
			o.unknown = true
			o.status.Outcome = RunOutcomeUnknown
		}
		if leasePanicked {
			o.runErr = errors.Join(o.runErr, ErrRunCommitLeaseClosePanicked)
			o.unknown = true
			o.status.Outcome = RunOutcomeUnknown
		}
		if lateCommitErr != nil || leasePanicked || !o.durableCommitSucceededLocked() {
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

// drainLateCompletionTasks is the one non-worker writer for proof that became
// ready after the Loop's safe point. The terminal checkpoint and the exact
// tool receipt must already be durable. A failed write is not retried here;
// the run keeps its error/quarantine state and still releases local ownership
// after all resources have joined.
func (o *runOwner) drainLateCompletionTasks() error {
	o.mu.Lock()
	callbackCtx := o.callbackCtx
	tasks := make([]runCompletionTaskDetails, 0, len(o.completionTasks))
	for id := uint64(1); id <= o.nextCompletionTask; id++ {
		task := o.completionTasks[id]
		if task == nil || !task.receiptDurable || task.resolutionDurable || task.pendingResourceUnproven || task.state != runCompletionTaskReady || !validRunCompletionProof(task.proof) {
			continue
		}
		tasks = append(tasks, runCompletionTaskDetails{
			id:                    id,
			intent:                task.intent,
			state:                 task.state,
			proof:                 task.proof,
			receiptDurable:        task.receiptDurable,
			proofExecutionDurable: task.proofExecutionDurable,
			resolutionDurable:     task.resolutionDurable,
		})
	}
	o.mu.Unlock()
	if len(tasks) == 0 {
		return nil
	}
	sink, _, ok := RunCommitFromContext(callbackCtx)
	if !ok {
		return ErrRunCommitOwnerUnavailable
	}
	commitCtx := o.CleanupCommitContext(callbackCtx)
	for _, task := range tasks {
		intent := task.intent
		if !task.proofExecutionDurable {
			execution := ActionExecution{
				RunID:                      intent.RunID,
				AssistantTranscriptOrdinal: intent.AssistantTranscriptOrdinal,
				ToolCallID:                 intent.ToolCallID,
				ActionID:                   intent.ActionID,
				OwnerGeneration:            intent.OwnerGeneration,
				DispatchState:              DispatchStateDispatched,
				ResourceState:              RunExecutionQuiescent,
				CompletionProof:            task.proof,
			}
			if err := callLateCompletionSink("late execution", func() error {
				return sink.RecordExecution(commitCtx, execution)
			}); err != nil {
				return runCommitPersistenceError{operation: "late execution", cause: err}
			}
			o.markCompletionProofExecutionDurable(task.id)
		}
		if err := callLateCompletionSink("late execution resolution", func() error {
			return sink.ResolveExecution(commitCtx, ExecutionResolution{
				RunID:    intent.RunID,
				ActionID: intent.ActionID,
				Evidence: ResolutionLocalResourcesStopped,
			})
		}); err != nil {
			return runCommitPersistenceError{operation: "late execution resolution", cause: err}
		}
		o.markCompletionResolved(task.id)
	}
	return nil
}

func callLateCompletionSink(operation string, call func() error) (err error) {
	defer func() {
		if recover() != nil {
			// Sink panic payloads can contain sensitive tool or workspace data.
			// Preserve only the operation name and the typed failure sentinel.
			err = runCommitSinkPanicError{operation: operation}
		}
	}()
	return call()
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
	if o.released {
		result := o.resultSnapshotLocked()
		runErr := o.runErr
		o.mu.Unlock()
		return result, runErr
	}
	o.timedOut = true
	o.startCleanupCommitContextLocked()
	// Return the caller's bounded-wait observation consistently. The supervisor
	// may move the live status to Quarantined when the callback has not yet
	// committed a transcript, but this immutable return snapshot records that
	// cleanup remains pending at the point the caller stopped waiting.
	o.status.ExecutionState = RunExecutionCleanupPending
	if o.workers > 0 || !o.callbackFinished || (o.releaseStarted && (o.status.Outcome == RunOutcomeSuccess || o.status.Outcome == "")) {
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
