package core

import (
	"context"
	"errors"
	"sync"
	"time"
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

type runOutcomeUnknown interface {
	OutcomeUnknown() bool
}

type runOwnerContextKey struct{}

type runOwner struct {
	mu                 sync.Mutex
	guard              RunSlotGuard
	claim              RunClaim
	ctx                context.Context
	cancel             context.CancelFunc
	done               chan struct{}
	changed            chan struct{}
	cleanup            chan struct{}
	backpressureSignal chan struct{}
	cleanupOnce        sync.Once

	status              RunStatus
	result              Result
	runErr              error
	callbackFinished    bool
	transcriptCommitted bool
	workers             int
	resources           int
	expiredBackpressure int
	cleanupTimeout      time.Duration
	unknown             bool
	cancelRequested     bool
	timedOut            bool
	released            bool
	onTimeout           func(Result, error)
	eventGapSource      interface{ GapCount() uint64 }
}

func newRunOwner(ctx context.Context, guard RunSlotGuard, claim RunClaim, timeout func(Result, error)) *runOwner {
	if guard == nil {
		guard = defaultRunSlotGuard
	}
	ownerCtx, cancel := context.WithCancel(ctx)
	return &runOwner{
		guard:              guard,
		claim:              claim,
		ctx:                ownerCtx,
		cancel:             cancel,
		done:               make(chan struct{}),
		changed:            make(chan struct{}, 1),
		cleanup:            make(chan struct{}),
		backpressureSignal: make(chan struct{}, 1),
		status: RunStatus{
			RunID:          claim.ID,
			ExecutionState: RunExecutionRunning,
		},
		onTimeout: timeout,
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

func markRunOutcomeUnknown(ctx context.Context) {
	if ctx == nil {
		return
	}
	tracker, ok := ctx.Value(runOwnerContextKey{}).(interface{ MarkOutcomeUnknown() })
	if ok && tracker != nil {
		tracker.MarkOutcomeUnknown()
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

func (o *runOwner) MarkOutcomeUnknown() {
	o.mu.Lock()
	o.unknown = true
	o.status.Outcome = RunOutcomeUnknown
	o.signalLocked()
	o.mu.Unlock()
}

func (o *runOwner) MarkCancelRequested() {
	o.mu.Lock()
	if !o.released && !o.timedOut && o.status.ExecutionState != RunExecutionQuiescent && o.status.ExecutionState != RunExecutionQuarantined {
		o.cancelRequested = true
		o.status.ExecutionState = RunExecutionCancelRequested
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
	callbackCtx := withRunWorkerTracker(o.ctx, o)
	go o.supervise()
	if ctxErr := callbackCtx.Err(); ctxErr != nil {
		o.finishUndispatched(ctxErr)
	} else {
		go func() {
			result, err, panicked := invokeRunCallback(callbackCtx, fn)
			o.mu.Lock()
			o.callbackFinished = true
			commitFailure := errors.Is(err, ErrCanonicalTranscriptInvalid) || errors.Is(err, ErrCanonicalTranscriptRequired)
			o.transcriptCommitted = !panicked && !commitFailure
			o.result = result
			o.runErr = err
			if panicked || commitFailure {
				o.unknown = true
			}
			var marker runOutcomeUnknown
			if errors.As(err, &marker) && marker.OutcomeUnknown() {
				o.unknown = true
			}
			switch {
			case o.unknown || result.Outcome == RunOutcomeUnknown:
				o.unknown = true
				o.status.Outcome = RunOutcomeUnknown
			case result.Outcome != "":
				o.status.Outcome = result.Outcome
			case err != nil && o.ctx.Err() != nil && errors.Is(err, o.ctx.Err()):
				o.status.Outcome = RunOutcomeCancelled
			case err != nil:
				o.status.Outcome = RunOutcomeError
			default:
				o.status.Outcome = RunOutcomeSuccess
			}
			if panicked || commitFailure {
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
			if o.uncommittedCallbackFinished() {
				return o.returnSnapshot()
			}
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
	o.cancelRequested = true
	o.runErr = err
	o.status.Outcome = RunOutcomeCancelled
	o.status.ExecutionState = RunExecutionDraining
	o.signalLocked()
	o.mu.Unlock()
}

func (o *runOwner) backpressureIsStillExpired() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.expiredBackpressure > 0 && !o.released && !o.timedOut
}

func invokeRunCallback(ctx context.Context, fn func(context.Context) (Result, error)) (result Result, err error, panicked bool) {
	defer func() {
		if recovered := recover(); recovered != nil {
			_ = recovered
			err = runCallbackPanicError{}
			panicked = true
		}
	}()
	result, err = fn(ctx)
	return result, err, false
}

func (o *runOwner) supervise() {
	for {
		<-o.changed
		o.mu.Lock()
		if o.timedOut && !o.quiescentLocked() {
			o.status.ExecutionState = RunExecutionQuarantined
		}
		if o.callbackFinished && !o.transcriptCommitted {
			o.status.ExecutionState = RunExecutionQuarantined
			o.mu.Unlock()
			return
		}
		if !o.quiescentLocked() {
			o.mu.Unlock()
			continue
		}
		if o.released {
			o.mu.Unlock()
			return
		}
		o.released = true
		claim := o.claim
		guard := o.guard
		o.mu.Unlock()

		guard.Release(claim)

		o.mu.Lock()
		o.status.ExecutionState = RunExecutionQuiescent
		o.status.PendingWorkers = 0
		o.cancel()
		close(o.done)
		o.mu.Unlock()
		return
	}
}

func (o *runOwner) quiescentLocked() bool {
	return o.callbackFinished && o.transcriptCommitted && o.workers == 0 && o.resources == 0
}

func (o *runOwner) cleanupPending() (Result, error) {
	o.mu.Lock()
	if o.released || o.quiescentLocked() {
		result := o.resultSnapshotLocked()
		runErr := o.runErr
		o.mu.Unlock()
		return result, runErr
	}
	o.timedOut = true
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

func (o *runOwner) uncommittedCallbackFinished() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.callbackFinished || o.transcriptCommitted {
		return false
	}
	o.cancel()
	return true
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
