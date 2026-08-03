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
	// Stop stops the watcher, cancels the poller goroutine and blocks until it
	// has exited. Stop is idempotent.
	Stop() error
	// Reload manually triggers one reconnect cycle without a file change.
	Reload(ctx context.Context) error
	// Name returns the logical name of the reloader.
	Name() string
}

// RegisterToolsFunc re-registers the currently advertised MCP tools after a
// reconnect. It is called with the tools reported by the reconnected client.
type RegisterToolsFunc func(tools []MCPTool)

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
	client   MCPClient
	register RegisterToolsFunc

	pollInterval  time.Duration
	backoffBase   time.Duration
	backoffFactor int
	maxBackoff    time.Duration
	maxRetries    int
	transport     MCPTransport

	path string

	mu             sync.Mutex
	pollCancel     context.CancelFunc
	pollDone       chan struct{}
	stopCh         chan struct{}
	stopped        bool
	connecting     bool
	pending        bool
	reconnectCount int
	baseline       bool
	lastMissing    bool
	snapshot       fileSnapshot
}

var _ HotReloader = (*DefaultHotReloader)(nil)

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
	}
}

// Watch starts the stdlib poller goroutine and records the baseline state of
// the config file before returning. The poller exits cleanly when Stop is
// called or ctx is canceled.
func (h *DefaultHotReloader) Watch(ctx context.Context, configPath string) error {
	if configPath == "" {
		return errors.New("mcp: hot reload: empty config path")
	}

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
	pollCtx, cancel := context.WithCancel(ctx)
	h.pollCancel = cancel
	h.pollDone = make(chan struct{})
	h.stopCh = make(chan struct{})
	h.mu.Unlock()

	// Establish the baseline snapshot synchronously so that Watch only returns
	// after the config state is captured. Otherwise a caller that writes to the
	// config immediately after Watch returns could race the poller goroutine's
	// very first detectChange() (which sets the baseline), and the change would
	// be silently absorbed into the baseline and never detected.
	h.detectChange()

	go h.poll(pollCtx)
	return nil
}

// Stop cancels the poller goroutine and waits for it to exit. It also signals
// any in-flight reconnect retry loop to abort. Stop is idempotent.
func (h *DefaultHotReloader) Stop() error {
	h.mu.Lock()
	if h.stopped {
		h.mu.Unlock()
		return nil
	}
	h.stopped = true
	if h.pollCancel != nil {
		h.pollCancel()
	}
	if h.stopCh != nil {
		close(h.stopCh)
	}
	done := h.pollDone
	h.mu.Unlock()

	if done != nil {
		<-done
	}
	return nil
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
func (h *DefaultHotReloader) Name() string { return "mcp-hot-reloader" }

// poll is the stdlib watcher loop. It establishes a baseline on the first tick
// and then calls scheduleReconnect whenever a config change is detected.
func (h *DefaultHotReloader) poll(ctx context.Context) {
	defer close(h.pollDone)

	ticker := time.NewTicker(h.pollInterval)
	defer ticker.Stop()

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
	h.mu.Lock()
	defer h.mu.Unlock()

	snap, present := readSnapshot(h.path)

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
func (h *DefaultHotReloader) scheduleReconnect(ctx context.Context) {
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
	h.mu.Unlock()

	go h.reconnectLoop(ctx)
}

// reconnectLoop runs the reconnect state machine, retrying with exponential
// backoff on connection failure up to maxRetries. After giving up it returns so
// the reloader stays in a degraded watching state and a later config change can
// recover it.
func (h *DefaultHotReloader) reconnectLoop(ctx context.Context) {
	defer func() {
		h.mu.Lock()
		h.connecting = false
		hadPending := h.pending
		h.pending = false
		stopped := h.stopped
		h.mu.Unlock()
		if hadPending && !stopped {
			h.scheduleReconnect(context.Background())
		}
	}()

	for attempt := 0; ; attempt++ {
		if h.runCycle(ctx) {
			return
		}
		if attempt >= h.maxRetries {
			return
		}
		delay := h.backoffDelay(attempt)
		select {
		case <-h.stopCh:
			return
		case <-time.After(delay):
		}
	}
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
func (h *DefaultHotReloader) runCycle(ctx context.Context) bool {
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

	if err := h.client.Disconnect(spanCtx); err != nil {
		logger.Warn("mcp.hot_reload.disconnect", "server", h.client.Name(), "err", err)
	}
	if err := h.client.Connect(spanCtx); err != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("mcp.hot_reload.connect_failed", "server", h.client.Name(), "err", err)
		return false
	}
	tools, err := h.client.ListTools(spanCtx)
	if err != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("mcp.hot_reload.list_tools_failed", "server", h.client.Name(), "err", err)
		return false
	}
	if h.register != nil {
		h.register(tools)
	}

	span.SetAttributes(tracing.Attribute{Key: "success", Value: true})
	logger.Info("mcp.hot_reload", "server", h.client.Name(), "tools", len(tools))
	return true
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
