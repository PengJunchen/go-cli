package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// closeQuietly best-effort closes an io.Closer.
func closeQuietly(c io.Closer) { _ = c.Close() } //nolint:errcheck // best-effort close

// gRPCAdapter implements ACPClient over a "gRPC" transport.
//
// IMPORTANT: This is an stdlib JSON-over-HTTP interpretation of the ACP gRPC
// contract. The project has zero external dependencies (stdlib only), so there
// is no real gRPC stack. Instead the adapter dials an HTTP endpoint and
// exchanges JSON messages over well-known routes that mirror the ACP
// service/method naming convention:
//
//   - POST {endpoint}/connect     -> establish a session
//   - POST {endpoint}/send        -> deliver an ACP message
//   - POST {endpoint}/disconnect  -> tear down a session
//   - GET  {endpoint}/stream      -> poll for inbound ACP messages
//
// The service/route convention mirrors how an ACP gRPC service would expose
// connect/send/disconnect/stream methods.
type gRPCAdapter struct {
	name      string
	endpoint  string
	transport ACPTransport
	client    *http.Client

	mu        sync.Mutex
	connected bool
	done      chan struct{}
	inbound   chan ACPMessage

	// Reconnection state (protected by reconnectMu).
	reconnectMu  sync.Mutex
	reconnecting bool
	pendingMsgs  []ACPMessage
	maxPending   int
}

// maxPendingMessages is the maximum number of messages buffered during
// reconnection before messages are dropped with a warning.
const maxPendingMessages = 64

// Compile-time assertion that gRPCAdapter satisfies ACPClient.
var _ ACPClient = (*gRPCAdapter)(nil)

// NewGRPCAdapter returns an ACPClient that exchanges JSON messages over HTTP
// with an ACP peer at endpoint (e.g. "http://127.0.0.1:9000/acp"). This is the
// stdlib JSON-over-HTTP interpretation of the ACP gRPC contract.
func NewGRPCAdapter(endpoint string, opts ...Option) ACPClient {
	a := &gRPCAdapter{
		name:       resolveName("grpc", opts),
		endpoint:   strings.TrimRight(endpoint, "/"),
		transport:  ACPTransportGRPC,
		client:     &http.Client{Timeout: 30 * time.Second},
		done:       make(chan struct{}),
		inbound:    make(chan ACPMessage, 16),
		maxPending: maxPendingMessages,
	}
	return a
}

// Connect establishes the ACP session, emitting an acp.connect span with
// transport/endpoint attributes. It also starts the inbound poll loop if it is
// not already running.
func (a *gRPCAdapter) Connect(ctx context.Context) error {
	span, spanCtx := tracing.SpanFromContext(ctx, "acp.connect", tracing.SpanKindInternal)
	logger := tracing.NewTraceLogger(span, slog.Default())
	span.SetAttributes(
		tracing.Attribute{Key: "transport", Value: a.transport.String()},
		tracing.Attribute{Key: "endpoint", Value: a.endpoint},
	)
	defer span.End()

	a.mu.Lock()
	if a.connected {
		a.mu.Unlock()
		span.SetStatus(tracing.SpanStatusOK, "")
		return nil
	}
	done := make(chan struct{})
	inbound := make(chan ACPMessage, 16)
	a.done = done
	a.inbound = inbound
	a.mu.Unlock()

	connectMsg := ACPMessage{Type: TypeConnect, SenderID: a.name, Timestamp: time.Now()}
	if err := a.post(spanCtx, "/connect", connectMsg); err != nil {
		span.SetStatus(tracing.SpanStatusError, err.Error())
		logger.Error("acp.connect.failed", "endpoint", a.endpoint, "err", err)
		return fmt.Errorf("acp: connect: %w", err)
	}

	a.mu.Lock()
	if !a.connected {
		a.connected = true
		go a.readLoop(spanCtx, done, inbound)
	}
	a.mu.Unlock()

	span.SetStatus(tracing.SpanStatusOK, "")
	logger.Info("acp.connect", "transport", a.transport.String(), "endpoint", a.endpoint)
	return nil
}

// Disconnect tears down the ACP session and stops the inbound poll loop.
func (a *gRPCAdapter) Disconnect(ctx context.Context) error {
	a.mu.Lock()
	if !a.connected {
		a.mu.Unlock()
		return nil
	}
	a.connected = false
	close(a.done)
	a.mu.Unlock()

	disconnectMsg := ACPMessage{Type: TypeDisconnect, SenderID: a.name, Timestamp: time.Now()}
	// Best-effort notification; the local session is already torn down.
	if err := a.post(ctx, "/disconnect", disconnectMsg); err != nil {
		slog.DebugContext(ctx, "acp.disconnect.notify", "name", a.name, "err", err)
	}
	return nil
}

// SendMessage delivers an ACP message to the peer, emitting an acp.send span
// with message_type/receiver_id attributes.
func (a *gRPCAdapter) SendMessage(ctx context.Context, msg ACPMessage) error {
	span, spanCtx := tracing.SpanFromContext(ctx, "acp.send", tracing.SpanKindInternal)
	logger := tracing.NewTraceLogger(span, slog.Default())
	span.SetAttributes(
		tracing.Attribute{Key: "message_type", Value: msg.Type},
		tracing.Attribute{Key: "receiver_id", Value: msg.ReceiverID},
	)
	defer span.End()

	a.mu.Lock()
	connected := a.connected
	a.mu.Unlock()
	if !connected {
		err := fmt.Errorf("acp: %s adapter not connected", a.name)
		span.SetStatus(tracing.SpanStatusError, err.Error())
		return err
	}

	if msg.Timestamp.IsZero() {
		msg.Timestamp = time.Now()
	}

	// During reconnection, queue messages instead of sending directly.
	a.reconnectMu.Lock()
	if a.reconnecting {
		if len(a.pendingMsgs) >= a.maxPending {
			a.reconnectMu.Unlock()
			err := fmt.Errorf("acp: pending queue full, message dropped")
			span.SetStatus(tracing.SpanStatusError, err.Error())
			logger.Warn("acp.reconnect.queue.full", "message_type", msg.Type)
			return err
		}
		a.pendingMsgs = append(a.pendingMsgs, msg)
		a.reconnectMu.Unlock()
		span.SetStatus(tracing.SpanStatusOK, "")
		logger.Info("acp.reconnect.queued", "message_type", msg.Type, "receiver_id", msg.ReceiverID)
		return nil
	}
	a.reconnectMu.Unlock()

	if err := a.post(spanCtx, "/send", msg); err != nil {
		span.SetStatus(tracing.SpanStatusError, err.Error())
		logger.Error("acp.send.failed", "message_type", msg.Type, "receiver_id", msg.ReceiverID, "err", err)
		return fmt.Errorf("acp: send message: %w", err)
	}

	span.SetStatus(tracing.SpanStatusOK, "")
	logger.Info("acp.send", "message_type", msg.Type, "receiver_id", msg.ReceiverID)
	return nil
}

// ReceiveMessages returns a channel of messages polled from the peer. It is
// closed when the adapter's session is torn down.
func (a *gRPCAdapter) ReceiveMessages() <-chan ACPMessage {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.connected {
		return nil
	}
	return a.inbound
}

// Name returns the logical client identity.
func (a *gRPCAdapter) Name() string { return a.name }

// post marshals msg and POSTs it to the route at the configured endpoint.
func (a *gRPCAdapter) post(ctx context.Context, route string, msg ACPMessage) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("acp: marshal payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint+route, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("acp: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer closeQuietly(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("acp: route %s returned status %d", route, resp.StatusCode)
	}
	return nil
}

// readLoop polls {endpoint}/stream and forwards parsed ACP messages onto the
// inbound channel until the session is torn down or the context is done.
// The loop backs off briefly when no messages are available to avoid a busy
// spin, which keeps it cheap for the long-poll style /stream route.
func (a *gRPCAdapter) readLoop(ctx context.Context, done chan struct{}, inbound chan ACPMessage) {
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		default:
		}

		streamURL := a.endpoint + "/stream"
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, streamURL, nil)
		if err != nil {
			if !a.doReconnect(ctx, done) {
				return
			}
			continue
		}
		resp, err := a.client.Do(req) //nolint:gosec // stream endpoint comes from trusted config
		if err != nil {
			if !a.doReconnect(ctx, done) {
				return
			}
			continue
		}

		if resp.StatusCode >= 500 {
			closeQuietly(resp.Body)
			if !a.doReconnect(ctx, done) {
				return
			}
			continue
		}

		sc := bufio.NewScanner(resp.Body)
		got := false
		for sc.Scan() {
			var msg ACPMessage
			if err := json.Unmarshal(sc.Bytes(), &msg); err != nil {
				continue
			}
			debugMessage(ctx, "acp.receive", msg)
			select {
			case inbound <- msg:
				got = true
			case <-done:
				closeQuietly(resp.Body)
				return
			case <-ctx.Done():
				closeQuietly(resp.Body)
				return
			}
		}
		closeQuietly(resp.Body)

		if !got {
			select {
			case <-time.After(50 * time.Millisecond):
			case <-done:
				return
			case <-ctx.Done():
				return
			}
		}
	}
}

// grpcReconnectBackoff returns the delay before the (zero-based) attempt-th
// reconnection try: 1s, 2s, 4s, 8s, 16s, 30s (capped).
func grpcReconnectBackoff(attempt int) time.Duration {
	d := time.Second
	for i := 0; i < attempt; i++ {
		d *= 2
		if d >= 30*time.Second {
			return 30 * time.Second
		}
	}
	return d
}

// Reconnect attempts to re-establish the gRPC session with exponential
// backoff. It is safe to call concurrently; if a reconnection is already in
// progress, the call waits for it to complete.
func (a *gRPCAdapter) Reconnect(ctx context.Context) error {
	a.mu.Lock()
	connected := a.connected
	done := a.done
	a.mu.Unlock()
	if !connected {
		return fmt.Errorf("acp: %s adapter not connected", a.name)
	}
	if !a.doReconnect(ctx, done) {
		return fmt.Errorf("acp: reconnection failed")
	}
	return nil
}

// doReconnect attempts to re-establish the session with exponential backoff.
// It returns true if reconnection succeeded, or false if done/ctx was
// cancelled. If a reconnection is already in progress, it waits for the
// existing one to complete.
func (a *gRPCAdapter) doReconnect(ctx context.Context, done chan struct{}) bool {
	a.reconnectMu.Lock()
	if a.reconnecting {
		a.reconnectMu.Unlock()
		// Wait for the existing reconnection to complete.
		for {
			a.reconnectMu.Lock()
			r := a.reconnecting
			a.reconnectMu.Unlock()
			if !r {
				return true
			}
			select {
			case <-time.After(10 * time.Millisecond):
			case <-done:
				return false
			case <-ctx.Done():
				return false
			}
		}
	}
	a.reconnecting = true
	a.reconnectMu.Unlock()

	for attempt := 0; ; attempt++ {
		backoff := grpcReconnectBackoff(attempt)
		slog.Info("acp.reconnect.attempt", "attempt", attempt+1, "delay", backoff, "endpoint", a.endpoint)

		timer := time.NewTimer(backoff)
		select {
		case <-timer.C:
		case <-done:
			timer.Stop()
			a.setReconnecting(false)
			return false
		case <-ctx.Done():
			timer.Stop()
			a.setReconnecting(false)
			return false
		}

		connectMsg := ACPMessage{Type: TypeConnect, SenderID: a.name, Timestamp: time.Now()}
		if err := a.post(ctx, "/connect", connectMsg); err != nil {
			slog.Info("acp.reconnect.failed", "attempt", attempt+1, "err", err)
			continue
		}

		a.flushPending(ctx)
		slog.Info("acp.reconnect.success", "endpoint", a.endpoint, "attempts", attempt+1)
		return true
	}
}

// setReconnecting updates the reconnecting flag under the reconnectMu lock.
func (a *gRPCAdapter) setReconnecting(v bool) {
	a.reconnectMu.Lock()
	a.reconnecting = v
	a.reconnectMu.Unlock()
}

// flushPending sends queued messages after a successful reconnection and
// clears the reconnecting flag.
func (a *gRPCAdapter) flushPending(ctx context.Context) {
	a.reconnectMu.Lock()
	pending := a.pendingMsgs
	a.pendingMsgs = nil
	a.reconnecting = false
	a.reconnectMu.Unlock()

	for _, msg := range pending {
		if err := a.post(ctx, "/send", msg); err != nil {
			slog.Warn("acp.reconnect.flush.failed", "message_type", msg.Type, "err", err)
		}
	}
}
