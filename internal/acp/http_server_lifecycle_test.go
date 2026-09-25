package acp

import (
	"context"
	"errors"
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
