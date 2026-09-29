package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

type invocationTrackedLog struct {
	mu     sync.Mutex
	buffer bytes.Buffer
	closed atomic.Bool
}

func (w *invocationTrackedLog) Write(p []byte) (int, error) {
	if w.closed.Load() {
		return 0, errors.New("write after invocation log close")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buffer.Write(p)
}

func (w *invocationTrackedLog) Close() error {
	w.closed.Store(true)
	return nil
}

func (w *invocationTrackedLog) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buffer.String()
}

type invocationGatedSink struct {
	started         chan struct{}
	release         chan struct{}
	shutdown        chan struct{}
	firstExport     sync.Once
	shutdownOnce    sync.Once
	mu              sync.Mutex
	exported        []string
	shutdownSaw     []string
	shutdownCalls   atomic.Int32
	exportCallCount atomic.Int32
}

func newInvocationGatedSink() *invocationGatedSink {
	return &invocationGatedSink{
		started:  make(chan struct{}),
		release:  make(chan struct{}),
		shutdown: make(chan struct{}),
	}
}

func (s *invocationGatedSink) ExportSpan(_ context.Context, span tracing.TraceSpan) error {
	s.exportCallCount.Add(1)
	s.mu.Lock()
	s.exported = append(s.exported, span.Name())
	s.mu.Unlock()
	s.firstExport.Do(func() {
		close(s.started)
		<-s.release
		slog.Default().Info("late invocation exporter callback")
	})
	return nil
}

func (s *invocationGatedSink) Shutdown(context.Context) error {
	s.shutdownCalls.Add(1)
	s.mu.Lock()
	s.shutdownSaw = append([]string(nil), s.exported...)
	s.mu.Unlock()
	s.shutdownOnce.Do(func() { close(s.shutdown) })
	return nil
}

func (s *invocationGatedSink) exportedNames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.exported...)
}

func (s *invocationGatedSink) namesAtShutdown() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.shutdownSaw...)
}

func newTestInvocationOwner(
	t *testing.T,
	producerDone <-chan struct{},
	sink *invocationGatedSink,
	exporterWaitBudget time.Duration,
) (*invocationCloseOwner, *tracing.AsyncExporter, *invocationTrackedLog, tracing.TraceSpan, *tracing.Tracer, *slog.Logger) {
	t.Helper()
	previousLogger := slog.Default()
	logFile := &invocationTrackedLog{}
	invocationLogger := slog.New(tracing.NewRedactingSlogHandler(slog.NewTextHandler(logFile, &slog.HandlerOptions{Level: slog.LevelInfo})))
	slog.SetDefault(invocationLogger)

	exporter := tracing.NewAsyncExporter(sink, 16, 4)
	tracer := tracing.NewTracer("invocation-owner-test", exporter)
	root, _ := tracer.Start(context.Background(), "cli.invocation", tracing.SpanKindInternal)
	owner := newInvocationCloseOwner(producerDone, invocationFinalizer{
		root:               root,
		tracer:             tracer,
		exporter:           exporter,
		stderr:             io.Discard,
		logFile:            logFile,
		previousLogger:     previousLogger,
		invocationLogger:   invocationLogger,
		exitCode:           1,
		exporterWaitBudget: exporterWaitBudget,
	})
	t.Cleanup(func() {
		select {
		case <-sink.release:
		default:
			close(sink.release)
		}
		select {
		case <-owner.Done():
		case <-time.After(3 * time.Second):
			_ = exporter.Shutdown(context.Background())
		}
		slog.SetDefault(previousLogger)
	})
	return owner, exporter, logFile, root, tracer, invocationLogger
}

func TestInvocationCloseOwnerWaitsForProducerBeforeRootAndLogFinalization(t *testing.T) {
	producerDone := make(chan struct{})
	sink := newInvocationGatedSink()
	owner, exporter, logFile, root, tracer, invocationLogger := newTestInvocationOwner(t, producerDone, sink, time.Second)
	owner.start()

	var stderr bytes.Buffer
	code, completed := waitForInvocationClose(owner, 25*time.Millisecond, &stderr)
	require.Equal(t, 1, code)
	require.False(t, completed, "a pending producer must bound the invocation waiter")
	require.Contains(t, stderr.String(), "invocation shutdown remains pending")
	select {
	case <-owner.initialDone:
		t.Fatal("root/exporter finalization began before the Assembly producer joined")
	default:
	}
	select {
	case <-owner.Done():
		t.Fatal("invocation owner completed while its Assembly producer remained active")
	default:
	}
	require.False(t, logFile.closed.Load(), "the invocation log remains owned while producer completion is unknown")
	require.Equal(t, invocationLogger, slog.Default(), "the invocation logger remains installed while the producer is unknown")
	require.Empty(t, sink.exportedNames(), "the root span remains open before producer completion")
	select {
	case <-sink.shutdown:
		t.Fatal("the trace exporter was shut down while the Assembly producer remained active")
	default:
	}

	child, _ := tracer.Start(root.Context(), "assembly.late_child", tracing.SpanKindInternal)
	child.End()
	slog.Default().Info("late Assembly producer log")
	close(sink.release)
	close(producerDone)
	select {
	case <-owner.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("invocation owner did not finish after its producer joined")
	}
	require.True(t, logFile.closed.Load())
	require.Contains(t, logFile.String(), "late Assembly producer log")
	require.Contains(t, sink.exportedNames(), "assembly.late_child")
	require.Contains(t, sink.exportedNames(), "cli.invocation")
	require.Contains(t, sink.namesAtShutdown(), "assembly.late_child")
	require.Contains(t, sink.namesAtShutdown(), "cli.invocation")
	require.EqualValues(t, 1, sink.shutdownCalls.Load())
	report, err := exporter.ShutdownWithReport(context.Background())
	require.NoError(t, err)
	require.EqualValues(t, 0, report.Stats.RejectedAfterClose)
}

func TestInvocationCloseOwnerRetainsLogUntilAsyncExporterOwnerFinishes(t *testing.T) {
	producerDone := make(chan struct{})
	close(producerDone)
	sink := newInvocationGatedSink()
	owner, exporter, logFile, root, tracer, invocationLogger := newTestInvocationOwner(t, producerDone, sink, 25*time.Millisecond)
	child, _ := tracer.Start(root.Context(), "assembly.child", tracing.SpanKindInternal)
	child.End()
	owner.start()

	select {
	case <-sink.started:
	case <-time.After(time.Second):
		t.Fatal("the AsyncExporter did not enter the gated sink callback")
	}
	code, completed := waitForInvocationClose(owner, time.Second, io.Discard)
	require.Equal(t, 1, code, "the bounded exporter waiter must report pending")
	require.False(t, completed, "the invocation owner remains pending while the sink callback is active")
	require.Equal(t, invocationLogger, slog.Default())
	require.False(t, logFile.closed.Load(), "the active sink callback still owns the invocation logger and file")
	select {
	case <-owner.Done():
		t.Fatal("AsyncExporter waiter timeout was misreported as callback completion")
	default:
	}
	select {
	case <-sink.shutdown:
		t.Fatal("inner exporter shutdown overlapped the active span callback")
	default:
	}

	close(sink.release)
	select {
	case <-owner.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("invocation owner did not observe the shared exporter close owner finish")
	}
	require.True(t, logFile.closed.Load())
	require.Contains(t, logFile.String(), "late invocation exporter callback")
	require.EqualValues(t, 1, sink.shutdownCalls.Load())
	report, err := exporter.ShutdownWithReport(context.Background())
	require.NoError(t, err)
	require.True(t, report.OwnerFinished)
	require.EqualValues(t, 0, report.Stats.RejectedAfterClose)
}

func TestInvocationCloseOwnerDoesNotRestoreOverNewerLogger(t *testing.T) {
	producerDone := make(chan struct{})
	close(producerDone)
	sink := newInvocationGatedSink()
	owner, _, logFile, root, tracer, _ := newTestInvocationOwner(t, producerDone, sink, 25*time.Millisecond)
	child, _ := tracer.Start(root.Context(), "assembly.child", tracing.SpanKindInternal)
	child.End()
	owner.start()
	select {
	case <-sink.started:
	case <-time.After(time.Second):
		t.Fatal("the AsyncExporter did not enter the gated sink callback")
	}
	_, completed := waitForInvocationClose(owner, time.Second, io.Discard)
	require.False(t, completed)

	newerLogger := slog.New(slog.NewTextHandler(io.Discard, nil))
	slog.SetDefault(newerLogger)
	close(sink.release)
	select {
	case <-owner.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("invocation owner did not finish after the exporter callback returned")
	}
	require.True(t, logFile.closed.Load())
	require.Equal(t, newerLogger, slog.Default(), "an older invocation owner must not restore over a newer logger")
}

func TestInvocationProducerBarrierWaitsForEveryAssemblyOwner(t *testing.T) {
	first := make(chan struct{})
	second := make(chan struct{})
	barrier := invocationProducerBarrier([]<-chan struct{}{first, second}, true)
	require.NotNil(t, barrier)
	close(first)
	require.False(t, waitForProducerBarrier(barrier, 10*time.Millisecond), "one pending Assembly owner keeps the invocation barrier open")
	close(second)
	require.True(t, waitForProducerBarrier(barrier, time.Second))
}

func TestInvocationProducerBarrierUnknownCompletionFailsClosed(t *testing.T) {
	require.Nil(t, invocationProducerBarrier(nil, false))
	require.Nil(t, invocationProducerBarrier([]<-chan struct{}{nil}, true))
}
