package tui

import (
	"context"
	"errors"
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

	// interactive enables keyboard-driven accordion navigation. When false
	// (the default for non-TTY / pipe input), the app renders every entry
	// expanded (legacy behaviour).
	interactive bool
}

// NewBubbleteaApp constructs an app wired to the given event source, using a
// default renderer registry, a fresh theme manager, and an unbuffered message
// queue. Options override these defaults.
func NewBubbleteaApp(events <-chan AgentEvent, opts ...AppOption) *BubbleteaApp {
	a := &BubbleteaApp{
		reg:        NewDefaultRegistry(),
		themeMgr:   NewThemeManager(),
		events:     events,
		msgCh:      make(chan Msg, 16),
		quitCh:     make(chan struct{}),
		done:       make(chan struct{}),
		streamBuf:  make(map[string]*strings.Builder),
		accordion:  NewAccordionModel(),
		interactive: isTerminal(),
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// Run starts the render loop. It returns when the context is canceled, the
// event channel closes, or Quit is invoked. The loop tears down internal
// resources before returning.
func (a *BubbleteaApp) Run(ctx context.Context) error {
	if !a.running.CompareAndSwap(false, true) {
		return errAlreadyRunning
	}
	defer func() {
		a.running.Store(false)
		a.cleanup()
	}()

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
		case _, ok := <-a.msgCh:
			if !ok {
				slog.Debug("tui.app.run", "stop", "msg_channel_closed")
				return nil
			}
			a.msgsSeen.Add(1)
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
// are rendered expanded (legacy behaviour).
func (a *BubbleteaApp) View() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.accordion == nil || a.accordion.Len() == 0 {
		return ""
	}
	return a.accordion.Render()
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
// result to the accordion model. Tool calls, tool results and thinking entries
// are stored as collapsible entries; other entries are expanded by default.
func (a *BubbleteaApp) handleEvent(ctx context.Context, ev AgentEvent) {
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

// addEntry appends a rendered frame to the accordion model. Streaming
// renderers replace the last entry instead of appending, giving a live-update
// effect.
func (a *BubbleteaApp) addEntry(ct string, out string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if isStreamingRenderContentType(ct) {
		if a.accordion.Len() == 0 {
			a.accordion.Add(entryFor(ct, out, a.interactive))
			return
		}
		last := a.accordion.Entries()[a.accordion.Len()-1]
		last.Full = out
		last.Summary = summarizeFirstLine(out, 80)
		last.Collapsed = false
		return
	}

	entry := entryFor(ct, out, a.interactive)
	// Only collapse gated content types when interactive; piped/legacy
	// consumers get the full transcript.
	if !a.interactive {
		entry.Collapsed = false
		for _, c := range entry.Children {
			c.Collapsed = false
		}
	}
	a.accordion.Add(entry)
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

// isTerminal reports whether stdin is a terminal (TTY). When true the
// interactive accordion mode is enabled with keyboard navigation.
func isTerminal() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
