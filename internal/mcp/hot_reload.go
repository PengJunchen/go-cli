package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// HotReloader watches an MCP server config file and, whenever it changes,
// tears down and re-establishes the MCPClient session and re-registers the
// tools it advertises. The interface is deliberately small so a watch can be
// driven by the default poller or a native filesystem watcher behind the same
// façade.
type HotReloader interface {
	// Watch starts watching the config file at configPath and begins the
	// reconnect-on-change loop. It returns an error when the reloader is
	// already watching, already stopped, or given an empty path.
	Watch(ctx context.Context, configPath string) error
	// Stop closes admission, cancels the watcher, and blocks until poller,
	// reconnect, and registration work have actually returned. Stop is
	// idempotent and can wait indefinitely for a callback that ignores context.
	Stop() error
	// Reload manually triggers one reconnect cycle without a file change.
	Reload(ctx context.Context) error
	// Name returns the logical name of the reloader.
	Name() string
}

// HotReloadLifecycle exposes the stop request and actual completion signal for
// lifecycle owners that must retain dependencies until all callbacks return.
// It is an optional capability so existing HotReloader implementations remain
// source-compatible.
type HotReloadLifecycle interface {
	// RequestStop closes admission and cancels the reloader's source without
	// waiting for in-flight client or registration callbacks.
	RequestStop()
	// Done is stable for the lifetime of the reloader and closes after all
	// poller, reconnect, and registration work has actually returned.
	Done() <-chan struct{}
	// Wait observes actual completion, bounded only by the caller's context.
	// It does not request a stop.
	Wait(ctx context.Context) error
}

// ErrHotReloadPending means the caller stopped waiting before the hot reload
// owner and its callbacks had actually returned.
var ErrHotReloadPending = errors.New("mcp: hot reload cleanup pending")

// ErrHotReloadCallbackPanic reports that a reload callback panicked. The panic
// value is deliberately omitted because it may contain sensitive data.
var ErrHotReloadCallbackPanic = errors.New("mcp: hot reload callback panicked")

// ErrHotReloadWorkerPanic reports an unexpected panic in the reloader owner.
var ErrHotReloadWorkerPanic = errors.New("mcp: hot reload worker panicked")

// ErrHotReloadInvalidPollInterval rejects a poll interval that cannot create a
// time.Ticker.
var ErrHotReloadInvalidPollInterval = errors.New("mcp: hot reload poll interval must be positive")

// HotReloadPendingError reports a waiter timeout while the reloader remains
// owned and active.
type HotReloadPendingError struct {
	Cause error
}

func (e *HotReloadPendingError) Error() string {
	if e == nil || e.Cause == nil {
		return ErrHotReloadPending.Error()
	}
	return ErrHotReloadPending.Error() + ": " + e.Cause.Error()
}

func (e *HotReloadPendingError) Unwrap() error {
	if e == nil {
		return ErrHotReloadPending
	}
	return e.Cause
}

func (e *HotReloadPendingError) Is(target error) bool {
	return target == ErrHotReloadPending
}

// HotReloadCallbackPanicError is a sanitized terminal error for a panic from a
// reload callback.
type HotReloadCallbackPanicError struct{}

func (*HotReloadCallbackPanicError) Error() string { return ErrHotReloadCallbackPanic.Error() }

func (*HotReloadCallbackPanicError) Is(target error) bool {
	return target == ErrHotReloadCallbackPanic
}

// HotReloadWorkerPanicError is a sanitized terminal error for an unexpected
// panic while reconnect work is running.
type HotReloadWorkerPanicError struct{}

func (*HotReloadWorkerPanicError) Error() string { return ErrHotReloadWorkerPanic.Error() }

func (*HotReloadWorkerPanicError) Is(target error) bool {
	return target == ErrHotReloadWorkerPanic
}

// RegisterToolsFunc re-registers the currently advertised MCP tools after a
// reconnect. It is called with the tools reported by the reconnected client.
type RegisterToolsFunc func(tools []MCPTool)

// MCPConfigReloadFunc builds and publishes one complete config-source
// generation. A returned error records a failed attempt while leaving the
// watcher active for a later change or manual reload.
type MCPConfigReloadFunc func(context.Context, string) error

// MCPConfigReloadErrorClass is a finite, redacted category for a failed
// config-generation attempt. It never includes callback error text.
type MCPConfigReloadErrorClass string

const (
	MCPConfigReloadErrorCandidate MCPConfigReloadErrorClass = "candidate_error"
	MCPConfigReloadErrorCanceled  MCPConfigReloadErrorClass = "context_canceled"
	MCPConfigReloadErrorDeadline  MCPConfigReloadErrorClass = "deadline_exceeded"
	MCPConfigReloadErrorPanic     MCPConfigReloadErrorClass = "callback_panic"
)

// MCPConfigReloadAttempt is the most recent terminal result from a config
// generation attempt. ErrorClass is deliberately redacted and does not expose
// the callback's error or any path, credential, or parser text it may contain.
type MCPConfigReloadAttempt struct {
	CompletedAt time.Time
	ErrorClass  MCPConfigReloadErrorClass
}

// Default values used by DefaultHotReloader. All of them live in one const
// block and can be overridden through the With* options.
const (
	// defaultPollInterval is how often the stdlib poller re-stats the config
	// file. The real fsnotify event watcher is substituted by this stdlib
	// poller to keep the module free of external dependencies.
	defaultPollInterval = 500 * time.Millisecond
	// defaultBackoffBase is the initial delay before the first retry.
	defaultBackoffBase = 1 * time.Second
	// defaultBackoffFactor grows the delay on each successive retry.
	defaultBackoffFactor = 2
	// defaultMaxBackoff caps the exponential backoff delay.
	defaultMaxBackoff = 8 * time.Second
	// defaultMaxRetries is how many reconnect attempts happen before giving up
	// and staying in a degraded watching state.
	defaultMaxRetries = 3
)

// hotReloaderOptions holds the configurable fields of a hot reloader during
// construction. It is an internal construction aid so that
// HotReloaderOption closures do not reference the concrete
// DefaultHotReloader type.
type hotReloaderOptions struct {
	pollInterval  time.Duration
	backoffBase   time.Duration
	backoffFactor int
	maxBackoff    time.Duration
	maxRetries    int
	transport     MCPTransport
}

// HotReloaderOption configures a DefaultHotReloader.
type HotReloaderOption func(*hotReloaderOptions)

// WithPollInterval overrides how often the stdlib poller re-checks the config
// file. Tests use a very small interval for fast, deterministic runs.
func WithPollInterval(d time.Duration) HotReloaderOption {
	return func(o *hotReloaderOptions) { o.pollInterval = d }
}

// WithBackoffBase overrides the initial retry backoff delay.
func WithBackoffBase(d time.Duration) HotReloaderOption {
	return func(o *hotReloaderOptions) { o.backoffBase = d }
}

// WithBackoffFactor overrides the multiplier applied to the backoff delay on
// every successive retry.
func WithBackoffFactor(n int) HotReloaderOption {
	return func(o *hotReloaderOptions) { o.backoffFactor = n }
}

// WithMaxBackoff caps the exponential backoff delay.
func WithMaxBackoff(d time.Duration) HotReloaderOption {
	return func(o *hotReloaderOptions) { o.maxBackoff = d }
}

// WithMaxRetries sets how many reconnect attempts happen before giving up.
func WithMaxRetries(n int) HotReloaderOption {
	return func(o *hotReloaderOptions) { o.maxRetries = n }
}

// WithTransport records the transport used for the watched server so it can be
// attached to the mcp.hot_reload span attributes.
func WithTransport(t MCPTransport) HotReloaderOption {
	return func(o *hotReloaderOptions) { o.transport = t }
}

// fileSnapshot captures the observable identity of the config file at one point
// in time. Comparing snapshots across polls detects a config change.
type fileSnapshot struct {
	modTime time.Time
	size    int64
	hash    string
}

// DefaultHotReloader is the reference HotReloader. It detects config changes by
// periodically stat-ing and reading the config path (stdlib polling) because
// the module keeps zero external dependencies and cannot pull in fsnotify. On
// change it runs the reconnect state machine
// (Disconnect -> Connect -> ListTools -> Re-register) with exponential-backoff
// retry on connection failure.
type DefaultHotReloader struct {
	client       MCPClient
	register     RegisterToolsFunc
	reloadConfig MCPConfigReloadFunc

	pollInterval  time.Duration
	backoffBase   time.Duration
	backoffFactor int
	maxBackoff    time.Duration
	maxRetries    int
	transport     MCPTransport

	path string

	mu             sync.Mutex
	pollCtx        context.Context
	pollCancel     context.CancelFunc
	stopCh         chan struct{}
	done           chan struct{}
	stopped        bool
	pollStarted    bool
	pollExited     bool
	doneClosed     bool
	stopErr        error
	connecting     bool
	pending        bool
	reconnectCount int
	baseline       bool
	lastMissing    bool
	snapshot       fileSnapshot
	lastAttempt    MCPConfigReloadAttempt
	attempted      bool
}

var _ HotReloader = (*DefaultHotReloader)(nil)
var _ HotReloadLifecycle = (*DefaultHotReloader)(nil)

// NewDefaultHotReloader returns a DefaultHotReloader that reconnects client and
// calls register with the freshly advertised tools after every reload. opts may
// tune the poll interval, backoff and retry limits.
func NewDefaultHotReloader(client MCPClient, register RegisterToolsFunc, opts ...HotReloaderOption) HotReloader {
	o := &hotReloaderOptions{
		pollInterval:  defaultPollInterval,
		backoffBase:   defaultBackoffBase,
		backoffFactor: defaultBackoffFactor,
		maxBackoff:    defaultMaxBackoff,
		maxRetries:    defaultMaxRetries,
		transport:     MCPTransportStdio,
	}
	for _, opt := range opts {
		opt(o)
	}
	return &DefaultHotReloader{
		client:        client,
		register:      register,
		pollInterval:  o.pollInterval,
		backoffBase:   o.backoffBase,
		backoffFactor: o.backoffFactor,
		maxBackoff:    o.maxBackoff,
		maxRetries:    o.maxRetries,
		transport:     o.transport,
		stopCh:        make(chan struct{}),
		done:          make(chan struct{}),
	}
}

// NewConfigHotReloader returns a reloader that delegates each selected-source
// change or manual reload to reload. Candidate failures are recorded as one
// attempt and do not stop the watcher.
func NewConfigHotReloader(reload MCPConfigReloadFunc, opts ...HotReloaderOption) *DefaultHotReloader {
	o := &hotReloaderOptions{
		pollInterval:  defaultPollInterval,
		backoffBase:   defaultBackoffBase,
		backoffFactor: defaultBackoffFactor,
		maxBackoff:    defaultMaxBackoff,
		maxRetries:    defaultMaxRetries,
		transport:     MCPTransportStreamableHTTP,
	}
	for _, opt := range opts {
		opt(o)
	}
	return &DefaultHotReloader{
		reloadConfig:  reload,
		pollInterval:  o.pollInterval,
		backoffBase:   o.backoffBase,
		backoffFactor: o.backoffFactor,
		maxBackoff:    o.maxBackoff,
		maxRetries:    o.maxRetries,
		transport:     o.transport,
		stopCh:        make(chan struct{}),
		done:          make(chan struct{}),
	}
}

// LastAttempt returns the most recent config-generation attempt and whether an
// attempt has completed. ErrorClass is a sanitized category; callback error
// text is never returned.
func (h *DefaultHotReloader) LastAttempt() (MCPConfigReloadAttempt, bool) {
	if h == nil {
		return MCPConfigReloadAttempt{}, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lastAttempt, h.attempted
}

// Watch starts the stdlib poller goroutine and records the baseline state of
// the config file before returning. Invalid polling intervals are rejected
// before reading the file or publishing the poll owner. The poller exits
// cleanly when Stop is called or ctx is canceled.
func (h *DefaultHotReloader) Watch(ctx context.Context, configPath string) error {
	if h.pollInterval <= 0 {
		return ErrHotReloadInvalidPollInterval
	}
	if h.client == nil && h.reloadConfig == nil {
		return errors.New("mcp: hot reload: no client or config reload callback")
	}
	if configPath == "" {
		return errors.New("mcp: hot reload: empty config path")
	}
	snap, present := readSnapshot(configPath)

	h.mu.Lock()

	if h.stopped {
		h.mu.Unlock()
		return errors.New("mcp: hot reload: already stopped")
	}
	if h.pollCancel != nil {
		h.mu.Unlock()
		return errors.New("mcp: hot reload: already watching")
	}

	h.path = configPath
	h.baseline = true
	h.snapshot = snap
	h.lastMissing = !present
	pollCtx, cancel := context.WithCancel(ctx)
	h.pollCtx = pollCtx
	h.pollCancel = cancel
	h.pollStarted = true
	// Publish and start the poll owner under the same lock as RequestStop. A
	// stop request can therefore either win before Watch publishes, or observe
	// the fully published poll owner and wait for it; it cannot miss a late
	// goroutine launch.
	go h.poll(pollCtx)
	h.mu.Unlock()
	return nil
}

// Stop requests stop and joins the actual poller/reconnect owner. It is
// idempotent and may wait without a bound when a client or registration
// callback ignores cancellation; use RequestStop followed by Wait(ctx) when a
// bounded caller wait is required. A reloader-owned callback that initiates
// shutdown must use RequestStop rather than waiting on its own completion.
func (h *DefaultHotReloader) Stop() error {
	h.RequestStop()
	<-h.done
	return h.completionError()
}

// RequestStop closes reload admission and cancels the poll/reconnect context.
// It never waits for an in-flight client or registration callback.
func (h *DefaultHotReloader) RequestStop() {
	h.mu.Lock()
	h.requestStopLocked()
	h.maybeCloseDoneLocked()
	h.mu.Unlock()
}

// Done returns the stable actual-completion channel.
func (h *DefaultHotReloader) Done() <-chan struct{} { return h.done }

// Wait bounds only this caller's wait; a timeout does not stop the owner.
func (h *DefaultHotReloader) Wait(ctx context.Context) error {
	if ctx == nil {
		return errors.New("mcp: hot reload: nil wait context")
	}
	select {
	case <-h.done:
		return h.completionError()
	default:
	}
	select {
	case <-h.done:
		return h.completionError()
	case <-ctx.Done():
		select {
		case <-h.done:
			return h.completionError()
		default:
			return &HotReloadPendingError{Cause: ctx.Err()}
		}
	}
}

func (h *DefaultHotReloader) requestStopLocked() {
	if h.stopped {
		return
	}
	h.stopped = true
	h.pending = false
	if h.pollCancel != nil {
		h.pollCancel()
	}
	close(h.stopCh)
}

func (h *DefaultHotReloader) recordStopErrorLocked(err error) {
	// The first actual terminal owner error is stable. Caller Wait deadlines
	// are returned directly and are never recorded as operation errors.
	if h.stopErr == nil && err != nil {
		h.stopErr = err
	}
}

func (h *DefaultHotReloader) maybeCloseDoneLocked() {
	if h.doneClosed || !h.stopped || h.connecting || (h.pollStarted && !h.pollExited) {
		return
	}
	h.doneClosed = true
	close(h.done)
}

func (h *DefaultHotReloader) completionError() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.stopErr
}

// Reload manually triggers one reconnect cycle without waiting for a config
// file change. It is asynchronous: the cycle runs on a background goroutine.
func (h *DefaultHotReloader) Reload(ctx context.Context) error {
	h.mu.Lock()
	if h.stopped {
		h.mu.Unlock()
		return errors.New("mcp: hot reload: reloader is stopped")
	}
	if h.pollCancel == nil {
		h.mu.Unlock()
		return errors.New("mcp: hot reload: not watching")
	}
	h.mu.Unlock()

	h.scheduleReconnect(ctx)
	return nil
}

// Name returns the logical name of the reloader.
func (h *DefaultHotReloader) Name() string {
	if h != nil && h.reloadConfig != nil {
		return "mcp-config-hot-reloader"
	}
	return "mcp-hot-reloader"
}

// poll is the stdlib watcher loop. It establishes a baseline on the first tick
// and then calls scheduleReconnect whenever a config change is detected.
func (h *DefaultHotReloader) poll(ctx context.Context) {
	ticker := time.NewTicker(h.pollInterval)
	defer func() {
		ticker.Stop()
		h.mu.Lock()
		h.pollExited = true
		h.maybeCloseDoneLocked()
		h.mu.Unlock()
	}()

	h.detectAndMaybeReconnect(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.detectAndMaybeReconnect(ctx)
		}
	}
}

// detectAndMaybeReconnect re-checks the config file and, if it changed since the
// last poll, triggers the reconnect state machine.
func (h *DefaultHotReloader) detectAndMaybeReconnect(ctx context.Context) {
	if h.detectChange() {
		h.scheduleReconnect(ctx)
	}
}

// detectChange compares the current config file state against the stored one.
// The first successful visit only establishes the baseline. It returns true
// once per observable change (creation, modification, or removal).
func (h *DefaultHotReloader) detectChange() bool {
	// Read the file outside the lock to avoid blocking other callers (Stop,
	// Reload, scheduleReconnect) during I/O.
	snap, present := readSnapshot(h.path)

	h.mu.Lock()
	defer h.mu.Unlock()

	if !h.baseline {
		h.baseline = true
		h.snapshot = snap
		h.lastMissing = !present
		return false
	}

	changed := present != !h.lastMissing || (present && snap != h.snapshot)
	h.snapshot = snap
	h.lastMissing = !present
	return changed
}

// scheduleReconnect kicks off one reconnect cycle on a background goroutine. If
// a cycle is already running, the change is recorded as pending and re-run once
// the active cycle finishes.
func (h *DefaultHotReloader) scheduleReconnect(_ context.Context) {
	h.mu.Lock()
	if h.stopped {
		h.mu.Unlock()
		return
	}
	if h.connecting {
		h.pending = true
		h.mu.Unlock()
		return
	}
	h.connecting = true
	// Use the reloader's own pollCtx so reconnect cycles are not canceled
	// by a short-lived caller context (e.g. an HTTP request).
	ctx := h.pollCtx
	h.mu.Unlock()

	go h.reconnectLoop(ctx)
}

// reconnectLoop runs the reconnect state machine, retrying with exponential
// backoff on connection failure up to maxRetries. After giving up it returns so
// the reloader stays in a degraded watching state and a later config change can
// recover it.
func (h *DefaultHotReloader) reconnectLoop(ctx context.Context) {
	defer func() {
		if recover() != nil {
			h.mu.Lock()
			h.recordStopErrorLocked(&HotReloadWorkerPanicError{})
			h.requestStopLocked()
			h.connecting = false
			h.maybeCloseDoneLocked()
			h.mu.Unlock()
		}
	}()

	for {
		for attempt := 0; ; attempt++ {
			if ctx.Err() != nil {
				break
			}
			succeeded, terminalErr := h.runCycle(ctx)
			if terminalErr != nil {
				h.mu.Lock()
				h.recordStopErrorLocked(terminalErr)
				h.requestStopLocked()
				h.connecting = false
				h.maybeCloseDoneLocked()
				h.mu.Unlock()
				return
			}
			if succeeded || attempt >= h.maxRetries {
				break
			}
			delay := h.backoffDelay(attempt)
			timer := time.NewTimer(delay)
			select {
			case <-h.stopCh:
				timer.Stop()
				attempt = h.maxRetries
			case <-timer.C:
			}
			if h.isStopped() {
				break
			}
		}

		h.mu.Lock()
		if h.pending && !h.stopped {
			h.pending = false
			h.mu.Unlock()
			continue
		}
		h.pending = false
		h.connecting = false
		h.maybeCloseDoneLocked()
		h.mu.Unlock()
		return
	}
}

func (h *DefaultHotReloader) isStopped() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.stopped
}

// backoffDelay computes the exponential backoff delay for the given (zero-based)
// attempt index, capped at maxBackoff.
func (h *DefaultHotReloader) backoffDelay(attempt int) time.Duration {
	d := h.backoffBase
	for i := 0; i < attempt; i++ {
		d *= time.Duration(h.backoffFactor)
		if d >= h.maxBackoff {
			return h.maxBackoff
		}
	}
	if d > h.maxBackoff {
		return h.maxBackoff
	}
	return d
}

// runCycle performs one Disconnect -> Connect -> ListTools -> Re-register pass
// and emits an mcp.hot_reload span. It returns true when the cycle completed
// and the tools were re-registered.
func (h *DefaultHotReloader) runCycle(ctx context.Context) (bool, error) {
	span, spanCtx := tracing.SpanFromContext(ctx, "mcp.hot_reload", tracing.SpanKindInternal)
	logger := tracing.NewTraceLogger(span, slog.Default())
	defer span.End()

	h.mu.Lock()
	h.reconnectCount++
	count := h.reconnectCount
	transport := h.transport
	path := h.path
	h.mu.Unlock()

	span.SetAttributes(
		tracing.Attribute{Key: "config_path", Value: path},
		tracing.Attribute{Key: "transport", Value: transport.String()},
		tracing.Attribute{Key: "reconnect_count", Value: count},
	)
	if !h.mayStartWork(ctx) {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		return false, nil
	}
	if h.reloadConfig != nil {
		err := h.callConfigReload(spanCtx, path)
		errorClass := mcpConfigReloadErrorClass(err)
		h.mu.Lock()
		h.lastAttempt = MCPConfigReloadAttempt{CompletedAt: time.Now(), ErrorClass: errorClass}
		h.attempted = true
		h.mu.Unlock()
		if err != nil {
			span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
			logger.Error("mcp.config_reload.failed", "attempt", count, "error_class", errorClass)
			if errors.Is(err, ErrHotReloadCallbackPanic) {
				return false, err
			}
			return true, nil
		}
		span.SetAttributes(tracing.Attribute{Key: "success", Value: true})
		logger.Info("mcp.config_reload.completed", "attempt", count)
		return true, nil
	}

	if err := h.client.Disconnect(spanCtx); err != nil {
		logger.Warn("mcp.hot_reload.disconnect", "server", h.client.Name(), "err", err)
	}
	if !h.mayStartWork(ctx) {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		return false, nil
	}
	if err := h.client.Connect(spanCtx); err != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("mcp.hot_reload.connect_failed", "server", h.client.Name(), "err", err)
		return false, nil
	}
	if !h.mayStartWork(ctx) {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		return false, nil
	}
	tools, err := h.client.ListTools(spanCtx)
	if err != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("mcp.hot_reload.list_tools_failed", "server", h.client.Name(), "err", err)
		return false, nil
	}
	if !h.mayStartWork(ctx) {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		return false, nil
	}
	if h.register != nil {
		if !h.mayStartWork(ctx) {
			span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
			return false, nil
		}
		if err := h.callRegister(tools); err != nil {
			span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
			logger.Error("mcp.hot_reload.register_failed", "server", h.client.Name(), "err", err)
			return false, err
		}
	}
	if !h.mayStartWork(ctx) {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		return false, nil
	}

	span.SetAttributes(tracing.Attribute{Key: "success", Value: true})
	logger.Info("mcp.hot_reload", "server", h.client.Name(), "tools", len(tools))
	return true, nil
}

func (h *DefaultHotReloader) callConfigReload(ctx context.Context, path string) (err error) {
	defer func() {
		if recover() != nil {
			err = &HotReloadCallbackPanicError{}
		}
	}()
	if h.reloadConfig == nil {
		return errors.New("mcp: hot reload: config reload callback is unavailable")
	}
	return h.reloadConfig(ctx, path)
}

func mcpConfigReloadErrorClass(err error) MCPConfigReloadErrorClass {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrHotReloadCallbackPanic):
		return MCPConfigReloadErrorPanic
	case errors.Is(err, context.DeadlineExceeded):
		return MCPConfigReloadErrorDeadline
	case errors.Is(err, context.Canceled):
		return MCPConfigReloadErrorCanceled
	default:
		return MCPConfigReloadErrorCandidate
	}
}

func (h *DefaultHotReloader) mayStartWork(ctx context.Context) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return !h.stopped && ctx.Err() == nil
}

func (h *DefaultHotReloader) callRegister(tools []MCPTool) (panicErr error) {
	defer func() {
		if recover() != nil {
			panicErr = &HotReloadCallbackPanicError{}
		}
	}()
	h.register(tools)
	return nil
}

// readSnapshot returns the current identity of the config file along with
// whether it exists and could be read. A missing or unreadable file yields the
// zero snapshot with present=false.
func readSnapshot(path string) (fileSnapshot, bool) {
	st, err := os.Stat(path)
	if err != nil {
		return fileSnapshot{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fileSnapshot{modTime: st.ModTime(), size: st.Size()}, true
	}
	sum := sha256.Sum256(data)
	return fileSnapshot{
		modTime: st.ModTime(),
		size:    int64(len(data)),
		hash:    hex.EncodeToString(sum[:]),
	}, true
}
