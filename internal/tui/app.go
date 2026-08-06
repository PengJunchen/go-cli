package tui

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Msg is the generic message type delivered to an App via Send. In a
// Bubbletea-based design this corresponds to tea.Msg; here it is any value the
// render loop may react to.
type Msg any

// AgentEvent is a discrete event consumed by the App from an event stream. It
// deliberately mirrors the shape required by the TUI spec, carrying a content
// type for renderer dispatch plus trace/span identifiers used for debug-time
// correlation (the TUI layer does not emit spans itself).
type AgentEvent struct {
	// Type classifies the event at the actor level (e.g. "run", "tool").
	Type string
	// Content is the event payload to render.
	Content string
	// ContentType selects the renderer (see the ContentType* constants).
	ContentType string
	// TraceID associates the event with a trace for debug logging.
	TraceID string
	// SpanID associates the event with a span for debug logging.
	SpanID string
	// Incremental marks a partial event that contains only a fragment of
	// the assistant's response (streaming tokens). The TUI accumulates
	// these into the last assistant entry instead of creating new entries.
	Incremental bool
	// TokenUsage carries token consumption data for "token_usage" events.
	// It is nil for all other event types.
	TokenUsage *TokenUsageData
}

// TokenUsageData mirrors core.TokenUsage for the TUI layer.
type TokenUsageData struct {
	InputTokens  int
	OutputTokens int
	MaxTokens    int
	Cost         float64
}

// App is the top-level TUI application entry point. It runs a
// message-queue-driven render loop over a stream of agent events, dispatches
// each event to the renderer for its content type, and exposes the accumulated
// view through View. Send enqueues messages; Quit stops the loop gracefully.
type App interface {
	// Run consumes events until the associated context is canceled, the event
	// channel closes, or Quit is called. It returns the loop's stopping error.
	Run(ctx context.Context) error
	// Send enqueues a message for the render loop to process.
	Send(msg Msg)
	// Quit requests a graceful stop of the render loop.
	Quit()
}

// errAlreadyRunning reports an attempt to invoke Run more than once.
var errAlreadyRunning = errors.New("tui: app already running")

// compile-time assertion that BubbleteaApp satisfies App.
var _ App = (*BubbleteaApp)(nil)

// AppOption configures a BubbleteaApp at construction time.
type AppOption func(*BubbleteaApp)

// WithRegistry sets the renderer registry used for content-type dispatch.
func WithRegistry(reg *RendererRegistry) AppOption {
	return func(a *BubbleteaApp) { a.reg = reg }
}

// WithThemeManager sets the theme manager used to resolve the active theme.
func WithThemeManager(mgr *ThemeManager) AppOption {
	return func(a *BubbleteaApp) { a.themeMgr = mgr }
}

// WithWidth sets the target render width in terminal columns.
func WithWidth(width int) AppOption {
	return func(a *BubbleteaApp) { a.width = width }
}

// WithOnUpdate registers a callback invoked after every view mutation. The
// callback receives the freshly rendered view string so it does not need to
// call View (which would deadlock on the internal mutex). The callback runs
// synchronously inside the render loop, so it must not block on the app.
func WithOnUpdate(fn func(string)) AppOption {
	return func(a *BubbleteaApp) { a.onUpdate = fn }
}

// WithSteerCallback registers a callback invoked when the user submits steer
// text (Enter in steer input mode). The callback receives the typed text and
// runs in a separate goroutine so it does not block the render loop.
func WithSteerCallback(cb func(string)) AppOption {
	return func(a *BubbleteaApp) { a.steerCallback = cb }
}

// WithCancelCallback registers a callback invoked when the user presses 'q' to
// quit/cancel. The callback typically calls TurnRunner.Cancel to cancel the
// running turn.
func WithCancelCallback(cb func()) AppOption {
	return func(a *BubbleteaApp) { a.cancelCallback = cb }
}

// WithPauseCallback registers a callback invoked when the user presses Space
// to pause agent execution.
func WithPauseCallback(cb func()) AppOption {
	return func(a *BubbleteaApp) { a.pauseCallback = cb }
}

// WithResumeCallback registers a callback invoked when the user presses Space
// to resume agent execution after a pause.
func WithResumeCallback(cb func()) AppOption {
	return func(a *BubbleteaApp) { a.resumeCallback = cb }
}

// BubbleteaApp is the default App implementation. It owns a render loop that
// merges three sources: the incoming agent-event channel, messages pushed via
// Send, and the quit/cancel signals. Streaming content types accumulate into a
// per-type buffer and replace the last rendered frame, producing a live-update
// effect without external dependencies.
//
// Tool calls, tool results and thinking events are rendered as collapsible
// Accordion entries. The user can navigate with Up/Down arrows and toggle
// collapse with Tab or Enter.
type BubbleteaApp struct {
	reg      *RendererRegistry
	themeMgr *ThemeManager
	events   <-chan AgentEvent
	msgCh    chan Msg
	quitCh   chan struct{}
	quitOnce sync.Once
	done     chan struct{}
	running  atomic.Bool
	cleaned  bool
	width    int

	mu         sync.Mutex
	accordion  *AccordionModel
	streamBuf  map[string]*strings.Builder
	eventsSeen atomic.Int64
	msgsSeen   atomic.Int64

	// streamingEntry points to the accordion entry currently receiving
	// incremental token chunks. It is set on the first incremental event
	// and cleared when a non-incremental event arrives.
	streamingEntry *AccordionEntry
	streamingBuf   strings.Builder

	// interactive enables keyboard-driven accordion navigation. When false
	// (the default for non-TTY / pipe input), the app renders every entry
	// expanded (legacy behavior).
	interactive bool

	// onUpdate is invoked after every entry mutation (add/replace). It lets
	// the caller stream the view to the terminal in real time instead of
	// waiting for the turn to finish. When nil no callback fires.
	onUpdate func(string)

	// steerInputMode is an atomic flag set by the keyboard loop to indicate
	// the app is in steer input mode. The keyboard loop reads it to decide
	// how to interpret key presses.
	steerInputMode atomic.Bool
	// steerInput is the accumulated text typed in steer input mode.
	steerInput string
	// steerCursor is the cursor position within steerInput.
	steerCursor int
	// steerCallback is invoked when the user submits steer text (Enter).
	steerCallback func(string)
	// cancelCallback is invoked when the user presses 'q' to quit/cancel.
	cancelCallback func()
	// pauseCallback is invoked when the user presses Space to pause.
	pauseCallback func()
	// resumeCallback is invoked when the user presses Space to resume.
	resumeCallback func()
	// paused tracks whether the agent is currently paused.
	paused bool

	// Token usage for the status bar.
	tokenInput  int
	tokenOutput int
	tokenMax    int
	tokenCost   float64

	// Spinner is shown while a tool call is in progress (between tool_call
	// and tool_result events). spinnerFrame advances on each view render.
	spinner       *SpinnerRenderer
	spinnerActive bool
	spinnerFrame  int
}

// NewBubbleteaApp constructs an app wired to the given event source, using a
// default renderer registry, a fresh theme manager, and an unbuffered message
// queue. Options override these defaults.
func NewBubbleteaApp(events <-chan AgentEvent, opts ...AppOption) *BubbleteaApp {
	a := &BubbleteaApp{
		reg:         NewDefaultRegistry(),
		themeMgr:    NewThemeManager(),
		events:      events,
		msgCh:       make(chan Msg, 16),
		quitCh:      make(chan struct{}),
		done:        make(chan struct{}),
		streamBuf:   make(map[string]*strings.Builder),
		accordion:   NewAccordionModel(),
		interactive: isTerminal(),
		spinner:     NewSpinnerRenderer(),
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// Run starts the render loop. It returns when the context is canceled, the
// event channel closes, or Quit is invoked. The loop tears down internal
// resources before returning.
//
// In interactive (TTY) mode the terminal is switched to raw mode so the
// keyboard loop can read single keypresses (arrow navigation, Tab/Enter to
// toggle, e/c for global expand/collapse, q to quit). The terminal is
// restored before Run returns.
func (a *BubbleteaApp) Run(ctx context.Context) error {
	if !a.running.CompareAndSwap(false, true) {
		return errAlreadyRunning
	}
	defer func() {
		a.running.Store(false)
		a.cleanup()
	}()

	// Enable keyboard navigation only when interactive AND the platform
	// supports raw mode. On unsupported platforms we degrade to the
	// always-expanded rendering.
	var kbdCancel context.CancelFunc
	if a.interactive {
		if oldTerm, err := makeRaw(int(os.Stdin.Fd())); err == nil {
			defer restoreRaw(int(os.Stdin.Fd()), oldTerm) //nolint:errcheck
			kbdCtx, cancel := context.WithCancel(ctx)
			kbdCancel = cancel
			go a.keyboardLoop(kbdCtx)
		}
	}
	if kbdCancel != nil {
		defer kbdCancel()
	}

	slog.Debug("tui.app.run", "started", true)
	for {
		select {
		case <-ctx.Done():
			slog.Debug("tui.app.run", "stop", "context_canceled")
			return ctx.Err()
		case <-a.quitCh:
			slog.Debug("tui.app.run", "stop", "quit")
			return nil
		case ev, ok := <-a.events:
			if !ok {
				slog.Debug("tui.app.run", "stop", "events_closed")
				return nil
			}
			a.eventsSeen.Add(1)
			a.handleEvent(ctx, ev)
		case msg, ok := <-a.msgCh:
			if !ok {
				slog.Debug("tui.app.run", "stop", "msg_channel_closed")
				return nil
			}
			a.msgsSeen.Add(1)
			if a.handleMsg(msg) {
				slog.Debug("tui.app.run", "stop", "quit_key")
				return nil
			}
		}
	}
}

// Send pushes a message into the render loop, dropping it if the queue is
// full so a slow renderer never blocks the producer.
func (a *BubbleteaApp) Send(msg Msg) {
	select {
	case a.msgCh <- msg:
	default:
	}
}

// Quit requests a graceful stop of the render loop. It is safe to call
// multiple times and from any goroutine.
func (a *BubbleteaApp) Quit() {
	a.quitOnce.Do(func() { close(a.quitCh) })
	slog.Debug("tui.app.quit", "requested", true)
}

// Done returns a channel that closes once the render loop has fully cleaned
// up and exited, enabling callers to wait on quit completion.
func (a *BubbleteaApp) Done() <-chan struct{} { return a.done }

// EventsProcessed reports how many agent events the loop has consumed.
func (a *BubbleteaApp) EventsProcessed() int64 { return a.eventsSeen.Load() }

// MessagesProcessed reports how many messages the loop has consumed via Send.
func (a *BubbleteaApp) MessagesProcessed() int64 { return a.msgsSeen.Load() }

// View returns the current rendered view. In interactive mode the accordion
// model produces collapsed/expanded output. In non-interactive mode all entries
// are rendered expanded (legacy behavior).
func (a *BubbleteaApp) View() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.renderView()
}

// renderView builds the full view string: the accordion content, optionally
// followed by the steer input prompt and the token usage status bar. The
// caller must hold a.mu.
func (a *BubbleteaApp) renderView() string {
	var sb strings.Builder
	if a.accordion != nil && a.accordion.Len() > 0 {
		sb.WriteString(a.accordion.Render())
	}
	// Spinner indicator (shown while a tool is executing).
	if a.spinnerActive && a.spinner != nil {
		sb.WriteString("\n")
		sb.WriteString(a.spinner.RenderFrame(a.spinnerFrame))
		sb.WriteString(" working...")
		a.spinnerFrame++
	}
	// Steer input prompt.
	if a.steerInputMode.Load() {
		sb.WriteString("\n> steer> ")
		sb.WriteString(a.steerInput)
		// Render a block cursor.
		sb.WriteString("\u2588")
	}
	// Token usage status bar.
	if a.tokenMax > 0 || a.tokenCost > 0 {
		sb.WriteString("\n")
		sb.WriteString(a.renderStatusBar())
	}
	// Pause indicator.
	if a.paused {
		sb.WriteString("\n[PAUSED - press Space to resume]")
	}
	return sb.String()
}

// renderStatusBar formats the token usage status bar. When the token usage
// percentage exceeds 80%, the text is wrapped in ANSI yellow to warn the user.
// The caller must hold a.mu.
func (a *BubbleteaApp) renderStatusBar() string {
	total := a.tokenInput + a.tokenOutput
	maxT := a.tokenMax
	pct := 0
	if maxT > 0 {
		pct = total * 100 / maxT
	}
	bar := fmt.Sprintf("Tokens: %d/%d (%d%%) | Cost: $%.4f", total, maxT, pct, a.tokenCost)
	if pct > 80 {
		// ANSI yellow foreground.
		return "\x1b[33m" + bar + "\x1b[0m"
	}
	return bar
}

// cleanup closes the done channel exactly once and releases per-type stream
// buffers.
func (a *BubbleteaApp) cleanup() {
	a.mu.Lock()
	if !a.cleaned {
		a.cleaned = true
		a.streamBuf = make(map[string]*strings.Builder)
		close(a.done)
	}
	a.mu.Unlock()
	slog.Debug("tui.app.cleanup", "cleaned", true)
}

// handleEvent dispatches a single agent event to its renderer and adds the
// result to the accordion model. Incremental events (streaming tokens) are
// accumulated into the last assistant entry instead of creating new entries.
// Tool calls, tool results and thinking entries are stored as collapsible
// entries; other entries are expanded by default.
func (a *BubbleteaApp) handleEvent(ctx context.Context, ev AgentEvent) {
	// Handle token_usage events by updating the status bar data and re-rendering.
	if ev.Type == "token_usage" && ev.TokenUsage != nil {
		var view string
		notify := a.onUpdate != nil
		func() {
			a.mu.Lock()
			defer a.mu.Unlock()
			a.tokenInput = ev.TokenUsage.InputTokens
			a.tokenOutput = ev.TokenUsage.OutputTokens
			a.tokenMax = ev.TokenUsage.MaxTokens
			a.tokenCost = ev.TokenUsage.Cost
			if notify {
				view = a.renderView()
			}
		}()
		if notify {
			a.onUpdate(view)
		}
		return
	}

	if ev.Incremental {
		a.handleIncremental(ctx, ev)
		return
	}

	// Finalize any active streaming entry so the next incremental event
	// creates a fresh entry rather than appending to a completed message.
	a.mu.Lock()
	hadStreaming := a.streamingEntry != nil
	a.streamingEntry = nil
	a.streamingBuf.Reset()
	a.mu.Unlock()

	// When the loop finishes streaming, it sends a non-incremental "message"
	// event carrying the complete content for downstream consumers (harness
	// result, lastMessageEvent). The TUI has already rendered the content
	// incrementally, so skip this duplicate.
	if hadStreaming && ev.Type == "message" {
		return
	}

	// Update spinner state for tool lifecycle events. The spinner is
	// activated on tool_call and deactivated on tool_result.
	if ev.ContentType == ContentTypeToolCall || ev.ContentType == ContentTypeToolResult {
		a.mu.Lock()
		if ev.ContentType == ContentTypeToolCall {
			a.spinnerActive = true
			a.spinnerFrame = 0
		} else {
			a.spinnerActive = false
		}
		a.mu.Unlock()
	}

	start := time.Now()
	ct := ev.ContentType
	r, ok := a.reg.Get(ct)
	if !ok {
		ct = DefaultContentType
		r, ok = a.reg.Get(DefaultContentType)
	}
	out := ev.Content
	if ok && r != nil {
		theme := Theme(DarkTheme{})
		if a.themeMgr != nil {
			theme = a.themeMgr.Get()
		}
		out = r.Render(ctx, ev.Content, RenderOpts{
			Theme:       theme,
			Width:       a.width,
			ContentType: ct,
		})
	}
	a.addEntry(ct, out)
	slog.DebugContext(ctx, "tui.app.render",
		"content_type", ct,
		"trace_id", ev.TraceID,
		"span_id", ev.SpanID,
		"latency_us", time.Since(start).Microseconds(),
	)
}

// handleIncremental processes a streaming token chunk. On the first chunk it
// creates a new accordion entry; on subsequent chunks it appends to the
// accumulated content and re-renders the entry in place, producing a live
// typewriter effect.
func (a *BubbleteaApp) handleIncremental(ctx context.Context, ev AgentEvent) {
	ct := ev.ContentType
	r, ok := a.reg.Get(ct)
	if !ok {
		ct = DefaultContentType
		r, ok = a.reg.Get(DefaultContentType)
	}

	var view string
	notify := a.onUpdate != nil

	func() {
		a.mu.Lock()
		defer a.mu.Unlock()

		a.streamingBuf.WriteString(ev.Content)
		accumulated := a.streamingBuf.String()

		// Render the accumulated content so styles wrap the full message.
		out := accumulated
		if ok && r != nil {
			theme := Theme(DarkTheme{})
			if a.themeMgr != nil {
				theme = a.themeMgr.Get()
			}
			out = r.Render(ctx, accumulated, RenderOpts{
				Theme:       theme,
				Width:       a.width,
				ContentType: ct,
			})
		}

		if a.streamingEntry == nil {
			entry := entryFor(ct, out, a.interactive)
			entry.Collapsed = false
			a.accordion.Add(entry)
			a.streamingEntry = entry
		} else {
			a.streamingEntry.Full = out
			a.streamingEntry.Summary = summarizeFirstLine(out, 80)
		}

		if notify {
			view = a.renderView()
		}
	}()

	if notify {
		a.onUpdate(view)
	}
}

// addEntry appends a rendered frame to the accordion model. Streaming
// renderers replace the last entry instead of appending, giving a live-update
// effect. After mutating the model the view is rendered (under the lock) and
// the onUpdate callback is invoked outside the lock so it can safely write to
// the terminal.
func (a *BubbleteaApp) addEntry(ct string, out string) {
	var view string
	notify := a.onUpdate != nil
	func() {
		a.mu.Lock()
		defer a.mu.Unlock()

		if isStreamingRenderContentType(ct) {
			if a.accordion.Len() == 0 {
				a.accordion.Add(entryFor(ct, out, a.interactive))
			} else {
				last := a.accordion.Entries()[a.accordion.Len()-1]
				last.Full = out
				last.Summary = summarizeFirstLine(out, 80)
				last.Collapsed = false
			}
		} else {
			entry := entryFor(ct, out, a.interactive)
			if !a.interactive {
				entry.Collapsed = false
				for _, c := range entry.Children {
					c.Collapsed = false
				}
			}
			a.accordion.Add(entry)
		}
		if notify {
			view = a.renderView()
		}
	}()
	if notify {
		a.onUpdate(view)
	}
}

// isStreamingRenderContentType reports whether the content type is a
// streaming-capable renderer type.
func isStreamingRenderContentType(ct string) bool {
	return ct == ContentTypeStreaming || ct == ContentTypeStreamingCode || ct == ContentTypeStreamingThink
}

// entryFor creates an AccordionEntry from a rendered output string. The entry
// is collapsed by default for gated content types when interactive mode is on.
func entryFor(ct, out string, interactive bool) *AccordionEntry {
	collapsed := interactive && defaultCollapsed(ct)
	return &AccordionEntry{
		ContentType: ct,
		Summary:     summarizeFirstLine(out, 80),
		Full:        out,
		Collapsed:   collapsed,
		Timestamp:   time.Now(),
	}
}

// summarizeFirstLine returns the first line of s, trimmed and truncated to
// maxRunes. If s is empty it returns "…".
func summarizeFirstLine(s string, maxRunes int) string {
	if s == "" {
		return "…"
	}
	line := s
	if idx := strings.Index(s, "\n"); idx >= 0 {
		line = s[:idx]
	}
	line = strings.TrimSpace(line)
	// Strip ANSI escape sequences for the summary.
	line = stripANSIPlain(line)
	if len(line) > maxRunes {
		return line[:maxRunes] + "…"
	}
	return line
}

// stripANSIPlain removes all ANSI escape sequences from s.
func stripANSIPlain(s string) string {
	var sb strings.Builder
	inEscape := false
	for _, r := range s {
		if r == '\x1b' {
			inEscape = true
			continue
		}
		if inEscape {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEscape = false
			}
			continue
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

// IsTerminal reports whether stdin is a terminal (TTY). When true the
// interactive accordion mode is enabled with keyboard navigation.
func IsTerminal() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// isTerminal is the unexported alias used by the constructor.
func isTerminal() bool { return IsTerminal() }
