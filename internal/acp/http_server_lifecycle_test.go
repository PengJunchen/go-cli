package acp

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pengjunchen/go-cli/internal/core"
)

type gatedHTTPServerDispatcher struct {
	started chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func (d *gatedHTTPServerDispatcher) Dispatch(context.Context, core.SubagentTask) (core.SubagentResult, error) {
	d.calls.Add(1)
	select {
	case d.started <- struct{}{}:
	default:
	}
	<-d.release
	return core.SubagentResult{Content: "done"}, nil
}

func (d *gatedHTTPServerDispatcher) ParallelDispatch(ctx context.Context, tasks []core.SubagentTask) ([]core.SubagentResult, error) {
	results := make([]core.SubagentResult, len(tasks))
	for i, task := range tasks {
		result, err := d.Dispatch(ctx, task)
		if err != nil {
			return nil, err
		}
		results[i] = result
	}
	return results, nil
}

func (*gatedHTTPServerDispatcher) ListRunning() []core.SubagentTask { return nil }

func TestHTTPServerStopJoinsDetachedDispatchBeforeRestart(t *testing.T) {
	dispatcher := &gatedHTTPServerDispatcher{
		started: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	var releaseOnce sync.Once
	releaseDispatcher := func() { releaseOnce.Do(func() { close(dispatcher.release) }) }
	server := NewHTTPServer("test", "127.0.0.1:0", NewCoreHandler(dispatcher, nil))
	if err := server.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	baseURL := "http://" + server.Addr()
	t.Cleanup(func() {
		releaseDispatcher()
		_ = server.Stop(context.Background())
	})

	response := postACP(t, baseURL, "/connect", ACPMessage{Type: TypeConnect, SenderID: "client"})
	response.Body.Close()
	response = postACP(t, baseURL, "/send", ACPMessage{Type: TypeMessage, SenderID: "client", Content: "wait"})
	response.Body.Close()
	select {
	case <-dispatcher.started:
	case <-time.After(time.Second):
		t.Fatal("detached dispatcher did not start")
	}
	server.BeginStop()
	if server.Running() {
		t.Fatal("BeginStop returned before closing HTTP admission")
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err := server.Stop(stopCtx)
	if err == nil {
		t.Fatal("Stop returned success while a detached ProcessMessage worker was still running")
	}
	var pending interface{ Done() <-chan struct{} }
	if !errors.As(err, &pending) || pending.Done() == nil {
		t.Fatalf("Stop error %T %v does not expose the generation completion", err, err)
	}
	select {
	case <-pending.Done():
		t.Fatal("generation completion closed before detached worker returned")
	default:
	}

	if err := server.Start(context.Background()); err == nil {
		t.Fatal("Start reused the server while the previous generation was still pending")
	}

	secondCtx, secondCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer secondCancel()
	secondErr := server.Stop(secondCtx)
	var secondPending interface{ Done() <-chan struct{} }
	if !errors.As(secondErr, &secondPending) || secondPending.Done() != pending.Done() {
		t.Fatalf("concurrent Stop did not observe the same generation: first=%v second=%v", err, secondErr)
	}

	releaseDispatcher()
	select {
	case <-pending.Done():
	case <-time.After(time.Second):
		t.Fatal("generation did not finish after detached worker returned")
	}
	if err := server.Stop(context.Background()); err != nil {
		t.Fatalf("Stop after completion: %v", err)
	}
	if err := server.Start(context.Background()); err != nil {
		t.Fatalf("Start after completed generation: %v", err)
	}
	if err := server.Stop(context.Background()); err != nil {
		t.Fatalf("final Stop: %v", err)
	}
	if calls := dispatcher.calls.Load(); calls != 1 {
		t.Fatalf("detached dispatch count = %d, want 1", calls)
	}
}

func TestHTTPServerStopBeforeStartAndFailedListenAreIdempotent(t *testing.T) {
	server := NewHTTPServer("test", "127.0.0.1:0", nil)
	if err := server.Stop(context.Background()); err != nil {
		t.Fatalf("Stop before Start: %v", err)
	}

	occupier := NewHTTPServer("occupier", "127.0.0.1:0", nil)
	if err := occupier.Start(context.Background()); err != nil {
		t.Fatalf("occupier Start: %v", err)
	}
	defer occupier.Stop(context.Background())

	server.addr = occupier.Addr()
	if err := server.Start(context.Background()); err == nil {
		t.Fatal("Start on an occupied address succeeded")
	}
	if err := server.Stop(context.Background()); err != nil {
		t.Fatalf("Stop after failed Start: %v", err)
	}

	server.addr = "127.0.0.1:0"
	if err := server.Start(context.Background()); err != nil {
		t.Fatalf("Start after failed listen: %v", err)
	}
	if err := server.Stop(context.Background()); err != nil {
		t.Fatalf("Stop after recovered Start: %v", err)
	}
}

func TestHTTPServerConcurrentStartIsIdempotent(t *testing.T) {
	server := NewHTTPServer("test", "127.0.0.1:0", nil)
	start := make(chan error, 2)
	for range 2 {
		go func() { start <- server.Start(context.Background()) }()
	}
	for range 2 {
		if err := <-start; err != nil {
			t.Fatalf("concurrent Start: %v", err)
		}
	}
	if err := server.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestHTTPServerStopJoinsRegisteredRequestHandler(t *testing.T) {
	server := NewHTTPServer("test", "127.0.0.1:0", nil)
	if err := server.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = server.Stop(context.Background()) })

	server.mu.Lock()
	generation := server.generation
	server.mu.Unlock()
	started := make(chan struct{})
	release := make(chan struct{})
	tracked := server.generationRequestMiddleware(generation, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(started)
		<-release
	}))
	go tracked.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/barrier", nil))
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("registered request handler did not start")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	err := server.Stop(ctx)
	var pending interface{ Done() <-chan struct{} }
	if !errors.As(err, &pending) || pending.Done() == nil {
		t.Fatalf("Stop error %T %v does not report the blocked handler", err, err)
	}
	select {
	case <-pending.Done():
		t.Fatal("generation completed while its registered HTTP handler was blocked")
	default:
	}

	close(release)
	select {
	case <-pending.Done():
	case <-time.After(time.Second):
		t.Fatal("generation did not join the released HTTP handler")
	}
	if err := server.Stop(context.Background()); err != nil {
		t.Fatalf("Stop after handler completion: %v", err)
	}
}

type panickingHTTPServerDispatcher struct{ started chan struct{} }

func (d *panickingHTTPServerDispatcher) Dispatch(context.Context, core.SubagentTask) (core.SubagentResult, error) {
	close(d.started)
	panic("private panic detail")
}

func (*panickingHTTPServerDispatcher) ParallelDispatch(context.Context, []core.SubagentTask) ([]core.SubagentResult, error) {
	return nil, nil
}

func (*panickingHTTPServerDispatcher) ListRunning() []core.SubagentTask { return nil }

func TestHTTPServerContainsDetachedWorkerPanicWithoutLeakingValue(t *testing.T) {
	dispatcher := &panickingHTTPServerDispatcher{started: make(chan struct{})}
	server := NewHTTPServer("test", "127.0.0.1:0", NewCoreHandler(dispatcher, nil))
	if err := server.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = server.Stop(context.Background()) })
	baseURL := "http://" + server.Addr()
	response := postACP(t, baseURL, "/connect", ACPMessage{Type: TypeConnect, SenderID: "panic-client"})
	response.Body.Close()
	response = postACP(t, baseURL, "/send", ACPMessage{Type: TypeMessage, SenderID: "panic-client", Content: "panic"})
	response.Body.Close()
	select {
	case <-dispatcher.started:
	case <-time.After(time.Second):
		t.Fatal("dispatcher did not start")
	}

	err := server.Stop(context.Background())
	if !errors.Is(err, ErrHTTPServerWorkerPanic) {
		t.Fatalf("Stop error = %v, want ErrHTTPServerWorkerPanic", err)
	}
	if strings.Contains(err.Error(), "private panic detail") {
		t.Fatalf("Stop leaked panic value: %v", err)
	}
}

type blockedSlogHandler struct {
	started chan struct{}
	release <-chan struct{}
}

func (h *blockedSlogHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *blockedSlogHandler) Handle(context.Context, slog.Record) error {
	select {
	case h.started <- struct{}{}:
	default:
	}
	<-h.release
	return nil
}

func (h *blockedSlogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *blockedSlogHandler) WithGroup(string) slog.Handler      { return h }

func TestHTTPServerStopWaitDeadlineIsNotBlockedByLogger(t *testing.T) {
	server := NewHTTPServer("test", "127.0.0.1:0", nil)
	if err := server.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	oldLogger := slog.Default()
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseHandler := func() { releaseOnce.Do(func() { close(release) }) }
	slog.SetDefault(slog.New(&blockedSlogHandler{started: started, release: release}))
	t.Cleanup(func() {
		releaseHandler()
		slog.SetDefault(oldLogger)
		_ = server.Stop(context.Background())
	})

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	stopResult := make(chan error, 1)
	go func() { stopResult <- server.Stop(ctx) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("close owner did not reach the server stop log")
	}
	select {
	case err := <-stopResult:
		var pending interface{ Done() <-chan struct{} }
		if !errors.As(err, &pending) || pending.Done() == nil {
			t.Fatalf("Stop error %T %v does not expose pending cleanup", err, err)
		}
		releaseHandler()
		select {
		case <-pending.Done():
		case <-time.After(time.Second):
			t.Fatal("server generation did not complete after logger returned")
		}
	case <-time.After(150 * time.Millisecond):
		releaseHandler()
		t.Fatal("Stop waiter remained blocked in the logger instead of honoring its context")
	}
}
