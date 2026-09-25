package tools

import (
	"context"
	"sync"
)

// PreparedCompletionProof names one of the closed, action-scoped completion
// paths that a prepared executor can positively prove. A proof is usable only
// after its signal's Done channel closes.
type PreparedCompletionProof string

const (
	// CompletionInlinePipelineJoined means the complete synchronous prepared
	// executor pipeline returned.
	CompletionInlinePipelineJoined PreparedCompletionProof = "inline_pipeline_joined"
	// CompletionQueuedJobJoined means the exact accepted prepared queue job's
	// captured executor returned.
	CompletionQueuedJobJoined PreparedCompletionProof = "queued_job_joined"
	// CompletionOwnedProcessGroupJoined means the local process leader and
	// output readers joined and the owned process group was positively absent.
	CompletionOwnedProcessGroupJoined PreparedCompletionProof = "owned_process_group_joined"
)

// PreparedCompletionSignal exposes the action-scoped completion channel for
// one sealed PreparedInvocation. The fields stay private so callers cannot
// manufacture completion authority. Proof returns an enum only after Done
// closes; an empty proof means the operation has no trusted completion proof.
type PreparedCompletionSignal struct {
	state *preparedCompletionState
}

// Done returns the positive-completion channel when the captured prepared
// path has one. Nil means that this invocation cannot currently provide a
// trusted completion signal.
func (s PreparedCompletionSignal) Done() <-chan struct{} {
	if s.state == nil {
		return nil
	}
	return s.state.doneChannel()
}

// Proof returns the completion enum only after its positive Done channel has
// closed. Before then, and for unknown or failed-to-prove execution paths, it
// returns the empty value.
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
	pipelineReturned   bool
	pipelineSuccessful bool
	finalized          bool
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
	if s == nil || !validPreparedCompletionProof(proof) {
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
	if proof == CompletionOwnedProcessGroupJoined {
		s.processMode = true
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
	defer s.mu.Unlock()
	if s.ownedDone != done || s.finalized {
		return
	}
	if s.processMode {
		s.pipelineReturned = true
		s.pipelineSuccessful = completed
		if !completed {
			s.proofInvalidated = true
			s.proof = ""
		}
		if !s.processObserved || s.processClosed {
			s.finishProcessLocked()
		}
		return
	}
	if !completed {
		s.proof = ""
		s.proofInvalidated = true
	}
	s.finalized = true
	close(done)
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
		s.processObserved = true
		s.processDone = done
		select {
		case <-done:
			s.processClosed = true
		default:
		}
		if s.pipelineReturned && s.processClosed {
			s.finishProcessLocked()
		}
		wait := !s.processClosed
		s.mu.Unlock()
		if wait {
			go func() {
				<-done
				s.mu.Lock()
				s.processClosed = true
				if s.pipelineReturned {
					s.finishProcessLocked()
				}
				s.mu.Unlock()
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

func (s *preparedCompletionState) finishProcessLocked() {
	if s.ownedDone == nil || !s.processMode || s.finalized {
		return
	}
	if !s.proofInvalidated && s.pipelineSuccessful && s.processObserved && s.processClosed {
		s.proof = CompletionOwnedProcessGroupJoined
	} else {
		s.proof = ""
	}
	s.finalized = true
	close(s.ownedDone)
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
	switch proof {
	case CompletionInlinePipelineJoined, CompletionQueuedJobJoined, CompletionOwnedProcessGroupJoined:
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
