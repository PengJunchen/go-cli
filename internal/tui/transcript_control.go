package tui

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/pengjunchen/go-cli/internal/core"
)

var errTranscriptRefreshContext = errors.New("tui: transcript refresh requires the active UI context")

type transcriptRefreshState uint8

const (
	transcriptRefreshPending transcriptRefreshState = iota
	transcriptRefreshClaimed
	transcriptRefreshApplied
	transcriptRefreshRejected
	transcriptRefreshCanceled
	transcriptRefreshClosed
)

type transcriptRefreshRequest struct {
	id      uint64
	refresh TranscriptRefresh

	mu    sync.Mutex
	state transcriptRefreshState
	ack   chan error
}

type transcriptRefreshControlMsg struct {
	request *transcriptRefreshRequest
}

type transcriptRefreshViewQueuedMsg struct {
	request *transcriptRefreshRequest
}

func newTranscriptRefreshRequest(id uint64, refresh TranscriptRefresh) *transcriptRefreshRequest {
	return &transcriptRefreshRequest{id: id, refresh: refresh, ack: make(chan error, 1)}
}

func (request *transcriptRefreshRequest) claim() bool {
	request.mu.Lock()
	defer request.mu.Unlock()
	if request.state != transcriptRefreshPending {
		return false
	}
	request.state = transcriptRefreshClaimed
	return true
}

func (request *transcriptRefreshRequest) completeApplied() bool {
	return request.complete(transcriptRefreshClaimed, transcriptRefreshApplied, nil)
}

func (request *transcriptRefreshRequest) completeClaimed(err error) bool {
	if err == nil {
		return request.completeApplied()
	}
	return request.complete(transcriptRefreshClaimed, transcriptRefreshRejected, err)
}

func (request *transcriptRefreshRequest) reject(err error) bool {
	return request.complete(transcriptRefreshPending, transcriptRefreshRejected, err)
}

func (request *transcriptRefreshRequest) cancelPending(err error) bool {
	return request.complete(transcriptRefreshPending, transcriptRefreshCanceled, err)
}

func (request *transcriptRefreshRequest) closePending(err error) bool {
	request.mu.Lock()
	defer request.mu.Unlock()
	if request.state != transcriptRefreshPending && request.state != transcriptRefreshClaimed {
		return false
	}
	request.state = transcriptRefreshClosed
	request.ack <- err
	return true
}

func (request *transcriptRefreshRequest) complete(expected, terminal transcriptRefreshState, err error) bool {
	request.mu.Lock()
	defer request.mu.Unlock()
	if request.state != expected {
		return false
	}
	request.state = terminal
	request.ack <- err
	return true
}

func (request *transcriptRefreshRequest) wait(ctx context.Context) error {
	for {
		select {
		case err := <-request.ack:
			return err
		case <-ctx.Done():
			if request.cancelPending(ctx.Err()) {
				return ctx.Err()
			}
			request.mu.Lock()
			state := request.state
			request.mu.Unlock()
			if state == transcriptRefreshClaimed {
				return <-request.ack
			}
			select {
			case err := <-request.ack:
				return err
			default:
				return ctx.Err()
			}
		}
	}
}

func (request *transcriptRefreshRequest) apply(model *teaModel) bool {
	refresh := request.refresh
	refresh.Snapshot = refresh.Snapshot.Clone()
	if err := model.validateTranscriptRefresh(refresh); err != nil {
		request.reject(err)
		return false
	}
	if !request.claim() {
		return false
	}

	model.mu.Lock()
	err := model.installTranscriptSnapshotLocked(refresh.Snapshot)
	model.runObservation = refresh.Observation
	model.durablePublication = refresh.Publication
	model.mu.Unlock()
	if err != nil {
		request.completeClaimed(err)
		return false
	}
	return true
}

func (m *teaModel) validateTranscriptRefresh(refresh TranscriptRefresh) error {
	if err := refresh.Snapshot.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	hasSnapshot := m.hasTranscriptSnapshot
	currentSession := m.transcriptSnapshot.PhysicalSessionID
	currentBranch := m.transcriptSnapshot.BranchID
	m.mu.Unlock()
	if !hasSnapshot || refresh.Snapshot.PhysicalSessionID != currentSession || refresh.Snapshot.BranchID != currentBranch {
		return ErrTranscriptRefreshIdentity
	}
	publication := refresh.Publication
	if publication.State != core.DurablePublicationUnbound && publication.State != core.DurablePublicationPending &&
		publication.State != core.DurablePublicationSynced && publication.State != core.DurablePublicationFailed {
		return fmt.Errorf("%w: invalid durable publication state %q", ErrTranscriptRefreshIdentity, publication.State)
	}
	if publication.State != core.DurablePublicationUnbound {
		if publication.RunID == "" || publication.SessionID != refresh.Snapshot.PhysicalSessionID || publication.OwnerGeneration == "" {
			return ErrTranscriptRefreshIdentity
		}
		if refresh.Observation.RunID != publication.RunID || refresh.Observation.OwnerGeneration != publication.OwnerGeneration {
			return ErrTranscriptRefreshIdentity
		}
		if m.expectedRunID != "" && publication.RunID != m.expectedRunID {
			return ErrTranscriptRefreshIdentity
		}
		if m.expectedOwnerGen != "" && publication.OwnerGeneration != m.expectedOwnerGen {
			return ErrTranscriptRefreshIdentity
		}
	}
	if publication.State == core.DurablePublicationSynced {
		if refresh.Observation.ExecutionState != core.RunExecutionQuiescent ||
			refresh.Observation.Outcome == core.RunOutcomeUnknown ||
			refresh.Observation.TerminalEntryID == "" ||
			refresh.Observation.TerminalOutcome != refresh.Observation.Outcome {
			return ErrTranscriptRefreshIdentity
		}
	}
	return nil
}

// TranscriptRefresher applies a canonical snapshot through a model-apply ACK.
// Queue admission and Program.Send completion are not application evidence.
type TranscriptRefresher interface {
	ApplyTranscriptRefresh(context.Context, TranscriptRefresh) error
}

// ApplyTranscriptRefresh sends one reliable control request to the active
// model and waits until Bubble Tea has queued its updated view for rendering.
func (a *BubbleteaApp) ApplyTranscriptRefresh(ctx context.Context, refresh TranscriptRefresh) error {
	if ctx == nil {
		return context.Canceled
	}
	if a.initialTranscriptErr != nil {
		return a.initialTranscriptErr
	}
	if err := a.waitForProgram(ctx); err != nil {
		return err
	}
	program := a.Program()
	if program == nil {
		return ErrTranscriptRefreshClosed
	}

	a.controlMu.Lock()
	if current := a.activeRefresh; current != nil {
		current.mu.Lock()
		active := current.state == transcriptRefreshPending || current.state == transcriptRefreshClaimed
		current.mu.Unlock()
		if active {
			a.controlMu.Unlock()
			return ErrTranscriptRefreshInFlight
		}
	}
	request := newTranscriptRefreshRequest(a.nextControlID.Add(1), refresh)
	a.activeRefresh = request
	a.controlMu.Unlock()
	defer func() {
		a.controlMu.Lock()
		if a.activeRefresh == request {
			a.activeRefresh = nil
		}
		a.controlMu.Unlock()
	}()

	a.sendMu.Lock()
	uiContextDone := a.uiContextDone
	runDone := a.activeRunDone
	a.sendMu.Unlock()
	if runDone == nil || uiContextDone == nil || ctx.Done() != uiContextDone {
		request.closePending(errTranscriptRefreshContext)
		return errTranscriptRefreshContext
	}
	if runHasEnded(runDone) {
		request.closePending(ErrTranscriptRefreshClosed)
		return ErrTranscriptRefreshClosed
	}
	program.Send(transcriptRefreshControlMsg{request: request})
	return request.wait(ctx)
}

func (a *BubbleteaApp) waitForProgram(ctx context.Context) error {
	for {
		ready := a.ProgramReady()
		done := a.Done()
		select {
		case <-ready:
			if a.Program() != nil {
				return nil
			}
			select {
			case <-done:
				return ErrTranscriptRefreshClosed
			case <-ctx.Done():
				return ctx.Err()
			}
		case <-done:
			return ErrTranscriptRefreshClosed
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (a *BubbleteaApp) closePendingTranscriptRefresh(err error) {
	a.controlMu.Lock()
	request := a.activeRefresh
	a.controlMu.Unlock()
	if request != nil {
		request.closePending(err)
	}
}

func (request *transcriptRefreshRequest) String() string {
	return fmt.Sprintf("transcript-refresh-%d", request.id)
}
