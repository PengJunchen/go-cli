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
}

// Compile-time assertion that gRPCAdapter satisfies ACPClient.
var _ ACPClient = (*gRPCAdapter)(nil)

// NewGRPCAdapter returns an ACPClient that exchanges JSON messages over HTTP
// with an ACP peer at endpoint (e.g. "http://127.0.0.1:9000/acp"). This is the
// stdlib JSON-over-HTTP interpretation of the ACP gRPC contract.
func NewGRPCAdapter(endpoint string, opts ...Option) ACPClient {
	a := &gRPCAdapter{
		name:      resolveName("grpc", opts),
		endpoint:  strings.TrimRight(endpoint, "/"),
		transport: ACPTransportGRPC,
		client:    &http.Client{Timeout: 30 * time.Second},
		done:      make(chan struct{}),
		inbound:   make(chan ACPMessage, 16),
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
	done := a.done
	inbound := a.inbound
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
			select {
			case <-time.After(100 * time.Millisecond):
				continue
			case <-done:
				return
			case <-ctx.Done():
				return
			}
		}
		resp, err := a.client.Do(req) //nolint:gosec // stream endpoint comes from trusted config
		if err != nil {
			select {
			case <-time.After(100 * time.Millisecond):
				continue
			case <-done:
				return
			case <-ctx.Done():
				return
			}
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
