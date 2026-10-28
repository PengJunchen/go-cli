package tools

import (
	"context"
	"errors"
	"sync"
)

// PreparedCompletionProof names one of the closed, action-scoped completion
// paths that a prepared executor can positively prove. A proof is usable only
// after its signal's Done channel closes.
type PreparedCompletionProof string

// PreparedCompletionSupported checks the captured completion contract of an
// exact sealed invocation without consuming its permit, acquiring a dispatch
// lease, enqueueing work, or executing a tool. A non-nil streamSink selects
// DispatchStreaming and must be the exact host-attested sink captured during
// preparation. Current cancellation, project trust and dispatch fence errors
// take precedence over an unsupported contract. Support is not completion
// evidence and never replaces dispatch's final authority checks or actual join.
func PreparedCompletionSupported(ctx context.Context, invocation PreparedInvocation, streamSink SynchronousStreamSink) (bool, error) {
	if err := validatePreparedContext(ctx); err != nil {
		return false, err
	}
	var prepared *preparedInvocation
	var streaming *preparedStreamingInvocation
	switch current := invocation.(type) {
	case *preparedInvocation:
		prepared = current
	case *preparedStreamingInvocation:
		streaming = current
		if current != nil {
			prepared = current.preparedInvocation
		}
	default:
		return false, nil
	}
	if prepared == nil {
		return false, nil
	}
	if prepared.consumed.Load() {
		return false, ErrPreparedInvocationConsumed
	}
	checkCurrent := func() error {
		return prepared.fence.WithSnapshot(func(current DispatchFenceSnapshot) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if current != prepared.generations {
				return ErrDispatchFenceChanged
			}
			return nil
		})
	}
	var err error
	if prepared.projectTrustBinding != nil {
		err = prepared.projectTrustBinding.CommitIfCurrent(ctx, checkCurrent)
	} else {
		err = checkCurrent()
	}
	if err != nil {
		return false, err
	}
	if prepared.execute == nil || prepared.completion == nil {
		return false, nil
	}
	if streamSink != nil && (streaming == nil || streaming.executeStreaming == nil || !isCapturedSynchronousStreamSink(streaming.streamingSink, streamSink)) {
		return false, nil
	}
	proof := prepared.completionProof
	if proof == CompletionQueuedJobJoined && (prepared.schedule == nil || streamSink != nil) {
		return false, nil
	}
	return validPreparedCompletionProof(proof) || proof == completionScopedSearchJoined || proof == completionLocalProcessOrPrestartNoProcessJoined, nil
}

const (
	// CompletionInlinePipelineJoined means the complete synchronous prepared
	// executor pipeline returned.
	CompletionInlinePipelineJoined PreparedCompletionProof = "inline_pipeline_joined"
	// CompletionMCPRequestJoined proves the exact local MCP request pipeline joined
	// after a known response or a known request rejection. It does not attest
	// remote side-effect containment; unknown transport outcomes never emit it.
	CompletionMCPRequestJoined PreparedCompletionProof = "mcp_request_joined"
	// CompletionWorkerRequestJoined proves an exact owned worker's local request
	// and cancellation callback joined after a validated result or known rejection.
	// It never proves worker exit, sandboxing or remote side-effect containment.
	CompletionWorkerRequestJoined PreparedCompletionProof = "worker_request_joined"
	// CompletionQueuedJobJoined means the exact accepted prepared queue job's
	// captured executor returned.
	CompletionQueuedJobJoined PreparedCompletionProof = "queued_job_joined"
	// CompletionOwnedProcessGroupJoined means the local process leader and
	// output readers joined and the owned process group was positively absent.
	CompletionOwnedProcessGroupJoined PreparedCompletionProof = "owned_process_group_joined"
	// completionScopedSearchJoined is a prepared-dispatch mode for a trusted
	// scoped search reader. Its final signal reports either the concrete
	// synchronous fallback proof or the concrete owned-process-group proof;
	// this mode value is never published as a completion proof.
	completionScopedSearchJoined PreparedCompletionProof = "scoped_search_joined"
	// completionLocalProcessOrPrestartNoProcessJoined is a prepared-dispatch
	// mode for a trusted local Bash tool. Its final signal reports either the
	// concrete owned-process-group proof or an explicit synchronous attestation
	// that Sandbox.Validate rejected the command before process start. The mode itself
	// is never published as a completion proof.
	completionLocalProcessOrPrestartNoProcessJoined PreparedCompletionProof = "local_process_or_prestart_no_process_joined"
)

// PreparedCompletionSignal exposes one sealed PreparedInvocation's
// action-scoped local pipeline/resource-join signal. Its fields stay private
// so callers cannot manufacture completion authority. Done may close after
// the observed local work returns even when containment remains unproved;
// callers must check this same signal's non-empty Proof before clearing a
// marker that requires the corresponding positive proof.
type PreparedCompletionSignal struct {
	state *preparedCompletionState
}

// Done returns the local completion/join channel when the captured prepared
// path has one. Closure alone does not prove containment or a broader resource
// boundary. Nil means that this invocation has no trusted completion path.
func (s PreparedCompletionSignal) Done() <-chan struct{} {
	if s.state == nil {
		return nil
	}
	return s.state.doneChannel()
}

// Proof returns the positive completion enum only after this signal's Done
// channel has closed. Before then, and for unknown or failed-to-prove paths,
// it returns the empty value; callers must require the proof matching their
// resource boundary rather than relying on Done alone.
func (s PreparedCompletionSignal) Proof() PreparedCompletionProof {
	if s.state == nil {
		return ""
	}
	return s.state.completedProof()
}

type preparedCompletionState struct {
	mu                 sync.RWMutex
	proof              PreparedCompletionProof
	done               <-chan struct{}
	proofInvalidated   bool
	ownedDone          chan struct{}
	processMode        bool
	processDone        <-chan struct{}
	processObserved    bool
	processClosed      bool
	inlineFallbackMode bool
	inlinePrestartMode bool
	inlineObserved     bool
	inlineClosed       bool
	pipelineReturned   bool
	pipelineSuccessful bool
	finalized          bool
	doneCallbacks      []func()
}

func newPreparedCompletionState() *preparedCompletionState {
	return &preparedCompletionState{}
}

func (s *preparedCompletionState) signal() PreparedCompletionSignal {
	if s == nil {
		return PreparedCompletionSignal{}
	}
	return PreparedCompletionSignal{state: s}
}

func (s *preparedCompletionState) doneChannel() <-chan struct{} {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.done
}

func (s *preparedCompletionState) completedProof() PreparedCompletionProof {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	done, proof := s.done, s.proof
	s.mu.RUnlock()
	return s.proofFromCompletedSnapshot(done, proof)
}

func (s *preparedCompletionState) proofFromCompletedSnapshot(done <-chan struct{}, proof PreparedCompletionProof) PreparedCompletionProof {
	if done == nil || !validPreparedCompletionProof(proof) {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.proofInvalidated || s.done != done || s.proof != proof {
		return ""
	}
	select {
	case <-done:
		return s.proof
	default:
		return ""
	}
}

func (s *preparedCompletionState) begin(proof PreparedCompletionProof) chan struct{} {
	if s == nil || (!validPreparedCompletionProof(proof) && proof != completionScopedSearchJoined && proof != completionLocalProcessOrPrestartNoProcessJoined) {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done != nil {
		return nil
	}
	done := make(chan struct{})
	s.done = done
	s.ownedDone = done
	if proof == CompletionOwnedProcessGroupJoined || proof == completionScopedSearchJoined || proof == completionLocalProcessOrPrestartNoProcessJoined {
		s.processMode = true
		s.inlineFallbackMode = proof == completionScopedSearchJoined
		s.inlinePrestartMode = proof == completionLocalProcessOrPrestartNoProcessJoined
	} else {
		s.proof = proof
	}
	return done
}

func (s *preparedCompletionState) finish(done chan struct{}, completed bool) {
	if s == nil || done == nil {
		return
	}
	s.mu.Lock()
	if s.ownedDone != done || s.finalized {
		s.mu.Unlock()
		return
	}
	if s.processMode {
		s.pipelineReturned = true
		s.pipelineSuccessful = completed
		if !completed {
			s.proofInvalidated = true
			s.proof = ""
		}
		var callbacks []func()
		if (!s.processObserved && !s.inlineObserved) || (s.processObserved && s.processClosed) || (s.inlineObserved && s.inlineClosed) {
			callbacks = s.finishResourceLocked()
		}
		s.mu.Unlock()
		runPreparedCompletionCallbacks(callbacks)
		return
	}
	if !completed {
		s.proof = ""
		s.proofInvalidated = true
	}
	s.finalized = true
	close(done)
	callbacks := s.takeDoneCallbacksLocked()
	s.mu.Unlock()
	runPreparedCompletionCallbacks(callbacks)
}

// addDoneCallback binds cleanup to this exact prepared completion. If the
// completion already finalized, cleanup runs immediately after releasing the
// state mutex.
func (s *preparedCompletionState) addDoneCallback(done chan struct{}, callback func()) {
	if callback == nil {
		return
	}
	if s == nil || done == nil {
		callback()
		return
	}
	s.mu.Lock()
	if s.ownedDone != done || s.finalized {
		s.mu.Unlock()
		callback()
		return
	}
	s.doneCallbacks = append(s.doneCallbacks, callback)
	s.mu.Unlock()
}

func (s *preparedCompletionState) attach(proof PreparedCompletionProof, done <-chan struct{}) bool {
	if s == nil || done == nil || !validPreparedCompletionProof(proof) {
		return false
	}
	s.mu.Lock()
	if proof == CompletionOwnedProcessGroupJoined {
		if s.ownedDone == nil || !s.processMode || s.processObserved || s.finalized {
			s.mu.Unlock()
			return false
		}
		if s.inlineObserved {
			s.mu.Unlock()
			return false
		}
		s.processObserved = true
		s.processDone = done
		select {
		case <-done:
			s.processClosed = true
		default:
		}
		var callbacks []func()
		if s.pipelineReturned && s.processClosed {
			callbacks = s.finishResourceLocked()
		}
		wait := !s.processClosed
		s.mu.Unlock()
		runPreparedCompletionCallbacks(callbacks)
		if wait {
			go func() {
				<-done
				s.mu.Lock()
				s.processClosed = true
				var callbacks []func()
				if s.pipelineReturned {
					callbacks = s.finishResourceLocked()
				}
				s.mu.Unlock()
				runPreparedCompletionCallbacks(callbacks)
			}()
		}
		return true
	}
	if proof == CompletionInlinePipelineJoined && s.processMode {
		if s.ownedDone == nil || (!s.inlineFallbackMode && !s.inlinePrestartMode) || s.inlineObserved || s.processObserved || s.finalized {
			s.mu.Unlock()
			return false
		}
		s.inlineObserved = true
		select {
		case <-done:
			s.inlineClosed = true
		default:
		}
		var callbacks []func()
		if s.pipelineReturned && s.inlineClosed {
			callbacks = s.finishResourceLocked()
		}
		wait := !s.inlineClosed
		s.mu.Unlock()
		runPreparedCompletionCallbacks(callbacks)
		if wait {
			go func() {
				<-done
				s.mu.Lock()
				s.inlineClosed = true
				var callbacks []func()
				if s.pipelineReturned {
					callbacks = s.finishResourceLocked()
				}
				s.mu.Unlock()
				runPreparedCompletionCallbacks(callbacks)
			}()
		}
		return true
	}
	defer s.mu.Unlock()
	if s.done != nil {
		return false
	}
	s.done = done
	if !s.proofInvalidated {
		s.proof = proof
	}
	return true
}

func (s *preparedCompletionState) finishResourceLocked() []func() {
	if s.ownedDone == nil || !s.processMode || s.finalized {
		return nil
	}
	s.proof = ""
	if !s.proofInvalidated && s.pipelineSuccessful {
		switch {
		case s.processObserved && s.processClosed:
			s.proof = CompletionOwnedProcessGroupJoined
		case (s.inlineFallbackMode || s.inlinePrestartMode) && s.inlineObserved && s.inlineClosed:
			s.proof = CompletionInlinePipelineJoined
		}
	}
	s.finalized = true
	close(s.ownedDone)
	return s.takeDoneCallbacksLocked()
}

func (s *preparedCompletionState) takeDoneCallbacksLocked() []func() {
	callbacks := s.doneCallbacks
	s.doneCallbacks = nil
	return callbacks
}

func runPreparedCompletionCallbacks(callbacks []func()) {
	for _, callback := range callbacks {
		callback()
	}
}

func (s *preparedCompletionState) invalidateProof() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.proofInvalidated = true
	s.proof = ""
	s.mu.Unlock()
}

func validPreparedCompletionProof(proof PreparedCompletionProof) bool {
	return IsPositiveCompletionProof(proof)
}

// IsPositiveCompletionProof recognizes only the closed positive proof enum
// shared by execution, durable storage and canonical transcript consumers.
// Recognition does not establish that a particular action completed: callers
// must still validate its exact invocation, join signal and journal coordinates.
// Empty observations and internal registration modes are never positive proof.
func IsPositiveCompletionProof(proof PreparedCompletionProof) bool {
	switch proof {
	case CompletionInlinePipelineJoined, CompletionQueuedJobJoined, CompletionOwnedProcessGroupJoined, CompletionMCPRequestJoined, CompletionWorkerRequestJoined:
		return true
	default:
		return false
	}
}

type preparedCompletionReporterKey struct{}

type preparedCompletionReporter func(PreparedCompletionProof, <-chan struct{})

func withPreparedCompletionReporter(ctx context.Context, reporter preparedCompletionReporter) context.Context {
	if ctx == nil || reporter == nil {
		return ctx
	}
	return context.WithValue(ctx, preparedCompletionReporterKey{}, reporter)
}

func reportPreparedCompletion(ctx context.Context, proof PreparedCompletionProof, done <-chan struct{}) {
	if ctx == nil || done == nil || !validPreparedCompletionProof(proof) {
		return
	}
	reporter, ok := ctx.Value(preparedCompletionReporterKey{}).(preparedCompletionReporter)
	if ok && reporter != nil {
		reporter(proof, done)
	}
}

func reportPreparedLocalProcessCompletion(ctx context.Context, lifecycle managedLocalResult) {
	var done <-chan struct{}
	switch {
	case lifecycle.containmentProven:
		completed := make(chan struct{})
		close(completed)
		done = completed
	case lifecycle.executionDone != nil && !lifecycle.forcedPipeClose:
		done = lifecycle.executionDone
	default:
		return
	}
	reportPreparedCompletion(ctx, CompletionOwnedProcessGroupJoined, done)
}

func reportPreparedNoProcessCompletion(ctx context.Context) {
	done := make(chan struct{})
	close(done)
	reportPreparedCompletion(ctx, CompletionInlinePipelineJoined, done)
}

// completionOutcomeKnown keeps a synchronous return from resolving an action
// whose executor still owns pending resources or reports unknown side effects.
func completionOutcomeKnown(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var unknown interface{ OutcomeUnknown() bool }
	if errors.As(err, &unknown) && unknown.OutcomeUnknown() {
		return false
	}
	var pending PendingExecutionError
	return !errors.As(err, &pending)
}

// finishAfterPending keeps the captured generation pinned until the exact
// pending resource joins. Its unknown outcome remains unproved even afterwards.
func (s *preparedCompletionState) finishAfterPending(done chan struct{}, completed bool, pending <-chan struct{}) {
	if pending == nil {
		s.finish(done, completed)
		return
	}
	select {
	case <-pending:
		s.finish(done, false)
	default:
		go func() { <-pending; s.finish(done, false) }()
	}
}
