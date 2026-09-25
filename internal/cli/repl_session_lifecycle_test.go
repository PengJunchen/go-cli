package cli

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/pengjunchen/go-cli/internal/session"
)

type blockedREPLFlushLogger struct {
	started chan struct{}
	release <-chan struct{}
}

func (h *blockedREPLFlushLogger) Enabled(context.Context, slog.Level) bool { return true }

func (h *blockedREPLFlushLogger) Handle(_ context.Context, record slog.Record) error {
	if record.Level < slog.LevelWarn {
		return nil
	}
	select {
	case h.started <- struct{}{}:
	default:
	}
	<-h.release
	return nil
}

func (h *blockedREPLFlushLogger) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *blockedREPLFlushLogger) WithGroup(string) slog.Handler      { return h }

func TestStopFlushTickerJoinsActiveFlushCallback(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseLogger := func() { releaseOnce.Do(func() { close(release) }) }
	sessionStore := session.NewJSONLSessionStore(filepath.Join(t.TempDir(), "session.jsonl"))
	pendingWrites := session.NewPendingSessionWrites()
	pendingWrites.Enqueue(session.SessionWrite{
		SessionID: "flush-test",
		Entry:     session.SessionEntry{ID: "invalid", Type: ""},
	})
	repl := &REPLSession{
		assembly:      &AgentAssembly{SessionManagement: SessionManagement{SessionStore: sessionStore}},
		pendingWrites: pendingWrites,
		logger:        slog.New(&blockedREPLFlushLogger{started: started, release: release}),
		spanCtx:       context.Background(),
	}
	repl.startFlushTicker()
	t.Cleanup(func() {
		releaseLogger()
		repl.stopFlushTicker()
	})
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("periodic flush did not reach its blocked logger")
	}

	stopDone := make(chan struct{})
	go func() {
		repl.stopFlushTicker()
		close(stopDone)
	}()
	select {
	case <-stopDone:
		t.Fatal("stopFlushTicker returned before the active flush callback exited")
	case <-time.After(30 * time.Millisecond):
	}

	releaseLogger()
	select {
	case <-stopDone:
	case <-time.After(time.Second):
		t.Fatal("stopFlushTicker did not return after the active callback exited")
	}
}

func TestStopFlushTickerIsSafeWhenNeverStarted(t *testing.T) {
	repl := &REPLSession{}
	repl.stopFlushTicker()
	repl.stopFlushTicker()
	if repl.flushTicker != nil || repl.flushDone != nil {
		t.Fatal("stopping an unstarted ticker should not create resources")
	}
}
