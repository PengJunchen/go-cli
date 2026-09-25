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
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

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

	mu      sync.Mutex
	current *adapterGeneration
}

type adapterGenerationState uint8

const (
	generationConnecting adapterGenerationState = iota
	generationActive
	generationClosing
	generationFailed
)

type adapterGeneration struct {
	state       adapterGenerationState
	ctx         context.Context
	cancel      context.CancelFunc
	ready       chan struct{}
	readyOnce   sync.Once
	inbound     chan ACPMessage
	sessionID   string
	connectErr  error
	terminalErr error

	cleanupStarted bool
	cleanupDone    chan struct{}
	cleanupErr     error

	operations sync.WaitGroup
}

const (
	maxHTTPACPResponseBytes = 4 << 20
	maxHTTPACPFrameBytes    = 1 << 20
	maxHTTPACPReadRetries   = 5
)

var (
	ErrReconnectUnsupported = errors.New("acp: explicit reconnect is unsupported; use Disconnect and Connect")
	// ErrHTTPConnectOutcomeUnknown identifies a dispatched create whose session ID could not be recovered.
	ErrHTTPConnectOutcomeUnknown = errors.New("acp: remote HTTP session creation outcome is unknown")

	errInvalidHTTPACPConfig = errors.New("acp: invalid private HTTP adapter configuration")
	errHTTPACPProtocol      = errors.New("acp: invalid private HTTP profile response")
	errHTTPUnauthorized     = errors.New("acp: HTTP authentication rejected")
	errHTTPForbidden        = errors.New("acp: HTTP request forbidden")
	errHTTPNotFound         = errors.New("acp: HTTP route or session not found")
	errACPNotConnected      = errors.New("acp: adapter not connected")
	errACPCleanupInProgress = errors.New("acp: previous session cleanup is still in progress")
	errACPWorkerPanic       = errors.New("acp: private HTTP client worker failed")
	errACPCleanupPanic      = errors.New("acp: private HTTP client cleanup failed")
	errHTTPBodyReadPanic    = errors.New("acp: private HTTP response body read panicked")
	errHTTPBodyClosePanic   = errors.New("acp: private HTTP response body close panicked")
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

type privateHTTPError struct {
	message string
	cause   error
}

func (e privateHTTPError) Error() string { return e.message }

func (e privateHTTPError) Unwrap() error { return e.cause }

type httpCleanupPendingError struct {
	cause error
	done  <-chan struct{}
}

func (e *httpCleanupPendingError) Error() string {
	return "acp: HTTP session cleanup is still pending"
}

func (e *httpCleanupPendingError) Unwrap() error { return e.cause }

// Done returns the stable completion channel for the owned cleanup operation.
func (e *httpCleanupPendingError) Done() <-chan struct{} { return e.done }

func unknownSessionCreation(cause error) error {
	message := ErrHTTPConnectOutcomeUnknown.Error()
	var routeErr *httpRouteError
	if errors.As(cause, &routeErr) {
		message = fmt.Sprintf("%s (HTTP status %d)", message, routeErr.statusCode)
	}
	return privateHTTPError{
		message: message,
		cause:   errors.Join(ErrHTTPConnectOutcomeUnknown, cause),
	}
}

type responseBodyOwner struct {
	body io.ReadCloser
	once sync.Once
	err  error
}

func (b *responseBodyOwner) Read(p []byte) (n int, readErr error) {
	if b == nil || b.body == nil {
		return 0, errHTTPACPProtocol
	}
	defer func() {
		if recover() != nil {
			n = 0
			readErr = errHTTPBodyReadPanic
		}
	}()
	n, readErr = b.body.Read(p)
	return n, readErr
}

func (b *responseBodyOwner) Close() error {
	if b == nil {
		return nil
	}
	b.once.Do(func() {
		b.err = closeHTTPResponseBody(b.body)
	})
	return b.err
}

func ownHTTPResponseBody(response *http.Response) *responseBodyOwner {
	if response == nil || response.Body == nil {
		return nil
	}
	owner := &responseBodyOwner{body: response.Body}
	response.Body = owner
	return owner
}

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
	}
	return a
}

// IsBuiltInHTTPClientLocallyJoined reports whether the exact HTTP adapter
// created by NewGRPCAdapter has no local worker left, or its current
// generation's cleanup operation has completed. It does not indicate whether
// a remote session deletion succeeded. Other ACPClient implementations cannot
// assert this package-owned completion fact by implementing an interface.
// The result is an instantaneous snapshot; callers must fence future Connect
// calls while relying on it for downstream cleanup.
func IsBuiltInHTTPClientLocallyJoined(client ACPClient) bool {
	adapter, ok := client.(*gRPCAdapter)
	if !ok || adapter == nil {
		return false
	}

	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	generation := adapter.current
	if generation == nil {
		return true
	}
	cleanupDone := generation.cleanupDone
	if cleanupDone == nil {
		return false
	}
	select {
	case <-cleanupDone:
		return true
	default:
		return false
	}
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
	if current := a.current; current != nil {
		switch current.state {
		case generationActive:
			a.mu.Unlock()
			span.SetStatus(tracing.SpanStatusOK, "")
			return nil
		case generationConnecting:
			ready := current.ready
			a.mu.Unlock()
			select {
			case <-ready:
			case <-ctx.Done():
				return ctx.Err()
			}
			a.mu.Lock()
			err := generationFailure(current)
			active := current.state == generationActive
			a.mu.Unlock()
			if !active {
				if err == nil {
					err = errACPNotConnected
				}
				return fmt.Errorf("acp: connect: %w", err)
			}
			span.SetStatus(tracing.SpanStatusOK, "")
			return nil
		case generationClosing:
			a.mu.Unlock()
			return fmt.Errorf("acp: connect: %w", errACPCleanupInProgress)
		case generationFailed:
			err := generationFailure(current)
			a.mu.Unlock()
			if err == nil {
				err = errACPNotConnected
			}
			return fmt.Errorf("acp: connect: %w", err)
		}
	}
	if err := ctx.Err(); err != nil {
		a.mu.Unlock()
		return err
	}

	connectionCtx, cancel := context.WithCancel(context.WithoutCancel(spanCtx))
	generation := &adapterGeneration{
		state:   generationConnecting,
		ctx:     connectionCtx,
		cancel:  cancel,
		ready:   make(chan struct{}),
		inbound: make(chan ACPMessage, 16),
	}
	generation.operations.Add(1)
	a.current = generation
	go a.runGeneration(spanCtx, generation)
	a.mu.Unlock()

	select {
	case <-generation.ready:
	case <-ctx.Done():
		return ctx.Err()
	}
	a.mu.Lock()
	err := generationFailure(generation)
	active := generation.state == generationActive
	a.mu.Unlock()
	if !active {
		if err == nil {
			err = errACPNotConnected
		}
		span.SetStatus(tracing.SpanStatusError, err.Error())
		logger.Error("acp.connect.failed", "endpoint", a.safeEndpointOrigin(), "err", err)
		return fmt.Errorf("acp: connect: %w", err)
	}
	span.SetStatus(tracing.SpanStatusOK, "")
	logger.Info("acp.connect", "transport", ACPHTTPProfileV1, "endpoint", a.safeEndpointOrigin())
	return nil
}

func (a *gRPCAdapter) runGeneration(connectCallerCtx context.Context, generation *adapterGeneration) {
	defer func() {
		if recover() != nil {
			a.mu.Lock()
			if generation.state == generationConnecting {
				generation.connectErr = errACPWorkerPanic
				generation.state = generationFailed
			} else if generation.terminalErr == nil && generation.state != generationClosing {
				generation.terminalErr = errACPWorkerPanic
				generation.state = generationFailed
			}
			generation.readyOnce.Do(func() { close(generation.ready) })
			a.mu.Unlock()
			generation.cancel()
		}
		close(generation.inbound)
		generation.operations.Done()
	}()

	connectCtx, cancelConnect := context.WithCancel(generation.ctx)
	stopCaller := context.AfterFunc(connectCallerCtx, cancelConnect)
	sessionID, connectErr := a.connectSession(connectCtx)
	stopCaller()
	cancelConnect()
	if connectErr == nil && connectCallerCtx.Err() != nil {
		connectErr = connectCallerCtx.Err()
	}

	a.mu.Lock()
	if sessionID != "" {
		generation.sessionID = sessionID
	}
	if connectErr != nil {
		generation.connectErr = connectErr
		if generation.state != generationClosing {
			generation.state = generationFailed
		}
		generation.readyOnce.Do(func() { close(generation.ready) })
		a.mu.Unlock()
		return
	}
	if generation.state == generationClosing {
		generation.connectErr = errACPCleanupInProgress
		generation.readyOnce.Do(func() { close(generation.ready) })
		a.mu.Unlock()
		return
	}
	generation.state = generationActive
	generation.readyOnce.Do(func() { close(generation.ready) })
	a.mu.Unlock()

	if readErr := a.readLoop(generation.ctx, generation.inbound, generation); readErr != nil {
		a.mu.Lock()
		if generation.ctx.Err() == nil && generation.state == generationActive {
			generation.terminalErr = readErr
			generation.state = generationFailed
		}
		a.mu.Unlock()
	}
}

func generationFailure(generation *adapterGeneration) error {
	if generation.cleanupErr != nil {
		return generation.cleanupErr
	}
	if generation.connectErr != nil {
		return generation.connectErr
	}
	return generation.terminalErr
}

// Disconnect fences the generation before cancellation. Exactly one cleanup
// owner waits for all requests and the reader, notifies the remote session, and
// publishes the resulting error. Other callers only wait for that result.
func (a *gRPCAdapter) Disconnect(ctx context.Context) error {
	a.mu.Lock()
	generation := a.current
	if generation == nil {
		a.mu.Unlock()
		return nil
	}
	if !generation.cleanupStarted {
		generation.cleanupStarted = true
		generation.cleanupDone = make(chan struct{})
		generation.state = generationClosing
		generation.cancel()
		go a.cleanupGeneration(ctx, generation)
	}
	cleanupDone := generation.cleanupDone
	a.mu.Unlock()

	select {
	case <-cleanupDone:
		a.mu.Lock()
		err := generation.cleanupErr
		a.mu.Unlock()
		return err
	case <-ctx.Done():
		select {
		case <-cleanupDone:
			a.mu.Lock()
			err := generation.cleanupErr
			a.mu.Unlock()
			return err
		default:
			return &httpCleanupPendingError{cause: ctx.Err(), done: cleanupDone}
		}
	}
}

func (a *gRPCAdapter) cleanupGeneration(ownerCtx context.Context, generation *adapterGeneration) {
	var cleanupErr error
	finished := false
	defer func() {
		if recover() != nil && !finished {
			a.finishCleanup(generation, errACPCleanupPanic)
		}
	}()

	generation.operations.Wait()
	a.mu.Lock()
	sessionID := generation.sessionID
	connectErr := generation.connectErr
	a.mu.Unlock()
	if err := ownerCtx.Err(); err != nil {
		cleanupErr = err
		if sessionID == "" && errors.Is(connectErr, ErrHTTPConnectOutcomeUnknown) {
			cleanupErr = errors.Join(connectErr, err)
		}
	} else if sessionID == "" && errors.Is(connectErr, ErrHTTPConnectOutcomeUnknown) {
		cleanupErr = connectErr
	} else if sessionID != "" {
		cleanupErr = a.postMessage(ownerCtx, "/disconnect", sessionID, ACPMessage{
			Type: TypeDisconnect, SenderID: a.name, Timestamp: time.Now(),
		})
	}
	a.finishCleanup(generation, cleanupErr)
	finished = true
}

func (a *gRPCAdapter) finishCleanup(generation *adapterGeneration, cleanupErr error) {
	a.mu.Lock()
	generation.cleanupErr = cleanupErr
	if cleanupErr == nil {
		generation.sessionID = ""
		if a.current == generation {
			a.current = nil
		}
	} else {
		generation.state = generationFailed
	}
	close(generation.cleanupDone)
	a.mu.Unlock()
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
	generation := a.current
	if generation == nil {
		a.mu.Unlock()
		err := fmt.Errorf("acp: %s adapter not connected: %w", a.name, errACPNotConnected)
		span.SetStatus(tracing.SpanStatusError, err.Error())
		return err
	}
	if generation.state != generationActive {
		err := generationFailure(generation)
		if err == nil {
			err = errACPNotConnected
		}
		a.mu.Unlock()
		wrapped := fmt.Errorf("acp: %s adapter not connected: %w", a.name, err)
		span.SetStatus(tracing.SpanStatusError, wrapped.Error())
		return wrapped
	}
	sessionID := generation.sessionID
	generation.operations.Add(1)
	a.mu.Unlock()
	defer generation.operations.Done()

	operationCtx, cancel := operationContext(spanCtx, generation.ctx)
	defer cancel()
	if msg.Timestamp.IsZero() {
		msg.Timestamp = time.Now()
	}
	msg.SenderID = a.name
	if err := a.postMessage(operationCtx, "/send", sessionID, msg); err != nil {
		span.SetStatus(tracing.SpanStatusError, err.Error())
		logger.Error("acp.send.failed", "message_type", msg.Type, "receiver_id", msg.ReceiverID, "err", err)
		return fmt.Errorf("acp: send message: %w", err)
	}

	span.SetStatus(tracing.SpanStatusOK, "")
	logger.Info("acp.send", "message_type", msg.Type, "receiver_id", msg.ReceiverID)
	return nil
}

func operationContext(callerCtx, generationCtx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(callerCtx)
	stop := context.AfterFunc(generationCtx, cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}

// ReceiveMessages returns a channel of messages polled from the peer. It is
// closed when the adapter's session is torn down.
func (a *gRPCAdapter) ReceiveMessages() <-chan ACPMessage {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.current == nil || a.current.state == generationConnecting || a.current.state == generationClosing || a.current.connectErr != nil {
		return nil
	}
	return a.current.inbound
}

// Name returns the logical client identity.
func (a *gRPCAdapter) Name() string { return a.name }

// postMessage sends one versioned envelope for the active opaque session.
func (a *gRPCAdapter) postMessage(ctx context.Context, route, sessionID string, msg ACPMessage) error {
	envelope := httpEnvelopeFromMessage(sessionID, msg)
	request, err := a.newPostRequest(ctx, route, envelope)
	if err != nil {
		return err
	}
	resp, err := a.client.Do(request)
	if err != nil {
		return privateHTTPError{message: "acp: HTTP request failed", cause: err}
	}
	if resp == nil {
		return errHTTPACPProtocol
	}
	body := ownHTTPResponseBody(resp)
	defer func() { _ = body.Close() }()
	return errors.Join(requireHTTP200(route, resp), body.Close())
}

func (a *gRPCAdapter) newPostRequest(ctx context.Context, route string, envelope httpACPEnvelope) (*http.Request, error) {
	envelope.Version = ACPHTTPProfileV1
	data, err := json.Marshal(envelope)
	if err != nil {
		return nil, privateHTTPError{message: "acp: marshal private HTTP envelope failed", cause: err}
	}
	request, err := a.newRequest(ctx, http.MethodPost, route, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	return request, nil
}

func (a *gRPCAdapter) connectSession(ctx context.Context) (candidateID string, resultErr error) {
	dispatched := false
	defer func() {
		if recover() != nil {
			candidateID = ""
			if dispatched {
				resultErr = unknownSessionCreation(errACPWorkerPanic)
			} else {
				resultErr = errACPWorkerPanic
			}
		}
	}()

	request, err := a.newPostRequest(ctx, "/connect", httpEnvelopeFromMessage("", ACPMessage{
		Type: TypeConnect, SenderID: a.name, Timestamp: time.Now(),
	}))
	if err != nil {
		return "", err
	}
	dispatched = true
	response, err := a.client.Do(request)
	if err != nil {
		return "", unknownSessionCreation(privateHTTPError{message: "acp: connect request failed", cause: err})
	}
	if response == nil {
		return "", unknownSessionCreation(errHTTPACPProtocol)
	}
	body := ownHTTPResponseBody(response)
	defer func() { resultErr = errors.Join(resultErr, body.Close()) }()
	if body == nil {
		return "", unknownSessionCreation(errHTTPACPProtocol)
	}
	if err := requireHTTP200("/connect", response); err != nil {
		if response.StatusCode < http.StatusBadRequest || response.StatusCode >= http.StatusInternalServerError {
			return "", unknownSessionCreation(err)
		}
		return "", err
	}
	payload, err := readHTTPResponseBody(response.Body, maxHTTPACPResponseBytes)
	if err != nil {
		return "", unknownSessionCreation(err)
	}
	var result struct {
		Version   string `json:"version"`
		SessionID string `json:"session_id"`
	}
	decodeErr := json.Unmarshal(payload, &result)
	candidateID = result.SessionID
	if !validOpaqueSessionID(candidateID, a.name) {
		candidateID = ""
	}
	var protocolErr error
	if decodeErr != nil || result.Version != ACPHTTPProfileV1 || !validOpaqueSessionID(result.SessionID, a.name) {
		protocolErr = errHTTPACPProtocol
	}
	if protocolErr != nil && candidateID == "" {
		protocolErr = unknownSessionCreation(protocolErr)
	}
	return candidateID, protocolErr
}

func closeHTTPResponseBody(body io.Closer) (closeErr error) {
	if body == nil {
		return nil
	}
	defer func() {
		if recover() != nil {
			closeErr = errors.Join(closeErr, errHTTPBodyClosePanic)
		}
	}()
	if err := body.Close(); err != nil {
		return privateHTTPError{message: "acp: close private HTTP response failed", cause: err}
	}
	return nil
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
		return nil, privateHTTPError{message: "acp: build private HTTP request failed", cause: err}
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
		return nil, privateHTTPError{message: "acp: read private HTTP response failed", cause: err}
	}
	if int64(len(data)) > maxBytes {
		return nil, errHTTPACPProtocol
	}
	return data, nil
}

// readLoop polls the active opaque session's /stream route. Transient network
// and server failures get a bounded retry on the same session ID; identity,
// authorization, and protocol errors terminate the generation.
func (a *gRPCAdapter) readLoop(ctx context.Context, inbound chan ACPMessage, generation *adapterGeneration) error {
	retries := 0
	for {
		if ctx.Err() != nil {
			return nil
		}

		sessionID := generation.sessionID
		if sessionID == "" {
			return errHTTPACPProtocol
		}
		streamRoute := "/stream?session_id=" + url.QueryEscape(sessionID) + "&sender_id=" + url.QueryEscape(a.name)
		req, err := a.newRequest(ctx, http.MethodGet, streamRoute, nil)
		if err != nil {
			return err
		}
		resp, err := a.client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			failure := privateHTTPError{message: "acp: stream request failed", cause: err}
			if err := a.retryStreamPoll(ctx, &retries, failure); err != nil {
				return err
			}
			continue
		}
		got, retryable, streamErr := readHTTPACPStreamResponse(ctx, inbound, resp, sessionID)
		if errors.Is(streamErr, context.Canceled) && ctx.Err() != nil {
			return nil
		}
		if retryable {
			if err := a.retryStreamPoll(ctx, &retries, streamErr); err != nil {
				return err
			}
			continue
		}
		if streamErr != nil {
			return streamErr
		}
		retries = 0

		if !got {
			select {
			case <-time.After(50 * time.Millisecond):
			case <-ctx.Done():
				return nil
			}
		}
	}
}

func readHTTPACPStreamResponse(ctx context.Context, inbound chan ACPMessage, response *http.Response, sessionID string) (got, retryable bool, resultErr error) {
	if response == nil {
		return false, false, errHTTPACPProtocol
	}
	body := ownHTTPResponseBody(response)
	defer func() { resultErr = errors.Join(resultErr, body.Close()) }()
	if body == nil {
		return false, false, errHTTPACPProtocol
	}
	if response.StatusCode >= 500 {
		statusErr := requireHTTP200("/stream", response)
		if closeErr := body.Close(); closeErr != nil {
			return false, false, errors.Join(statusErr, closeErr)
		}
		return false, true, statusErr
	}
	if err := requireHTTP200("/stream", response); err != nil {
		return false, false, err
	}
	mediaType, _, mediaErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if mediaErr != nil || mediaType != "application/x-ndjson" || response.ContentLength > maxHTTPACPResponseBytes {
		return false, false, errHTTPACPProtocol
	}

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 32*1024), maxHTTPACPFrameBytes)
	totalBytes := 0
	for scanner.Scan() {
		totalBytes += len(scanner.Bytes()) + 1
		if totalBytes > maxHTTPACPResponseBytes {
			return got, false, errHTTPACPProtocol
		}
		var envelope httpACPEnvelope
		if err := json.Unmarshal(scanner.Bytes(), &envelope); err != nil || envelope.Version != ACPHTTPProfileV1 || envelope.SessionID != sessionID {
			return got, false, errHTTPACPProtocol
		}
		msg := envelope.message()
		debugMessage(ctx, "acp.receive", msg)
		select {
		case inbound <- msg:
			got = true
		case <-ctx.Done():
			return got, false, ctx.Err()
		}
	}
	if scanErr := scanner.Err(); scanErr != nil {
		readErr := privateHTTPError{message: "acp: read private HTTP stream failed", cause: scanErr}
		return got, !errors.Is(scanErr, errHTTPBodyReadPanic), readErr
	}
	return got, false, nil
}

func (a *gRPCAdapter) retryStreamPoll(ctx context.Context, retries *int, failure error) error {
	if *retries >= maxHTTPACPReadRetries {
		return fmt.Errorf("acp: stream retry limit exceeded: %w", failure)
	}
	delay := grpcReconnectBackoff(*retries)
	*retries++
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return nil
	}
}

// grpcReconnectBackoff returns the bounded poll retry delay.
func grpcReconnectBackoff(attempt int) time.Duration {
	d := 100 * time.Millisecond
	for i := 0; i < attempt; i++ {
		d *= 2
		if d >= time.Second {
			return time.Second
		}
	}
	return d
}

// Reconnect is retained for package compatibility, but this client does not
// create replacement HTTP sessions or perform a second stream consumer. The
// generation reader already retries transient polls on the same opaque ID.
func (a *gRPCAdapter) Reconnect(context.Context) error {
	return ErrReconnectUnsupported
}
