package tui

import (
	"context"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
)

type barrierStatusRenderer struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (renderer *barrierStatusRenderer) Render(_ context.Context, content string, _ RenderOpts) string {
	renderer.once.Do(func() { close(renderer.started) })
	<-renderer.release
	return content
}

func (*barrierStatusRenderer) Name() string { return "barrier-status" }

func (*barrierStatusRenderer) Supports(contentType string) bool {
	return contentType == ContentTypeStatus
}

func TestRefreshAdmissionIsNotApplyAck(t *testing.T) {
	initial := appControlSnapshot("session-1", "main", "old")
	events := make(chan AgentEvent)
	renderer := &barrierStatusRenderer{started: make(chan struct{}), release: make(chan struct{})}
	registry := NewDefaultRegistry()
	registry.Register(renderer)
	app, err := NewBubbleteaAppWithTranscript(events, initial,
		WithRegistry(registry), WithRunPublicationIdentity("run-1", "owner-1"))
	require.NoError(t, err)

	uiCtx, cancelUI := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- app.Run(uiCtx) }() //nolint:errcheck
	select {
	case <-app.ProgramReady():
	case <-time.After(time.Second):
		t.Fatal("Bubble Tea program did not become ready")
	}

	events <- AgentEvent{Type: "status", ContentType: ContentTypeStatus, Content: "blocking update"}
	select {
	case <-renderer.started:
	case <-time.After(time.Second):
		t.Fatal("model did not enter the blocking update")
	}

	refresh := appControlRefresh("updated", core.DurablePublicationPending)
	applyDone := make(chan error, 1)
	go func() { applyDone <- app.ApplyTranscriptRefresh(uiCtx, refresh) }() //nolint:errcheck
	waitForActiveTranscriptRefresh(t, app)
	time.Sleep(20 * time.Millisecond)
	select {
	case err := <-applyDone:
		t.Fatalf("refresh returned before teaModel.Update applied it: %v", err)
	default:
	}

	close(renderer.release)
	select {
	case err := <-applyDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("refresh did not return after model application")
	}
	app.model.mu.Lock()
	require.Equal(t, "updated", app.model.transcriptSnapshot.Rows[0].Message.Content)
	require.Equal(t, core.DurablePublicationPending, app.model.durablePublication.State)
	app.model.mu.Unlock()

	cancelUI()
	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatal("Bubble Tea app did not stop after its UI context was canceled")
	}
}

func TestTranscriptRefreshRejectsIdentityMismatchBeforeMutation(t *testing.T) {
	initial := appControlSnapshot("session-1", "main", "old")
	app, err := NewBubbleteaAppWithTranscript(make(chan AgentEvent), initial,
		WithRunPublicationIdentity("run-1", "owner-1"))
	require.NoError(t, err)

	uiCtx, cancelUI := context.WithCancel(context.Background())
	defer cancelUI()
	runDone := make(chan error, 1)
	go func() { runDone <- app.Run(uiCtx) }() //nolint:errcheck
	select {
	case <-app.ProgramReady():
	case <-time.After(time.Second):
		t.Fatal("Bubble Tea program did not become ready")
	}

	refresh := appControlRefresh("wrong-session", core.DurablePublicationPending)
	refresh.Snapshot = appControlSnapshot("session-2", "main", "wrong-session")
	err = app.ApplyTranscriptRefresh(uiCtx, refresh)
	require.ErrorIs(t, err, ErrTranscriptRefreshIdentity)
	app.model.mu.Lock()
	require.Equal(t, "old", app.model.transcriptSnapshot.Rows[0].Message.Content)
	app.model.mu.Unlock()

	app.Quit()
	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatal("Bubble Tea app did not stop after Quit")
	}
}

func TestTranscriptRefreshCancelBeforeClaimRejectsLateUpdate(t *testing.T) {
	request := newTranscriptRefreshRequest(1, TranscriptRefresh{})
	require.True(t, request.cancelPending(context.Canceled))
	require.False(t, request.claim(), "a late model update cannot claim canceled control")
	require.ErrorIs(t, <-request.ack, context.Canceled)
	require.False(t, request.completeApplied(), "canceled request cannot be acknowledged twice")
}

func TestTranscriptRefreshClaimMakesCancellationWaitForApplyAck(t *testing.T) {
	request := newTranscriptRefreshRequest(2, TranscriptRefresh{})
	require.True(t, request.claim())

	ctx, cancel := context.WithCancel(context.Background())
	waitDone := make(chan error, 1)
	go func() { waitDone <- request.wait(ctx) }() //nolint:errcheck
	cancel()
	select {
	case err := <-waitDone:
		t.Fatalf("claimed apply returned before its model ACK: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	require.True(t, request.completeApplied())
	select {
	case err := <-waitDone:
		require.NoError(t, err, "cancellation after claim cannot roll back applied state")
	case <-time.After(time.Second):
		t.Fatal("claimed apply did not wake after ACK")
	}
}

func TestTranscriptRefreshInFlightIsBoundedToOne(t *testing.T) {
	initial := appControlSnapshot("session-1", "main", "old")
	events := make(chan AgentEvent)
	renderer := &barrierStatusRenderer{started: make(chan struct{}), release: make(chan struct{})}
	registry := NewDefaultRegistry()
	registry.Register(renderer)
	app, err := NewBubbleteaAppWithTranscript(events, initial,
		WithRegistry(registry), WithRunPublicationIdentity("run-1", "owner-1"))
	require.NoError(t, err)
	uiCtx, cancelUI := context.WithCancel(context.Background())
	defer cancelUI()
	runDone := make(chan error, 1)
	go func() { runDone <- app.Run(uiCtx) }() //nolint:errcheck
	select {
	case <-app.ProgramReady():
	case <-time.After(time.Second):
		t.Fatal("Bubble Tea program did not become ready")
	}
	events <- AgentEvent{Type: "status", ContentType: ContentTypeStatus, Content: "blocking update"}
	select {
	case <-renderer.started:
	case <-time.After(time.Second):
		t.Fatal("model did not enter the blocking update")
	}

	firstDone := make(chan error, 1)
	go func() {
		firstDone <- app.ApplyTranscriptRefresh(uiCtx, appControlRefresh("first", core.DurablePublicationPending))
	}() //nolint:errcheck
	waitForActiveTranscriptRefresh(t, app)
	time.Sleep(20 * time.Millisecond)
	require.ErrorIs(t, app.ApplyTranscriptRefresh(uiCtx, appControlRefresh("second", core.DurablePublicationPending)), ErrTranscriptRefreshInFlight)

	close(renderer.release)
	select {
	case err := <-firstDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("first refresh did not receive its model ACK")
	}
	app.Quit()
	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatal("Bubble Tea app did not stop after Quit")
	}
}

func appControlSnapshot(sessionID, branchID, content string) CanonicalTranscriptSnapshot {
	return CanonicalTranscriptSnapshot{
		PhysicalSessionID: sessionID,
		BranchID:          branchID,
		Revision:          "revision-1",
		ActiveLeafEntryID: "user-1",
		Rows: []CanonicalTranscriptRow{{
			Key:     DurableTranscriptKey{PhysicalSessionID: sessionID, BranchID: branchID, EntryID: "user-1"},
			Kind:    TranscriptRowUser,
			Message: core.AgentMessage{Role: "user", Content: content},
		}},
	}
}

func appControlRefresh(content string, publicationState core.DurablePublicationState) TranscriptRefresh {
	snapshot := appControlSnapshot("session-1", "main", content)
	snapshot.Revision = "revision-2"
	return TranscriptRefresh{
		Snapshot: snapshot,
		Observation: RunObservation{
			RunID: "run-1", OwnerGeneration: "owner-1",
			Outcome: core.RunOutcomeUnknown, ExecutionState: core.RunExecutionQuiescent,
		},
		Publication: core.DurablePublication{
			State: publicationState, RunID: "run-1", SessionID: "session-1", OwnerGeneration: "owner-1",
		},
	}
}

func TestTranscriptRefreshSavedRequiresExactQuiescentTerminalProof(t *testing.T) {
	initial := appControlSnapshot("session-1", "main", "old")
	app, err := NewBubbleteaAppWithTranscript(make(chan AgentEvent), initial,
		WithRunPublicationIdentity("run-1", "owner-1"))
	require.NoError(t, err)

	refresh := appControlRefresh("updated", core.DurablePublicationSynced)
	refresh.Observation = RunObservation{
		RunID: "run-1", OwnerGeneration: "owner-1", Outcome: core.RunOutcomeSuccess,
		ExecutionState: core.RunExecutionQuiescent, TerminalEntryID: "terminal-1",
		TerminalOutcome: core.RunOutcomeSuccess,
	}
	require.NoError(t, app.model.validateTranscriptRefresh(refresh))

	invalid := refresh
	invalid.Observation.RunID = "other-run"
	require.ErrorIs(t, app.model.validateTranscriptRefresh(invalid), ErrTranscriptRefreshIdentity)
	invalid = refresh
	invalid.Observation.OwnerGeneration = "other-owner"
	require.ErrorIs(t, app.model.validateTranscriptRefresh(invalid), ErrTranscriptRefreshIdentity)
	invalid = refresh
	invalid.Observation.ExecutionState = core.RunExecutionCleanupPending
	require.ErrorIs(t, app.model.validateTranscriptRefresh(invalid), ErrTranscriptRefreshIdentity)
	invalid = refresh
	invalid.Observation.TerminalEntryID = ""
	require.ErrorIs(t, app.model.validateTranscriptRefresh(invalid), ErrTranscriptRefreshIdentity)
	invalid = refresh
	invalid.Observation.TerminalOutcome = core.RunOutcomeError
	require.ErrorIs(t, app.model.validateTranscriptRefresh(invalid), ErrTranscriptRefreshIdentity)

	app.model.mu.Lock()
	app.model.runObservation = refresh.Observation
	app.model.durablePublication = refresh.Publication
	app.model.mu.Unlock()
	require.Contains(t, app.model.View(), "[saved: success]")

	app.model.mu.Lock()
	app.model.runObservation.ExecutionState = core.RunExecutionCleanupPending
	app.model.mu.Unlock()
	require.NotContains(t, app.model.View(), "[saved: success]")
	require.Contains(t, app.model.View(), "[durable publication: unconfirmed]")
}

func TestCoreAndLocalEventGapsRenderAsSeparateFacts(t *testing.T) {
	app, err := NewBubbleteaAppWithTranscript(make(chan AgentEvent), appControlSnapshot("session-1", "main", "old"))
	require.NoError(t, err)
	app.model.localSendGapCount.Store(2)
	app.model.mu.Lock()
	app.model.runObservation.EventGapCount = 3
	app.model.durablePublication = core.DurablePublication{State: core.DurablePublicationPending}
	app.model.mu.Unlock()
	view := app.model.View()
	require.Contains(t, view, "Core event gaps: 3")
	require.Contains(t, view, "Local TUI queue gaps: 2")
	require.Contains(t, view, "durable publication: pending")
	require.NotContains(t, view, "[saved")
}

func waitForActiveTranscriptRefresh(t *testing.T, app *BubbleteaApp) *transcriptRefreshRequest {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		app.controlMu.Lock()
		request := app.activeRefresh
		app.controlMu.Unlock()
		if request != nil {
			return request
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("transcript refresh request was not installed")
	return nil
}

var _ tea.Msg = transcriptRefreshControlMsg{}
