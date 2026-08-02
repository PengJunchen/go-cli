package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// StdioAdapter implements ACPClient over an io.Reader/io.Writer pair
// (typically stdin/stdout). It exchanges newline-delimited JSON messages
// (JSON-RPC style) with a peer, which is what makes the protocol testable
// in-process.
type StdioAdapter struct {
	name      string
	transport ACPTransport
	in        *bufio.Reader
	out       io.Writer

	mu        sync.Mutex
	connected bool
	done      chan struct{}
	inbound   chan ACPMessage
}

// Compile-time assertion that StdioAdapter satisfies ACPClient.
var _ ACPClient = (*StdioAdapter)(nil)

// NewStdioAdapter returns an ACPClient that reads frames from r and writes
// frames to w as newline-delimited JSON.
func NewStdioAdapter(r io.Reader, w io.Writer, opts ...Option) ACPClient {
	return &StdioAdapter{
		name:      resolveName("stdio", opts),
		transport: ACPTransportStdio,
		in:        bufio.NewReader(r),
		out:       w,
	}
}

// Connect establishes the stdio session, emitting an acp.connect span with
// transport/endpoint attributes. It announces the session to the peer and
// starts the receiver goroutine.
func (s *StdioAdapter) Connect(ctx context.Context) error {
	span, spanCtx := tracing.SpanFromContext(ctx, "acp.connect", tracing.SpanKindInternal)
	logger := tracing.NewTraceLogger(span, slog.Default())
	span.SetAttributes(
		tracing.Attribute{Key: "transport", Value: s.transport.String()},
		tracing.Attribute{Key: "endpoint", Value: "stdio://stdin-stdout"},
	)
	defer span.End()

	s.mu.Lock()
	if s.connected {
		s.mu.Unlock()
		span.SetStatus(tracing.SpanStatusOK, "")
		return nil
	}
	done := make(chan struct{})
	inbound := make(chan ACPMessage, 16)
	s.done = done
	s.inbound = inbound
	s.mu.Unlock()

	connectMsg := ACPMessage{Type: TypeConnect, SenderID: s.name, Timestamp: time.Now()}
	if err := s.writeMessage(connectMsg); err != nil {
		s.mu.Lock()
		s.connected = false
		s.mu.Unlock()
		span.SetStatus(tracing.SpanStatusError, err.Error())
		logger.Error("acp.connect.failed", "transport", s.transport.String(), "err", err)
		return fmt.Errorf("acp: stdio connect: %w", err)
	}

	s.mu.Lock()
	s.connected = true
	s.mu.Unlock()
	go s.readLoop(spanCtx, done, inbound)

	span.SetStatus(tracing.SpanStatusOK, "")
	logger.Info("acp.connect", "transport", s.transport.String(), "endpoint", "stdio://stdin-stdout")
	return nil
}

// Disconnect tears down the stdio session and stops the receiver goroutine.
// A disconnect frame is written to the peer (best-effort) so the peer can
// close its side and unblock the in-progress read.
func (s *StdioAdapter) Disconnect(ctx context.Context) error {
	s.mu.Lock()
	if !s.connected {
		s.mu.Unlock()
		return nil
	}
	s.connected = false
	close(s.done)
	out := s.out
	s.mu.Unlock()

	disconnectMsg := ACPMessage{Type: TypeDisconnect, SenderID: s.name, Timestamp: time.Now()}
	_ = writeLine(out, disconnectMsg) //nolint:errcheck // best-effort disconnect notify

	// Wait briefly for the receiver goroutine to observe the session teardown.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(20 * time.Millisecond):
	}
	return nil
}

// SendMessage delivers an ACP message to the peer as a newline-delimited JSON
// frame, emitting an acp.send span with message_type/receiver_id attributes.
func (s *StdioAdapter) SendMessage(ctx context.Context, msg ACPMessage) error {
	span, spanCtx := tracing.SpanFromContext(ctx, "acp.send", tracing.SpanKindInternal)
	logger := tracing.NewTraceLogger(span, slog.Default())
	span.SetAttributes(
		tracing.Attribute{Key: "message_type", Value: msg.Type},
		tracing.Attribute{Key: "receiver_id", Value: msg.ReceiverID},
	)
	defer span.End()

	s.mu.Lock()
	connected := s.connected
	out := s.out
	s.mu.Unlock()
	if !connected {
		err := fmt.Errorf("acp: %s adapter not connected", s.name)
		span.SetStatus(tracing.SpanStatusError, err.Error())
		return err
	}

	if msg.Timestamp.IsZero() {
		msg.Timestamp = time.Now()
	}
	if err := writeLine(out, msg); err != nil {
		span.SetStatus(tracing.SpanStatusError, err.Error())
		logger.Error("acp.send.failed", "message_type", msg.Type, "receiver_id", msg.ReceiverID, "err", err)
		return fmt.Errorf("acp: stdio send: %w", err)
	}

	span.SetStatus(tracing.SpanStatusOK, "")
	logger.Info("acp.send", "message_type", msg.Type, "receiver_id", msg.ReceiverID)
	slog.DebugContext(spanCtx, "acp.send.ok", "message_type", msg.Type, "receiver_id", msg.ReceiverID)
	return nil
}

// ReceiveMessages returns a channel of messages read from the peer. It is
// closed when the receiver goroutine exits.
func (s *StdioAdapter) ReceiveMessages() <-chan ACPMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.connected || s.inbound == nil {
		return nil
	}
	return s.inbound
}

// Name returns the logical client identity.
func (s *StdioAdapter) Name() string { return s.name }

// writeMessage writes an ACPMessage to the underlying writer.
func (s *StdioAdapter) writeMessage(msg ACPMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return writeLine(s.out, msg)
}

// writeLine marshals an ACPMessage and writes it as a JSON line.
func writeLine(w io.Writer, msg ACPMessage) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("acp: marshal message: %w", err)
	}
	if _, err := fmt.Fprintln(w, string(data)); err != nil {
		return fmt.Errorf("acp: write message: %w", err)
	}
	return nil
}

// readLoop reads newline-delimited JSON frames from the reader and forwards
// parsed ACP messages onto the inbound channel. It exits on session teardown,
// context cancellation, or an end-of-stream on the reader.
func (s *StdioAdapter) readLoop(ctx context.Context, done chan struct{}, inbound chan ACPMessage) {
	defer close(inbound)
	for {
		line, err := s.readLine(ctx, done)
		if err != nil {
			if err != io.EOF && err != io.ErrClosedPipe {
				slog.Warn("acp.receive.read", "name", s.name, "err", err)
			}
			return
		}
		var msg ACPMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
		debugMessage(ctx, "acp.receive", msg)
		select {
		case inbound <- msg:
		case <-done:
			return
		case <-ctx.Done():
			return
		}
	}
}

// readLine reads a single newline-delimited line, honoring session teardown
// and context cancellation where the underlying reader is a pipe.
func (s *StdioAdapter) readLine(ctx context.Context, done chan struct{}) ([]byte, error) {
	type result struct {
		line []byte
		err  error
	}
	resCh := make(chan result, 1)
	go func() {
		line, err := s.in.ReadBytes('\n')
		resCh <- result{line: line, err: err}
	}()

	select {
	case <-done:
		return nil, io.ErrClosedPipe
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-resCh:
		return res.line, res.err
	}
}
