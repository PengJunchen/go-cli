package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const reviewStdioMaxFrameBytes = 1 << 20

type stdioPending interface {
	error
	Done() <-chan struct{}
	UnknownDelivery() bool
}

func TestStdioConnectDeclaresPrivateProfileVersion(t *testing.T) {
	var out bytes.Buffer
	client := NewStdioAdapter(strings.NewReader(""), &out)
	require.NoError(t, client.Connect(context.Background()))

	line, err := readLineLen(&out)
	require.NoError(t, err)
	var connect map[string]any
	require.NoError(t, json.Unmarshal(line, &connect))
	assert.Equal(t, "connect", connect["type"])
	assert.Equal(t, "go-cli-acp-json/1", connect["version"])
}

func TestStdioConnectRejectsMissingTransportSide(t *testing.T) {
	var out bytes.Buffer
	missingReader := NewStdioAdapter(nil, &out)
	require.ErrorIs(t, missingReader.Connect(context.Background()), errStdioMissingInput)
	assert.Empty(t, out.Bytes(), "connect must not announce a half-configured transport")

	missingWriter := NewStdioAdapter(strings.NewReader(""), nil)
	require.ErrorIs(t, missingWriter.Connect(context.Background()), errStdioMissingOutput)
}

func TestStdioFrameLimitUsesPayloadBytesAndCRLFTerminator(t *testing.T) {
	msg := ACPMessage{Type: TypeMessage, SenderID: "peer"}
	empty, err := json.Marshal(msg)
	require.NoError(t, err)
	msg.Content = strings.Repeat("x", reviewStdioMaxFrameBytes-len(empty))
	frame, err := json.Marshal(msg)
	require.NoError(t, err)
	require.Len(t, frame, reviewStdioMaxFrameBytes)

	client := NewStdioAdapter(strings.NewReader(string(frame)+"\r\n"), &bytes.Buffer{})
	require.NoError(t, client.Connect(context.Background()))
	select {
	case got, open := <-client.ReceiveMessages():
		require.True(t, open, "exactly-limit payload with CRLF must be accepted")
		assert.Len(t, got.Content, reviewStdioMaxFrameBytes-len(empty))
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for exact-limit frame")
	}

	overLimit := strings.Repeat("x", reviewStdioMaxFrameBytes+1) + "\n" + string(frame) + "\n"
	oversizedClient := NewStdioAdapter(strings.NewReader(overLimit), &bytes.Buffer{})
	require.NoError(t, oversizedClient.Connect(context.Background()))
	select {
	case _, open := <-oversizedClient.ReceiveMessages():
		assert.False(t, open, "oversized frame must terminate the receive session")
	case <-time.After(time.Second):
		t.Fatal("oversized frame did not terminate the receive session")
	}
}

func TestStdioCleanEOFClosesReceiveAndFencesSend(t *testing.T) {
	reader, peerWriter := io.Pipe()
	var out bytes.Buffer
	client := NewStdioAdapter(reader, &out)
	require.NoError(t, client.Connect(context.Background()))
	inbound := client.ReceiveMessages()
	require.NotNil(t, inbound)

	require.NoError(t, peerWriter.Close())
	select {
	case _, open := <-inbound:
		assert.False(t, open, "clean EOF must close the receive channel")
	case <-time.After(time.Second):
		t.Fatal("receive channel did not close after peer EOF")
	}

	err := client.SendMessage(context.Background(), ACPMessage{Type: TypeMessage, Content: "after-eof"})
	require.ErrorIs(t, err, errStdioNotConnected, "clean EOF must fence later outbound frames")

	var connect ACPMessage
	line, err := readLineLen(&out)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(line, &connect))
	assert.Equal(t, TypeConnect, connect.Type, "the only outbound frame should be the handshake")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, client.Disconnect(ctx))
}

func TestStdioOutboundFrameLimitRejectsBeforeWriteWithoutPoisoning(t *testing.T) {
	reader, peerWriter := io.Pipe()
	var out bytes.Buffer
	client := NewStdioAdapter(reader, &out)
	require.NoError(t, client.Connect(context.Background()))
	t.Cleanup(func() {
		_ = peerWriter.Close()
		_ = reader.Close()
	})

	msg := ACPMessage{Type: TypeMessage, Timestamp: time.Date(2026, time.September, 25, 0, 0, 0, 0, time.UTC)}
	base, err := json.Marshal(msg)
	require.NoError(t, err)
	msg.Content = strings.Repeat("x", stdioMaxFrameBytes-len(base))
	encoded, err := json.Marshal(msg)
	require.NoError(t, err)
	require.Len(t, encoded, stdioMaxFrameBytes, "exactly the configured outbound payload limit is allowed")
	require.NoError(t, client.SendMessage(context.Background(), msg))

	beforeOversized := append([]byte(nil), out.Bytes()...)
	msg.Content += "x"
	err = client.SendMessage(context.Background(), msg)
	require.ErrorIs(t, err, errStdioTooLarge)
	var pending stdioPending
	assert.False(t, errors.As(err, &pending), "oversize rejection happens before dispatch and is not unknown delivery")
	assert.Equal(t, beforeOversized, out.Bytes(), "oversized frame must not write a partial prefix")

	require.NoError(t, client.SendMessage(context.Background(), ACPMessage{Type: TypeMessage, Content: "session-remains-usable"}),
		"a rejected frame must not poison the live session")
	require.NoError(t, peerWriter.Close())
	inbound := client.ReceiveMessages()
	select {
	case _, open := <-inbound:
		assert.False(t, open)
	case <-time.After(time.Second):
		t.Fatal("receive channel did not close after peer EOF")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, client.Disconnect(ctx))
}

func TestStdioMalformedFrameTerminatesSession(t *testing.T) {
	valid, err := json.Marshal(ACPMessage{Type: TypeMessage, SenderID: "peer", Content: "must-not-arrive"})
	require.NoError(t, err)
	input := "not json\n" + string(valid) + "\n"
	client := NewStdioAdapter(strings.NewReader(input), &bytes.Buffer{})
	require.NoError(t, client.Connect(context.Background()))

	select {
	case got, open := <-client.ReceiveMessages():
		assert.False(t, open, "malformed frame must terminate the session before later frames")
		assert.Empty(t, got)
	case <-time.After(time.Second):
		t.Fatal("malformed frame did not terminate the receive session")
	}
}

func TestStdioRejectsNonObjectAndUnknownTypeFrames(t *testing.T) {
	for _, frame := range []string{
		"null\n",
		"{}\n",
		`{"type":"future"}` + "\n",
	} {
		t.Run(strings.TrimSpace(frame), func(t *testing.T) {
			client := NewStdioAdapter(strings.NewReader(frame), &bytes.Buffer{})
			require.NoError(t, client.Connect(context.Background()))
			inbound := client.ReceiveMessages()
			select {
			case _, open := <-inbound:
				assert.False(t, open, "frames without an ACP object and known type must terminate the session")
			case <-time.After(time.Second):
				t.Fatal("invalid ACP envelope did not terminate the session")
			}
			err := client.SendMessage(context.Background(), ACPMessage{Type: TypeMessage, Content: "must-not-send"})
			require.ErrorIs(t, err, errStdioNotConnected)
		})
	}
}

func TestStdioTruncatedFrameAtEOFIsRejected(t *testing.T) {
	client := NewStdioAdapter(strings.NewReader(`{"type":"message"}`), &bytes.Buffer{})
	require.NoError(t, client.Connect(context.Background()))

	select {
	case got, open := <-client.ReceiveMessages():
		assert.False(t, open, "a frame without LF/CRLF terminator is truncated")
		assert.Empty(t, got)
	case <-time.After(time.Second):
		t.Fatal("truncated frame did not terminate the receive session")
	}
}

func TestStdioCallerOwnedBlockedReaderRemainsPendingUntilReadReturns(t *testing.T) {
	reader, peerWriter := io.Pipe()
	started := make(chan struct{})
	wrappedReader := &signalReadStarted{reader: reader, started: started}
	var out bytes.Buffer
	client := NewStdioAdapter(wrappedReader, &out)
	require.NoError(t, client.Connect(context.Background()))
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("reader worker did not enter Read")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err := client.Disconnect(ctx)
	pendingDone := requireStdioPending(t, err, false)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assertChannelOpen(t, pendingDone, "Disconnect cannot claim a caller-owned blocked Read has joined")

	// The adapter must not close caller-owned input. Closing the peer is what
	// makes the real Read return and allows the pending owner to finish.
	require.NoError(t, peerWriter.Close())
	assertChannelCloses(t, pendingDone)
}

func TestStdioOwnedPipeDisconnectClosesAndJoinsWorkers(t *testing.T) {
	clientInput, peerInput := io.Pipe()
	peerOutput, clientOutput := io.Pipe()
	readerStarted := make(chan struct{})
	reader := &signalReadStarted{reader: clientInput, started: readerStarted}
	client := NewStdioAdapter(reader, clientOutput, WithStdioOwnedTransportCloser(closeAll{clientInput, clientOutput}))
	t.Cleanup(func() {
		_ = peerInput.Close()
		_ = peerOutput.Close()
	})

	connectFrame := make(chan []byte, 1)
	go func() {
		line, _ := readLineLen(peerOutput)
		connectFrame <- line
	}()
	require.NoError(t, client.Connect(context.Background()))
	inbound := client.ReceiveMessages()
	select {
	case line := <-connectFrame:
		var msg ACPMessage
		require.NoError(t, json.Unmarshal(line, &msg))
		assert.Equal(t, TypeConnect, msg.Type)
		assert.Equal(t, ACPJSONProfileVersionV1, msg.Version)
	case <-time.After(time.Second):
		t.Fatal("peer did not receive connect frame")
	}
	select {
	case <-readerStarted:
	case <-time.After(time.Second):
		t.Fatal("reader worker did not enter the owned pipe")
	}

	sendCtx, sendCancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	sendResult := make(chan error, 1)
	go func() {
		sendResult <- client.SendMessage(sendCtx, ACPMessage{Type: TypeMessage, Content: "blocked-on-peer"})
	}()
	var sendErr error
	boundedSendReturn := true
	select {
	case sendErr = <-sendResult:
	case <-time.After(250 * time.Millisecond):
		boundedSendReturn = false
	}
	sendCancel()
	var sendDone <-chan struct{}
	if boundedSendReturn {
		sendDone = requireStdioPending(t, sendErr, true)
		assertChannelOpen(t, sendDone, "owned output pipe Write is still blocked before Disconnect closes it")
	}

	disconnectCtx, disconnectCancel := context.WithTimeout(context.Background(), time.Second)
	err := client.Disconnect(disconnectCtx)
	disconnectCancel()
	require.NoError(t, err, "owned pipe endpoints should be closed and their workers joined")
	if !boundedSendReturn {
		select {
		case <-sendResult:
			t.Fatal("SendMessage waited for Disconnect to close its caller context instead of returning pending")
		case <-time.After(250 * time.Millisecond):
			t.Fatal("owned closer did not unblock the pending Write")
		}
	}
	assertChannelCloses(t, sendDone)
	select {
	case _, open := <-inbound:
		assert.False(t, open)
	case <-time.After(time.Second):
		t.Fatal("owned input pipe reader did not exit")
	}
}

func TestStdioOwnedTransportJoinRunsAfterWorkersAndReportsErrors(t *testing.T) {
	clientInput, peerInput := io.Pipe()
	readerReturned := make(chan struct{})
	joinCalled := make(chan bool, 1)
	joinErr := errors.New("process join failed")
	owned := &joinTrackingStdioCloser{
		close: func() error { return clientInput.Close() },
		join: func() error {
			select {
			case <-readerReturned:
				joinCalled <- true
			default:
				joinCalled <- false
			}
			return joinErr
		},
	}
	reader := &returnTrackingReader{reader: clientInput, returned: readerReturned}
	client := NewStdioAdapter(reader, &bytes.Buffer{}, WithStdioOwnedTransportCloser(owned))
	t.Cleanup(func() {
		_ = peerInput.Close()
		_ = clientInput.Close()
	})
	require.NoError(t, client.Connect(context.Background()))

	err := client.Disconnect(context.Background())
	assert.ErrorIs(t, err, joinErr, "the owned process Join error is part of Disconnect completion")
	select {
	case workersHadReturned := <-joinCalled:
		assert.True(t, workersHadReturned, "Join must run after the adapter reader callback has returned")
	case <-time.After(time.Second):
		t.Fatal("owned process Join was not called")
	}
}

func TestStdioBlockingOwnedCloserRemainsTrackedAcrossDisconnectWaiters(t *testing.T) {
	clientInput, peerInput := io.Pipe()
	readerStarted := make(chan struct{})
	closer := &blockingStdioCloser{started: make(chan struct{}), release: make(chan struct{}), next: clientInput}
	client := NewStdioAdapter(&signalReadStarted{reader: clientInput, started: readerStarted}, &bytes.Buffer{}, WithStdioOwnedTransportCloser(closer))
	t.Cleanup(func() {
		closeOnce(closer.release, &closer.releaseOnce)
		_ = peerInput.Close()
		_ = clientInput.Close()
	})
	require.NoError(t, client.Connect(context.Background()))
	select {
	case <-readerStarted:
	case <-time.After(time.Second):
		t.Fatal("reader worker did not enter Read")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	err := client.Disconnect(ctx)
	cancel()
	done := requireStdioPending(t, err, false)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assertChannelOpen(t, done, "Close has not returned, so cleanup is not joined")
	select {
	case <-closer.started:
	case <-time.After(time.Second):
		t.Fatal("owned closer was not invoked by the unique cleanup owner")
	}

	secondCtx, secondCancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	secondErr := client.Disconnect(secondCtx)
	secondCancel()
	secondDone := requireStdioPending(t, secondErr, false)
	assert.True(t, done == secondDone, "repeated Disconnect must await the same cleanup owner")
	assert.Equal(t, 1, closer.callCount(), "one Disconnect owner must invoke the closer once")

	closeOnce(closer.release, &closer.releaseOnce)
	assertChannelCloses(t, done)
}

func TestStdioCallerOwnedBlockedWriteIsPendingUnknownAndNotReplayed(t *testing.T) {
	writer := newBlockingWriteNumber(2)
	reader, peerWriter := io.Pipe()
	t.Cleanup(func() {
		_ = reader.Close()
		_ = peerWriter.Close()
		writer.releaseOnce()
	})
	client := NewStdioAdapter(reader, writer)
	require.NoError(t, client.Connect(context.Background()))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	sendDone := make(chan error, 1)
	go func() {
		sendDone <- client.SendMessage(ctx, ACPMessage{Type: TypeMessage, Content: "once"})
	}()
	select {
	case <-writer.started:
	case <-time.After(time.Second):
		writer.releaseOnce()
		t.Fatal("SendMessage did not enter the blocking Write")
	}

	var sendErr error
	select {
	case sendErr = <-sendDone:
	case <-time.After(250 * time.Millisecond):
		writer.releaseOnce()
		t.Fatal("SendMessage did not return pending when its context expired")
	}
	pendingDone := requireStdioPending(t, sendErr, true)
	assert.ErrorIs(t, sendErr, context.DeadlineExceeded)
	assertChannelOpen(t, pendingDone, "the Write worker has not returned yet")

	callsBefore := writer.callCount()
	preCanceledCtx, preCanceledCancel := context.WithCancel(context.Background())
	preCanceledCancel()
	preCanceledErr := client.SendMessage(preCanceledCtx, ACPMessage{Type: TypeMessage, Content: "not-started"})
	assert.ErrorIs(t, preCanceledErr, context.Canceled)
	var preCanceledPending stdioPending
	assert.False(t, errors.As(preCanceledErr, &preCanceledPending), "a pre-canceled send was not dispatched")
	assert.Equal(t, callsBefore, writer.callCount())

	secondCtx, secondCancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	secondErr := client.SendMessage(secondCtx, ACPMessage{Type: TypeMessage, Content: "no-replay"})
	secondCancel()
	assert.ErrorIs(t, secondErr, context.DeadlineExceeded)
	var secondPending stdioPending
	assert.False(t, errors.As(secondErr, &secondPending) && secondPending.UnknownDelivery(), "a call canceled while waiting for the write gate was not dispatched")
	assert.Equal(t, callsBefore, writer.callCount(), "a waiting call must not pass the blocked write or be replayed")

	writer.releaseOnce()
	assertChannelCloses(t, pendingDone)

	disconnectCtx, disconnectCancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	disconnectErr := client.Disconnect(disconnectCtx)
	disconnectCancel()
	disconnectDone := requireStdioPending(t, disconnectErr, false)
	require.NoError(t, peerWriter.Close())
	assertChannelCloses(t, disconnectDone)
}

func TestStdioDisconnectWithActiveSendReportsUnknownDelivery(t *testing.T) {
	clientInput, peerInput := io.Pipe()
	peerOutput, clientOutput := io.Pipe()
	writer := &signalWriteAt{writer: clientOutput, blockAt: 2, started: make(chan struct{})}
	t.Cleanup(func() {
		_ = clientInput.Close()
		_ = peerInput.Close()
		_ = peerOutput.Close()
		_ = clientOutput.Close()
	})
	client := NewStdioAdapter(clientInput, writer)
	connectFrame := make(chan []byte, 1)
	go func() {
		line, _ := readLineLen(peerOutput)
		connectFrame <- line
	}()
	require.NoError(t, client.Connect(context.Background()))
	select {
	case <-connectFrame:
	case <-time.After(time.Second):
		t.Fatal("peer did not receive the connect frame")
	}

	sendResult := make(chan error, 1)
	go func() {
		sendResult <- client.SendMessage(context.Background(), ACPMessage{Type: TypeMessage, Content: "active-during-disconnect"})
	}()
	select {
	case <-writer.started:
	case <-time.After(time.Second):
		t.Fatal("SendMessage did not enter the blocking Write")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	err := client.Disconnect(ctx)
	cancel()
	done := requireStdioPending(t, err, true)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assertChannelOpen(t, done, "active caller-owned Write still owns cleanup")
	assert.Equal(t, 2, writer.callCount(), "Disconnect must not append a frame while the send owns the writer gate")

	select {
	case sendErr := <-sendResult:
		require.Error(t, sendErr, "the active send is canceled by session cleanup and is not replayed")
		var sendPending stdioPending
		require.ErrorAs(t, sendErr, &sendPending)
		assert.True(t, sendPending.UnknownDelivery())
	case <-time.After(time.Second):
		t.Fatal("active SendMessage did not return pending after Disconnect fenced the session")
	}
	require.NoError(t, clientOutput.Close())
	require.NoError(t, peerInput.Close())
	assertChannelCloses(t, done)
}

func TestStdioPendingDisconnectCancelsQueuedWrite(t *testing.T) {
	var out bytes.Buffer
	session := &stdioSession{
		gate:   make(chan struct{}, 1),
		joined: make(chan struct{}),
	}
	session.gate <- struct{}{}
	result := &stdioWriteResult{
		done:       make(chan struct{}),
		started:    make(chan struct{}),
		ioReturned: make(chan struct{}),
		state:      stdioWriteQueued,
	}
	session.activeWrite = result
	session.workers = 1
	cleanupDone := result.done
	client := &StdioAdapter{out: &out, closing: true}

	err := client.pendingDisconnect(session, context.DeadlineExceeded, cleanupDone)
	pendingDone := requireStdioPending(t, err, false)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assertChannelOpen(t, pendingDone, "the queued worker has not joined yet")

	result.mu.Lock()
	state := result.state
	result.mu.Unlock()
	assert.Equal(t, stdioWriteCanceled, state, "known-not-dispatched must cancel the queued write before returning")

	client.runDisconnectWrite(session, result, ACPMessage{Type: TypeDisconnect})
	assert.Empty(t, out.Bytes(), "the worker must honor cancellation and skip Write")
	assertChannelCloses(t, pendingDone)
}

func TestStdioConcurrentFramesAreSerialized(t *testing.T) {
	writer := newSerializationProbeWriter()
	reader, peerWriter := io.Pipe()
	t.Cleanup(func() {
		_ = reader.Close()
		_ = peerWriter.Close()
		writer.releaseFirstOnce()
	})
	client := NewStdioAdapter(reader, writer)
	require.NoError(t, client.Connect(context.Background()))

	firstDone := make(chan error, 1)
	go func() {
		firstDone <- client.SendMessage(context.Background(), ACPMessage{Type: TypeMessage, Content: "first"})
	}()
	select {
	case <-writer.firstSendStarted:
	case <-time.After(time.Second):
		writer.releaseFirstOnce()
		t.Fatal("first send did not reach writer")
	}

	const concurrent = 12
	var wg sync.WaitGroup
	errCh := make(chan error, concurrent)
	wg.Add(concurrent)
	for i := 0; i < concurrent; i++ {
		go func(i int) {
			defer wg.Done()
			errCh <- client.SendMessage(context.Background(), ACPMessage{Type: TypeMessage, Content: fmt.Sprintf("parallel-%02d", i)})
		}(i)
	}

	// Give overlapping implementations time to enter Write while the first
	// complete frame is deliberately held open.
	time.Sleep(30 * time.Millisecond)
	assert.Equal(t, 2, writer.callCount(), "no second frame may enter while the first write is active")
	assert.False(t, writer.overlapped(), "complete ACP frames must share one writer serialization gate")
	writer.releaseFirstOnce()
	require.NoError(t, <-firstDone)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		require.NoError(t, err)
	}

	lines := bytes.Split(bytes.TrimSuffix(writer.bytes(), []byte("\n")), []byte("\n"))
	require.Len(t, lines, concurrent+2, "connect plus all sent messages must form complete frames")
	seen := map[string]bool{}
	for _, line := range lines {
		var msg ACPMessage
		require.NoError(t, json.Unmarshal(line, &msg), "interleaved output is not a JSON frame: %q", line)
		if msg.Type == TypeMessage {
			seen[msg.Content] = true
		}
	}
	require.Len(t, seen, concurrent+1)

	disconnectCtx, disconnectCancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	disconnectErr := client.Disconnect(disconnectCtx)
	disconnectCancel()
	disconnectDone := requireStdioPending(t, disconnectErr, false)
	require.NoError(t, peerWriter.Close())
	assertChannelCloses(t, disconnectDone)
}

func TestStdioPartialWriteFailureFencesSession(t *testing.T) {
	writer := &partialFailureWriter{}
	reader, peerWriter := io.Pipe()
	t.Cleanup(func() {
		_ = reader.Close()
		_ = peerWriter.Close()
	})
	client := NewStdioAdapter(reader, writer)
	require.NoError(t, client.Connect(context.Background()))

	err := client.SendMessage(context.Background(), ACPMessage{Type: TypeMessage, Content: "partial"})
	require.Error(t, err)
	assert.ErrorIs(t, err, errTestPartialWrite)

	err = client.SendMessage(context.Background(), ACPMessage{Type: TypeMessage, Content: "must-not-follow-partial"})
	assert.ErrorIs(t, err, errStdioNotConnected)
	assert.Equal(t, 2, writer.callCount(), "a later frame must not follow a possibly partial JSON record")
}

type signalReadStarted struct {
	reader  io.Reader
	started chan struct{}
	once    sync.Once
}

type closeAll []io.Closer

func (closers closeAll) Close() error {
	var errs []error
	for _, closer := range closers {
		if err := closer.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

type joinTrackingStdioCloser struct {
	close func() error
	join  func() error
}

func (c *joinTrackingStdioCloser) Close() error { return c.close() }
func (c *joinTrackingStdioCloser) Join() error  { return c.join() }

type returnTrackingReader struct {
	reader   io.Reader
	returned chan struct{}
	once     sync.Once
}

func (r *returnTrackingReader) Read(data []byte) (int, error) {
	n, err := r.reader.Read(data)
	if err != nil {
		r.once.Do(func() { close(r.returned) })
	}
	return n, err
}

type blockingStdioCloser struct {
	started     chan struct{}
	startedOnce sync.Once
	release     chan struct{}
	releaseOnce sync.Once
	mu          sync.Mutex
	calls       int
	next        io.Closer
}

var errTestPartialWrite = errors.New("partial transport failure")

type partialFailureWriter struct {
	mu    sync.Mutex
	calls int
}

func (w *partialFailureWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls++
	if w.calls == 2 {
		return 1, errTestPartialWrite
	}
	return len(data), nil
}

func (w *partialFailureWriter) callCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls
}

func (c *blockingStdioCloser) Close() error {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	c.startedOnce.Do(func() { close(c.started) })
	<-c.release
	return c.next.Close()
}

func (c *blockingStdioCloser) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func closeOnce(ch chan struct{}, once *sync.Once) { once.Do(func() { close(ch) }) }

func (r *signalReadStarted) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	return r.reader.Read(p)
}

type blockingWriteNumber struct {
	mu               sync.Mutex
	calls            int
	blockAt          int
	started          chan struct{}
	release          chan struct{}
	releaseOnceValue sync.Once
	output           bytes.Buffer
}

type signalWriteAt struct {
	mu      sync.Mutex
	writer  io.Writer
	blockAt int
	started chan struct{}
	calls   int
}

func (w *signalWriteAt) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.calls++
	call := w.calls
	if call == w.blockAt {
		close(w.started)
	}
	w.mu.Unlock()
	return w.writer.Write(p)
}

func (w *signalWriteAt) callCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls
}

func newBlockingWriteNumber(blockAt int) *blockingWriteNumber {
	return &blockingWriteNumber{
		blockAt: blockAt,
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (w *blockingWriteNumber) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.calls++
	call := w.calls
	w.mu.Unlock()
	if call == w.blockAt {
		close(w.started)
		<-w.release
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.output.Write(p)
}

func (w *blockingWriteNumber) releaseOnce() { w.releaseOnceValue.Do(func() { close(w.release) }) }

func (w *blockingWriteNumber) callCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls
}

type serializationProbeWriter struct {
	mu               sync.Mutex
	calls            int
	active           int
	overlappedWrite  bool
	firstSendStarted chan struct{}
	releaseFirst     chan struct{}
	releaseOnceValue sync.Once
	output           bytes.Buffer
}

func newSerializationProbeWriter() *serializationProbeWriter {
	return &serializationProbeWriter{
		firstSendStarted: make(chan struct{}),
		releaseFirst:     make(chan struct{}),
	}
}

func (w *serializationProbeWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.calls++
	call := w.calls
	w.active++
	if w.active > 1 {
		w.overlappedWrite = true
	}
	w.mu.Unlock()
	if call == 2 {
		close(w.firstSendStarted)
		<-w.releaseFirst
	}
	for _, b := range p {
		time.Sleep(time.Microsecond)
		w.mu.Lock()
		_ = w.output.WriteByte(b)
		w.mu.Unlock()
	}
	w.mu.Lock()
	w.active--
	w.mu.Unlock()
	return len(p), nil
}

func (w *serializationProbeWriter) releaseFirstOnce() {
	w.releaseOnceValue.Do(func() { close(w.releaseFirst) })
}

func (w *serializationProbeWriter) callCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls
}

func (w *serializationProbeWriter) overlapped() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.overlappedWrite
}

func (w *serializationProbeWriter) bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.output.Bytes()...)
}

func requireStdioPending(t *testing.T, err error, unknownDelivery bool) <-chan struct{} {
	t.Helper()
	var pending stdioPending
	require.ErrorAs(t, err, &pending, "operation must expose its actual pending worker")
	assert.Equal(t, unknownDelivery, pending.UnknownDelivery())
	require.NotNil(t, pending.Done())
	return pending.Done()
}

func assertChannelOpen(t *testing.T, ch <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-ch:
		t.Fatal(message)
	default:
	}
}

func assertChannelCloses(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("pending operation Done did not close after the underlying I/O returned")
	}
}
