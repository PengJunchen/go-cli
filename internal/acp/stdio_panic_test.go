package acp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const stdioPanicChildEnv = "GO_CLI_STDIO_PANIC_TEST"

func TestStdioWriterPanicIsContainedAndFencesSession(t *testing.T) {
	if !runStdioPanicChild(t, "writer") {
		return
	}

	reader, peerWriter := io.Pipe()
	writer := &panicNthWriter{panicAt: 2}
	client := NewStdioAdapter(reader, writer)
	t.Cleanup(func() {
		closeIgnored(peerWriter)
		closeIgnored(reader)
	})
	require.NoError(t, client.Connect(context.Background()))

	err := client.SendMessage(context.Background(), ACPMessage{Type: TypeMessage, Content: "private-payload"})
	// The recovered panic fences the session before the writer worker closes
	// its join channel, so the caller may observe either the terminal writer
	// error or a pending wrapper that already contains that terminal error.
	var panicErr StdioWritePanicError
	require.ErrorAs(t, err, &panicErr)
	var unknownDelivery interface{ UnknownDelivery() bool }
	require.ErrorAs(t, err, &unknownDelivery)
	assert.True(t, unknownDelivery.UnknownDelivery())
	var pending stdioPending
	var pendingDone <-chan struct{}
	if errors.As(err, &pending) {
		assert.True(t, pending.UnknownDelivery(), "a pending write must preserve unknown delivery")
		pendingDone = pending.Done()
		require.NotNil(t, pendingDone, "pending send must expose its writer worker join")
	} else {
		require.EqualError(t, err, "acp: stdio send: acp: stdio writer panicked after dispatch; delivery unknown")
	}
	assert.NotContains(t, err.Error(), "private-payload")
	assert.Equal(t, 2, writer.callCount())

	fencedErr := client.SendMessage(context.Background(), ACPMessage{Type: TypeMessage, Content: "must-not-write"})
	assert.ErrorIs(t, fencedErr, errStdioNotConnected)
	assert.Equal(t, 2, writer.callCount(), "a writer panic must fence the session")

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	disconnectErr := client.Disconnect(canceled)
	done := requireStdioPending(t, disconnectErr, true)
	closeIgnored(peerWriter)
	closeIgnored(reader)
	assertChannelCloses(t, done)
	if pendingDone != nil {
		assertChannelCloses(t, pendingDone)
	}
	terminalErr := client.Disconnect(context.Background())
	var terminalPanic StdioWritePanicError
	require.ErrorAs(t, terminalErr, &terminalPanic, "joined Disconnect must retain the terminal writer panic")
	assert.True(t, terminalPanic.UnknownDelivery())
	assert.NotContains(t, terminalErr.Error(), "private-payload")
}

func TestStdioReaderPanicIsContainedAndTerminal(t *testing.T) {
	if !runStdioPanicChild(t, "reader") {
		return
	}

	client := NewStdioAdapter(panicReader{}, &bytes.Buffer{})
	require.NoError(t, client.Connect(context.Background()))
	messages := client.ReceiveMessages()
	require.NotNil(t, messages)
	select {
	case _, open := <-messages:
		assert.False(t, open, "a reader panic must fence and close the inbound stream")
	case <-time.After(time.Second):
		t.Fatal("reader panic did not terminate the inbound stream")
	}

	err := client.Disconnect(context.Background())
	require.EqualError(t, err, "acp: stdio reader panicked")
	var panicErr StdioReaderPanicError
	require.ErrorAs(t, err, &panicErr)
	assert.NotContains(t, err.Error(), "private-panic-value")
}

func TestStdioDisconnectWriterPanicRemainsUnknownUntilJoin(t *testing.T) {
	if !runStdioPanicChild(t, "disconnect-writer") {
		return
	}

	reader, peerWriter := io.Pipe()
	writer := &panicNthWriter{panicAt: 2, started: make(chan struct{})}
	client := NewStdioAdapter(reader, writer)
	t.Cleanup(func() {
		closeIgnored(peerWriter)
		closeIgnored(reader)
	})
	require.NoError(t, client.Connect(context.Background()))

	ctx, cancel := context.WithCancel(context.Background())
	disconnectResult := make(chan error, 1)
	go func() { disconnectResult <- client.Disconnect(ctx) }()
	select {
	case <-writer.started:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("disconnect writer did not enter Write")
	}
	cancel()
	disconnectErr := <-disconnectResult
	done := requireStdioPending(t, disconnectErr, true)
	closeIgnored(peerWriter)
	closeIgnored(reader)
	assertChannelCloses(t, done)

	terminalErr := client.Disconnect(context.Background())
	var panicErr StdioWritePanicError
	require.ErrorAs(t, terminalErr, &panicErr)
	assert.True(t, panicErr.UnknownDelivery())
	assert.NotContains(t, terminalErr.Error(), "private-payload")
}

func TestStdioOwnedCloserPanicIsContainedAndReported(t *testing.T) {
	if !runStdioPanicChild(t, "closer") {
		return
	}

	reader, peerWriter := io.Pipe()
	closer := &panicCloser{started: make(chan struct{})}
	client := NewStdioAdapter(reader, &bytes.Buffer{}, WithStdioOwnedTransportCloser(closer))
	t.Cleanup(func() {
		closeIgnored(peerWriter)
		closeIgnored(reader)
	})
	require.NoError(t, client.Connect(context.Background()))

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	disconnectErr := client.Disconnect(canceled)
	done := requireStdioPending(t, disconnectErr, false)
	select {
	case <-closer.started:
	case <-time.After(time.Second):
		t.Fatal("owned Close was not invoked")
	}
	assertChannelOpen(t, done, "the blocked reader still belongs to cleanup")

	closeIgnored(peerWriter)
	closeIgnored(reader)
	assertChannelCloses(t, done)
	terminalErr := client.Disconnect(context.Background())
	require.EqualError(t, terminalErr, "acp: stdio transport closer panicked")
	var panicErr StdioCloserPanicError
	require.ErrorAs(t, terminalErr, &panicErr)
	assert.NotContains(t, terminalErr.Error(), "private-panic-value")
	assert.Equal(t, 1, closer.callCount(), "a recovered closer panic must not trigger an unowned retry")
	assert.EqualError(t, client.Disconnect(context.Background()), "acp: stdio transport closer panicked")
	assert.Equal(t, 1, closer.callCount())
}

func runStdioPanicChild(t *testing.T, scenario string) bool {
	t.Helper()
	if os.Getenv(stdioPanicChildEnv) == scenario {
		return true
	}

	command := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$")
	command.Env = append(os.Environ(), stdioPanicChildEnv+"="+scenario)
	output, err := command.CombinedOutput()
	require.NoError(t, err, "isolated panic scenario %q failed: %s", scenario, output)
	return false
}

type panicNthWriter struct {
	mu      sync.Mutex
	calls   int
	panicAt int
	started chan struct{}
	once    sync.Once
}

func (w *panicNthWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.calls++
	calls := w.calls
	w.mu.Unlock()
	if calls == w.panicAt {
		if w.started != nil {
			w.once.Do(func() { close(w.started) })
		}
		panic("private-payload")
	}
	return len(p), nil
}

func (w *panicNthWriter) callCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls
}

type panicReader struct{}

func (panicReader) Read([]byte) (int, error) {
	panic("private-panic-value")
}

type panicCloser struct {
	mu      sync.Mutex
	calls   int
	started chan struct{}
	once    sync.Once
}

func (c *panicCloser) Close() error {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	c.once.Do(func() { close(c.started) })
	panic("private-panic-value")
}

func (c *panicCloser) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

var _ io.Closer = (*panicCloser)(nil)
