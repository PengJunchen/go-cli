package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/x/term"
)

// Msg is the generic message type delivered to an App via Send. It corresponds
// to tea.Msg in the bubbletea-based design: any value pushed through Send is
// forwarded to the model's Update as a tea.Msg.
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
	// ToolCallID associates streaming output with the originating tool
	// call. Populated for tool_output events; empty for other types.
	ToolCallID string
	// Stream identifies the output source for tool_output events: "stdout"
	// or "stderr".
	Stream string
}

// TokenUsageData mirrors core.TokenUsage for the TUI layer.
type TokenUsageData struct {
	InputTokens  int
	OutputTokens int
	MaxTokens    int
	Cost         float64
}

// App is the top-level TUI application entry point. It runs a message-queue-
// driven render loop over a stream of agent events, dispatches each event to
// the renderer for its content type, and exposes the accumulated view through
// View. Send enqueues messages; Quit stops the loop gracefully.
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

// WithInputReader sets an alternative io.Reader for keyboard input instead of
// os.Stdin. Bubbletea is wired to this reader via tea.WithInput so the TUI and
// an Esc-key interrupt monitor can share a multiplexed stdin.
func WithInputReader(r io.Reader) AppOption {
	return func(a *BubbleteaApp) { a.inputReader = r }
}

// WithOnUpdate registers a callback invoked after every view mutation. The
// callback receives the freshly rendered view string so it does not need to
// call View (which would deadlock on the internal mutex). The callback runs
// synchronously inside the model update, so it must not block on the app.
//
// In interactive (TTY) mode bubbletea owns terminal rendering and the callback
// is not invoked; it only fires in non-interactive mode so callers can stream
// the view to stdout themselves.
func WithOnUpdate(fn func(string)) AppOption {
	return func(a *BubbleteaApp) { a.onUpdate = fn }
}

// WithSteerCallback registers a callback invoked when the user submits steer
// text (Enter in steer input mode). The callback receives the typed text and
// runs in a separate goroutine so it does not block the render loop.
func WithSteerCallback(cb func(string)) AppOption {
	return func(a *BubbleteaApp) { a.steerCallback = cb }
}

// WithCancelCallback registers a callback invoked when the user presses 'q' or
// Ctrl+C to quit/cancel. The callback typically calls TurnRunner.Cancel to
// cancel the running turn.
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

// WithApprovalChannel wires a channel that delivers ApprovalRequest values
// from the approval middleware. When a request arrives the TUI renders an
// interactive approval entry and the user resolves it with y/a/n/d keys.
func WithApprovalChannel(ch <-chan ApprovalRequest) AppOption {
	return func(a *BubbleteaApp) { a.approvalCh = ch }
}

// BubbleteaApp is the default App implementation. It is a thin adapter over a
// bubbletea tea.Program: the teaModel owns all TUI state (accordion, streaming,
// steer, token usage, spinner) and the event-handling logic, while this struct
// preserves the public App surface (Run/Send/Quit/Done/View) and the WithXxx
// option contract so existing callers compile unchanged.
type BubbleteaApp struct {
	// model owns all TUI state and implements tea.Model. It is constructed in
	// NewBubbleteaApp so tests can drive it directly without a running program.
	model *teaModel

	// program is created in Run (it needs the run context) and drives the
	// bubbletea event loop.
	program *tea.Program

	// done closes once Run has fully cleaned up and exited.
	done chan struct{}

	// quitOnce guards the quit channel so Quit is idempotent.
	quitOnce sync.Once
	quitCh   chan struct{}

	// running guards against concurrent Run invocations.
	running atomic.Bool
	// cleaned tracks whether cleanup has already closed done.
	cleaned bool

	// Option fields applied by WithXxx. They are mirrored into the model at
	// construction time and retained here for test inspection.
	reg        *RendererRegistry
	themeMgr   *ThemeManager
	events     <-chan AgentEvent
	width      int
	inputReader io.Reader
	interactive bool

	onUpdate       func(string)
	steerCallback  func(string)
	cancelCallback func()
	pauseCallback  func()
	resumeCallback func()
	approvalCh     <-chan ApprovalRequest
}

// NewBubbleteaApp constructs an app wired to the given event source, using a
// default renderer registry, a fresh theme manager, and an unbuffered message
// queue. Options override these defaults. The teaModel is built immediately so
// it can be exercised directly by tests; the tea.Program is created lazily in
// Run where the run context is available.
func NewBubbleteaApp(events <-chan AgentEvent, opts ...AppOption) *BubbleteaApp {
	a := &BubbleteaApp{
		reg:         NewDefaultRegistry(),
		themeMgr:    NewThemeManager(),
		events:      events,
		done:        make(chan struct{}),
		quitCh:      make(chan struct{}),
		interactive: isTerminal(),
	}
	for _, opt := range opts {
		opt(a)
	}
	a.model = newTeaModel(a)
	return a
}

// Run starts the bubbletea program. It returns when the context is canceled,
// the event channel closes, or Quit is invoked. The loop tears down internal
// resources before returning.
//
// In interactive (TTY) mode bubbletea owns terminal rendering (raw mode,
// keyboard, resize, diff-based repaint) on stderr. In non-interactive mode the
// renderer is disabled and the WithOnUpdate callback (if any) streams the view
// to stdout, matching the legacy single-stream behavior until 34-7 removes it.
func (a *BubbleteaApp) Run(ctx context.Context) error {
	if !a.running.CompareAndSwap(false, true) {
		return errAlreadyRunning
	}
	// runCtx is cancelled when Run returns so any waitForEvent/waitForMsg
	// command goroutines still blocked on their channels unblock and exit
	// (bubbletea cannot kill a cmd func blocked on a user channel). It is also
	// passed to the program as its context so context cancellation propagates
	// to the event loop.
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	defer func() {
		a.running.Store(false)
		a.cleanup()
	}()

	a.model.runDone = runCtx.Done()
	a.program = tea.NewProgram(a.model, a.programOptions(runCtx)...)
	_, err := a.program.Run()
	return err
}

// programOptions builds the tea.ProgramOption set for the current run. Input is
// taken from WithInputReader when provided, otherwise os.Stdin in interactive
// mode, or disabled entirely in non-interactive mode so bubbletea does not try
// to open /dev/tty. Signal handling is disabled because the CLI manages its own
// InterruptHandler.
func (a *BubbleteaApp) programOptions(ctx context.Context) []tea.ProgramOption {
	opts := []tea.ProgramOption{
		tea.WithContext(ctx),
		tea.WithoutSignalHandler(),
	}
	switch {
	case a.inputReader != nil:
		opts = append(opts, tea.WithInput(a.inputReader), tea.WithOutput(os.Stderr))
	case a.interactive:
		opts = append(opts, tea.WithOutput(os.Stderr))
	default:
		// Non-interactive: disable input (no keyboard) and rendering (the
		// onUpdate callback streams the view to stdout instead).
		opts = append(opts, tea.WithoutRenderer(), tea.WithInput(nil))
	}
	return opts
}

// Send pushes a message into the model's message queue, dropping it if the
// queue is full so a slow renderer never blocks the producer. It is safe to
// call before Run starts; the queued message is drained once the program's
// event loop begins.
func (a *BubbleteaApp) Send(msg Msg) {
	a.model.send(msg)
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
func (a *BubbleteaApp) EventsProcessed() int64 { return a.model.eventsSeen.Load() }

// MessagesProcessed reports how many messages the loop has consumed via Send.
func (a *BubbleteaApp) MessagesProcessed() int64 { return a.model.msgsSeen.Load() }

// View returns the current rendered view. It is safe for concurrent callers and
// delegates to the model under its mutex. In interactive mode the accordion
// model produces collapsed/expanded output; in non-interactive mode all entries
// are rendered expanded (legacy behavior).
func (a *BubbleteaApp) View() string {
	return a.model.View()
}

// cleanup resets per-run state and closes the done channel exactly once. It is
// called from Run's deferred cleanup path.
func (a *BubbleteaApp) cleanup() {
	if a.model != nil {
		a.model.mu.Lock()
		a.model.streamBuf = make(map[string]*strings.Builder)
		a.model.mu.Unlock()
	}
	if !a.cleaned {
		a.cleaned = true
		close(a.done)
	}
	slog.Debug("tui.app.cleanup", "cleaned", true)
}

// ---------- teaModel (bubbletea tea.Model) ----------

// teaModel implements tea.Model. It owns ALL the TUI state (accordion,
// streaming, steer, token usage, spinner) and the event-handling logic that
// previously lived in the self-implemented render loop.
type teaModel struct {
	// configuration
	reg        *RendererRegistry
	themeMgr   *ThemeManager
	width      int
	height     int
	interactive bool

	// event / message sources
	events <-chan AgentEvent
	msgCh  chan Msg
	quitCh <-chan struct{}
	// runDone is closed (via runCtx cancellation) when the current Run exits,
	// unblocking lingering waitForEvent/waitForMsg command goroutines so they
	// do not leak. It is nil when the model is exercised without a program.
	runDone <-chan struct{}

	// accordion + streaming
	accordion      *AccordionModel
	streamBuf      map[string]*strings.Builder
	streamingEntry *AccordionEntry
	streamingBuf   strings.Builder
	// streamingMD caches rendered markdown lines during incremental streaming
	// so each token chunk does not trigger an O(n²) full re-render. It is
	// lazily created and Reset when a new streaming message begins.
	streamingMD *StreamingMarkdownRenderer

	// steer input
	steerInputMode bool
	steerInput     string
	steerCursor    int
	steerCallback  func(string)

	// callbacks
	onUpdate       func(string)
	cancelCallback func()
	pauseCallback  func()
	resumeCallback func()
	paused         bool

	// approval flow: approvalCh delivers requests from the middleware;
	// pendingApproval is the request currently awaiting a user decision.
	approvalCh      <-chan ApprovalRequest
	pendingApproval *ApprovalRequest

	// token usage
	tokenInput  int
	tokenOutput int
	tokenMax    int
	tokenCost   float64

	// spinner (bubbles spinner) shown while a tool call is in progress.
	spinner       spinner.Model
	spinnerActive bool

	// quitting is set when the user requests quit so View can render a
	// goodbye line.
	quitting bool

	// concurrency: guards all mutable state above. Update runs single-threaded
	// inside bubbletea, but external callers (tests, View) access the model
	// concurrently, so the mutex serializes View against in-flight updates.
	mu        sync.Mutex
	eventsSeen atomic.Int64
	msgsSeen   atomic.Int64
}

// newTeaModel builds a teaModel from the option fields already applied to the
// BubbleteaApp adapter.
func newTeaModel(a *BubbleteaApp) *teaModel {
	return &teaModel{
		reg:            a.reg,
		themeMgr:       a.themeMgr,
		width:          a.width,
		interactive:    a.interactive,
		events:         a.events,
		msgCh:          make(chan Msg, 16),
		quitCh:         a.quitCh,
		streamBuf:      make(map[string]*strings.Builder),
		accordion:      NewAccordionModel(),
		onUpdate:       a.onUpdate,
		steerCallback:  a.steerCallback,
		cancelCallback: a.cancelCallback,
		pauseCallback:  a.pauseCallback,
		resumeCallback: a.resumeCallback,
		approvalCh:     a.approvalCh,
		spinner:        spinner.New(spinner.WithSpinner(spinner.MiniDot)),
	}
}

// Init starts the bubbletea command loop. It arms listeners for the agent event
// stream, messages pushed via Send, the quit signal, and (when wired) approval
// requests from the middleware. The spinner tick is started on demand.
func (m *teaModel) Init() tea.Cmd {
	done := m.runDone
	cmds := []tea.Cmd{
		waitForEvent(m.events, done),
		waitForMsg(m.msgCh, done),
		waitForQuit(m.quitCh, done),
	}
	if m.approvalCh != nil {
		cmds = append(cmds, waitForApproval(m.approvalCh, done))
	}
	return tea.Batch(cmds...)
}

// Update handles all incoming tea.Msg values: agent events from the stream,
// keyboard input, terminal resizes, spinner ticks, and messages pushed via Send.
func (m *teaModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case agentEventMsg:
		m.eventsSeen.Add(1)
		cmd := m.handleEvent(msg.event)
		return m, tea.Batch(waitForEvent(m.events, m.runDone), cmd)
	case approvalRequestMsg:
		m.mu.Lock()
		m.pendingApproval = &msg.req
		view := ""
		if m.shouldNotify() {
			view = m.renderViewLocked()
		}
		m.mu.Unlock()
		m.notify(view)
		return m, waitForApproval(m.approvalCh, m.runDone)
	case tea.KeyMsg:
		return m, m.handleKey(msg)
	case tea.WindowSizeMsg:
		m.mu.Lock()
		m.width = msg.Width
		m.height = msg.Height
		m.mu.Unlock()
		return m, nil
	case spinner.TickMsg:
		m.mu.Lock()
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		active := m.spinnerActive
		m.mu.Unlock()
		if !active {
			return m, nil
		}
		return m, cmd
	case tea.QuitMsg:
		return m, nil
	default:
		// A message pushed via Send that is not a recognized tea message.
		m.msgsSeen.Add(1)
		return m, waitForMsg(m.msgCh, m.runDone)
	}
}

// View returns the full rendered view: accordion content, optionally the
// spinner, the steer input prompt, the token usage status bar, the pause
// indicator, and a goodbye line when quitting.
func (m *teaModel) View() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.renderViewLocked()
}

// renderViewLocked builds the full view string. The caller must hold m.mu.
func (m *teaModel) renderViewLocked() string {
	var sb strings.Builder
	if m.accordion != nil && m.accordion.Len() > 0 {
		sb.WriteString(m.accordion.Render())
	}
	// Spinner indicator (shown while a tool is executing).
	if m.spinnerActive {
		sb.WriteString("\n")
		sb.WriteString(m.spinner.View())
		sb.WriteString(" working...")
	}
	// Pending approval entry (interactive approval flow).
	if m.pendingApproval != nil {
		sb.WriteString("\n")
		sb.WriteString(renderApprovalRequest(m.pendingApproval))
	}
	// Steer input prompt.
	if m.steerInputMode {
		sb.WriteString("\n> steer> ")
		sb.WriteString(m.steerInput)
		// Render a block cursor.
		sb.WriteString("\u2588")
	}
	// Token usage status bar.
	if m.tokenMax > 0 || m.tokenCost > 0 {
		sb.WriteString("\n")
		sb.WriteString(m.renderStatusBarLocked())
	}
	// Pause indicator.
	if m.paused {
		sb.WriteString("\n[PAUSED - press Space to resume]")
	}
	// Goodbye line when the user requested quit.
	if m.quitting {
		sb.WriteString("\n[quitting...]")
	}
	return sb.String()
}

// renderStatusBarLocked formats the token usage status bar. When the token
// usage percentage exceeds 80%, the text is wrapped in ANSI yellow to warn the
// user. The caller must hold m.mu.
func (m *teaModel) renderStatusBarLocked() string {
	total := m.tokenInput + m.tokenOutput
	maxT := m.tokenMax
	pct := 0
	if maxT > 0 {
		pct = total * 100 / maxT
	}
	bar := fmt.Sprintf("Tokens: %d/%d (%d%%) | Cost: $%.4f", total, maxT, pct, m.tokenCost)
	if pct > 80 {
		// ANSI yellow foreground.
		return "\x1b[33m" + bar + "\x1b[0m"
	}
	return bar
}

// ---------- command factories ----------

// agentEventMsg wraps an AgentEvent delivered from the event stream so the
// model can distinguish stream events from messages pushed via Send.
type agentEventMsg struct {
	event AgentEvent
}

// approvalRequestMsg wraps an ApprovalRequest delivered from the approval
// channel so the model can render it as an interactive approval entry.
type approvalRequestMsg struct {
	req ApprovalRequest
}

// waitForApproval returns a tea.Cmd that blocks until an ApprovalRequest
// arrives on the channel. When done closes (the run is ending) it returns nil
// so the command goroutine exits instead of leaking.
func waitForApproval(ch <-chan ApprovalRequest, done <-chan struct{}) tea.Cmd {
	return func() tea.Msg {
		select {
		case req, ok := <-ch:
			if !ok {
				return nil
			}
			return approvalRequestMsg{req: req}
		case <-done:
			return nil
		}
	}
}

// waitForEvent returns a tea.Cmd that blocks until an event arrives on the
// stream. When the stream closes it returns tea.Quit so the program exits
// cleanly; otherwise it returns the event wrapped in an agentEventMsg. When
// done closes (the run is ending) it returns nil so the command goroutine can
// exit instead of leaking.
func waitForEvent(events <-chan AgentEvent, done <-chan struct{}) tea.Cmd {
	return func() tea.Msg {
		select {
		case ev, ok := <-events:
			if !ok {
				return tea.Quit()
			}
			return agentEventMsg{event: ev}
		case <-done:
			return nil
		}
	}
}

// waitForMsg returns a tea.Cmd that blocks until a message is pushed via Send.
// When the message channel closes it returns tea.Quit. When done closes it
// returns nil so the command goroutine can exit instead of leaking.
func waitForMsg(ch chan Msg, done <-chan struct{}) tea.Cmd {
	return func() tea.Msg {
		select {
		case msg, ok := <-ch:
			if !ok {
				return tea.Quit()
			}
			return tea.Msg(msg)
		case <-done:
			return nil
		}
	}
}

// waitForQuit returns a tea.Cmd that blocks until the quit channel closes or
// done closes, then returns tea.Quit. This makes Quit() race-free regardless of
// whether the program has started. Selecting on done ensures the command
// goroutine exits instead of leaking when the run ends (e.g. when the event
// channel closes and the program exits via waitForEvent's tea.Quit).
func waitForQuit(ch <-chan struct{}, done <-chan struct{}) tea.Cmd {
	return func() tea.Msg {
		select {
		case <-ch:
			return tea.Quit()
		case <-done:
			return nil
		}
	}
}

// send pushes a message onto the model's queue, dropping it when full so a slow
// consumer never blocks the producer.
func (m *teaModel) send(msg Msg) {
	select {
	case m.msgCh <- msg:
	default:
	}
}

// ---------- event handling ----------

// handleEvent dispatches a single agent event to its renderer and adds the
// result to the accordion model. Incremental events (streaming tokens) are
// accumulated into the last assistant entry instead of creating new entries.
// Tool calls, tool results and thinking entries are stored as collapsible
// entries; other entries are expanded by default. It returns a tea.Cmd (a
// spinner tick) when the spinner is activated, nil otherwise.
func (m *teaModel) handleEvent(ev AgentEvent) tea.Cmd {
	ctx := context.Background()

	// Handle token_usage events by updating the status bar data and re-rendering.
	if ev.Type == "token_usage" && ev.TokenUsage != nil {
		var view string
		notify := m.shouldNotify()
		m.mu.Lock()
		m.tokenInput = ev.TokenUsage.InputTokens
		m.tokenOutput = ev.TokenUsage.OutputTokens
		m.tokenMax = ev.TokenUsage.MaxTokens
		m.tokenCost = ev.TokenUsage.Cost
		if notify {
			view = m.renderViewLocked()
		}
		m.mu.Unlock()
		m.notify(view)
		return nil
	}

	if ev.Incremental {
		m.handleIncremental(ctx, ev)
		return nil
	}

	// Finalize any active streaming entry so the next incremental event
	// creates a fresh entry rather than appending to a completed message.
	m.mu.Lock()
	hadStreaming := m.streamingEntry != nil
	m.streamingEntry = nil
	m.streamingBuf.Reset()
	m.mu.Unlock()

	// When the loop finishes streaming, it sends a non-incremental "message"
	// event carrying the complete content for downstream consumers (harness
	// result, lastMessageEvent). The TUI has already rendered the content
	// incrementally, so skip this duplicate.
	if hadStreaming && ev.Type == "message" {
		return nil
	}

	// Update spinner state for tool lifecycle events. The spinner is
	// activated on tool_call and deactivated on tool_result.
	activateSpinner := false
	if ev.ContentType == ContentTypeToolCall || ev.ContentType == ContentTypeToolResult {
		m.mu.Lock()
		if ev.ContentType == ContentTypeToolCall {
			m.spinnerActive = true
			activateSpinner = true
		} else {
			m.spinnerActive = false
		}
		m.mu.Unlock()
	}

	start := time.Now()
	ct := ev.ContentType
	r, ok := m.reg.Get(ct)
	if !ok {
		ct = DefaultContentType
		r, ok = m.reg.Get(DefaultContentType)
	}
	out := ev.Content
	if ok && r != nil {
		theme := Theme(DarkTheme{})
		if m.themeMgr != nil {
			theme = m.themeMgr.Get()
		}
		out = r.Render(ctx, ev.Content, RenderOpts{
			Theme:       theme,
			Width:       m.width,
			ContentType: ct,
			Stream:      ev.Stream,
		})
	}

	// tool_output events append to the last tool_call entry instead of
	// creating a new top-level entry, keeping streaming output grouped
	// with its originating tool call.
	if ct == ContentTypeToolOutput {
		var view string
		notify := m.shouldNotify()
		appended := false
		m.mu.Lock()
		if len(m.accordion.entries) > 0 {
			last := m.accordion.entries[len(m.accordion.entries)-1]
			if last.ContentType == ContentTypeToolCall {
				if last.Full != "" {
					last.Full += "\n" + out
				} else {
					last.Full = out
				}
				last.Summary = summarizeFirstLine(last.Full, 80)
				appended = true
				if notify {
					view = m.renderViewLocked()
				}
			}
		}
		m.mu.Unlock()
		if appended {
			m.notify(view)
			slog.DebugContext(ctx, "tui.app.render",
				"content_type", ct,
				"trace_id", ev.TraceID,
				"span_id", ev.SpanID,
				"latency_us", time.Since(start).Microseconds(),
			)
			return nil
		}
		// Fall through to addEntry if no tool_call entry to append to.
	}

	m.addEntry(ct, out)
	slog.DebugContext(ctx, "tui.app.render",
		"content_type", ct,
		"trace_id", ev.TraceID,
		"span_id", ev.SpanID,
		"latency_us", time.Since(start).Microseconds(),
	)

	if activateSpinner {
		return func() tea.Msg { return m.spinner.Tick() }
	}
	return nil
}

// handleIncremental processes a streaming token chunk. On the first chunk it
// creates a new accordion entry; on subsequent chunks it appends to the
// accumulated content and re-renders the entry in place, producing a live
// typewriter effect.
//
// For markdown-style content (assistant text/code streaming) the rendering goes
// through StreamingMarkdownRenderer, which caches already-rendered stable lines
// and re-renders only the trailing unstable lines. This avoids re-parsing the
// full accumulated buffer on every token chunk, which was O(n²).
func (m *teaModel) handleIncremental(ctx context.Context, ev AgentEvent) {
	ct := ev.ContentType
	r, ok := m.reg.Get(ct)
	if !ok {
		ct = DefaultContentType
		r, ok = m.reg.Get(DefaultContentType)
	}

	var view string
	notify := m.shouldNotify()

	m.mu.Lock()
	m.streamingBuf.WriteString(ev.Content)
	accumulated := m.streamingBuf.String()

	theme := Theme(DarkTheme{})
	if m.themeMgr != nil {
		theme = m.themeMgr.Get()
	}
	opts := RenderOpts{
		Theme:       theme,
		Width:       m.width,
		ContentType: ct,
	}

	// A nil streamingEntry marks the first chunk of a new streaming message.
	// Reset the streaming markdown cache before rendering so stale rendered
	// lines from the previous message are dropped.
	if m.streamingEntry == nil && m.streamingMD != nil {
		m.streamingMD.Reset()
	}

	// Render the accumulated content so styles wrap the full message.
	out := accumulated
	if ok && r != nil {
		if m.isStreamingMarkdown(ct) {
			// Incremental markdown rendering: cache stable lines and only
			// re-render the unstable tail, avoiding an O(n²) full re-render
			// per streaming token.
			if m.streamingMD == nil {
				m.streamingMD = NewStreamingMarkdownRenderer(NewMarkdownRenderer())
			}
			out = m.streamingMD.RenderIncremental(ctx, accumulated, opts)
		} else {
			out = r.Render(ctx, accumulated, opts)
		}
	}

	if m.streamingEntry == nil {
		entry := entryFor(ct, out, m.interactive)
		entry.Collapsed = false
		m.accordion.Add(entry)
		m.streamingEntry = entry
	} else {
		m.streamingEntry.Full = out
		m.streamingEntry.Summary = summarizeFirstLine(out, 80)
	}

	if notify {
		view = m.renderViewLocked()
	}
	m.mu.Unlock()

	m.notify(view)
}

// isStreamingMarkdown reports whether an incremental event of content type ct
// should be rendered as streaming markdown via StreamingMarkdownRenderer
// instead of the registry's renderer. Assistant text (markdown and the
// streaming text/code variants) is treated as markdown; streaming thinking is
// faint plain text and keeps the registry's StreamingThinkingRenderer.
func (m *teaModel) isStreamingMarkdown(ct string) bool {
	switch ct {
	case ContentTypeMarkdown, ContentTypeStreaming, ContentTypeStreamingCode:
		return true
	}
	return false
}

// addEntry appends a rendered frame to the accordion model. Streaming
// renderers replace the last entry instead of appending, giving a live-update
// effect. After mutating the model the view is rendered (under the lock) and
// the onUpdate callback is invoked outside the lock so it can safely write to
// the terminal.
func (m *teaModel) addEntry(ct string, out string) {
	var view string
	notify := m.shouldNotify()
	m.mu.Lock()
	if isStreamingRenderContentType(ct) {
		if m.accordion.Len() == 0 {
			m.accordion.Add(entryFor(ct, out, m.interactive))
		} else {
			last := m.accordion.Entries()[m.accordion.Len()-1]
			last.Full = out
			last.Summary = summarizeFirstLine(out, 80)
			last.Collapsed = false
		}
	} else {
		entry := entryFor(ct, out, m.interactive)
		if !m.interactive {
			entry.Collapsed = false
			for _, c := range entry.Children {
				c.Collapsed = false
			}
		}
		m.accordion.Add(entry)
	}
	if notify {
		view = m.renderViewLocked()
	}
	m.mu.Unlock()
	m.notify(view)
}

// shouldNotify reports whether the onUpdate callback should fire for a view
// mutation. It only fires in non-interactive mode: in interactive (TTY) mode
// bubbletea owns terminal rendering, so the callback is suppressed to avoid
// double rendering (the callback is removed entirely in 34-7).
func (m *teaModel) shouldNotify() bool {
	return m.onUpdate != nil && !m.interactive
}

// notify invokes the onUpdate callback with a freshly rendered view. It must
// be called outside m.mu so the callback can safely write to the terminal.
func (m *teaModel) notify(view string) {
	if m.onUpdate != nil && !m.interactive {
		m.onUpdate(view)
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

// ---------- keyboard handling ----------

// handleKey processes a tea.KeyMsg. In steer input mode keys edit the steer
// buffer; otherwise they navigate the accordion, toggle pause, enter steer
// mode, or quit. It returns a tea.Cmd (tea.Quit when quitting, nil otherwise).
// The caller must NOT hold m.mu; handleKey acquires it.
func (m *teaModel) handleKey(msg tea.KeyMsg) tea.Cmd {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.steerInputMode {
		return m.handleSteerKeyLocked(msg)
	}
	// When an approval request is pending, intercept y/a/n/d keys to resolve
	// it before any other key processing.
	if m.pendingApproval != nil {
		return m.handleApprovalKeyLocked(msg)
	}
	switch msg.Type {
	case tea.KeyTab:
		// Enter steer input mode.
		m.steerInputMode = true
		m.steerInput = ""
		m.steerCursor = 0
	case tea.KeyEsc, tea.KeyCtrlC:
		return m.quitLocked()
	case tea.KeySpace:
		m.togglePauseLocked()
	case tea.KeyUp:
		m.accordion.Select(-1)
	case tea.KeyDown:
		m.accordion.Select(1)
	case tea.KeyEnter:
		m.accordion.Toggle()
	case tea.KeyRunes:
		switch string(msg.Runes) {
		case "q":
			return m.quitLocked()
		case "e":
			m.accordion.ExpandAll()
		case "c":
			m.accordion.CollapseAll()
		}
	}
	return nil
}

// handleSteerKeyLocked processes a key received while in steer input mode. The
// caller must hold m.mu. It returns a tea.Cmd (tea.Quit on Esc, nil otherwise).
func (m *teaModel) handleSteerKeyLocked(msg tea.KeyMsg) tea.Cmd {
	switch msg.Type {
	case tea.KeyEsc:
		// Esc exits steer mode without submitting.
		m.steerInputMode = false
		m.steerInput = ""
		m.steerCursor = 0
	case tea.KeyEnter:
		// Enter submits the steer text via the callback and exits steer mode.
		input := m.steerInput
		m.steerInputMode = false
		m.steerInput = ""
		m.steerCursor = 0
		if m.steerCallback != nil && input != "" {
			cb := m.steerCallback
			// Run in a goroutine so it does not block the render loop.
			go cb(input)
		}
	case tea.KeyBackspace:
		if m.steerCursor > 0 {
			m.steerInput = m.steerInput[:m.steerCursor-1] + m.steerInput[m.steerCursor:]
			m.steerCursor--
		}
	case tea.KeyCtrlA:
		m.steerCursor = 0
	case tea.KeyCtrlE:
		m.steerCursor = len(m.steerInput)
	case tea.KeyCtrlW:
		m.deleteWordLocked()
	case tea.KeyCtrlU:
		// Ctrl+U clears the steer input line.
		m.steerInput = ""
		m.steerCursor = 0
	case tea.KeyRunes:
		for _, r := range msg.Runes {
			m.steerInput = m.steerInput[:m.steerCursor] + string(r) + m.steerInput[m.steerCursor:]
			m.steerCursor++
		}
	}
	return nil
}

// handleApprovalKeyLocked processes a key while an approval request is pending.
// y allows once, a always allows, n denies, and d toggles the diff preview
// (currently informational only). Any other key is ignored. The caller must
// hold m.mu.
func (m *teaModel) handleApprovalKeyLocked(msg tea.KeyMsg) tea.Cmd {
	if msg.Type != tea.KeyRunes {
		return nil
	}
	switch string(msg.Runes) {
	case "y":
		m.pendingApproval.ResponseCh <- ApprovalResponse{Decision: ApprovalAllow}
		m.pendingApproval = nil
	case "a":
		m.pendingApproval.ResponseCh <- ApprovalResponse{Decision: ApprovalAlwaysAllow}
		m.pendingApproval = nil
	case "n":
		m.pendingApproval.ResponseCh <- ApprovalResponse{Decision: ApprovalDeny}
		m.pendingApproval = nil
	case "d":
		// Diff preview is always shown when available; 'd' is a no-op
		// placeholder for future toggle behavior.
	}
	return nil
}

// togglePauseLocked flips the paused state and invokes the matching callback.
// The caller must hold m.mu.
func (m *teaModel) togglePauseLocked() {
	if m.paused {
		m.paused = false
		if m.resumeCallback != nil {
			m.resumeCallback()
		}
	} else {
		m.paused = true
		if m.pauseCallback != nil {
			m.pauseCallback()
		}
	}
}

// quitLocked marks the model as quitting, invokes the cancel callback, and
// returns the tea.Quit command. The caller must hold m.mu.
func (m *teaModel) quitLocked() tea.Cmd {
	m.quitting = true
	if m.cancelCallback != nil {
		m.cancelCallback()
	}
	return tea.Quit
}

// deleteWordLocked deletes the word before the cursor (Ctrl+W behavior). The
// caller must hold m.mu.
func (m *teaModel) deleteWordLocked() {
	if m.steerCursor == 0 {
		return
	}
	// Skip trailing spaces.
	end := m.steerCursor
	for end > 0 && m.steerInput[end-1] == ' ' {
		end--
	}
	// Skip word characters.
	for end > 0 && m.steerInput[end-1] != ' ' {
		end--
	}
	m.steerInput = m.steerInput[:end] + m.steerInput[m.steerCursor:]
	m.steerCursor = end
}

// ---------- terminal detection ----------

// IsTerminal reports whether stdin is a real terminal (TTY). When true the
// interactive accordion mode is enabled with keyboard navigation. A char-device
// check alone is insufficient (e.g. /dev/null is a char device but not a TTY),
// so this uses a real ioctl-based TTY probe so bubbletea never falls back to
// opening /dev/tty in non-interactive contexts such as tests.
func IsTerminal() bool {
	return term.IsTerminal(os.Stdin.Fd())
}

// isTerminal is the unexported alias used by the constructor.
func isTerminal() bool { return IsTerminal() }
