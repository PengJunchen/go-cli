//go:build !headless

package tui

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var _ interface {
	Send(Msg) error
	CriticalSendContext(context.Context, Msg) error
	LocalSendGapCount() uint64
} = (*BubbleteaApp)(nil)

// TestTUISendReportsBackpressure requires callers to observe overflow instead
// of treating a full UI queue as successful delivery.
func TestTUISendReportsBackpressure(t *testing.T) {
	app := NewBubbleteaApp(nil)
	for i := 0; i < cap(app.model.msgCh); i++ {
		require.NoError(t, app.Send(i))
	}

	err := app.Send("overflow")
	var full *SendQueueFullError
	require.ErrorAs(t, err, &full)
	require.ErrorIs(t, err, ErrSendQueueFull)
	require.Equal(t, cap(app.model.msgCh), full.Capacity)
	require.EqualValues(t, 1, app.LocalSendGapCount())
	require.Contains(t, app.View(), "Local TUI queue gaps: 1 messages not delivered")
}

// TestTUISendGapCountTracksBurstFailures keeps the UI-local delivery gap count
// separate from core.EventGapCount and verifies each rejected message counts.
func TestTUISendGapCountTracksBurstFailures(t *testing.T) {
	app := NewBubbleteaApp(nil)
	for i := 0; i < cap(app.model.msgCh); i++ {
		require.NoError(t, app.Send(i))
	}

	for i := 0; i < 3; i++ {
		err := app.Send(i)
		require.ErrorIs(t, err, ErrSendQueueFull)
	}
	require.EqualValues(t, 3, app.LocalSendGapCount())
	require.Contains(t, app.View(), "Local TUI queue gaps: 3 messages not delivered")
	app.model.Update(agentEventMsg{event: AgentEvent{Type: "done", Content: "status update"}})
	require.EqualValues(t, 3, app.LocalSendGapCount(), "a status update must not clear local gaps")
	require.Contains(t, app.View(), "Local TUI queue gaps: 3 messages not delivered")
}

// TestTUISendAfterQuitReportsClosed verifies Send does not accept a message
// after the app has been asked to stop.
func TestTUISendAfterQuitReportsClosed(t *testing.T) {
	app := NewBubbleteaApp(nil)
	app.Quit()

	err := app.Send("late")
	var closed *AppClosedError
	require.ErrorAs(t, err, &closed)
	require.ErrorIs(t, err, ErrAppClosed)
}

func TestTUISendAfterRunDoneWithSpaceReportsClosed(t *testing.T) {
	app := NewBubbleteaApp(nil)
	runDone := make(chan struct{})
	close(runDone)
	app.sendMu.Lock()
	app.activeRunDone = runDone
	app.running.Store(true)
	app.sendMu.Unlock()

	var closed *AppClosedError
	require.ErrorAs(t, app.Send("late"), &closed)
	require.Empty(t, app.model.msgCh)
	require.ErrorAs(t, app.CriticalSendContext(context.Background(), "critical"), &closed)
	require.Empty(t, app.model.msgCh)

	app.sendMu.Lock()
	app.activeRunDone = nil
	app.running.Store(false)
	app.sendMu.Unlock()
}

func TestTUICriticalSendAfterQuitBeforeRunWithSpaceReportsClosed(t *testing.T) {
	app := NewBubbleteaApp(nil)
	app.Quit()

	err := app.CriticalSendContext(context.Background(), "critical")
	var closed *AppClosedError
	require.ErrorAs(t, err, &closed)
	require.ErrorIs(t, err, ErrAppClosed)
	require.Empty(t, app.model.msgCh)
}

func TestTUISendConcurrentQuitHasAdmittedMessageOutcome(t *testing.T) {
	for iteration := 0; iteration < 25; iteration++ {
		app := NewBubbleteaApp(nil)
		start := make(chan struct{})
		sendResult := make(chan error, 1)
		quitDone := make(chan struct{})
		go func() {
			<-start
			sendResult <- app.Send("admitted")
		}()
		go func() {
			<-start
			app.Quit()
			close(quitDone)
		}()
		close(start)

		err := <-sendResult
		<-quitDone
		switch {
		case err == nil:
			require.Len(t, app.model.msgCh, 1, "successful Send must mean admitted before Quit")
			require.Equal(t, "admitted", <-app.model.msgCh)
		case errors.Is(err, ErrAppClosed):
			require.Empty(t, app.model.msgCh, "closed Send must not enqueue")
		default:
			t.Fatalf("unexpected concurrent Send result: %v", err)
		}
	}
}

func TestTUICriticalSendBeforeRunReportsQueueFull(t *testing.T) {
	app := NewBubbleteaApp(nil)
	for i := 0; i < cap(app.model.msgCh); i++ {
		require.NoError(t, app.Send(i))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	err := app.CriticalSendContext(ctx, "critical")
	require.ErrorIs(t, err, ErrSendQueueFull)
	require.Less(t, time.Since(start), time.Second)
	require.EqualValues(t, 1, app.LocalSendGapCount())
}

func TestTUICriticalSendPreCanceledContextIsHonored(t *testing.T) {
	app := NewBubbleteaApp(nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := app.CriticalSendContext(ctx, "critical")
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, len(app.model.msgCh), "a pre-canceled context must not enqueue")
	require.EqualValues(t, 1, app.LocalSendGapCount())
}

func TestTUICriticalSendCanceledPreRunFullQueueReportsCancellation(t *testing.T) {
	app := NewBubbleteaApp(nil)
	for i := 0; i < cap(app.model.msgCh); i++ {
		require.NoError(t, app.Send(i))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := app.CriticalSendContext(ctx, "critical")
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, cap(app.model.msgCh), len(app.model.msgCh))
	require.EqualValues(t, 1, app.LocalSendGapCount())
}

func TestTUICriticalSendContextWaitsForSpace(t *testing.T) {
	app, releaseModel, runErr := startBlockedQueueRun(context.Background(), t)
	defer releaseModel()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- app.CriticalSendContext(ctx, "critical") }()
	assertCriticalSendBlocked(t, result)

	releaseModel()
	select {
	case err := <-result:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("critical send did not use queue space")
	}

	app.Quit()
	select {
	case err := <-runErr:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("app did not stop after Quit")
	}
}

func TestTUICriticalSendContextHonorsCancellation(t *testing.T) {
	app, releaseModel, runErr := startBlockedQueueRun(context.Background(), t)
	defer releaseModel()

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- app.CriticalSendContext(ctx, "critical") }()
	assertCriticalSendBlocked(t, result)
	cancel()

	select {
	case err := <-result:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("critical send did not return after context cancellation")
	}
	require.EqualValues(t, 1, app.LocalSendGapCount())

	releaseModel()
	app.Quit()
	select {
	case err := <-runErr:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("app did not stop after Quit")
	}
}

func TestTUICriticalSendContextReturnsAppClosed(t *testing.T) {
	app, releaseModel, runErr := startBlockedQueueRun(context.Background(), t)
	defer releaseModel()

	result := make(chan error, 1)
	go func() { result <- app.CriticalSendContext(context.Background(), "critical") }()
	assertCriticalSendBlocked(t, result)
	app.Quit()

	select {
	case err := <-result:
		var closed *AppClosedError
		require.ErrorAs(t, err, &closed)
		require.ErrorIs(t, err, ErrAppClosed)
	case <-time.After(time.Second):
		t.Fatal("critical send did not return after app close")
	}

	releaseModel()
	select {
	case err := <-runErr:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("app did not stop after Quit")
	}
}

func TestTUICriticalSendContextStopsWhenRunEnds(t *testing.T) {
	runCtx, cancelRun := context.WithCancel(context.Background())
	app, releaseModel, runErr := startBlockedQueueRun(runCtx, t)
	defer releaseModel()

	result := make(chan error, 1)
	go func() { result <- app.CriticalSendContext(context.Background(), "critical") }()
	assertCriticalSendBlocked(t, result)
	cancelRun()
	<-app.model.msgCh // Model a consumer freeing one slot as the old run stops.
	select {
	case app.queueSpace <- struct{}{}:
	default:
	}

	select {
	case err := <-result:
		var closed *AppClosedError
		require.ErrorAs(t, err, &closed)
		require.ErrorIs(t, err, ErrAppClosed)
	case <-time.After(time.Second):
		t.Fatal("critical send did not observe the old run ending")
	}
	releaseModel()

	select {
	case err := <-runErr:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("app run did not return after context cancellation")
	}
	for len(app.model.msgCh) > 0 {
		msg := <-app.model.msgCh
		if text, ok := msg.(string); ok {
			require.NotEqual(t, "critical", text, "a message rejected after run cancellation must never enter the local queue")
		}
	}

	// The cumulative Done channel remains closed, but Run is intentionally
	// restartable. A fresh sender must bind to the new run generation.
	newRunErr := make(chan error, 1)
	go func() { newRunErr <- app.Run(context.Background()) }()
	require.Eventually(t, func() bool { return app.running.Load() && app.Program() != nil },
		time.Second, time.Millisecond, "app did not start a new run")
	require.NoError(t, app.CriticalSendContext(context.Background(), "new generation"))
	app.Quit()
	select {
	case err := <-newRunErr:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("restarted app did not stop after Quit")
	}
}

func startBlockedQueueRun(runCtx context.Context, t *testing.T) (*BubbleteaApp, func(), <-chan error) {
	t.Helper()
	blocked := make(chan struct{})
	releaseUpdate := make(chan struct{})
	var blockOnce sync.Once
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseUpdate) }) }
	app := NewBubbleteaApp(make(chan AgentEvent), WithOnUpdate(func(string) {
		blockOnce.Do(func() {
			close(blocked)
			<-releaseUpdate
		})
	}))
	app.interactive = false
	app.model.interactive = false

	runErr := make(chan error, 1)
	runDone := make(chan struct{})
	go func() {
		runErr <- app.Run(runCtx)
		close(runDone)
	}()
	stop := func() {
		release()
		app.Quit()
		select {
		case <-runDone:
		case <-time.After(time.Second):
			t.Error("app did not stop during test cleanup")
		}
	}
	t.Cleanup(stop)

	require.NoError(t, app.Send(TodoUpdateMsg{Items: []TodoItem{{ID: "blocking", Content: "blocking"}}}))
	require.Eventually(t, func() bool {
		select {
		case <-blocked:
			return true
		default:
			return false
		}
	}, time.Second, time.Millisecond, "render loop did not block in the test callback")
	for i := 0; i < cap(app.model.msgCh)+1; i++ {
		require.NoError(t, app.Send(i))
	}
	require.Equal(t, cap(app.model.msgCh), len(app.model.msgCh), "the bounded local queue should be full")
	select {
	case <-app.queueSpace:
	default:
		t.Fatal("the pump did not publish its bounded-queue space notification for its one in-flight item")
	}
	return app, release, runErr
}

func assertCriticalSendBlocked(t *testing.T, result <-chan error) {
	t.Helper()
	select {
	case err := <-result:
		t.Fatalf("critical send returned before the queue opened: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
}
