package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

const (
	stdioMaxFrameBytes   = 1 << 20
	stdioReadBufferBytes = 32 << 10
	stdioDisconnectGrace = 100 * time.Millisecond
	stdioDisconnectWait  = 250 * time.Millisecond
)

var (
	errStdioNotConnected        = errors.New("acp: stdio adapter not connected")
	errStdioClosing             = errors.New("acp: stdio adapter is closing")
	errStdioClosed              = errors.New("acp: stdio adapter is closed")
	errStdioMissingInput        = errors.New("acp: stdio reader is required")
	errStdioMissingOutput       = errors.New("acp: stdio writer is required")
	errStdioMalformed           = errors.New("acp: malformed stdio frame")
	errStdioTooLarge            = errors.New("acp: stdio frame exceeds size limit")
	errStdioReaderPanic   error = StdioReaderPanicError{}
	errStdioCloserPanic   error = StdioCloserPanicError{}
	errStdioWriterPanic   error = StdioWritePanicError{}
)

// StdioPendingError reports that a stdio operation has not completed its
// tracked cleanup by the time the caller stopped waiting. Done closes only
// after the underlying callback and its tracked worker have unwound.
// UnknownDelivery is true only when a write had started before it became
// pending, since the peer may have received some or all of that frame.
type StdioPendingError struct {
	operation string
	cause     error
	done      <-chan struct{}
	unknown   bool
}

func (e *StdioPendingError) Error() string {
	if e.cause == nil {
		return "acp: stdio operation pending"
	}
	return fmt.Sprintf("acp: stdio %s pending: %v", e.operation, e.cause)
}

func (e *StdioPendingError) Unwrap() error { return e.cause }

// Done is closed when the actual blocked operation or close callback has
// returned or panicked and its tracked worker has unwound. It does not prove
// that a panicking closer released the underlying resource; Disconnect returns
// the recorded terminal error after join.
func (e *StdioPendingError) Done() <-chan struct{} { return e.done }

// UnknownDelivery reports whether a started write may have reached the peer.
func (e *StdioPendingError) UnknownDelivery() bool { return e.unknown }

// StdioReaderPanicError reports that a recovered reader panic terminated the
// receive stream.
type StdioReaderPanicError struct{}

func (StdioReaderPanicError) Error() string { return "acp: stdio reader panicked" }

// StdioCloserPanicError reports that the owned transport closer panicked. The
// callback unwound, but the error does not establish that its resource closed.
type StdioCloserPanicError struct{}

func (StdioCloserPanicError) Error() string { return "acp: stdio transport closer panicked" }

// StdioWritePanicError reports a recovered Writer panic after dispatch began.
// The writer callback has unwound, but the peer may have received some or all
// of the frame, so delivery remains unknown.
type StdioWritePanicError struct{}

func (StdioWritePanicError) Error() string {
	return "acp: stdio writer panicked after dispatch; delivery unknown"
}

// UnknownDelivery reports that a panicking writer may have partially delivered
// the frame before it unwound.
func (StdioWritePanicError) UnknownDelivery() bool { return true }

type stdioSession struct {
	stop         chan struct{}
	inbound      chan ACPMessage
	gate         chan struct{}
	receiveReady bool              // guarded by StdioAdapter.mu
	activeWrite  *stdioWriteResult // guarded by StdioAdapter.mu

	workers int // guarded by StdioAdapter.mu
	joined  chan struct{}
}

type stdioWriteResult struct {
	mu         sync.Mutex
	done       chan struct{}
	started    chan struct{}
	ioReturned chan struct{}
	state      stdioWriteState
	wasStarted bool  // published before started or done closes
	err        error // published before done closes
}

type stdioWriteState uint8

const (
	stdioWriteQueued stdioWriteState = iota
	stdioWriteStarted
	stdioWriteCanceled
)

// StdioAdapter implements ACPClient over a caller-supplied io.Reader/io.Writer
// pair using bounded newline-delimited JSON frames.
type StdioAdapter struct {
	name      string
	transport ACPTransport
	in        *bufio.Reader
	out       io.Writer
	owned     io.Closer

	mu              sync.Mutex
	session         *stdioSession
	connecting      bool
	connected       bool
	receiveDisabled bool
	closing         bool
	closed          bool
	ownedCloseErr   error
	terminalErr     error
	cleanupDone     chan struct{}
}

var _ ACPClient = (*StdioAdapter)(nil)

// NewStdioAdapter returns an ACPClient that reads bounded newline-delimited
// JSON frames from r and writes complete frames to w.
func NewStdioAdapter(r io.Reader, w io.Writer, opts ...Option) ACPClient {
	resolved := adapterOptions{name: "stdio"}
	for _, opt := range opts {
		if opt != nil {
			opt(&resolved)
		}
	}
	var bufferedInput *bufio.Reader
	if r != nil {
		bufferedInput = bufio.NewReaderSize(r, stdioReadBufferBytes)
	}
	return &StdioAdapter{
		name:      resolved.name,
		transport: ACPTransportStdio,
		in:        bufferedInput,
		out:       w,
		owned:     resolved.stdioOwnedTransportCloser,
	}
}

// Connect establishes a stdio session, emits a profile-declaring connect
// frame, and starts the single bounded reader worker.
func (s *StdioAdapter) Connect(ctx context.Context) error {
	span, spanCtx := tracing.SpanFromContext(ctx, "acp.connect", tracing.SpanKindInternal)
	logger := tracing.NewTraceLogger(span, slog.Default())
	span.SetAttributes(
		tracing.Attribute{Key: "transport", Value: s.transport.String()},
		tracing.Attribute{Key: "endpoint", Value: "stdio://stdin-stdout"},
	)
	defer span.End()
	if s.in == nil {
		span.SetStatus(tracing.SpanStatusError, errStdioMissingInput.Error())
		return errStdioMissingInput
	}
	if s.out == nil {
		span.SetStatus(tracing.SpanStatusError, errStdioMissingOutput.Error())
		return errStdioMissingOutput
	}

	s.mu.Lock()
	switch {
	case s.connected:
		s.mu.Unlock()
		span.SetStatus(tracing.SpanStatusOK, "")
		return nil
	case s.closing:
		done := s.cleanupDone
		s.mu.Unlock()
		return s.pendingFromChannel("connect", errStdioClosing, done, false)
	case s.closed:
		s.mu.Unlock()
		return errStdioClosed
	case s.connecting:
		s.mu.Unlock()
		return errors.New("acp: stdio connect already in progress")
	}
	s.connecting = true
	session := &stdioSession{
		stop:    make(chan struct{}),
		inbound: make(chan ACPMessage, 16),
		gate:    make(chan struct{}, 1),
		joined:  make(chan struct{}),
	}
	s.session = session
	s.mu.Unlock()

	connectMsg := ACPMessage{
		Version:   ACPJSONProfileVersionV1,
		Type:      TypeConnect,
		SenderID:  s.name,
		Timestamp: time.Now(),
	}
	write, err := s.writeFrame(ctx, session, connectMsg, true)
	if err != nil {
		s.mu.Lock()
		if !write.started && !s.closing {
			s.connecting = false
			s.session = nil
			s.mu.Unlock()
		} else {
			s.mu.Unlock()
			s.startCleanup(session, false)
		}
		span.SetStatus(tracing.SpanStatusError, err.Error())
		logger.Error("acp.connect.failed", "transport", s.transport.String(), "err", err)
		return fmt.Errorf("acp: stdio connect: %w", err)
	}

	s.mu.Lock()
	if s.closing || s.session != session {
		s.connecting = false
		s.mu.Unlock()
		span.SetStatus(tracing.SpanStatusError, errStdioClosing.Error())
		return errStdioClosing
	}
	s.connecting = false
	s.connected = true
	session.receiveReady = true
	s.addWorkerLocked(session)
	s.mu.Unlock()
	go s.readLoop(context.WithoutCancel(spanCtx), session)

	span.SetStatus(tracing.SpanStatusOK, "")
	logger.Info("acp.connect", "transport", s.transport.String(), "endpoint", "stdio://stdin-stdout")
	return nil
}

// Disconnect fences future sends, performs a best-effort serialized disconnect
// frame when no write is active, and waits only for real cleanup completion.
// Terminal reader/writer panics and owned closer errors are returned after the
// corresponding callback has unwound and tracked workers have joined.
func (s *StdioAdapter) Disconnect(ctx context.Context) error {
	s.mu.Lock()
	session := s.session
	connected := s.connected
	if session == nil {
		s.mu.Unlock()
		return nil
	}
	s.receiveDisabled = true
	s.mu.Unlock()

	if err := ctx.Err(); err != nil {
		done := s.startCleanup(session, false)
		return s.pendingDisconnect(session, err, done)
	}
	done := s.startCleanup(session, connected)
	waitCtx := ctx
	var cancel context.CancelFunc
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		waitCtx, cancel = context.WithTimeout(ctx, stdioDisconnectWait)
		defer cancel()
	}
	select {
	case <-done:
		s.mu.Lock()
		closeErr := s.ownedCloseErr
		terminalErr := s.terminalErr
		s.mu.Unlock()
		return errors.Join(terminalErr, closeErr)
	case <-waitCtx.Done():
		return s.pendingDisconnect(session, waitCtx.Err(), done)
	}
}

// SendMessage writes one serialized JSON frame. A canceled call waiting for
// the writer gate is known not dispatched; cancellation after Write starts is
// reported as pending with unknown delivery and is never replayed.
func (s *StdioAdapter) SendMessage(ctx context.Context, msg ACPMessage) error {
	span, spanCtx := tracing.SpanFromContext(ctx, "acp.send", tracing.SpanKindInternal)
	logger := tracing.NewTraceLogger(span, slog.Default())
	span.SetAttributes(
		tracing.Attribute{Key: "message_type", Value: msg.Type},
		tracing.Attribute{Key: "receiver_id", Value: msg.ReceiverID},
	)
	defer span.End()

	s.mu.Lock()
	session := s.session
	connected := s.connected && !s.closing
	s.mu.Unlock()
	if !connected || session == nil {
		err := fmt.Errorf("%w: %s", errStdioNotConnected, s.name)
		span.SetStatus(tracing.SpanStatusError, err.Error())
		return err
	}
	if msg.Timestamp.IsZero() {
		msg.Timestamp = time.Now()
	}
	_, err := s.writeFrame(ctx, session, msg, false)
	if err != nil {
		span.SetStatus(tracing.SpanStatusError, err.Error())
		logger.Error("acp.send.failed", "message_type", msg.Type, "receiver_id", msg.ReceiverID, "err", err)
		return fmt.Errorf("acp: stdio send: %w", err)
	}
	span.SetStatus(tracing.SpanStatusOK, "")
	logger.Info("acp.send", "message_type", msg.Type, "receiver_id", msg.ReceiverID)
	slog.DebugContext(spanCtx, "acp.send.ok", "message_type", msg.Type, "receiver_id", msg.ReceiverID)
	return nil
}

// ReceiveMessages returns the current session's inbound channel while the
// session is connected. Callers that need to observe final closure should keep
// the channel returned before Disconnect.
func (s *StdioAdapter) ReceiveMessages() <-chan ACPMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.session == nil || !s.session.receiveReady || s.receiveDisabled {
		return nil
	}
	return s.session.inbound
}

func (s *StdioAdapter) Name() string { return s.name }

type stdioFrameWrite struct {
	started bool
}

func (s *StdioAdapter) writeFrame(ctx context.Context, session *stdioSession, msg ACPMessage, connecting bool) (stdioFrameWrite, error) {
	if err := ctx.Err(); err != nil {
		return stdioFrameWrite{}, err
	}
	frame, err := marshalFrame(msg)
	if err != nil {
		return stdioFrameWrite{}, err
	}
	select {
	case session.gate <- struct{}{}:
	case <-session.stop:
		return stdioFrameWrite{}, errStdioClosing
	case <-ctx.Done():
		return stdioFrameWrite{}, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		<-session.gate
		return stdioFrameWrite{}, err
	}

	s.mu.Lock()
	allowed := s.session == session && !s.closing && !s.closed && (s.connected || connecting && s.connecting)
	if !allowed {
		s.mu.Unlock()
		<-session.gate
		return stdioFrameWrite{}, errStdioNotConnected
	}
	s.addWorkerLocked(session)
	s.mu.Unlock()

	result := &stdioWriteResult{
		done:       make(chan struct{}),
		started:    make(chan struct{}),
		ioReturned: make(chan struct{}),
	}
	go func() {
		s.mu.Lock()
		stillAllowed := s.session == session && !s.closing && !s.closed && (s.connected || connecting && s.connecting)
		result.mu.Lock()
		switch {
		case result.state == stdioWriteCanceled:
			result.err = ctx.Err()
			if result.err == nil {
				result.err = errStdioClosing
			}
		case ctx.Err() != nil:
			result.state = stdioWriteCanceled
			result.err = ctx.Err()
		case !stillAllowed:
			result.state = stdioWriteCanceled
			result.err = errStdioClosing
		default:
			result.state = stdioWriteStarted
			result.wasStarted = true
			session.activeWrite = result
			close(result.started)
		}
		started := result.state == stdioWriteStarted
		result.mu.Unlock()
		s.mu.Unlock()
		if started {
			result.err = writeFrameBytes(s.out, frame)
			if stdioUnknownDelivery(result.err) {
				s.rememberTerminalError(result.err)
			}
			close(result.ioReturned)
			s.mu.Lock()
			if session.activeWrite == result {
				session.activeWrite = nil
			}
			s.mu.Unlock()
			if result.err != nil {
				// A failed or short frame can leave the byte stream at an
				// unknown boundary. Fence this session instead of appending
				// later frames after a partial JSON record.
				s.startCleanup(session, false)
			}
		}
		<-session.gate
		s.workerDone(session)
		close(result.done)
	}()

	select {
	case <-result.started:
		return s.awaitStartedWrite(ctx, session, result)
	case <-result.done:
		return stdioFrameWrite{started: result.wasStarted}, result.err
	case <-ctx.Done():
		select {
		case <-result.done:
			return stdioFrameWrite{started: result.wasStarted}, result.err
		default:
		}
		if s.markWriteCanceledBeforeStart(result) {
			return s.awaitStartedWrite(ctx, session, result)
		}
		return stdioFrameWrite{}, ctx.Err()
	case <-session.stop:
		select {
		case <-result.done:
			return stdioFrameWrite{started: result.wasStarted}, result.err
		default:
		}
		if s.markWriteCanceledBeforeStart(result) {
			return s.awaitStartedWrite(ctx, session, result)
		}
		return stdioFrameWrite{}, errStdioClosing
	}
}

// markWriteCanceledBeforeStart arbitrates cancellation against the worker's
// dispatch commitment. A caller that wins while queued guarantees Write will
// not be invoked; once the worker commits, delivery is treated as unknown.
func (s *StdioAdapter) markWriteCanceledBeforeStart(result *stdioWriteResult) bool {
	result.mu.Lock()
	defer result.mu.Unlock()
	if result.state == stdioWriteStarted {
		return true
	}
	if result.state == stdioWriteQueued {
		result.state = stdioWriteCanceled
	}
	return false
}

func (s *StdioAdapter) awaitStartedWrite(ctx context.Context, session *stdioSession, result *stdioWriteResult) (stdioFrameWrite, error) {
	select {
	case <-result.done:
		return stdioFrameWrite{started: true}, result.err
	case <-ctx.Done():
		select {
		case <-result.done:
			return stdioFrameWrite{started: true}, result.err
		default:
		}
		cause := ctx.Err()
		select {
		case <-result.ioReturned:
			cause = errors.Join(cause, result.err)
		default:
		}
		return stdioFrameWrite{started: true}, &StdioPendingError{
			operation: "write",
			cause:     cause,
			done:      result.done,
			unknown:   true,
		}
	case <-session.stop:
		select {
		case <-result.done:
			return stdioFrameWrite{started: true}, result.err
		default:
		}
		cause := error(errStdioClosing)
		select {
		case <-result.ioReturned:
			cause = errors.Join(cause, result.err)
		default:
		}
		return stdioFrameWrite{started: true}, &StdioPendingError{
			operation: "write",
			cause:     cause,
			done:      result.done,
			unknown:   true,
		}
	}
}

func (s *StdioAdapter) readLoop(ctx context.Context, session *stdioSession) {
	defer func() {
		close(session.inbound)
		s.workerDone(session)
	}()
	for {
		frame, err := readBoundedFrame(s.in)
		if err != nil {
			if errors.Is(err, errStdioReaderPanic) {
				s.startCleanup(session, false)
				s.rememberTerminalError(err)
				slog.Warn("acp.receive.read", "name", s.name, "err", err)
				return
			}
			if errors.Is(err, io.EOF) {
				// EOF is a terminal transport event. Close the receive channel
				// and fence sends so the adapter cannot write into a dead session.
				s.startCleanup(session, false)
				return
			}
			slog.Warn("acp.receive.read", "name", s.name, "err", err)
			s.startCleanup(session, false)
			return
		}
		if !utf8.Valid(frame) {
			s.startCleanup(session, false)
			return
		}
		var msg ACPMessage
		if err := json.Unmarshal(frame, &msg); err != nil {
			slog.Warn("acp.receive.decode", "name", s.name, "err", err)
			s.startCleanup(session, false)
			return
		}
		if !isKnownACPMessageType(msg.Type) {
			slog.Warn("acp.receive.decode", "name", s.name, "err", errStdioMalformed)
			s.startCleanup(session, false)
			return
		}
		debugMessage(ctx, "acp.receive", msg)
		select {
		case session.inbound <- msg:
		case <-session.stop:
			return
		case <-ctx.Done():
			return
		}
	}
}

// readBoundedFrame reads one LF-terminated frame. The configured limit counts
// payload bytes only, excluding LF and an optional CR immediately before it.
func readBoundedFrame(reader *bufio.Reader) (frame []byte, err error) {
	defer func() {
		if recover() != nil {
			frame = nil
			err = errStdioReaderPanic
		}
	}()

	for {
		fragment, err := reader.ReadSlice('\n')
		if len(frame)+len(fragment) > stdioMaxFrameBytes+2 {
			return nil, errStdioTooLarge
		}
		frame = append(frame, fragment...)
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			if errors.Is(err, io.EOF) && len(frame) == 0 {
				return nil, io.EOF
			}
			if errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("%w: unterminated frame", errStdioMalformed)
			}
			return nil, fmt.Errorf("acp: read stdio frame: %w", err)
		}
		if len(frame) == 0 || frame[len(frame)-1] != '\n' {
			return nil, fmt.Errorf("%w: missing LF", errStdioMalformed)
		}
		frame = frame[:len(frame)-1]
		if len(frame) > 0 && frame[len(frame)-1] == '\r' {
			frame = frame[:len(frame)-1]
		}
		if len(frame) > stdioMaxFrameBytes {
			return nil, errStdioTooLarge
		}
		return frame, nil
	}
}

func (s *StdioAdapter) startCleanup(session *stdioSession, sendDisconnect bool) <-chan struct{} {
	s.mu.Lock()
	if s.cleanupDone != nil {
		done := s.cleanupDone
		s.mu.Unlock()
		return done
	}
	cleanupDone := make(chan struct{})
	s.cleanupDone = cleanupDone
	s.closing = true
	s.connected = false
	s.connecting = false
	close(session.stop)

	var disconnectWrite *stdioWriteResult
	if sendDisconnect && s.session == session && !s.closed {
		select {
		case session.gate <- struct{}{}:
			s.addWorkerLocked(session)
			disconnectWrite = &stdioWriteResult{
				done:       make(chan struct{}),
				started:    make(chan struct{}),
				ioReturned: make(chan struct{}),
			}
			session.activeWrite = disconnectWrite
		default:
		}
	}
	if session.workers == 0 {
		close(session.joined)
	}
	owned := s.owned
	s.mu.Unlock()

	if disconnectWrite != nil {
		msg := ACPMessage{Type: TypeDisconnect, SenderID: s.name, Timestamp: time.Now()}
		go s.runDisconnectWrite(session, disconnectWrite, msg)
	}

	go func() {
		if disconnectWrite != nil {
			timer := time.NewTimer(stdioDisconnectGrace)
			select {
			case <-disconnectWrite.done:
				if !timer.Stop() {
					<-timer.C
				}
			case <-timer.C:
			case <-session.joined:
			}
		}

		closerDone := make(chan struct{})
		if owned == nil {
			close(closerDone)
		} else {
			go func() {
				closeErr := closeOwnedTransport(owned)
				<-session.joined
				var joinErr error
				if joiner, ok := owned.(interface{ Join() error }); ok {
					joinErr = joiner.Join()
				}
				s.mu.Lock()
				s.ownedCloseErr = errors.Join(closeErr, joinErr)
				s.mu.Unlock()
				close(closerDone)
			}()
		}
		<-closerDone
		<-session.joined
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		close(cleanupDone)
	}()
	return cleanupDone
}

func (s *StdioAdapter) runDisconnectWrite(session *stdioSession, result *stdioWriteResult, msg ACPMessage) {
	defer func() {
		<-session.gate
		s.workerDone(session)
		close(result.done)
	}()

	frame, err := marshalFrame(msg)
	if err != nil {
		s.mu.Lock()
		result.mu.Lock()
		result.err = err
		if result.state == stdioWriteQueued {
			result.state = stdioWriteCanceled
		}
		result.mu.Unlock()
		if session.activeWrite == result {
			session.activeWrite = nil
		}
		s.mu.Unlock()
		return
	}

	s.mu.Lock()
	result.mu.Lock()
	started := result.state == stdioWriteQueued
	if started {
		result.state = stdioWriteStarted
		result.wasStarted = true
		close(result.started)
	}
	result.mu.Unlock()
	s.mu.Unlock()

	if started {
		result.err = writeFrameBytes(s.out, frame)
		if stdioUnknownDelivery(result.err) {
			s.rememberTerminalError(result.err)
		}
		close(result.ioReturned)
	}
	s.mu.Lock()
	if session.activeWrite == result {
		session.activeWrite = nil
	}
	s.mu.Unlock()
}

func (s *StdioAdapter) addWorkerLocked(session *stdioSession) {
	session.workers++
}

func (s *StdioAdapter) workerDone(session *stdioSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session.workers--
	if session.workers == 0 && s.closing {
		select {
		case <-session.joined:
		default:
			close(session.joined)
		}
	}
}

func (s *StdioAdapter) pendingFromChannel(operation string, cause error, done <-chan struct{}, unknown bool) error {
	if done == nil {
		done = make(chan struct{})
	}
	select {
	case <-done:
		return cause
	default:
	}
	return &StdioPendingError{operation: operation, cause: cause, done: done, unknown: unknown}
}

func (s *StdioAdapter) pendingDisconnect(session *stdioSession, cause error, done <-chan struct{}) error {
	if done == nil {
		done = make(chan struct{})
	}
	select {
	case <-done:
		return cause
	default:
	}
	unknown := s.cancelQueuedOrCheckWriteInFlight(session)
	return s.pendingFromChannel("disconnect", cause, done, unknown)
}

func (s *StdioAdapter) rememberTerminalError(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	s.terminalErr = errors.Join(s.terminalErr, err)
	s.mu.Unlock()
}

func stdioUnknownDelivery(err error) bool {
	var unknown interface{ UnknownDelivery() bool }
	return errors.As(err, &unknown) && unknown.UnknownDelivery()
}

// cancelQueuedOrCheckWriteInFlight arbitrates a pending Disconnect against the
// writer worker. A queued frame is canceled before reporting known-not-sent;
// once the worker commits the write, delivery is unknown until Write returns.
func (s *StdioAdapter) cancelQueuedOrCheckWriteInFlight(session *stdioSession) bool {
	s.mu.Lock()
	active := session.activeWrite
	unknown := stdioUnknownDelivery(s.terminalErr)
	if active == nil {
		s.mu.Unlock()
		return unknown
	}
	active.mu.Lock()
	switch active.state {
	case stdioWriteQueued:
		active.state = stdioWriteCanceled
	case stdioWriteStarted:
		if !unknown {
			select {
			case <-active.ioReturned:
			default:
				unknown = true
			}
		}
	}
	active.mu.Unlock()
	s.mu.Unlock()
	return unknown
}

// marshalFrame encodes one complete outbound frame and applies the same
// payload-byte limit as the inbound parser before any transport write begins.
func marshalFrame(msg ACPMessage) ([]byte, error) {
	data, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("acp: marshal message: %w", err)
	}
	if len(data) > stdioMaxFrameBytes {
		return nil, errStdioTooLarge
	}
	return append(data, '\n'), nil
}

func closeOwnedTransport(closer io.Closer) (err error) {
	defer func() {
		if recover() != nil {
			err = errStdioCloserPanic
		}
	}()
	return closer.Close()
}

func writeFrameBytes(w io.Writer, frame []byte) (err error) {
	defer func() {
		if recover() != nil {
			err = errStdioWriterPanic
		}
	}()
	n, err := w.Write(frame)
	if err != nil {
		return fmt.Errorf("acp: write message: %w", err)
	}
	if n != len(frame) {
		return fmt.Errorf("acp: write message: %w", io.ErrShortWrite)
	}
	return nil
}

func isKnownACPMessageType(messageType string) bool {
	switch messageType {
	case TypeConnect, TypeDisconnect, TypeMessage, TypeResponse, TypeAck, TypeError, TypeRPC:
		return true
	default:
		return false
	}
}
