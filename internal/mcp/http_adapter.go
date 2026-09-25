package mcp

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
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/tracing"
)

// HTTPClientAdapter is an MCPClient that communicates with an MCP server over
// the MCP 2025-06-18 Streamable HTTP transport.
type HTTPClientAdapter struct {
	cfg    MCPServerConfig
	client *http.Client
	mu     sync.Mutex

	current *httpConnectionGeneration
	nextID  int64
}

type httpConnectionGeneration struct {
	endpoint        string
	sessionID       string
	protocolVersion string
	connected       bool
	closing         bool
	lifeCtx         context.Context
	cancelLife      context.CancelFunc
	active          int
	quiescent       chan struct{}
	connectOp       *httpConnectOperation
	closeOp         *httpCloseOperation
}

type httpConnectOperation struct {
	done   chan struct{}
	cancel context.CancelFunc
	result error
}

type httpCloseOperation struct {
	done     chan struct{}
	deadline time.Time
	ownerCtx context.Context
	result   error
}

type httpConnectWaitPolicy struct {
	operationCtx context.Context
	owner        bool
}

var _ MCPClient = (*HTTPClientAdapter)(nil)

var errCrossOriginRedirect = errors.New("mcp: cross-origin redirect rejected")

// ErrHTTPOperationPending reports that an operation's waiter stopped waiting,
// while the actual HTTP operation remains owned and may still be in flight.
var ErrHTTPOperationPending = errors.New("mcp: HTTP operation remains pending")

type httpOperationPendingError struct {
	operation string
	cause     error
}

func (e *httpOperationPendingError) Error() string {
	return "mcp: " + e.operation + " remains pending while the HTTP operation is still in flight"
}

func (e *httpOperationPendingError) Unwrap() error { return e.cause }

func (e *httpOperationPendingError) Is(target error) bool {
	return target == ErrHTTPOperationPending
}

type httpRequestError struct {
	method string
	cause  error
}

func (e *httpRequestError) Error() string { return "mcp: HTTP " + e.method + " request failed" }

func (e *httpRequestError) Unwrap() error { return e.cause }

// ErrUnsupportedOAuth reports that this HTTP adapter does not implement the
// authorization flow configured for an MCP server. Callers may still provide
// an already-issued BearerToken.
var ErrUnsupportedOAuth = errors.New("mcp: OAuth configuration is unsupported by the Streamable HTTP adapter")

// ErrUnsupportedHTTPTransport reports that the configured HTTP transport is
// the deprecated HTTP+SSE profile rather than Streamable HTTP.
var ErrUnsupportedHTTPTransport = errors.New("mcp: only the Streamable HTTP transport is supported")

// HTTPProtocolError describes an invalid Streamable HTTP response or
// lifecycle transition without including remote response bodies.
type HTTPProtocolError struct {
	message string
	cause   error
}

func (e *HTTPProtocolError) Error() string { return "mcp: " + e.message }

func (e *HTTPProtocolError) Unwrap() error { return e.cause }

func httpProtocolError(format string, args ...any) error {
	return &HTTPProtocolError{message: fmt.Sprintf(format, args...)}
}

func httpProtocolErrorCause(message string, cause error) error {
	return &HTTPProtocolError{message: message, cause: cause}
}

const streamableHTTPProtocolVersion = "2025-06-18"

var errSSEResponseMissing = errors.New("mcp: SSE stream ended without the matching JSON-RPC response")
var errSSELineTooLong = errors.New("mcp: SSE line exceeds response limit")

// defaultHTTPTimeout bounds individual HTTP requests. Network fetch
// operations (e.g. web search) can legitimately take several seconds, but
// without a cap a hung server would block the agent loop forever.
const defaultHTTPTimeout = 60 * time.Second

// maxHTTPResponseSize caps the size of an HTTP response body read by the
// adapter. This prevents a malicious or buggy server from exhausting memory
// by streaming an arbitrarily large response.
const maxHTTPResponseSize = 10 * 1024 * 1024 // 10 MB

var errHTTPResponseTooLarge = fmt.Errorf("mcp: response body exceeds %d byte limit", maxHTTPResponseSize)

type responseByteBudget struct {
	remaining int64
}

func newResponseByteBudget(limit int64) *responseByteBudget {
	return &responseByteBudget{remaining: limit}
}

func (b *responseByteBudget) wrap(source io.Reader) io.Reader {
	return responseBudgetReader{source: source, budget: b}
}

type responseBudgetReader struct {
	source io.Reader
	budget *responseByteBudget
}

func (r responseBudgetReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	remaining := r.budget.remaining
	maxProbe := remaining + 1
	if int64(len(p)) > maxProbe {
		p = p[:maxProbe]
	}
	n, err := r.source.Read(p)
	if int64(n) > remaining {
		r.budget.remaining = 0
		return 0, errHTTPResponseTooLarge
	}
	r.budget.remaining -= int64(n)
	return n, err
}

// readLimitedBody reads at most maxHTTPResponseSize bytes from r. If the body
// exceeds the limit, an error is returned so the caller can abort instead of
// allocating unbounded memory.
func readLimitedBody(r io.Reader) ([]byte, error) {
	return readLimitedBodyWithBudget(r, newResponseByteBudget(maxHTTPResponseSize))
}

func readLimitedBodyWithBudget(r io.Reader, budget *responseByteBudget) ([]byte, error) {
	raw, err := io.ReadAll(budget.wrap(r))
	if err != nil {
		return nil, err
	}
	return raw, nil
}

func sameHTTPOrigin(left, right *url.URL) bool {
	if left == nil || right == nil {
		return false
	}
	leftScheme := strings.ToLower(left.Scheme)
	rightScheme := strings.ToLower(right.Scheme)
	if leftScheme != rightScheme || (leftScheme != "http" && leftScheme != "https") {
		return false
	}
	if !strings.EqualFold(left.Hostname(), right.Hostname()) {
		return false
	}
	return effectiveHTTPPort(left) == effectiveHTTPPort(right)
}

func effectiveHTTPPort(u *url.URL) string {
	if port := u.Port(); port != "" {
		if value, err := strconv.Atoi(port); err == nil {
			return strconv.Itoa(value)
		}
		return port
	}
	if strings.EqualFold(u.Scheme, "https") {
		return "443"
	}
	return "80"
}

func resolveSameOriginEndpoint(configuredURL, endpoint string) (string, error) {
	base, err := url.Parse(configuredURL)
	if err != nil {
		return "", httpProtocolError("configured HTTP endpoint is invalid")
	}
	baseScheme := strings.ToLower(base.Scheme)
	if base.Host == "" || (baseScheme != "http" && baseScheme != "https") {
		return "", fmt.Errorf("mcp: configured endpoint must be an absolute HTTP URL")
	}
	ref, err := url.Parse(endpoint)
	if err != nil {
		return "", httpProtocolError("HTTP endpoint is invalid")
	}
	resolved := base.ResolveReference(ref)
	if !sameHTTPOrigin(base, resolved) {
		return "", fmt.Errorf("mcp: endpoint must remain on the configured origin %q", base.Host)
	}
	return resolved.String(), nil
}

// doHTTP preserves the configured client's transport and options while
// refusing redirects that would move a request to a different origin.
func (a *HTTPClientAdapter) doHTTP(req *http.Request) (*http.Response, error) {
	client := *a.client
	checkRedirect := client.CheckRedirect
	initialURL := *req.URL
	client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if !sameHTTPOrigin(&initialURL, next.URL) {
			return errCrossOriginRedirect
		}
		if checkRedirect != nil {
			if err := checkRedirect(next, via); err != nil {
				return err
			}
		} else if len(via) >= 10 {
			return fmt.Errorf("stopped after 10 redirects")
		}
		// The caller's CheckRedirect callback can mutate next.URL after the
		// first origin check, so validate the actual target immediately before
		// net/http sends the redirected request.
		if !sameHTTPOrigin(&initialURL, next.URL) {
			return errCrossOriginRedirect
		}
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		// *url.Error formats the request URL, which can include credentials and
		// query parameters. Keep the cause chain for errors.Is while exposing
		// only a method-level diagnostic.
		return nil, &httpRequestError{method: req.Method, cause: err}
	}
	return resp, nil
}

// NewHTTPClientAdapter returns an HTTPClientAdapter for the given config. The
// underlying *http.Client is SSRF-safe: it blocks private, link-local, and
// cloud-metadata IP ranges at dial time (defending against DNS rebinding)
// while permitting loopback so local MCP servers keep working.
func NewHTTPClientAdapter(cfg MCPServerConfig) *HTTPClientAdapter {
	return &HTTPClientAdapter{
		cfg:    cfg,
		client: tools.NewSSRFSafeHTTPClientAllowLoopback(defaultHTTPTimeout),
	}
}

// NewHTTPClientAdapterWithClient returns an HTTPClientAdapter using the given
// *http.Client, allowing callers to override transport or timeouts. When client
// is nil, a loopback-allowing SSRF-safe client protects all requests against
// internal-IP targeting.
func NewHTTPClientAdapterWithClient(cfg MCPServerConfig, client *http.Client) *HTTPClientAdapter {
	if client == nil {
		client = tools.NewSSRFSafeHTTPClientAllowLoopback(defaultHTTPTimeout)
	}
	return &HTTPClientAdapter{cfg: cfg, client: client}
}

// Connect performs the 2025-06-18 initialize/initialized lifecycle over the
// configured Streamable HTTP endpoint. It never probes or falls back to the
// deprecated HTTP+SSE transport.
func (a *HTTPClientAdapter) Connect(ctx context.Context) error {
	if a.cfg.OAuthConfig != nil {
		return ErrUnsupportedOAuth
	}
	if a.cfg.Transport != MCPTransportStreamableHTTP {
		return ErrUnsupportedHTTPTransport
	}
	endpoint, err := resolveSameOriginEndpoint(a.cfg.URL, a.cfg.URL)
	if err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}

	for {
		a.mu.Lock()
		gen := a.current
		if gen != nil && gen.closeOp != nil {
			closeOp := gen.closeOp
			a.mu.Unlock()
			if err := waitForHTTPOperation(ctx, closeOp.done, "connect"); err != nil {
				return err
			}
			a.mu.Lock()
			closeErr := closeOp.result
			if a.current == gen && closeErr == nil {
				a.current = nil
			}
			a.mu.Unlock()
			if closeErr != nil {
				return closeErr
			}
			continue
		}
		if gen != nil {
			if gen.connected && !gen.closing {
				a.mu.Unlock()
				return nil
			}
			if gen.connectOp != nil {
				op := gen.connectOp
				a.mu.Unlock()
				return waitForConnect(ctx, op, httpConnectWaitPolicy{})
			}
			a.mu.Unlock()
			return httpProtocolError("previous connection attempt failed; disconnect before reconnecting")
		}
		if err := ctx.Err(); err != nil {
			a.mu.Unlock()
			return contextCause(ctx)
		}

		lifeCtx, cancelLife := context.WithCancel(context.Background())
		connectCtx, cancelConnect, stopLife := linkedCancellation(ctx, lifeCtx)
		gen = &httpConnectionGeneration{
			endpoint:   endpoint,
			lifeCtx:    lifeCtx,
			cancelLife: cancelLife,
			active:     1,
			quiescent:  make(chan struct{}),
		}
		op := &httpConnectOperation{done: make(chan struct{}), cancel: cancelConnect}
		gen.connectOp = op
		id := a.nextID
		a.nextID++
		a.current = gen
		a.mu.Unlock()

		go a.runConnect(connectCtx, gen, op, stopLife, id)
		return waitForConnect(ctx, op, httpConnectWaitPolicy{operationCtx: connectCtx, owner: true})
	}
}

// Disconnect closes the local connection and asks a session-aware server to
// delete the remote session. A 405 response is permitted by the transport and
// does not prove that any in-flight remote action stopped.
func (a *HTTPClientAdapter) Disconnect(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	a.mu.Lock()
	gen := a.current
	if gen == nil {
		a.mu.Unlock()
		return nil
	}
	if gen.closeOp != nil {
		op := gen.closeOp
		a.mu.Unlock()
		return waitForClose(ctx, op)
	}
	op := &httpCloseOperation{
		done:     make(chan struct{}),
		deadline: time.Now().Add(defaultHTTPTimeout),
		ownerCtx: context.WithoutCancel(ctx),
	}
	gen.closeOp = op
	gen.closing = true
	gen.connected = false
	gen.cancelLife()
	if gen.connectOp != nil {
		gen.connectOp.cancel()
	}
	quiescent := gen.quiescent
	a.mu.Unlock()
	go a.runClose(gen, op, quiescent)
	return waitForClose(ctx, op)
}

func (a *HTTPClientAdapter) runConnect(ctx context.Context, gen *httpConnectionGeneration, op *httpConnectOperation, stopLife func() bool, id int64) {
	sessionID, version, err := a.performConnect(ctx, gen.endpoint, id)
	stopLife()
	a.mu.Lock()
	if err == nil && ctx.Err() != nil {
		err = contextCause(ctx)
	}
	op.cancel()
	gen.sessionID = sessionID
	gen.protocolVersion = version
	if err == nil && (a.current != gen || gen.closing) {
		err = context.Canceled
	}
	if err == nil {
		gen.connected = true
	}
	op.result = err
	endHTTPActivityLocked(gen)
	close(op.done)
	a.mu.Unlock()
}

func (a *HTTPClientAdapter) runClose(gen *httpConnectionGeneration, op *httpCloseOperation, quiescent <-chan struct{}) {
	closeErr := a.performClose(gen, op, quiescent)
	a.mu.Lock()
	op.result = closeErr
	close(op.done)
	a.mu.Unlock()
}

func (a *HTTPClientAdapter) performConnect(ctx context.Context, endpoint string, id int64) (sessionID, version string, retErr error) {
	defer func() {
		if recover() != nil {
			retErr = httpProtocolError("initialize handshake operation panicked; remote session state may be unknown")
		}
	}()
	sessionID, version, retErr = a.doInitialize(ctx, endpoint, id)
	if retErr != nil {
		retErr = fmt.Errorf("mcp: initialize handshake: %w", retErr)
		return sessionID, version, retErr
	}
	if err := ctx.Err(); err != nil {
		return sessionID, version, contextCause(ctx)
	}
	slog.Info("mcp.http.initialized", "server", a.cfg.Name, "protocolVersion", version)
	slog.Info("mcp.http.connect", "server", a.cfg.Name)
	return sessionID, version, nil
}

func (a *HTTPClientAdapter) performClose(gen *httpConnectionGeneration, op *httpCloseOperation, quiescent <-chan struct{}) (closeErr error) {
	defer func() {
		if recover() != nil {
			closeErr = httpProtocolError("session teardown operation panicked; remote state may be unknown")
		}
	}()
	<-quiescent
	a.mu.Lock()
	endpoint, version, sessionID := gen.endpoint, gen.protocolVersion, gen.sessionID
	connectErr := error(nil)
	if gen.connectOp != nil {
		connectErr = gen.connectOp.result
	}
	a.mu.Unlock()

	if sessionID == "" {
		if connectErr != nil {
			closeErr = httpProtocolErrorCause("initialization failed before a deletable session was known; remote state may be unknown", connectErr)
		}
	} else {
		deleteCtx, cancel := context.WithDeadline(op.ownerCtx, op.deadline)
		defer cancel()
		req, err := a.newHTTPMessageRequest(deleteCtx, http.MethodDelete, endpoint, nil, version, sessionID, "", "")
		if err != nil {
			closeErr = fmt.Errorf("mcp: build session DELETE: %w", err)
		} else {
			resp, err := a.doHTTP(req)
			if err != nil {
				closeErr = fmt.Errorf("mcp: delete session: %w", err)
			} else {
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusMethodNotAllowed && (resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices) {
					closeErr = httpProtocolError("session DELETE returned HTTP status %d", resp.StatusCode)
				}
			}
		}
	}
	return closeErr
}

func linkedCancellation(parent, cancelOn context.Context) (context.Context, context.CancelFunc, func() bool) {
	ctx, cancel := context.WithCancel(parent)
	callbackDone := make(chan struct{})
	var callbackOnce sync.Once
	finishCallback := func() { callbackOnce.Do(func() { close(callbackDone) }) }
	stopCallback := context.AfterFunc(cancelOn, func() {
		defer finishCallback()
		cancel()
	})
	stop := func() bool {
		stopped := stopCallback()
		if stopped {
			finishCallback()
		}
		<-callbackDone
		return stopped
	}
	return ctx, cancel, stop
}

func beginHTTPActivityLocked(gen *httpConnectionGeneration) {
	if gen.active == 0 {
		gen.quiescent = make(chan struct{})
	}
	gen.active++
}

func endHTTPActivityLocked(gen *httpConnectionGeneration) {
	gen.active--
	if gen.active == 0 {
		close(gen.quiescent)
	}
}

func waitForHTTPOperation(ctx context.Context, done <-chan struct{}, operation string) error {
	select {
	case <-done:
		return nil
	default:
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		select {
		case <-done:
			return nil
		default:
		}
		return &httpOperationPendingError{operation: operation, cause: contextCause(ctx)}
	}
}

func waitForClose(ctx context.Context, op *httpCloseOperation) error {
	if err := waitForHTTPOperation(ctx, op.done, "disconnect"); err != nil {
		return err
	}
	return op.result
}

func waitForConnect(ctx context.Context, op *httpConnectOperation, policy httpConnectWaitPolicy) error {
	select {
	case <-op.done:
		return op.result
	default:
	}
	if policy.owner {
		select {
		case <-op.done:
			return op.result
		case <-ctx.Done():
			op.cancel()
			if err := waitForHTTPOperationDone(op.done); err == nil {
				return op.result
			}
			return &httpOperationPendingError{operation: "connect", cause: contextCause(ctx)}
		case <-policy.operationCtx.Done():
			if err := waitForHTTPOperationDone(op.done); err == nil {
				return op.result
			}
			return &httpOperationPendingError{operation: "connect", cause: contextCause(policy.operationCtx)}
		}
	}
	if err := waitForHTTPOperation(ctx, op.done, "connect"); err != nil {
		return err
	}
	return op.result
}

func waitForHTTPOperationDone(done <-chan struct{}) error {
	select {
	case <-done:
		return nil
	default:
		return ErrHTTPOperationPending
	}
}

func contextCause(ctx context.Context) error {
	if ctx == nil {
		return context.Canceled
	}
	err := ctx.Err()
	cause := context.Cause(ctx)
	if err == nil {
		return cause
	}
	if cause == nil || errors.Is(cause, err) {
		return causeOrErr(cause, err)
	}
	return errors.Join(err, cause)
}

func causeOrErr(cause, fallback error) error {
	if cause != nil {
		return cause
	}
	return fallback
}

// Name returns the logical server name.
func (a *HTTPClientAdapter) Name() string { return a.cfg.Name }

// ListTools calls the tools/list JSON-RPC method.
func (a *HTTPClientAdapter) ListTools(ctx context.Context) ([]MCPTool, error) {
	res, err := a.request(ctx, "tools/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	raw, ok := res["tools"].([]any)
	if !ok {
		return nil, fmt.Errorf("mcp: unexpected tools/list result")
	}
	tools := make([]MCPTool, 0, len(raw))
	for _, item := range raw {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		tools = append(tools, MCPTool{
			Name:        stringOf(obj["name"]),
			Description: stringOf(obj["description"]),
			ArgsSchema:  mapOf(obj["inputSchema"]),
		})
	}
	return tools, nil
}

// CallTool invokes the named tool via the tools/call JSON-RPC method.
func (a *HTTPClientAdapter) CallTool(ctx context.Context, name string, args map[string]any) (*MCPToolResult, error) {
	span, spanCtx := tracing.SpanFromContext(ctx, "mcp.tool.call", tracing.SpanKindInternal)
	span.SetAttributes(
		tracing.Attribute{Key: "tool_name", Value: name},
		tracing.Attribute{Key: "server", Value: a.cfg.Name},
		tracing.Attribute{Key: "transport", Value: "streamable-http"},
	)
	defer span.End()

	res, err := a.request(spanCtx, "tools/call", map[string]any{
		"name":      name,
		"arguments": args,
	})
	if err != nil {
		return nil, fmt.Errorf("mcp: call tool %q: %w", name, toolCallOutcomeError(name, err))
	}

	result := &MCPToolResult{
		Content: res["content"],
		IsError: boolVal(res["isError"]),
	}
	if result.Content == nil {
		result.Content = ""
	}
	return result, nil
}

// request sends a JSON-RPC request via HTTP POST and returns the result.
func (a *HTTPClientAdapter) request(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	a.mu.Lock()
	gen := a.current
	if gen == nil || !gen.connected || gen.closing {
		a.mu.Unlock()
		return nil, httpProtocolError("request %q requires an initialized connection", method)
	}
	id := a.nextID
	a.nextID++
	endpoint, version, sessionID := gen.endpoint, gen.protocolVersion, gen.sessionID
	beginHTTPActivityLocked(gen)
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		endHTTPActivityLocked(gen)
		a.mu.Unlock()
	}()

	requestCtx, cancel, stopLife := linkedCancellation(ctx, gen.lifeCtx)
	defer func() {
		stopLife()
		cancel()
	}()

	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return nil, fmt.Errorf("mcp: marshal request: %w", err)
	}
	if err := requestCtx.Err(); err != nil {
		return nil, err
	}

	req, err := a.newHTTPMessageRequest(requestCtx, http.MethodPost, endpoint, body, version, sessionID, "application/json, text/event-stream", "")
	if err != nil {
		return nil, fmt.Errorf("mcp: build request: %w", err)
	}
	resp, err := a.doHTTP(req)
	if err != nil {
		return nil, newRequestBoundaryError(fmt.Errorf("mcp: do request: %w", err), true, false)
	}
	responseBudget := newResponseByteBudget(maxHTTPResponseSize)
	result, cursor, resumable, responseErr := a.readRequestResponse(resp, id, sessionID, responseBudget)
	_ = resp.Body.Close()
	if responseErr != nil && resumable && cursor != "" && requestCtx.Err() == nil {
		result, responseErr = a.resumeRequestResponse(requestCtx, endpoint, version, sessionID, id, cursor, responseBudget)
	}
	if responseErr != nil {
		return nil, markRequestDispatched(responseErr)
	}
	return result, nil
}

// truncate returns s trimmed to at most n characters with an ellipsis.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func (a *HTTPClientAdapter) newHTTPMessageRequest(ctx context.Context, method, endpoint string, body []byte, version, sessionID, accept, lastEventID string) (*http.Request, error) {
	endpoint, err := resolveSameOriginEndpoint(a.cfg.URL, endpoint)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	a.applyAuthHeaders(req)
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	req.Header.Del("MCP-Protocol-Version")
	if version != "" {
		req.Header.Set("MCP-Protocol-Version", version)
	}
	req.Header.Del("Mcp-Session-Id")
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	req.Header.Del("Last-Event-ID")
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	return req, nil
}

func (a *HTTPClientAdapter) readRequestResponse(resp *http.Response, id int64, sessionID string, budget *responseByteBudget) (map[string]any, string, bool, error) {
	if resp.StatusCode != http.StatusOK {
		return nil, "", false, httpProtocolError("JSON-RPC POST returned HTTP status %d", resp.StatusCode)
	}
	if err := validateResponseSessionID(resp, sessionID); err != nil {
		return nil, "", false, err
	}
	return a.readRPCResponse(resp, id, budget)
}

func (a *HTTPClientAdapter) readRPCResponse(resp *http.Response, id int64, budget *responseByteBudget) (map[string]any, string, bool, error) {
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		return nil, "", false, httpProtocolError("invalid response Content-Type")
	}
	switch strings.ToLower(mediaType) {
	case "application/json":
		raw, readErr := readLimitedBodyWithBudget(resp.Body, budget)
		if readErr != nil {
			if errors.Is(readErr, errHTTPResponseTooLarge) {
				return nil, "", false, httpProtocolError("JSON-RPC response body exceeds size limit")
			}
			return nil, "", false, httpProtocolErrorCause("reading JSON-RPC response failed", readErr)
		}
		result, parseErr := parseJSONRPCResponse(raw, id)
		return result, "", false, parseErr
	case "text/event-stream":
		result, cursor, streamErr := readSSEResponseWithBudget(resp.Body, id, budget)
		if streamErr == nil {
			return result, cursor, false, nil
		}
		return nil, cursor, canResumeSSE(streamErr) && cursor != "", streamErr
	default:
		return nil, "", false, httpProtocolError("unsupported response Content-Type %q", mediaType)
	}
}

func validateResponseSessionID(resp *http.Response, current string) error {
	values, present := resp.Header[http.CanonicalHeaderKey("Mcp-Session-Id")]
	if !present {
		return nil
	}
	if len(values) != 1 || !validSessionID(values[0]) || values[0] != current {
		return httpProtocolError("server changed or returned an invalid MCP session ID")
	}
	return nil
}

func validSessionID(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < 0x21 || value[i] > 0x7e {
			return false
		}
	}
	return true
}

func validLastEventID(value string) bool {
	if value == "" || !utf8.ValidString(value) {
		return false
	}
	if value[0] == ' ' || value[0] == '\t' || value[len(value)-1] == ' ' || value[len(value)-1] == '\t' {
		return false
	}
	for i := 0; i < len(value); i++ {
		// WHATWG SSE permits UTF-8 event IDs except NUL, CR, and LF. A
		// Last-Event-ID request header must also follow HTTP field-value syntax:
		// internal SP/HTAB and obs-text are allowed, but other controls, DEL,
		// and leading/trailing whitespace are not.
		if (value[i] < 0x20 && value[i] != '\t') || value[i] == 0x7f {
			return false
		}
	}
	return true
}

func canResumeSSE(err error) bool {
	if errors.Is(err, errSSEResponseMissing) {
		return true
	}
	var boundaryErr *requestBoundaryError
	if errors.As(err, &boundaryErr) {
		return false
	}
	var protocolErr *HTTPProtocolError
	if errors.As(err, &protocolErr) {
		return false
	}
	var rpcErr *rpcResponseError
	return !errors.As(err, &rpcErr)
}

func readSSEResponse(body io.Reader, expectedID int64) (map[string]any, string, error) {
	return readSSEResponseWithBudget(body, expectedID, newResponseByteBudget(maxHTTPResponseSize))
}

func readSSEResponseWithBudget(body io.Reader, expectedID int64, budget *responseByteBudget) (map[string]any, string, error) {
	reader := bufio.NewReaderSize(budget.wrap(body), 64*1024)
	if prefix, _ := reader.Peek(3); len(prefix) == 3 && bytes.Equal(prefix, []byte{0xef, 0xbb, 0xbf}) {
		if _, err := reader.Discard(3); err != nil {
			return nil, "", err
		}
	}
	var dataLines []string
	eventID := ""
	eventIDSet := false
	cursor := ""

	dispatch := func() (map[string]any, bool, error) {
		if eventIDSet {
			cursor = ""
			if validLastEventID(eventID) {
				cursor = eventID
			}
		}
		if len(dataLines) == 0 {
			dataLines = nil
			eventID = ""
			eventIDSet = false
			return nil, false, nil
		}
		data := []byte(strings.Join(dataLines, "\n"))
		dataLines = nil
		eventID = ""
		eventIDSet = false
		result, isResponse, err := parseSSEMessage(data, expectedID)
		return result, isResponse, err
	}

	for {
		lineBytes, terminated, err := readSSELine(reader)
		if err != nil {
			if errors.Is(err, errHTTPResponseTooLarge) {
				return nil, cursor, httpProtocolErrorCause("SSE response body exceeds size limit", err)
			}
			if errors.Is(err, errSSELineTooLong) {
				return nil, cursor, httpProtocolError("SSE event line exceeds response limit")
			}
			if errors.Is(err, io.EOF) {
				return nil, cursor, errSSEResponseMissing
			}
			return nil, cursor, err
		}
		if !terminated {
			// An event whose final line lacks an end-of-line is incomplete and
			// must be discarded at EOF.
			return nil, cursor, errSSEResponseMissing
		}
		line := string(lineBytes)
		if line == "" {
			result, isResponse, err := dispatch()
			if err != nil {
				return nil, cursor, err
			}
			if isResponse {
				return result, cursor, nil
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, hasColon := strings.Cut(line, ":")
		if !hasColon {
			value = ""
		}
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "data":
			dataLines = append(dataLines, value)
		case "id":
			if !strings.ContainsRune(value, '\x00') {
				eventID = value
				eventIDSet = true
			}
		}
	}
}

func readSSELine(reader *bufio.Reader) ([]byte, bool, error) {
	line := make([]byte, 0, 128)
	for {
		value, err := reader.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) {
				if len(line) == 0 {
					return nil, false, io.EOF
				}
				return nil, false, nil
			}
			return nil, false, err
		}
		switch value {
		case '\n':
			return line, true, nil
		case '\r':
			next, peekErr := reader.Peek(1)
			if len(next) > 0 && next[0] == '\n' {
				if _, err := reader.ReadByte(); err != nil {
					return nil, false, err
				}
			} else if peekErr != nil && !errors.Is(peekErr, io.EOF) {
				return nil, false, peekErr
			}
			return line, true, nil
		default:
			line = append(line, value)
			if len(line) > maxHTTPResponseSize {
				return nil, false, errSSELineTooLong
			}
		}
	}
}

func parseSSEMessage(raw []byte, expectedID int64) (map[string]any, bool, error) {
	if err := validateJSONRPCObjectFields(raw, "SSE JSON-RPC message", "jsonrpc", "id", "method", "params", "result", "error"); err != nil {
		return nil, false, newRequestBoundaryError(fmt.Errorf("mcp: invalid SSE JSON-RPC message: %w", err), true, false)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope == nil {
		return nil, false, newRequestBoundaryError(fmt.Errorf("mcp: invalid SSE JSON-RPC message"), true, false)
	}
	var jsonrpc string
	if err := json.Unmarshal(envelope["jsonrpc"], &jsonrpc); err != nil || jsonrpc != "2.0" {
		return nil, false, newRequestBoundaryError(fmt.Errorf("mcp: invalid SSE JSON-RPC version"), true, false)
	}
	if methodRaw, hasMethod := envelope["method"]; hasMethod {
		if _, hasResult := envelope["result"]; hasResult {
			return nil, false, httpProtocolError("SSE JSON-RPC message has both method and result")
		}
		if _, hasError := envelope["error"]; hasError {
			return nil, false, httpProtocolError("SSE JSON-RPC message has both method and error")
		}
		var method string
		if err := json.Unmarshal(methodRaw, &method); err != nil || method == "" {
			return nil, false, httpProtocolError("SSE JSON-RPC method is invalid")
		}
		if _, hasID := envelope["id"]; hasID {
			return nil, false, httpProtocolError("server request in SSE stream is unsupported")
		}
		return nil, false, nil // A valid server notification precedes the response.
	}
	result, err := parseJSONRPCResponse(raw, expectedID)
	if err != nil {
		return nil, false, err
	}
	return result, true, nil
}

func (a *HTTPClientAdapter) resumeRequestResponse(ctx context.Context, endpoint, version, sessionID string, id int64, cursor string, budget *responseByteBudget) (map[string]any, error) {
	if !validLastEventID(cursor) {
		return nil, httpProtocolError("cannot resume SSE stream with an invalid event ID")
	}
	req, err := a.newHTTPMessageRequest(ctx, http.MethodGet, endpoint, nil, version, sessionID, "text/event-stream", cursor)
	if err != nil {
		return nil, err
	}
	resp, err := a.doHTTP(req)
	if err != nil {
		return nil, fmt.Errorf("mcp: resume SSE stream: %w", err)
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return nil, httpProtocolError("SSE resume GET returned HTTP status %d", resp.StatusCode)
	}
	if err := validateResponseSessionID(resp, sessionID); err != nil {
		return nil, err
	}
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, "text/event-stream") {
		return nil, httpProtocolError("SSE resume GET returned invalid Content-Type")
	}
	result, _, err := readSSEResponseWithBudget(resp.Body, id, budget)
	return result, err
}

func markRequestDispatched(err error) error {
	if err == nil || requestMayHaveBeenDispatched(err) {
		return err
	}
	var rpcErr *rpcResponseError
	if errors.As(err, &rpcErr) {
		return err
	}
	return newRequestBoundaryError(err, true, false)
}

// ProtocolVersion returns the protocol version negotiated during the
// initialize handshake, or "" before Connect.
func (a *HTTPClientAdapter) ProtocolVersion() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.current == nil || !a.current.connected || a.current.closing {
		return ""
	}
	return a.current.protocolVersion
}

// applyAuthHeaders applies only caller-provided headers and a preconfigured
// bearer token. OAuth negotiation is rejected before the adapter connects.
func (a *HTTPClientAdapter) applyAuthHeaders(req *http.Request) {
	for k, v := range a.cfg.Headers {
		req.Header.Set(k, v)
	}
	if a.cfg.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+a.cfg.BearerToken)
	}
}

// doInitialize performs the 2025-06-18 initialize/initialized exchange and
// returns any session identity learned before a later handshake error.
func (a *HTTPClientAdapter) doInitialize(ctx context.Context, endpoint string, id int64) (sessionID, version string, retErr error) {
	defer func() {
		if recover() != nil {
			retErr = httpProtocolError("initialize handshake operation panicked; remote session state may be unknown")
		}
	}()
	initParams := map[string]any{
		"protocolVersion": streamableHTTPProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "go-cli", "version": "1.0.0"},
	}
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "initialize",
		"params":  initParams,
	})
	if err != nil {
		return "", "", fmt.Errorf("mcp: marshal initialize request: %w", err)
	}
	req, err := a.newHTTPMessageRequest(ctx, http.MethodPost, endpoint, body, "", "", "application/json, text/event-stream", "")
	if err != nil {
		return "", "", fmt.Errorf("mcp: build initialize request: %w", err)
	}
	resp, err := a.doHTTP(req)
	if err != nil {
		return "", "", fmt.Errorf("mcp: initialize request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusUnauthorized {
			return "", "", httpProtocolError("authentication required (HTTP 401)")
		}
		return "", "", httpProtocolError("initialize POST returned HTTP status %d", resp.StatusCode)
	}
	sessionID, err = initializeSessionID(resp)
	if err != nil {
		return sessionID, "", err
	}
	result, _, _, err := a.readRPCResponse(resp, id, newResponseByteBudget(maxHTTPResponseSize))
	if err != nil {
		return sessionID, "", fmt.Errorf("mcp: invalid initialize response: %w", err)
	}
	version, _ = result["protocolVersion"].(string)
	if version != streamableHTTPProtocolVersion {
		return sessionID, version, httpProtocolError("server negotiated unsupported protocol version %q", version)
	}

	notification, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	})
	if err != nil {
		return sessionID, version, fmt.Errorf("mcp: marshal initialized notification: %w", err)
	}
	notifReq, err := a.newHTTPMessageRequest(ctx, http.MethodPost, endpoint, notification, version, sessionID, "application/json, text/event-stream", "")
	if err != nil {
		return sessionID, version, fmt.Errorf("mcp: build initialized notification: %w", err)
	}
	notifResp, err := a.doHTTP(notifReq)
	if err != nil {
		return sessionID, version, fmt.Errorf("mcp: send initialized notification: %w", err)
	}
	defer func() { _ = notifResp.Body.Close() }()
	if notifResp.StatusCode != http.StatusAccepted {
		return sessionID, version, httpProtocolError("initialized notification returned HTTP status %d, want 202", notifResp.StatusCode)
	}
	if err := validateResponseSessionID(notifResp, sessionID); err != nil {
		return sessionID, version, err
	}
	bodyBytes, err := readLimitedBody(notifResp.Body)
	if err != nil {
		return sessionID, version, fmt.Errorf("mcp: read initialized notification acknowledgement: %w", err)
	}
	if len(bodyBytes) != 0 {
		return sessionID, version, httpProtocolError("initialized notification acknowledgement must have an empty body")
	}

	return sessionID, version, nil
}

func initializeSessionID(resp *http.Response) (string, error) {
	values, present := resp.Header[http.CanonicalHeaderKey("Mcp-Session-Id")]
	if !present {
		return "", nil
	}
	if len(values) != 1 || !validSessionID(values[0]) {
		return "", httpProtocolError("initialize response contained an invalid MCP session ID")
	}
	return values[0], nil
}
