package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// JSONRPCLineTransport is an in-process transport used by the SDK adapters to
// exchange newline-delimited JSON-RPC messages with an MCP server. It reads
// request/response frames from an io.Reader and writes them to an io.Writer.
//
// For the Stdio transport the reader/writer are the spawned subprocess's
// stdout/stdin pipes; for the HTTP/SSE transports they would be backed by an
// HTTP stream. Because it only needs a reader/writer pair, tests can drive the
// adapters over an in-memory connection without importing any external SDK.
type JSONRPCLineTransport struct {
	in     *bufio.Reader
	out    io.Writer
	closeF func() error
	owned  *stdioProcessOwner

	mu            sync.Mutex
	nextID        int64
	notifications chan json.RawMessage
}

// NewJSONRPCLineTransport returns a transport that writes frames to out and
// reads them from in. closeFn is invoked when Close is called and may be nil.
func NewJSONRPCLineTransport(in io.Reader, out io.Writer, closeFn func() error) *JSONRPCLineTransport {
	return &JSONRPCLineTransport{
		in:            bufio.NewReader(in),
		out:           out,
		closeF:        closeFn,
		notifications: make(chan json.RawMessage, 16),
	}
}

// defaultJSONRPCTimeout bounds individual JSON-RPC requests when the caller
// has not set its own deadline. This prevents a hung server from blocking
// the agent loop indefinitely.
const defaultJSONRPCTimeout = 30 * time.Second

// maxStdioResponseSize matches the HTTP adapter's established response cap.
// A single Request accounts for all consumed frames, including notifications
// and responses with unrelated IDs.
const maxStdioResponseSize = maxHTTPResponseSize

var errStdioResponseTooLarge = fmt.Errorf("response exceeds %d byte limit", maxStdioResponseSize)

// Request sends a JSON-RPC request with a fresh id and blocks until a response
// with a matching id arrives. It returns the parsed result object. Cancellation
// interrupts blocking I/O only when this transport owns a stdio child; a
// caller-owned Reader must be released by its owner because this transport
// cannot close it safely.
//
// If the caller's context has no deadline, a default 30-second timeout is
// applied. For caller-owned readers, an active blocking Read still has to be
// released by the reader's owner; only a process-backed stdio transport can
// actively interrupt that I/O on timeout.
func (t *JSONRPCLineTransport) Request(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	// Apply a default timeout when the caller hasn't set a deadline.
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultJSONRPCTimeout)
		defer cancel()
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, contextCause(ctx)
	}

	id := t.nextID
	t.nextID++
	req, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return nil, fmt.Errorf("mcp: marshal request: %w", err)
	}

	owner := t.owned
	if owner != nil && !owner.beginIO() {
		cause := newRequestBoundaryError(fmt.Errorf("mcp: stdio process is stopping"), false, false)
		return nil, &stdioPendingError{operation: "request", cause: cause, done: owner.done}
	}
	if owner != nil {
		defer owner.endIO()
	}

	type requestCancellation struct {
		mu       sync.Mutex
		finished bool
		canceled bool
	}
	var cancellation requestCancellation
	var callbackDone chan struct{}
	var stopCallback func() bool
	watchJoined := false
	finishWatch := func() bool {
		if owner == nil || watchJoined {
			return false
		}
		cancellation.mu.Lock()
		cancellation.finished = true
		wasCanceled := cancellation.canceled
		cancellation.mu.Unlock()
		if !stopCallback() {
			<-callbackDone
		}
		cancellation.mu.Lock()
		wasCanceled = wasCanceled || cancellation.canceled
		cancellation.mu.Unlock()
		watchJoined = true
		return wasCanceled
	}
	if owner != nil {
		callbackDone = make(chan struct{})
		stopCallback = context.AfterFunc(ctx, func() {
			defer close(callbackDone)
			cancellation.mu.Lock()
			if !cancellation.finished {
				cancellation.canceled = true
				owner.startStop()
			}
			cancellation.mu.Unlock()
		})
		defer finishWatch()
	}

	ownedRequestError := func(cause error, dispatchPossible, retryable bool) error {
		if cause == nil {
			cause = context.Canceled
		}
		boundary := newRequestBoundaryError(cause, dispatchPossible, retryable)
		if owner == nil {
			return boundary
		}
		return &stdioPendingError{
			operation: "request",
			cause:     boundary,
			done:      owner.done,
			unknown:   dispatchPossible,
		}
	}
	if err := ctx.Err(); err != nil {
		if owner != nil {
			owner.startStop()
			finishWatch()
			return nil, ownedRequestError(contextCause(ctx), false, false)
		}
		return nil, contextCause(ctx)
	}

	if _, err := fmt.Fprintln(t.out, string(req)); err != nil { //nolint:errcheck
		if owner != nil {
			wasStopping := owner.isStopping()
			owner.startStop()
			wasCanceled := finishWatch()
			cause := fmt.Errorf("mcp: write request: %w", err)
			if wasCanceled || ctx.Err() != nil {
				cause = contextCause(ctx)
			}
			return nil, ownedRequestError(cause, true, !wasStopping && ctx.Err() == nil)
		}
		return nil, newRequestBoundaryError(fmt.Errorf("mcp: write request: %w", err), true, true)
	}

	budget := newResponseByteBudget(int64(maxStdioResponseSize))
	for {
		select {
		case <-ctx.Done():
			if owner != nil {
				owner.startStop()
				finishWatch()
				return nil, ownedRequestError(contextCause(ctx), true, false)
			}
			return nil, newRequestBoundaryError(contextCause(ctx), true, false)
		default:
		}

		line, readErr := readLimitedStdioLine(t.in, budget)
		if readErr != nil {
			if errors.Is(readErr, errStdioResponseTooLarge) {
				cause := &stdioProtocolError{cause: readErr}
				if owner != nil {
					owner.startStop()
					finishWatch()
					return nil, ownedRequestError(cause, true, false)
				}
				return nil, newRequestBoundaryError(cause, true, false)
			}
			cause := fmt.Errorf("mcp: read response: %w", readErr)
			if owner != nil {
				wasStopping := owner.isStopping()
				owner.startStop()
				wasCanceled := finishWatch()
				if wasCanceled || ctx.Err() != nil {
					cause = contextCause(ctx)
				}
				return nil, ownedRequestError(cause, true, !wasStopping && !wasCanceled && ctx.Err() == nil)
			}
			return nil, newRequestBoundaryError(cause, true, true)
		}

		var frame map[string]json.RawMessage
		if err := json.Unmarshal(line, &frame); err != nil {
			continue
		}

		// Forward notifications (frames without an id field) to the
		// notification channel. Non-blocking: if the channel is full the
		// notification is dropped so request/response handling is never
		// stalled.
		if _, hasID := frame["id"]; !hasID {
			select {
			case t.notifications <- json.RawMessage(strings.TrimSpace(string(line))):
			default:
			}
			continue
		}

		var responseID int64
		if len(frame["id"]) == 0 || json.Unmarshal(frame["id"], &responseID) != nil || responseID != id {
			continue
		}
		result, responseErr := parseJSONRPCResponse(line, id)
		if owner != nil && finishWatch() {
			return nil, ownedRequestError(contextCause(ctx), true, false)
		}
		return result, responseErr
	}
}

func readLimitedStdioLine(reader *bufio.Reader, budget *responseByteBudget) ([]byte, error) {
	var line []byte
	for {
		if budget.remaining <= 0 {
			return nil, errStdioResponseTooLarge
		}

		fragment, err := reader.ReadSlice('\n')
		if int64(len(fragment)) > budget.remaining || len(fragment) > maxStdioResponseSize-len(line) {
			budget.remaining = 0
			return nil, errStdioResponseTooLarge
		}
		budget.remaining -= int64(len(fragment))

		var ok bool
		line, ok = appendStdioResponseFragment(line, fragment)
		if !ok {
			budget.remaining = 0
			return nil, errStdioResponseTooLarge
		}
		if err == nil {
			return line, nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			// A full budget without a delimiter cannot become an in-budget frame.
			if budget.remaining == 0 {
				return nil, errStdioResponseTooLarge
			}
			continue
		}
		return line, err
	}
}

func appendStdioResponseFragment(line, fragment []byte) ([]byte, bool) {
	if len(fragment) > maxStdioResponseSize-len(line) {
		return nil, false
	}
	needed := len(line) + len(fragment)
	if needed > cap(line) {
		capacity := cap(line) * 2
		if capacity < 4096 {
			capacity = 4096
		}
		if capacity < needed {
			capacity = needed
		}
		if capacity > maxStdioResponseSize {
			capacity = maxStdioResponseSize
		}
		grown := make([]byte, len(line), capacity)
		copy(grown, line)
		line = grown
	}
	return append(line, fragment...), true
}

type stdioProtocolError struct {
	cause error
}

func (e *stdioProtocolError) Error() string {
	return fmt.Sprintf("mcp: stdio protocol error: %v", e.cause)
}

func (e *stdioProtocolError) Unwrap() error { return e.cause }

func (*stdioProtocolError) isStdioProtocolError() {}

// Send sends a JSON-RPC notification (no id, no response expected) to the
// server. It is used for the notifications/initialized message in the MCP
// handshake.
func (t *JSONRPCLineTransport) Send(method string, params map[string]any) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.owned != nil && !t.owned.beginIO() {
		return &stdioPendingError{operation: "send", cause: fmt.Errorf("mcp: stdio process is stopping"), done: t.owned.done}
	}
	if t.owned != nil {
		defer t.owned.endIO()
	}

	msg, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return fmt.Errorf("mcp: marshal notification: %w", err)
	}
	if _, err := fmt.Fprintln(t.out, string(msg)); err != nil { //nolint:errcheck
		if t.owned != nil {
			t.owned.startStop()
			return &stdioPendingError{operation: "send", cause: fmt.Errorf("mcp: write notification: %w", err), done: t.owned.done, unknown: true}
		}
		return fmt.Errorf("mcp: write notification: %w", err)
	}
	return nil
}

// Close releases the underlying connection, if one was supplied.
func (t *JSONRPCLineTransport) Close() error {
	if t.owned != nil {
		completed, err := t.owned.stopAndWait(context.Background(), "close")
		if !completed {
			return err
		}
		if err != nil {
			return err
		}
	}
	if t.closeF != nil {
		return t.closeF()
	}
	return nil
}

// Notifications returns a read-only channel that yields JSON-RPC notification
// frames (messages without an id field) received from the server. The channel
// is buffered; if it is full, additional notifications are silently dropped so
// that request/response handling is never blocked.
func (t *JSONRPCLineTransport) Notifications() <-chan json.RawMessage {
	return t.notifications
}

// stdioProcessOwner is the sole owner of a child started for the stdio
// adapter. Its Done channel closes only after the exact exec.Cmd has been
// waited and all active transport I/O has returned.
type stdioProcessOwner struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser

	mu       sync.Mutex
	cond     *sync.Cond
	activeIO int
	stopping bool
	done     chan struct{}
	waitErr  error
}

func newStdioProcessOwner(cmd *exec.Cmd, stdin io.WriteCloser, stdout io.ReadCloser) *stdioProcessOwner {
	owner := &stdioProcessOwner{
		cmd:    cmd,
		stdin:  stdin,
		stdout: stdout,
		done:   make(chan struct{}),
	}
	owner.cond = sync.NewCond(&owner.mu)
	return owner
}

func (o *stdioProcessOwner) beginIO() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.stopping {
		return false
	}
	o.activeIO++
	return true
}

func (o *stdioProcessOwner) endIO() {
	o.mu.Lock()
	o.activeIO--
	if o.activeIO == 0 {
		o.cond.Broadcast()
	}
	o.mu.Unlock()
}

// startStop elects one cleanup goroutine. The process handle and pipes are
// retained until that goroutine has closed the owned pipes, killed and waited
// the child, and all transport operations have unwound.
func (o *stdioProcessOwner) startStop() <-chan struct{} {
	o.mu.Lock()
	if !o.stopping {
		o.stopping = true
		go o.stopAndJoin()
	}
	o.mu.Unlock()
	return o.done
}

func (o *stdioProcessOwner) stopAndJoin() {
	if o.stdin != nil {
		_ = o.stdin.Close()
	}
	if o.stdout != nil {
		_ = o.stdout.Close()
	}

	var cleanupErr error
	if o.cmd.Process != nil {
		if err := o.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			cleanupErr = err
		}
	}
	if err := o.cmd.Wait(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			cleanupErr = errors.Join(cleanupErr, err)
		}
	}

	o.mu.Lock()
	for o.activeIO > 0 {
		o.cond.Wait()
	}
	o.waitErr = cleanupErr
	close(o.done)
	o.mu.Unlock()
}

func (o *stdioProcessOwner) isStopping() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.stopping
}

func (o *stdioProcessOwner) stopAndWait(ctx context.Context, operation string) (bool, error) {
	o.startStop()
	completed, err := waitForMCPDone(ctx, operation, o.done)
	if completed {
		return true, o.waitErr
	}
	return false, err
}

func waitForMCPDone(ctx context.Context, operation string, done <-chan struct{}) (bool, error) {
	select {
	case <-done:
		return true, nil
	default:
	}
	select {
	case <-done:
		return true, nil
	case <-ctx.Done():
		select {
		case <-done:
			return true, nil
		default:
			return false, &stdioPendingError{operation: operation, cause: ctx.Err(), done: done}
		}
	}
}

// stdioPendingError keeps the exact process completion receipt visible through
// error wrapping. For dispatched requests it also carries the unknown-outcome
// fact; for teardown waiters it only reports local cleanup still in progress.
type stdioPendingError struct {
	operation string
	cause     error
	done      <-chan struct{}
	unknown   bool
}

func (e *stdioPendingError) Error() string {
	return fmt.Sprintf("mcp: stdio %s pending: %v", e.operation, e.cause)
}

func (e *stdioPendingError) Unwrap() error                  { return e.cause }
func (e *stdioPendingError) Done() <-chan struct{}          { return e.done }
func (e *stdioPendingError) ExecutionDone() <-chan struct{} { return e.done }
func (e *stdioPendingError) OutcomeUnknown() bool           { return e.unknown }

// AdapterOption configures an SDK adapter.
type AdapterOption func(*adapterCore)

// WithConnection overrides the default transport with a caller-supplied
// in-process connection. It is primarily used by tests to drive an adapter
// against an in-memory server without spawning a subprocess. The adapter does
// not own the supplied reader or writer, so their owner must release any
// blocking I/O; Connect and Request cannot safely interrupt a caller-owned
// read.
func WithConnection(conn *JSONRPCLineTransport) AdapterOption {
	return func(c *adapterCore) { c.conn = conn; c.externalConn = true }
}

// WithoutInitialize disables the automatic MCP initialize/initialized
// handshake during Connect. It is used by tests that drive lightweight fake
// servers which do not implement the initialize method.
func WithoutInitialize() AdapterOption {
	return func(c *adapterCore) { c.skipInitialize = true }
}

// adapterCore holds the shared state and MCPClient logic used by both SDK
// adapters. The OfficialSDKAdapter and Mark3labsAdapter embed it so they are
// interchangeable MCPClient implementations backed by the same self-contained
// transport (no external SDK dependency).
type adapterCore struct {
	cfg          MCPServerConfig
	conn         *JSONRPCLineTransport
	externalConn bool
	proc         *exec.Cmd
	procOwner    *stdioProcessOwner
	connecting   bool
	connectDone  chan struct{}
	connectStop  context.CancelFunc

	mu                sync.Mutex
	connected         bool
	maxReconnect      int
	reconnectAttempts int
	skipInitialize    bool
	protocolVersion   string
}

// defaultMaxReconnect is the number of reconnection attempts made when a Call
// fails due to a transport-level connection loss before giving up.
const defaultMaxReconnect = 3

// newAdapterCore builds the core for a given config and options.
func newAdapterCore(cfg MCPServerConfig, opts []AdapterOption) *adapterCore {
	c := &adapterCore{cfg: cfg, maxReconnect: defaultMaxReconnect}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Connect establishes the session. For a Stdio server it spawns the configured
// command and wraps its stdin/stdout in a JSON-RPC line transport.
func (c *adapterCore) Connect(ctx context.Context) error {
	for {
		c.mu.Lock()
		if c.connected {
			if owner := c.procOwner; owner == nil || !owner.isStopping() {
				c.mu.Unlock()
				return nil
			}
			c.connected = false
		}
		if c.connecting {
			done := c.connectDone
			c.mu.Unlock()
			completed, err := waitForMCPDone(ctx, "connect", done)
			if !completed {
				return err
			}
			continue
		}
		if previous := c.procOwner; previous != nil {
			c.mu.Unlock()
			completed, err := previous.stopAndWait(ctx, "connect")
			if !completed {
				return err
			}
			c.mu.Lock()
			if c.procOwner == previous {
				c.procOwner = nil
				c.proc = nil
				if !c.externalConn {
					c.conn = nil
				}
			}
			c.mu.Unlock()
			if err != nil {
				return fmt.Errorf("mcp: previous stdio cleanup: %w", err)
			}
			continue
		}

		if c.conn == nil {
			if c.cfg.Transport != MCPTransportStdio {
				c.mu.Unlock()
				return fmt.Errorf("mcp: transport %s not supported without an explicit connection", c.cfg.Transport)
			}
			if strings.TrimSpace(c.cfg.Command) == "" {
				c.mu.Unlock()
				return fmt.Errorf("mcp: server %q requires a command for stdio transport", c.cfg.Name)
			}

			cmd := exec.Command(c.cfg.Command, c.cfg.Args...) //nolint:gosec // G204: server executable comes from trusted config
			cmd.Env = append(cmd.Environ(), c.cfg.Env...)
			stdin, err := cmd.StdinPipe()
			if err != nil {
				c.mu.Unlock()
				return fmt.Errorf("mcp: stdin pipe: %w", err)
			}
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				_ = stdin.Close()
				c.mu.Unlock()
				return fmt.Errorf("mcp: stdout pipe: %w", err)
			}
			if err := cmd.Start(); err != nil {
				_ = stdin.Close()
				_ = stdout.Close()
				c.mu.Unlock()
				return fmt.Errorf("mcp: start server %q: %w", c.cfg.Name, err)
			}
			owner := newStdioProcessOwner(cmd, stdin, stdout)
			c.proc = cmd
			c.procOwner = owner
			c.conn = NewJSONRPCLineTransport(stdout, stdin, nil)
			c.conn.owned = owner
		}

		owner := c.procOwner
		conn := c.conn
		skipInitialize := c.skipInitialize
		handshakeCtx, cancelHandshake := context.WithCancel(ctx)
		connectDone := make(chan struct{})
		c.connecting = true
		c.connectDone = connectDone
		c.connectStop = cancelHandshake
		c.mu.Unlock()

		var version string
		var connectErr error
		if !skipInitialize {
			version, connectErr = doInitialize(handshakeCtx, conn)
		}

		c.mu.Lock()
		current := c.connecting && c.connectDone == connectDone && c.procOwner == owner && c.conn == conn
		if connectErr == nil && current && handshakeCtx.Err() == nil && (owner == nil || !owner.isStopping()) {
			c.connected = true
			c.protocolVersion = version
		} else {
			c.connected = false
			if connectErr == nil {
				connectErr = handshakeCtx.Err()
				if connectErr == nil {
					connectErr = errors.New("mcp: connect operation was interrupted")
				}
			}
			if owner != nil {
				owner.startStop()
			}
		}
		if c.connecting && c.connectDone == connectDone {
			c.connecting = false
			c.connectDone = nil
			c.connectStop = nil
			close(connectDone)
		}
		c.mu.Unlock()
		cancelHandshake()

		if connectErr == nil {
			return nil
		}
		if owner != nil {
			completed, cleanupErr := owner.stopAndWait(ctx, "connect")
			if completed {
				c.mu.Lock()
				if c.procOwner == owner {
					c.conn = nil
				}
				c.mu.Unlock()
			}
			if cleanupErr != nil {
				connectErr = errors.Join(connectErr, cleanupErr)
			}
		}
		return fmt.Errorf("mcp: initialize handshake: %w", connectErr)
	}
}

// Disconnect tears down the session and stops any spawned subprocess.
func (c *adapterCore) Disconnect(ctx context.Context) error {
	c.mu.Lock()
	if !c.connected && c.procOwner == nil && !c.connecting {
		c.mu.Unlock()
		return nil
	}
	c.connected = false
	owner := c.procOwner
	connectDone := c.connectDone
	if c.connectStop != nil {
		c.connectStop()
	}
	if owner != nil {
		owner.startStop()
	}
	external := c.externalConn
	conn := c.conn
	if owner == nil && !external {
		c.conn = nil
	}
	c.mu.Unlock()

	if owner != nil {
		completed, err := owner.stopAndWait(ctx, "disconnect")
		if !completed {
			return err
		}
		if connectDone != nil {
			completed, err = waitForMCPDone(ctx, "disconnect", connectDone)
			if !completed {
				return err
			}
		}
		c.mu.Lock()
		if c.procOwner == owner {
			c.procOwner = nil
			c.proc = nil
			if !external {
				c.conn = nil
			}
		}
		c.mu.Unlock()
		if err != nil {
			return err
		}
	} else if connectDone != nil {
		completed, err := waitForMCPDone(ctx, "disconnect", connectDone)
		if !completed {
			return err
		}
	} else if !external && conn != nil {
		if err := conn.Close(); err != nil {
			return err
		}
	}

	slog.Info("mcp.disconnect", "server", c.cfg.Name, "transport", c.cfg.Transport)
	return nil
}

// reconnect tears down the current session and re-establishes it. It is called
// by callWithReconnect when a transport-level error suggests the connection was
// lost. The caller is responsible for enforcing the maxReconnect budget and
// backoff between attempts.
func (c *adapterCore) reconnect(ctx context.Context) error {
	// Disconnect ignoring errors — we are already in a degraded state.
	_ = c.Disconnect(ctx) //nolint:errcheck // best-effort teardown
	return c.Connect(ctx)
}

// reconnectBackoff returns the delay before the (zero-based) attempt-th
// reconnection try: 1s, 2s, 4s, 8s, ... capped at 8s.
func reconnectBackoff(attempt int) time.Duration {
	d := time.Second
	for i := 0; i < attempt; i++ {
		d *= 2
		if d >= 8*time.Second {
			return 8 * time.Second
		}
	}
	return d
}

// isConnectionError reports whether err likely indicates a transport-level
// connection loss rather than a server-side RPC error. RPC errors are
// server-level responses and must not trigger a reconnect.
func isConnectionError(err error) bool {
	if err == nil {
		return false
	}
	var boundaryErr *requestBoundaryError
	return errors.As(err, &boundaryErr) && boundaryErr.retryable
}

func toolCallOutcomeError(name string, err error) error {
	if !requestMayHaveBeenDispatched(err) {
		return err
	}
	return &unknownToolOutcomeError{tool: name, cause: err}
}

// requestLocked snapshots c.conn under c.mu and sends a JSON-RPC request.
// Holding the lock only for the pointer read (not the blocking Request call)
// prevents a nil-pointer dereference when a concurrent Disconnect or reconnect
// nils out c.conn between the read and the call.
func (c *adapterCore) requestLocked(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	c.mu.Lock()
	conn := c.conn
	connecting := c.connecting
	c.mu.Unlock()
	if connecting {
		return nil, fmt.Errorf("mcp: connection is initializing")
	}
	if conn == nil {
		return nil, fmt.Errorf("mcp: connection is not established")
	}
	return conn.Request(ctx, method, params)
}

// callWithReconnect sends a JSON-RPC request and, when the error looks like a
// transport-level connection loss, attempts to reconnect (with exponential
// backoff) up to maxReconnect times before returning. On a successful
// reconnect the request is retried once.
func (c *adapterCore) callWithReconnect(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	res, err := c.requestLocked(ctx, method, params)
	if err == nil {
		return res, nil
	}
	if !isConnectionError(err) {
		return nil, err
	}

	for attempt := 0; attempt < c.maxReconnect; attempt++ {
		c.mu.Lock()
		c.reconnectAttempts++
		c.mu.Unlock()

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(reconnectBackoff(attempt)):
		}

		if rErr := c.reconnect(ctx); rErr != nil {
			err = rErr
			continue
		}
		// Reconnected — retry the request.
		if res, err = c.requestLocked(ctx, method, params); err == nil {
			return res, nil
		}
		if !isConnectionError(err) {
			return nil, err
		}
	}
	return nil, err
}

// ListTools returns the tools declared by the server via the tools/list
// JSON-RPC method.
func (c *adapterCore) ListTools(ctx context.Context) ([]MCPTool, error) {
	return listToolPages(ctx, func(ctx context.Context, params map[string]any) (map[string]any, error) {
		return c.callWithReconnect(ctx, "tools/list", params)
	})
}

// CallTool invokes the named tool via the tools/call JSON-RPC method, emitting
// an mcp.tool.call span with tool_name/server/transport attributes.
func (c *adapterCore) CallTool(ctx context.Context, name string, args map[string]any) (*MCPToolResult, error) {
	span, spanCtx := tracing.SpanFromContext(ctx, "mcp.tool.call", tracing.SpanKindInternal)
	logger := tracing.NewTraceLogger(span, slog.Default())
	span.SetAttributes(
		tracing.Attribute{Key: "tool_name", Value: name},
		tracing.Attribute{Key: "server", Value: c.cfg.Name},
		tracing.Attribute{Key: "transport", Value: c.cfg.Transport.String()},
	)
	defer span.End()

	start := time.Now()
	res, err := c.requestLocked(spanCtx, "tools/call", map[string]any{
		"name":      name,
		"arguments": args,
	})
	var result *MCPToolResult
	if err == nil {
		result, err = decodeCallToolResult(res)
	}
	if err != nil {
		err = toolCallOutcomeError(name, err)
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("mcp.tool.call.failed", "tool", name, "server", c.cfg.Name, "err", err)
		return nil, fmt.Errorf("mcp: call tool %q: %w", name, err)
	}

	span.SetAttributes(tracing.Attribute{Key: "success", Value: true})
	logger.Info("mcp.tool.call",
		"tool", name,
		"server", c.cfg.Name,
		"duration_ms", time.Since(start).Milliseconds(),
		"success", true)

	return result, nil
}

// Name returns the logical server name.
func (c *adapterCore) Name() string { return c.cfg.Name }

// ProtocolVersion returns the protocol version negotiated during the
// initialize handshake, or "" before Connect.
func (c *adapterCore) ProtocolVersion() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.protocolVersion
}

// Notifications returns a read-only channel that yields JSON-RPC notification
// frames received from the server, or nil if the adapter is not connected.
func (c *adapterCore) Notifications() <-chan json.RawMessage {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return nil
	}
	return conn.Notifications()
}

// doInitialize performs the MCP initialize/initialized handshake over the
// supplied connection without holding adapterCore.mu while I/O blocks.
func doInitialize(ctx context.Context, conn *JSONRPCLineTransport) (string, error) {
	if conn == nil {
		return "", fmt.Errorf("mcp: connection is not established")
	}
	res, err := conn.Request(ctx, "initialize", map[string]any{
		"protocolVersion": LatestProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "go-cli", "version": "1.0.0"},
	})
	if err != nil {
		return "", err
	}
	version, _ := res["protocolVersion"].(string)
	if !IsSupportedProtocolVersion(version) {
		return "", fmt.Errorf("unsupported protocol version: %s", version)
	}
	if err := conn.Send("notifications/initialized", map[string]any{}); err != nil {
		return "", err
	}
	return version, nil
}

// OfficialSDKAdapter is an MCPClient adapter that corresponds to the official
// Go MCP SDK's client transports. It uses a self-contained in-process
// transport (see JSONRPCLineTransport) so no external SDK module is required.
type OfficialSDKAdapter struct {
	*adapterCore
}

var _ MCPClient = (*OfficialSDKAdapter)(nil)

// NewOfficialSDKAdapter returns an OfficialSDKAdapter for the given config.
func NewOfficialSDKAdapter(cfg MCPServerConfig, opts ...AdapterOption) *OfficialSDKAdapter {
	return &OfficialSDKAdapter{adapterCore: newAdapterCore(cfg, opts)}
}

// Mark3labsAdapter is an MCPClient adapter symmetric to OfficialSDKAdapter. It
// corresponds to the mark3labs/mcp-go SDK's client role but is implemented it
// against the same self-contained transport, keeping both adapters
// interchangeable with no external dependency.
type Mark3labsAdapter struct {
	*adapterCore
}

var _ MCPClient = (*Mark3labsAdapter)(nil)

// NewMark3labsAdapter returns a Mark3labsAdapter for the given config.
func NewMark3labsAdapter(cfg MCPServerConfig, opts ...AdapterOption) *Mark3labsAdapter {
	return &Mark3labsAdapter{adapterCore: newAdapterCore(cfg, opts)}
}

// stringOf returns the string value of v, or "" when v is nil or not a string.
func stringOf(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
