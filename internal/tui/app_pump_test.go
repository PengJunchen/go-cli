//go:build !headless

package tui

import (
	"context"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
)

func TestAppOwnedPumpJoinsBeforeDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	messages := make(chan Msg, 1)
	queueSpace := make(chan struct{}, 1)
	sendStarted := make(chan struct{})
	messages <- "queued"

	pumpDone := startMessagePump(ctx, messages, queueSpace, func(tea.Msg) {
		close(sendStarted)
		<-ctx.Done()
	})
	select {
	case <-sendStarted:
	case <-time.After(time.Second):
		t.Fatal("message pump did not enter Program.Send")
	}

	cancel()
	select {
	case <-pumpDone:
	case <-time.After(time.Second):
		t.Fatal("message pump remained alive after Program context cancellation")
	}
}

func TestBubbleteaAppDoneJoinsIdleSourceAndApprovalCommands(t *testing.T) {
	app := NewBubbleteaApp(make(chan AgentEvent), WithApprovalChannel(make(chan ApprovalRequest)))
	uiCtx, cancelUI := context.WithCancel(context.Background())
	defer cancelUI()
	runDone := make(chan error, 1)
	go func() { runDone <- app.Run(uiCtx) }()
	select {
	case <-app.ProgramReady():
	case <-time.After(time.Second):
		t.Fatal("Bubble Tea program did not become ready")
	}

	app.Quit()
	select {
	case <-app.Done():
	case <-time.After(time.Second):
		t.Fatal("app did not join its event and approval commands after Quit")
	}
	require.NoError(t, <-runDone)
}

func TestSnapshotAppKeepsUIAliveAfterEventSourceClosesUntilRefreshAck(t *testing.T) {
	snapshot := CanonicalTranscriptSnapshot{
		PhysicalSessionID: "session-event-close",
		BranchID:          "branch-event-close",
		Revision:          "revision-event-close",
	}
	events := make(chan AgentEvent)
	app, err := NewBubbleteaAppWithTranscript(events, snapshot, WithWidth(80))
	require.NoError(t, err)

	uiCtx, cancelUI := context.WithCancel(context.Background())
	defer cancelUI()
	runDone := make(chan error, 1)
	go func() { runDone <- app.Run(uiCtx) }()
	select {
	case <-app.ProgramReady():
	case <-time.After(time.Second):
		t.Fatal("Bubble Tea program did not become ready")
	}

	close(events)
	select {
	case <-app.EventSourceDone():
	case <-time.After(time.Second):
		t.Fatal("model did not observe event-source completion")
	}
	select {
	case <-app.Done():
		t.Fatal("snapshot-backed app quit when only its event source closed")
	default:
	}

	require.NoError(t, app.ApplyTranscriptRefresh(uiCtx, TranscriptRefresh{
		Snapshot:    snapshot,
		Publication: core.DurablePublication{State: core.DurablePublicationUnbound},
	}))
	app.Quit()
	select {
	case <-app.Done():
	case <-time.After(time.Second):
		t.Fatal("app did not join after explicit quit")
	}
	require.NoError(t, <-runDone)
}
