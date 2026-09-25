package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// closeQuietly best-effort closes an io.Closer.
func closeQuietly(c io.Closer) { _ = c.Close() } //nolint:errcheck // best-effort close

// gRPCAdapter implements ACPClient over go-cli's private JSON-over-HTTP profile.
// The historical name is retained for source compatibility; this is not an
// implementation of official ACP or gRPC. The adapter exchanges versioned
// envelopes over these routes:
//
//   - POST {endpoint}/connect     -> establish a session
//   - POST {endpoint}/send        -> deliver an ACP message
//   - POST {endpoint}/disconnect  -> tear down a session
//   - GET  {endpoint}/stream      -> poll for inbound ACP messages
type gRPCAdapter struct {
	name        string
	endpoint    string
	bearerToken string
	client      *http.Client

	mu        sync.Mutex
	connected bool
	sessionID string
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

const (
	maxHTTPACPResponseBytes = 4 << 20
	maxHTTPACPFrameBytes    = 1 << 20
)

var (
	errInvalidHTTPACPConfig = errors.New("acp: invalid private HTTP adapter configuration")
	errHTTPACPProtocol      = errors.New("acp: invalid private HTTP profile response")
	errHTTPUnauthorized     = errors.New("acp: HTTP authentication rejected")
	errHTTPForbidden        = errors.New("acp: HTTP request forbidden")
	errHTTPNotFound         = errors.New("acp: HTTP route or session not found")
)

type httpRouteError struct {
	route      string
	statusCode int
	statusErr  error
}

func (e *httpRouteError) Error() string {
	return fmt.Sprintf("acp: route %s returned status %d", e.route, e.statusCode)
}

func (e *httpRouteError) Unwrap() error { return e.statusErr }

// Compile-time assertion that gRPCAdapter satisfies ACPClient.
var _ ACPClient = (*gRPCAdapter)(nil)

// NewGRPCAdapter returns an ACPClient that exchanges the private
// go-cli-acp-json/1 profile over HTTP with endpoint. The historical constructor
// name is retained for compatibility; the wire protocol is neither official
// ACP nor gRPC.
func NewGRPCAdapter(endpoint string, opts ...Option) ACPClient {
	options := adapterOptions{name: "grpc"}
	for _, opt := range opts {
		if opt != nil {
			opt(&options)
		}
	}
	a := &gRPCAdapter{
		name:        options.name,
		endpoint:    strings.TrimRight(endpoint, "/"),
		bearerToken: options.httpBearerToken,
		client: &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
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
		tracing.Attribute{Key: "transport", Value: ACPHTTPProfileV1},
		tracing.Attribute{Key: "endpoint", Value: a.safeEndpointOrigin()},
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

	sessionID, err := a.connectSession(spanCtx)
	if err != nil {
		span.SetStatus(tracing.SpanStatusError, err.Error())
		logger.Error("acp.connect.failed", "endpoint", a.safeEndpointOrigin(), "err", err)
		return fmt.Errorf("acp: connect: %w", err)
	}

	a.mu.Lock()
	if !a.connected {
		a.sessionID = sessionID
		a.connected = true
		go a.readLoop(spanCtx, done, inbound)
	} else {
		a.mu.Unlock()
		a.bestEffortDisconnect(spanCtx, sessionID)
		a.mu.Lock()
	}
	a.mu.Unlock()

	span.SetStatus(tracing.SpanStatusOK, "")
	logger.Info("acp.connect", "transport", ACPHTTPProfileV1, "endpoint", a.safeEndpointOrigin())
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
	sessionID := a.sessionID
	a.sessionID = ""
	close(a.done)
	a.mu.Unlock()

	disconnectMsg := ACPMessage{Type: TypeDisconnect, SenderID: a.name, Timestamp: time.Now()}
	// Best-effort notification; the local session is already torn down.
	if err := a.postMessage(ctx, "/disconnect", sessionID, disconnectMsg); err != nil {
		slog.DebugContext(ctx, "acp.disconnect.notify", "name", a.name, "err", err)
		return err
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
	sessionID := a.sessionID
	a.mu.Unlock()
	if !connected {
		err := fmt.Errorf("acp: %s adapter not connected", a.name)
		span.SetStatus(tracing.SpanStatusError, err.Error())
		return err
	}

	if msg.Timestamp.IsZero() {
		msg.Timestamp = time.Now()
	}
	msg.SenderID = a.name

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

	if err := a.postMessage(spanCtx, "/send", sessionID, msg); err != nil {
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

// postMessage sends one versioned envelope for the active opaque session.
func (a *gRPCAdapter) postMessage(ctx context.Context, route, sessionID string, msg ACPMessage) error {
	envelope := httpEnvelopeFromMessage(sessionID, msg)
	resp, err := a.postEnvelope(ctx, route, envelope)
	if err != nil {
		return err
	}
	defer closeQuietly(resp.Body)
	return requireHTTP200(route, resp)
}

func (a *gRPCAdapter) postEnvelope(ctx context.Context, route string, envelope httpACPEnvelope) (*http.Response, error) {
	envelope.Version = ACPHTTPProfileV1
	data, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("acp: marshal private HTTP envelope: %w", err)
	}
	request, err := a.newRequest(ctx, http.MethodPost, route, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	return a.client.Do(request)
}

func (a *gRPCAdapter) connectSession(ctx context.Context) (string, error) {
	response, err := a.postEnvelope(ctx, "/connect", httpEnvelopeFromMessage("", ACPMessage{
		Type: TypeConnect, SenderID: a.name, Timestamp: time.Now(),
	}))
	if err != nil {
		return "", fmt.Errorf("acp: connect request: %w", err)
	}
	defer closeQuietly(response.Body)
	if err := requireHTTP200("/connect", response); err != nil {
		return "", err
	}
	body, err := readHTTPResponseBody(response.Body, maxHTTPACPResponseBytes)
	if err != nil {
		return "", err
	}
	var result struct {
		Version   string `json:"version"`
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(body, &result); err != nil || result.Version != ACPHTTPProfileV1 || !validOpaqueSessionID(result.SessionID, a.name) {
		return "", errHTTPACPProtocol
	}
	return result.SessionID, nil
}

func (a *gRPCAdapter) bestEffortDisconnect(ctx context.Context, sessionID string) {
	if sessionID == "" {
		return
	}
	if err := a.postMessage(ctx, "/disconnect", sessionID, ACPMessage{
		Type: TypeDisconnect, SenderID: a.name, Timestamp: time.Now(),
	}); err != nil {
		slog.DebugContext(ctx, "acp.disconnect.notify", "name", a.name, "err", err)
	}
}

func (a *gRPCAdapter) newRequest(ctx context.Context, method, route string, body io.Reader) (*http.Request, error) {
	baseURL, err := url.Parse(a.endpoint)
	if err != nil || baseURL.Scheme != "http" && baseURL.Scheme != "https" || baseURL.Host == "" || baseURL.User != nil || baseURL.RawQuery != "" || baseURL.Fragment != "" || baseURL.ForceQuery || strings.Contains(a.endpoint, "#") {
		return nil, errInvalidHTTPACPConfig
	}
	targetURL, err := url.Parse(strings.TrimRight(a.endpoint, "/") + route)
	if err != nil || targetURL.Scheme != baseURL.Scheme || !strings.EqualFold(targetURL.Host, baseURL.Host) || targetURL.User != nil {
		return nil, errInvalidHTTPACPConfig
	}
	request, err := http.NewRequestWithContext(ctx, method, targetURL.String(), body)
	if err != nil {
		return nil, fmt.Errorf("acp: build private HTTP request: %w", err)
	}
	request.Header.Set(ACPHTTPProfileHeader, ACPHTTPProfileV1)
	if a.bearerToken != "" {
		if !validBearerToken(a.bearerToken) {
			return nil, errInvalidHTTPACPConfig
		}
		request.Header.Set("Authorization", "Bearer "+a.bearerToken)
	}
	return request, nil
}

func validBearerToken(token string) bool {
	if token == "" {
		return true
	}
	for _, r := range token {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-._~+/=", r) {
			continue
		}
		return false
	}
	return true
}

func validOpaqueSessionID(sessionID, displayAlias string) bool {
	return sessionID != "" && len(sessionID) <= 256 && strings.TrimSpace(sessionID) == sessionID && sessionID != displayAlias
}

func (a *gRPCAdapter) safeEndpointOrigin() string {
	endpoint, err := url.Parse(a.endpoint)
	if err != nil || endpoint.Scheme == "" || endpoint.Host == "" || endpoint.User != nil {
		return "<invalid-endpoint>"
	}
	return strings.ToLower(endpoint.Scheme) + "://" + strings.ToLower(endpoint.Host)
}

func requireHTTP200(route string, response *http.Response) error {
	if response.StatusCode == http.StatusOK {
		return nil
	}
	var statusErr error
	switch response.StatusCode {
	case http.StatusUnauthorized:
		statusErr = errHTTPUnauthorized
	case http.StatusForbidden:
		statusErr = errHTTPForbidden
	case http.StatusNotFound:
		statusErr = errHTTPNotFound
	}
	return &httpRouteError{route: route, statusCode: response.StatusCode, statusErr: statusErr}
}

func readHTTPResponseBody(body io.Reader, maxBytes int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("acp: read private HTTP response: %w", err)
	}
	if int64(len(data)) > maxBytes {
		return nil, errHTTPACPProtocol
	}
	return data, nil
}

// readLoop polls the active opaque session's /stream route and forwards only
// exact-version envelopes for that session onto the inbound channel.
func (a *gRPCAdapter) readLoop(ctx context.Context, done chan struct{}, inbound chan ACPMessage) {
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		default:
		}

		a.mu.Lock()
		sessionID := a.sessionID
		a.mu.Unlock()
		if sessionID == "" {
			return
		}
		streamRoute := "/stream?session_id=" + url.QueryEscape(sessionID) + "&sender_id=" + url.QueryEscape(a.name)
		req, err := a.newRequest(ctx, http.MethodGet, streamRoute, nil)
		if err != nil {
			return
		}
		resp, err := a.client.Do(req)
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
		if err := requireHTTP200("/stream", resp); err != nil {
			closeQuietly(resp.Body)
			slog.DebugContext(ctx, "acp.stream.failed", "err", err)
			return
		}

		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 32*1024), maxHTTPACPFrameBytes)
		got := false
		totalBytes := 0
		for sc.Scan() {
			totalBytes += len(sc.Bytes()) + 1
			if totalBytes > maxHTTPACPResponseBytes {
				closeQuietly(resp.Body)
				slog.DebugContext(ctx, "acp.stream.failed", "err", errHTTPACPProtocol)
				return
			}
			var envelope httpACPEnvelope
			if err := json.Unmarshal(sc.Bytes(), &envelope); err != nil || envelope.Version != ACPHTTPProfileV1 || envelope.SessionID != sessionID {
				closeQuietly(resp.Body)
				slog.DebugContext(ctx, "acp.stream.failed", "err", errHTTPACPProtocol)
				return
			}
			msg := envelope.message()
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
		if err := sc.Err(); err != nil {
			slog.DebugContext(ctx, "acp.stream.failed", "err", errHTTPACPProtocol)
			return
		}

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

// doReconnect delays another poll using the current server-issued session ID.
// It never creates a replacement session. If a reconnect delay is already in
// progress, callers wait for it to finish.
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
		slog.Info("acp.reconnect.attempt", "attempt", attempt+1, "delay", backoff, "endpoint", a.safeEndpointOrigin())

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

		a.flushPending(ctx)
		slog.Info("acp.reconnect.poll.resume", "endpoint", a.safeEndpointOrigin(), "attempts", attempt+1)
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
	a.mu.Lock()
	sessionID := a.sessionID
	a.mu.Unlock()

	for _, msg := range pending {
		if err := a.postMessage(ctx, "/send", sessionID, msg); err != nil {
			slog.Warn("acp.reconnect.flush.failed", "message_type", msg.Type, "err", err)
		}
	}
}
