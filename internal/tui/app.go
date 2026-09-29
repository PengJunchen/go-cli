package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/term"
	"github.com/muesli/termenv"
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
	// ToolCallID is the source-provided tool identifier on tool lifecycle and
	// output events. The live view associates it only within the active group.
	ToolCallID string
	// Stream identifies the output source for tool_output events: "stdout"
	// or "stderr".
	Stream string
	// IsError preserves the structured error marker from tool events. On a
	// uniquely associated tool_result, it sets the call status to Error.
	IsError bool
	// RunID and transcript coordinates identify the producer-owned occurrence.
	RunID                      string
	CanonicalTranscriptIndex   *uint64
	AssistantTranscriptOrdinal *uint64
	// ReceiptOutcome carries the exact typed tool receipt outcome when present.
	ReceiptOutcome string
	// SourceTruncated is copied only from structured source metadata.
	SourceTruncated *bool
	// ToolCalls carries the assistant-proposed tool calls attached to a
	// message event. These are provisional until a matching tool_call event
	// confirms execution has started.
	ToolCalls []ToolCallData
}

// ToolCallData is the presentation-safe subset of an assistant tool call.
// ArgsUnavailable is set when the source arguments are not a valid JSON value
// or cannot be copied safely; callers should render that state explicitly.
type ToolCallData struct {
	ID              string
	Name            string
	Args            any
	ArgsUnavailable bool
}

// activeToolCallGroup contains only presentation-time associations for the
// current assistant tool-call message. It is reset at the next assistant
// message boundary and does not create durable session identifiers.
type activeToolCallGroup struct {
	byID      map[string]*AccordionEntry
	ambiguous map[string]struct{}
	consumed  map[string]struct{}
}

func newActiveToolCallGroup() *activeToolCallGroup {
	return &activeToolCallGroup{
		byID:      make(map[string]*AccordionEntry),
		ambiguous: make(map[string]struct{}),
		consumed:  make(map[string]struct{}),
	}
}

func (g *activeToolCallGroup) lookup(id string) (*AccordionEntry, bool) {
	if g == nil || id == "" {
		return nil, false
	}
	if _, ambiguous := g.ambiguous[id]; ambiguous {
		return nil, false
	}
	if _, consumed := g.consumed[id]; consumed {
		return nil, false
	}
	entry, ok := g.byID[id]
	return entry, ok
}

func (g *activeToolCallGroup) add(id string, entry *AccordionEntry) bool {
	if g == nil || id == "" || entry == nil {
		return false
	}
	if _, ambiguous := g.ambiguous[id]; ambiguous {
		return false
	}
	if _, consumed := g.consumed[id]; consumed {
		return false
	}
	if _, exists := g.byID[id]; exists {
		delete(g.byID, id)
		g.ambiguous[id] = struct{}{}
		return false
	}
	g.byID[id] = entry
	return true
}

// consume retires an ID after its single terminal result receipt. Later events
// with the same ID remain visible but cannot be attached to the old call.
func (g *activeToolCallGroup) consume(id string) {
	if g == nil || id == "" {
		return
	}
	delete(g.byID, id)
	g.consumed[id] = struct{}{}
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
	// Send enqueues a message for the render loop to process, returning an
	// inspectable error if the bounded queue is full or the app is stopping.
	Send(msg Msg) error
	// CriticalSendContext enqueues a critical message, waiting for queue space
	// while the current app run remains active and ctx is not canceled.
	CriticalSendContext(ctx context.Context, msg Msg) error
	// LocalSendGapCount reports messages rejected by the local TUI queue.
	LocalSendGapCount() uint64
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

// WithFollowUpCallback registers a callback invoked when the user submits
// follow-up text (Enter in follow-up input mode). The callback receives the
// typed text and runs in a separate goroutine so it does not block the render
// loop.
func WithFollowUpCallback(cb func(string)) AppOption {
	return func(a *BubbleteaApp) { a.followUpCallback = cb }
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

// WithThinkingVisibility controls how thinking-chain entries are displayed:
// "show" (default) expands them, "collapse" folds them to a summary, "hide"
// suppresses them entirely.
func WithThinkingVisibility(mode string) AppOption {
	return func(a *BubbleteaApp) { a.thinkingVisibility = mode }
}

// WithModelInfo sets the model name displayed on the status bar's first line.
func WithModelInfo(model string) AppOption {
	return func(a *BubbleteaApp) { a.modelName = model }
}

// WithTurnCount sets the current turn number displayed on the status bar.
func WithTurnCount(n int) AppOption {
	return func(a *BubbleteaApp) { a.turnCount = n }
}

// WithSessionInfo sets the session ID displayed on the status bar's first line.
func WithSessionInfo(sessionID string) AppOption {
	return func(a *BubbleteaApp) { a.sessionID = sessionID }
}

// WithModeLabel sets the mode label (e.g. "chat" or "plan") shown on the
// status bar's first line.
func WithModeLabel(label string) AppOption {
	return func(a *BubbleteaApp) { a.modeLabel = label }
}

// WithThemeConfig resolves and applies the named theme (dark/light/monokai/
// solarized/auto) to the theme manager. "auto" or an empty name falls back to
// the default dark theme.
func WithThemeConfig(theme string) AppOption {
	return func(a *BubbleteaApp) {
		if a.themeMgr == nil {
			return
		}
		name := strings.TrimSpace(strings.ToLower(theme))
		if name == "" || name == "auto" {
			name = "dark"
		}
		if err := a.themeMgr.Set(name); err != nil {
			slog.Debug("tui.app.theme_config", "theme", theme, "err", err)
		}
	}
}

// WithWordWrap sets the markdown word-wrap width (0 disables wrapping).
func WithWordWrap(width int) AppOption {
	return func(a *BubbleteaApp) { a.wordWrap = width }
}

// WithDiffStyle sets the diff rendering style ("unified", "split", or "auto").
func WithDiffStyle(style string) AppOption {
	return func(a *BubbleteaApp) { a.diffStyle = style }
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
	// bubbletea event loop. Accessed atomically because Run writes it from
	// a goroutine while Program() may be read from another.
	program atomic.Pointer[tea.Program]

	// done closes once Run has fully cleaned up and exited.
	done chan struct{}
	// queueSpace receives a coalesced notification after the render loop takes
	// a queued message, allowing critical senders to wait without polling.
	queueSpace chan struct{}
	// sendMu protects send admission and the active run/quit state.
	sendMu        sync.Mutex
	activeRunDone <-chan struct{}
	quitRequested bool

	// quitOnce guards the quit channel so Quit is idempotent.
	quitOnce sync.Once
	quitCh   chan struct{}

	// programReady is closed once Run has stored the tea.Program, allowing
	// callers to wait for program availability without polling. It is closed
	// at most once via readyOnce so Run can be called again on the same
	// instance without panicking (the channel remains closed for subsequent
	// runs; callers should always pair ProgramReady with a nil-check on
	// Program()).
	programReady chan struct{}
	readyOnce    sync.Once

	// running guards against concurrent Run invocations.
	running atomic.Bool
	// cleaned tracks whether cleanup has already closed done.
	cleaned bool

	// Option fields applied by WithXxx. They are mirrored into the model at
	// construction time and retained here for test inspection.
	reg         *RendererRegistry
	themeMgr    *ThemeManager
	events      <-chan AgentEvent
	width       int
	inputReader io.Reader
	interactive bool

	onUpdate           func(string)
	steerCallback      func(string)
	followUpCallback   func(string)
	cancelCallback     func()
	pauseCallback      func()
	resumeCallback     func()
	approvalCh         <-chan ApprovalRequest
	thinkingVisibility string

	// Status bar info fields (34-7): model name, turn count, session ID and
	// mode label are rendered on the first status bar line.
	modelName string
	turnCount int
	sessionID string
	modeLabel string
	wordWrap  int
	diffStyle string
}

// NewBubbleteaApp constructs an app wired to the given event source, using a
// default renderer registry, a fresh theme manager, and an unbuffered message
// queue. Options override these defaults. The teaModel is built immediately so
// it can be exercised directly by tests; the tea.Program is created lazily in
// Run where the run context is available.
func NewBubbleteaApp(events <-chan AgentEvent, opts ...AppOption) *BubbleteaApp {
	a := &BubbleteaApp{
		reg:          NewDefaultRegistry(),
		themeMgr:     NewThemeManager(),
		events:       events,
		done:         make(chan struct{}),
		queueSpace:   make(chan struct{}, 1),
		quitCh:       make(chan struct{}),
		programReady: make(chan struct{}),
		interactive:  isTerminal(),
	}
	for _, opt := range opts {
		opt(a)
	}
	// Set the lipgloss color profile based on NO_COLOR/CLICOLOR/CLICOLOR_FORCE
	// env vars and the render stream (stderr). This ensures color detection is
	// consistent with the stream lipgloss actually renders to.
	lipgloss.SetColorProfile(resolveColorProfile(os.Stderr))
	a.model = newTeaModel(a)
	return a
}

// Run starts the bubbletea program. It returns when the context is canceled,
// the event channel closes, or Quit is invoked. The loop tears down internal
// resources before returning.
//
// In interactive (TTY) mode bubbletea owns terminal rendering (raw mode,
// keyboard, resize, diff-based repaint) on stderr. In non-interactive mode the
// renderer is disabled; callers that need streaming output in non-interactive
// mode can still use WithOnUpdate, though interactive.go no longer wires it
// (the final assistant message is printed directly instead).
func (a *BubbleteaApp) Run(ctx context.Context) error {
	if !a.running.CompareAndSwap(false, true) {
		return errAlreadyRunning
	}
	// runCtx is canceled when Run returns so any waitForEvent/waitForMsg
	// command goroutines still blocked on their channels unblock and exit
	// (bubbletea cannot kill a cmd func blocked on a user channel). It is also
	// passed to the program as its context so context cancellation propagates
	// to the event loop.
	runCtx, cancelRun := context.WithCancel(ctx)
	defer func() {
		cancelRun()
		a.sendMu.Lock()
		a.activeRunDone = nil
		a.running.Store(false)
		a.sendMu.Unlock()
		a.program.Store(nil)
		a.cleanup()
	}()

	a.sendMu.Lock()
	a.activeRunDone = runCtx.Done()
	a.sendMu.Unlock()
	a.model.runDone = runCtx.Done()
	prog := tea.NewProgram(a.model, a.programOptions(runCtx)...)
	a.program.Store(prog)
	a.readyOnce.Do(func() { close(a.programReady) })
	_, err := prog.Run()
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
		// Non-interactive: disable input (no keyboard) and rendering. If a
		// WithOnUpdate callback is wired, it streams the view to stdout;
		// otherwise the caller prints the final result directly.
		opts = append(opts, tea.WithoutRenderer(), tea.WithInput(nil))
	}
	return opts
}

// Quit requests a graceful stop of the render loop. It is safe to call
// multiple times and from any goroutine.
func (a *BubbleteaApp) Quit() {
	a.quitOnce.Do(func() {
		a.sendMu.Lock()
		a.quitRequested = true
		close(a.quitCh)
		a.sendMu.Unlock()
	})
	slog.Debug("tui.app.quit", "requested", true)
}

// Done returns a channel that closes once the render loop has fully cleaned
// up and exited, enabling callers to wait on quit completion.
func (a *BubbleteaApp) Done() <-chan struct{} { return a.done }

// Program returns the underlying tea.Program after Run has started.
// Returns nil before Run is called or after it has exited.
func (a *BubbleteaApp) Program() *tea.Program {
	return a.program.Load()
}

// ProgramReady returns a channel that closes once Run has stored the
// tea.Program. Callers can wait on it instead of polling Program().
// The channel is already-closed if Run has started, and remains open
// if Run has not been called yet. The channel is closed at most once;
// if Run is called again on the same instance the channel stays closed
// from the first run, so callers should always pair ProgramReady with
// a nil-check on Program() to handle the window where the program has
// not yet been stored.
func (a *BubbleteaApp) ProgramReady() <-chan struct{} {
	return a.programReady
}

// EventsProcessed reports how many agent events the loop has consumed.
func (a *BubbleteaApp) EventsProcessed() int64 { return a.model.eventsSeen.Load() }

// MessagesProcessed reports how many messages the loop has consumed via Send.
func (a *BubbleteaApp) MessagesProcessed() int64 { return a.model.msgsSeen.Load() }

// LocalSendGapCount reports messages rejected by the local TUI message queue.
// It is independent of any event-gap count reported by Core.
func (a *BubbleteaApp) LocalSendGapCount() uint64 {
	return a.model.localSendGapCount.Load()
}

// View returns the current rendered view. It is safe for concurrent callers and
// delegates to the model under its mutex. In interactive mode the accordion
// model produces collapsed/expanded output; in non-interactive mode all entries
// are rendered expanded (legacy behavior).
func (a *BubbleteaApp) View() string {
	return a.model.View()
}

// ThemeManager returns the theme manager used by this app. Callers can use it
// to switch themes at runtime via Set().
func (a *BubbleteaApp) ThemeManager() *ThemeManager {
	return a.themeMgr
}

// cleanup resets per-run state and closes the done channel exactly once. It is
// called from Run's deferred cleanup path.
func (a *BubbleteaApp) cleanup() {
	if a.model != nil {
		a.model.mu.Lock()
		a.model.streamBuf = make(map[string]*strings.Builder)
		a.model.todoSnapshot = nil
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
	reg         *RendererRegistry
	themeMgr    *ThemeManager
	width       int
	height      int
	interactive bool

	// event / message sources
	events     <-chan AgentEvent
	msgCh      chan Msg
	queueSpace chan<- struct{}
	quitCh     <-chan struct{}
	// runDone is closed (via runCtx cancellation) when the current Run exits,
	// unblocking lingering waitForEvent/waitForMsg command goroutines so they
	// do not leak. It is nil when the model is exercised without a program.
	runDone <-chan struct{}

	// accordion + streaming
	accordion      *AccordionModel
	streamBuf      map[string]*strings.Builder
	streamingEntry *AccordionEntry
	streamingBuf   strings.Builder
	toolCallGroup  *activeToolCallGroup
	// streamingMD caches rendered markdown lines during incremental streaming
	// so each token chunk does not trigger an O(n²) full re-render. It is
	// lazily created and Reset when a new streaming message begins.
	streamingMD *StreamingMarkdownRenderer

	// steer input
	steerInputMode bool
	steerInput     string
	steerCursor    int
	steerCallback  func(string)

	// follow-up input
	followUpInputMode bool
	followUpInput     string
	followUpCursor    int
	followUpCallback  func(string)

	// callbacks
	onUpdate       func(string)
	cancelCallback func()
	pauseCallback  func()
	resumeCallback func()
	paused         bool

	// interaction flow serializes approval and ask_user modals in arrival order.
	approvalCh          <-chan ApprovalRequest
	approvalReaderDone  bool
	activeInteraction   *pendingInteraction
	queuedInteractions  []pendingInteraction
	interactionSequence uint64

	// thinkingVisibility controls how thinking entries are rendered:
	// "show" (default), "collapse", or "hide".
	thinkingVisibility string

	// status bar info (34-7): model/turn/session/mode on line 1, tokens/cost
	// on line 2.
	modelName string
	turnCount int
	sessionID string
	modeLabel string
	wordWrap  int
	diffStyle string

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

	// helpOverlay is set when the user presses '?' to show the keyboard
	// shortcut help overlay. While true the overlay is modal: only Esc or
	// '?' close it and all other keys are ignored. It does not block agent
	// event flow — it only affects UI rendering.
	helpOverlay bool

	// todoSnapshot holds the latest todo list snapshot for the persistent
	// progress panel. It is updated when a ContentTypeTodo event or a
	// TodoUpdateMsg arrives, and rendered above the status bar.
	todoSnapshot []TodoItem

	// concurrency: guards all mutable state above. Update runs single-threaded
	// inside bubbletea, but external callers (tests, View) access the model
	// concurrently, so the mutex serializes View against in-flight updates.
	mu                sync.Mutex
	eventsSeen        atomic.Int64
	msgsSeen          atomic.Int64
	localSendGapCount atomic.Uint64
}

// newTeaModel builds a teaModel from the option fields already applied to the
// BubbleteaApp adapter.
func newTeaModel(a *BubbleteaApp) *teaModel {
	return &teaModel{
		reg:                a.reg,
		themeMgr:           a.themeMgr,
		width:              a.width,
		interactive:        a.interactive,
		events:             a.events,
		msgCh:              make(chan Msg, 16),
		queueSpace:         a.queueSpace,
		quitCh:             a.quitCh,
		streamBuf:          make(map[string]*strings.Builder),
		accordion:          NewAccordionModel(),
		onUpdate:           a.onUpdate,
		steerCallback:      a.steerCallback,
		followUpCallback:   a.followUpCallback,
		cancelCallback:     a.cancelCallback,
		pauseCallback:      a.pauseCallback,
		resumeCallback:     a.resumeCallback,
		approvalCh:         a.approvalCh,
		thinkingVisibility: a.thinkingVisibility,
		modelName:          a.modelName,
		turnCount:          a.turnCount,
		sessionID:          a.sessionID,
		modeLabel:          a.modeLabel,
		wordWrap:           a.wordWrap,
		diffStyle:          a.diffStyle,
		spinner:            spinner.New(spinner.WithSpinner(spinner.MiniDot)),
	}
}

// Init starts the bubbletea command loop. It arms listeners for the agent event
// stream, messages pushed via Send, the quit signal, and (when wired) approval
// requests from the middleware. The spinner tick is started on demand.
func (m *teaModel) Init() tea.Cmd {
	done := m.runDone
	cmds := []tea.Cmd{
		waitForEvent(m.events, done),
		waitForMsg(m.msgCh, done, m.queueSpace),
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
	case agentEventStreamClosedMsg, quitRequestedMsg:
		m.mu.Lock()
		m.cancelInteractionsLocked()
		m.mu.Unlock()
		return m, tea.Quit
	case approvalChannelClosedMsg:
		m.mu.Lock()
		m.approvalReaderDone = true
		m.cancelApprovalInteractionsLocked()
		view := ""
		if m.shouldNotify() {
			view = m.renderViewLocked()
		}
		m.mu.Unlock()
		m.notify(view)
		return m, nil
	case approvalRequestMsg:
		m.mu.Lock()
		token, finished := m.enqueueApprovalLocked(msg.req)
		view := ""
		if m.shouldNotify() {
			view = m.renderViewLocked()
		}
		var next tea.Cmd
		if !m.approvalReaderDone && m.approvalCh != nil {
			next = waitForApproval(m.approvalCh, m.runDone)
		}
		m.mu.Unlock()
		m.notify(view)
		var cancelWait tea.Cmd
		if finished != nil {
			cancelWait = waitForInteractionCancel(msg.req.Context, token, finished, m.runDone)
		}
		return m, tea.Batch(next, cancelWait)
	case HITLMessage:
		return m.handleHITLMessage(msg)
	case interactionCanceledMsg:
		m.mu.Lock()
		m.cancelInteractionLocked(msg.token, msg.err)
		view := ""
		if m.shouldNotify() {
			view = m.renderViewLocked()
		}
		m.mu.Unlock()
		m.notify(view)
		return m, nil
	case tea.KeyMsg:
		return m, m.handleKey(msg)
	case tea.WindowSizeMsg:
		m.mu.Lock()
		m.width = msg.Width
		m.height = msg.Height
		if current := m.activeInteraction; current != nil && current.approval != nil {
			current.detailOffset = 0
			current.reviewedDetailLines = nil
			current.reviewedDetailCount = 0
		}
		// Panel dimensions (leftWidth, rightWidth, panelHeight) are derived
		// on the fly in renderViewLocked() from m.width/m.height, so no
		// separate fields are needed. When width >= splitWidthThreshold,
		// the layout splits into left (conversation) and right (tool
		// accordion) panels; below the threshold it stays single-column.
		m.mu.Unlock()
		return m, nil
	case spinner.TickMsg:
		m.mu.Lock()
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		active := m.spinnerActive
		// Advance the per-entry spinner frame for all running tool_call
		// entries so each tool shows its own animated spinner icon.
		if active {
			for _, e := range m.accordion.entries {
				if e.ContentType == ContentTypeToolCall && e.ToolStatus == ToolStatusRunning {
					e.SpinnerFrame++
				}
			}
		}
		m.mu.Unlock()
		if !active {
			return m, nil
		}
		return m, cmd
	case tea.QuitMsg:
		return m, nil
	case TodoUpdateMsg:
		m.msgsSeen.Add(1)
		var view string
		notify := m.shouldNotify()
		m.mu.Lock()
		m.todoSnapshot = msg.Items
		if notify {
			view = m.renderViewLocked()
		}
		m.mu.Unlock()
		m.notify(view)
		return m, waitForMsg(m.msgCh, m.runDone, m.queueSpace)
	default:
		// A message pushed via Send that is not a recognized tea message.
		m.msgsSeen.Add(1)
		return m, waitForMsg(m.msgCh, m.runDone, m.queueSpace)
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

// splitWidthThreshold is the minimum terminal width for the left-right split
// layout. Below this, the TUI falls back to a single-column layout.
const splitWidthThreshold = 120

// renderViewLocked builds the full view string. The caller must hold m.mu.
//
// When the terminal is at least splitWidthThreshold columns wide, the accordion
// content is split into a left panel (conversation: user/assistant/thinking)
// and a right panel (tool_call entries with their children), joined
// horizontally with a vertical separator. Below the threshold, or when no tool
// entries exist, the classic single-column layout is used.
func renderInputWithCaret(input string, cursor int) string {
	if cursor < 0 {
		cursor = 0
	}
	if cursor > len(input) {
		cursor = len(input)
	}
	for cursor > 0 && cursor < len(input) && !utf8.RuneStart(input[cursor]) {
		cursor--
	}
	return safeTerminalText(input[:cursor]) + "█" + safeTerminalText(input[cursor:])
}

func (m *teaModel) renderViewLocked() string {
	if current := m.activeInteraction; current != nil && current.approval != nil {
		return m.withLocalSendGapNotice(m.renderApprovalInteractionLocked(current))
	}
	// When the help overlay is open, render it instead of the normal view.
	// Agent event flow is unaffected — only UI rendering is redirected.
	if m.helpOverlay {
		return m.withLocalSendGapNotice(m.renderHelpOverlay())
	}
	var sb strings.Builder

	// --- Accordion content (split or single-column) ---
	// Only activate the split layout when tool entries exist; otherwise
	// the conversation gets the full width.
	useSplit := m.width >= splitWidthThreshold &&
		m.accordion != nil && m.accordion.Len() > 0 && m.height > 0
	if useSplit {
		hasTools := false
		for _, e := range m.accordion.entries {
			if isToolEntry(e) {
				hasTools = true
				break
			}
		}
		useSplit = hasTools
	}

	if useSplit {
		leftWidth := m.width / 2
		rightWidth := m.width - leftWidth - 1 // -1 for the separator column
		if rightWidth < 1 {
			rightWidth = 1
		}
		// Reserve ~2 lines for the status bar at the bottom.
		panelHeight := m.height - 2
		if panelHeight < 1 {
			panelHeight = 0 // no clipping
		}

		leftPanel := m.accordion.RenderView(leftWidth, panelHeight, false)
		rightPanel := m.accordion.RenderView(rightWidth, panelHeight, true)

		switch {
		case rightPanel == "" && leftPanel == "":
			// nothing to render
		case rightPanel == "":
			sb.WriteString(leftPanel)
		case leftPanel == "":
			sb.WriteString(rightPanel)
		default:
			leftStyled := lipgloss.NewStyle().Width(leftWidth).Render(leftPanel)
			rightStyled := lipgloss.NewStyle().Width(rightWidth).Render(rightPanel)
			sb.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, leftStyled, "│", rightStyled))
		}
	} else if m.accordion != nil && m.accordion.Len() > 0 {
		sb.WriteString(m.accordion.Render())
	}

	// --- Bottom section (always full-width) ---
	// Spinner indicator (shown while a tool is executing).
	if m.spinnerActive {
		sb.WriteString("\n")
		sb.WriteString(m.spinner.View())
		sb.WriteString(" working...")
	}
	// The active interaction is rendered as a modal above the status bar.
	if current := m.activeInteraction; current != nil {
		sb.WriteString("\n")
		if current.approval != nil {
			sb.WriteString(renderApprovalRequestDisplay(current.approval, current.showDiff))
		} else if current.hitl != nil {
			sb.WriteString(renderHITLQuestion(current.hitl, current.answer, current.selectedOption))
		}
	}
	// Steer input prompt.
	if m.steerInputMode {
		sb.WriteString("\n> steer> ")
		sb.WriteString(renderInputWithCaret(m.steerInput, m.steerCursor))
	}
	// Follow-up input prompt.
	if m.followUpInputMode {
		sb.WriteString("\n> followup> ")
		sb.WriteString(renderInputWithCaret(m.followUpInput, m.followUpCursor))
	}
	// Todo progress panel (persistent, above the status bar).
	if todoPanel := m.renderTodoPanelLocked(); todoPanel != "" {
		sb.WriteString("\n")
		sb.WriteString(todoPanel)
	}
	// Status bar (model/turn/session/mode + tokens/cost). Shown when any
	// status info is available.
	if m.tokenMax > 0 || m.tokenCost > 0 || m.modelName != "" || m.turnCount > 0 || m.sessionID != "" || m.modeLabel != "" {
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
	return m.withLocalSendGapNotice(sb.String())
}

func (m *teaModel) withLocalSendGapNotice(view string) string {
	count := m.localSendGapCount.Load()
	if count == 0 {
		return view
	}
	if view != "" {
		view += "\n"
	}
	return view + fmt.Sprintf("[Local TUI queue gaps: %d messages not delivered]", count)
}

func (m *teaModel) renderApprovalInteractionLocked(current *pendingInteraction) string {
	width := m.width
	if width <= 0 {
		width = 80
	}
	rows := approvalDetailViewportRows(m.height)
	total := walkApprovalDetailLines(current.approval, current.showDiff, approvalDetailWidth(width), nil)
	if current.reviewedDetailCount != total {
		current.reviewedDetailCount = total
		current.reviewedDetailLines = nil
	}
	offset := normalizeApprovalDetailOffset(total, rows, current.detailOffset)
	current.detailOffset = offset
	if rows > 0 {
		if current.reviewedDetailLines == nil {
			current.reviewedDetailLines = make(map[int]struct{}, total)
		}
		end := min(total, offset+rows)
		for line := offset; line < end; line++ {
			current.reviewedDetailLines[line] = struct{}{}
		}
	}
	reviewed := len(current.reviewedDetailLines) >= total && total > 0
	view, _ := renderApprovalRequestPage(
		current.approval,
		current.showDiff,
		current.token,
		len(m.queuedInteractions),
		width,
		m.height,
		rows,
		offset,
		reviewed,
	)
	return view
}

func (m *teaModel) scrollApprovalDetailsLocked(delta int) {
	current := m.activeInteraction
	if current == nil || current.approval == nil {
		return
	}
	width := m.width
	if width <= 0 {
		width = 80
	}
	rows := approvalDetailViewportRows(m.height)
	total := walkApprovalDetailLines(current.approval, current.showDiff, approvalDetailWidth(width), nil)
	current.detailOffset = normalizeApprovalDetailOffset(total, rows, current.detailOffset+delta)
}

func normalizeApprovalDetailOffset(total, rows, offset int) int {
	if rows <= 0 {
		return 0
	}
	maxOffset := max(0, total-rows)
	return min(max(offset, 0), maxOffset)
}

func approvalDetailsReviewed(current *pendingInteraction, total int) bool {
	if current == nil || total <= 0 || len(current.reviewedDetailLines) < total {
		return false
	}
	for line := 0; line < total; line++ {
		if _, ok := current.reviewedDetailLines[line]; !ok {
			return false
		}
	}
	return true
}

// statusBarLineStyle styles the first status bar line (model/turn/session/mode).
var statusBarLineStyle = lipgloss.NewStyle().Bold(true)

// helpOverlayTitleStyle styles the help overlay title.
var helpOverlayTitleStyle = lipgloss.NewStyle().Bold(true).Underline(true)

// helpOverlayKeyStyle styles the key column in the help overlay.
var helpOverlayKeyStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#7D56F4"))

// renderHelpOverlay returns a styled string listing all keyboard shortcuts.
// The caller must hold m.mu.
func (m *teaModel) renderHelpOverlay() string {
	shortcuts := []struct{ key, desc string }{
		{"Tab", "Toggle steer mode"},
		{"Esc", "Cancel / Close overlay"},
		{"Space", "Pause/resume agent"},
		{"q", "Quit"},
		{"e", "Expand all tool calls"},
		{"c", "Collapse all tool calls"},
		{"f", "Follow-up mode"},
		{"Up/Down", "Navigate accordion"},
		{"Enter", "Toggle entry"},
		{"?", "Show this help"},
		{"Ctrl+C", "Force quit"},
		{"Ctrl+A", "Move cursor to start"},
		{"Ctrl+E", "Move cursor to end"},
		{"Ctrl+W", "Delete previous word"},
		{"Ctrl+U", "Clear input line"},
	}

	var sb strings.Builder
	sb.WriteString(helpOverlayTitleStyle.Render("Keyboard Shortcuts"))
	sb.WriteString("\n")
	sb.WriteString(strings.Repeat("─", 40))
	sb.WriteString("\n")
	for _, sc := range shortcuts {
		key := helpOverlayKeyStyle.Render(fmt.Sprintf("%-10s", sc.key))
		sb.WriteString(key)
		sb.WriteString("  ")
		sb.WriteString(sc.desc)
		sb.WriteString("\n")
	}
	sb.WriteString("\n")
	sb.WriteString(lipgloss.NewStyle().Faint(true).Render("Press ? or Esc to close"))

	return lipgloss.NewStyle().
		Padding(1, 2).
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("#7D56F4")).
		Render(sb.String())
}

// statusBarWarnStyle styles the token usage line when usage exceeds 80%.
var statusBarWarnStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#FFC000"))

// renderWidth returns the effective width for content rendering: the
// configured word-wrap width when non-zero, otherwise the terminal width.
func (m *teaModel) renderWidth() int {
	if m.wordWrap > 0 {
		return m.wordWrap
	}
	return m.width
}

// renderStatusBarLocked formats a two-line status bar. The first line shows
// model/turn/session/mode info; the second shows token usage and cost. When
// token usage exceeds 80%, the second line is rendered in warning color.
// The caller must hold m.mu.
func (m *teaModel) renderStatusBarLocked() string {
	// Line 1: model | turn #N | session | mode
	parts := make([]string, 0, 4)
	if m.modelName != "" {
		parts = append(parts, m.modelName)
	}
	if m.turnCount > 0 {
		parts = append(parts, fmt.Sprintf("turn #%d", m.turnCount))
	}
	if m.sessionID != "" {
		// Truncate long session IDs to the first 8 chars for the status bar.
		sid := m.sessionID
		if len(sid) > 8 {
			sid = sid[:8]
		}
		parts = append(parts, sid)
	}
	if m.modeLabel != "" {
		parts = append(parts, m.modeLabel)
	}
	line1 := strings.Join(parts, " | ")

	// Line 2: tokens/cost — only rendered when token data is available.
	if m.tokenMax == 0 && m.tokenCost == 0 {
		if line1 == "" {
			return ""
		}
		return statusBarLineStyle.Render(line1)
	}
	total := m.tokenInput + m.tokenOutput
	maxT := m.tokenMax
	pct := 0
	if maxT > 0 {
		pct = total * 100 / maxT
	}
	line2 := fmt.Sprintf("Tokens: %d/%d (%d%%) | Cost: $%.4f", total, maxT, pct, m.tokenCost)
	if pct > 80 {
		line2 = statusBarWarnStyle.Render(line2)
	}

	if line1 == "" {
		return line2
	}
	return statusBarLineStyle.Render(line1) + "\n" + line2
}

// ---------- command factories ----------

// agentEventMsg wraps an AgentEvent delivered from the event stream so the
// model can distinguish stream events from messages pushed via Send.
type agentEventMsg struct {
	event AgentEvent
}

type agentEventStreamClosedMsg struct{}

type quitRequestedMsg struct{}

type approvalChannelClosedMsg struct{}

// approvalRequestMsg wraps an ApprovalRequest delivered from the approval
// channel so the model can render it as an interactive approval entry.
type approvalRequestMsg struct {
	req ApprovalRequest
}

type pendingInteraction struct {
	token               uint64
	approval            *ApprovalRequest
	hitl                *HITLMessage
	answer              string
	selectedOption      int
	showDiff            bool
	detailOffset        int
	reviewedDetailCount int
	reviewedDetailLines map[int]struct{}
	finished            chan struct{}
}

type interactionCanceledMsg struct {
	token uint64
	err   error
}

// waitForApproval returns a tea.Cmd that blocks until an ApprovalRequest
// arrives on the channel. When done closes (the run is ending) it returns nil
// so the command goroutine exits instead of leaking.
func waitForApproval(ch <-chan ApprovalRequest, done <-chan struct{}) tea.Cmd {
	return func() tea.Msg {
		select {
		case req, ok := <-ch:
			if !ok {
				return approvalChannelClosedMsg{}
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
				return agentEventStreamClosedMsg{}
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
func waitForMsg(ch chan Msg, done <-chan struct{}, queueSpace chan<- struct{}) tea.Cmd {
	return func() tea.Msg {
		select {
		case msg, ok := <-ch:
			if !ok {
				return tea.Quit()
			}
			select {
			case queueSpace <- struct{}{}:
			default:
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
			return quitRequestedMsg{}
		case <-done:
			return nil
		}
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

	// Handle todo events by capturing the todo list snapshot and
	// re-rendering the persistent progress panel.
	if ev.ContentType == ContentTypeTodo {
		items, err := parseTodoItems(ev.Content)
		if err != nil {
			slog.DebugContext(ctx, "tui.app.todo_parse_error", "err", err)
			return nil
		}
		var view string
		notify := m.shouldNotify()
		m.mu.Lock()
		m.todoSnapshot = items
		if notify {
			view = m.renderViewLocked()
		}
		m.mu.Unlock()
		m.notify(view)
		return nil
	}

	// A done event is a lifecycle marker. Its content can repeat the final
	// answer, so it is never rendered as user-visible text.
	if ev.Type == "done" {
		m.mu.Lock()
		m.streamingEntry = nil
		m.streamingBuf.Reset()
		m.toolCallGroup = nil
		m.mu.Unlock()
		return nil
	}

	if ev.Incremental {
		m.handleIncremental(ctx, ev)
		return nil
	}

	isAssistantMessage := ev.Type == "message" || ev.ContentType == ContentTypeAssistant
	if isAssistantMessage {
		m.closeToolCallGroup()
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
	if hadStreaming && isAssistantMessage {
		m.startToolCallGroup(ev.ToolCalls)
		return nil
	}

	// Finalize any active thinking entries before processing the new event.
	// When a non-thinking, non-incremental event arrives, the last thinking
	// entry is auto-collapsed with a "thought for Xs" duration summary.
	if ev.ContentType != ContentTypeThinking && ev.ContentType != ContentTypeStreamingThink {
		m.finalizeThinking()
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
			Width:       m.renderWidth(),
			ContentType: ct,
			Stream:      ev.Stream,
		})
	}

	if ct == ContentTypeToolCall {
		m.handleToolCallEvent(ev, out)
		slog.DebugContext(ctx, "tui.app.render",
			"content_type", ct,
			"trace_id", ev.TraceID,
			"span_id", ev.SpanID,
			"latency_us", time.Since(start).Microseconds(),
		)
		return func() tea.Msg { return m.spinner.Tick() }
	}

	if ct == ContentTypeToolResult {
		m.handleToolResultEvent(ev, out)
		slog.DebugContext(ctx, "tui.app.render",
			"content_type", ct,
			"trace_id", ev.TraceID,
			"span_id", ev.SpanID,
			"latency_us", time.Since(start).Microseconds(),
		)
		return nil
	}

	// Tool output joins only a uniquely associated call in the active
	// assistant group. Missing, duplicate, and late IDs stay visible as
	// standalone incomplete entries.
	if ct == ContentTypeToolOutput {
		m.handleToolOutputEvent(ev, out)
		slog.DebugContext(ctx, "tui.app.render",
			"content_type", ct,
			"trace_id", ev.TraceID,
			"span_id", ev.SpanID,
			"latency_us", time.Since(start).Microseconds(),
		)
		return nil
	}

	// Respect the thinking visibility setting: "hide" skips the entry entirely,
	// "collapse" forces it collapsed (overriding the default expanded state).
	if ct == ContentTypeThinking {
		if m.thinkingVisibility == "hide" {
			slog.DebugContext(ctx, "tui.app.render",
				"content_type", ct,
				"trace_id", ev.TraceID,
				"span_id", ev.SpanID,
				"latency_us", time.Since(start).Microseconds(),
				"skipped", "thinking_visibility=hide",
			)
			return nil
		}
	}

	if !isAssistantMessage || ev.Content != "" || len(ev.ToolCalls) == 0 {
		m.addEntry(ct, out, ev.ToolCallID)
	}
	if isAssistantMessage {
		m.startToolCallGroup(ev.ToolCalls)
	}

	// Apply thinking visibility: "collapse" forces the entry collapsed.
	if ct == ContentTypeThinking && m.thinkingVisibility == "collapse" {
		m.mu.Lock()
		if len(m.accordion.entries) > 0 {
			last := m.accordion.entries[len(m.accordion.entries)-1]
			if last.ContentType == ContentTypeThinking {
				last.Collapsed = true
			}
		}
		m.mu.Unlock()
	}

	// Record thinking start time for duration display when auto-collapsed.
	if ct == ContentTypeThinking {
		m.mu.Lock()
		if len(m.accordion.entries) > 0 {
			last := m.accordion.entries[len(m.accordion.entries)-1]
			if last.ContentType == ContentTypeThinking {
				last.ToolStartTime = time.Now()
			}
		}
		m.mu.Unlock()
	}

	slog.DebugContext(ctx, "tui.app.render",
		"content_type", ct,
		"trace_id", ev.TraceID,
		"span_id", ev.SpanID,
		"latency_us", time.Since(start).Microseconds(),
	)

	return nil
}

func (m *teaModel) closeToolCallGroup() {
	m.mu.Lock()
	m.toolCallGroup = nil
	m.mu.Unlock()
}

// startToolCallGroup renders assistant-proposed calls as pending entries and
// builds an ephemeral, group-local ID index. Duplicate and empty IDs remain
// visible but cannot be used to associate later events.
func (m *teaModel) startToolCallGroup(calls []ToolCallData) {
	if len(calls) == 0 {
		return
	}

	group := newActiveToolCallGroup()
	counts := make(map[string]int, len(calls))
	for _, call := range calls {
		if call.ID != "" {
			counts[call.ID]++
		}
	}
	for id, count := range counts {
		if count > 1 {
			group.ambiguous[id] = struct{}{}
		}
	}

	var view string
	notify := m.shouldNotify()
	m.mu.Lock()
	m.toolCallGroup = group
	for _, call := range calls {
		entry := entryFor(ContentTypeToolCall, renderToolCallDetails(call), m.interactive, call.ID)
		entry.ToolStatus = ToolStatusPending
		m.accordion.AddStandalone(entry)
		if call.ID != "" && counts[call.ID] == 1 {
			group.byID[call.ID] = entry
		}
	}
	if notify {
		view = m.renderViewLocked()
	}
	m.mu.Unlock()
	m.notify(view)
}

func renderToolCallDetails(call ToolCallData) string {
	name := call.Name
	if name == "" {
		name = "(unnamed)"
	}
	if call.ArgsUnavailable {
		return safeTerminalText("[tool] " + name + " (detail unavailable)")
	}

	args := call.Args
	if args == nil {
		return safeTerminalText((&ToolCallRenderer{}).RenderCollapsed(name, nil))
	}
	payload, err := json.Marshal(args)
	if err != nil {
		return safeTerminalText("[tool] " + name + " (detail unavailable)")
	}
	if object, ok := args.(map[string]any); ok {
		displayArgs := make(map[string]any, len(object))
		for key, value := range object {
			switch value.(type) {
			case nil, bool, string,
				int, int8, int16, int32, int64,
				uint, uint8, uint16, uint32, uint64,
				float32, float64:
				displayArgs[key] = value
			default:
				raw, marshalErr := json.Marshal(value)
				if marshalErr != nil {
					return safeTerminalText("[tool] " + name + " (detail unavailable)")
				}
				displayArgs[key] = string(raw)
			}
		}
		return safeTerminalText((&ToolCallRenderer{}).RenderCollapsed(name, displayArgs))
	}
	return safeTerminalText("[tool] " + name + "(" + string(payload) + ")")
}

// handleToolCallEvent confirms execution only when an actual tool_call event
// arrives. When the active assistant group cannot identify the event uniquely,
// the call remains visible as an independent running entry without creating a
// guessed association.
func (m *teaModel) handleToolCallEvent(ev AgentEvent, rendered string) {
	var view string
	notify := m.shouldNotify()
	m.mu.Lock()
	if m.toolCallGroup == nil {
		m.toolCallGroup = newActiveToolCallGroup()
	}
	group := m.toolCallGroup
	target, matched := group.lookup(ev.ToolCallID)
	if matched && target.ToolStatus == ToolStatusPending {
		target.ToolStatus = ToolStatusRunning
		target.ToolStartTime = time.Now()
	} else if matched {
		// A second start with the same ID inside one group is ambiguous. Keep
		// the original entry intact and show the observed call independently.
		delete(group.byID, ev.ToolCallID)
		group.ambiguous[ev.ToolCallID] = struct{}{}
		matched = false
	}
	if !matched {
		target = entryFor(ContentTypeToolCall, safeTerminalText(rendered), m.interactive, ev.ToolCallID)
		target.ToolStatus = ToolStatusRunning
		target.ToolStartTime = time.Now()
		m.accordion.AddStandalone(target)
		group.add(ev.ToolCallID, target)
	}
	m.spinnerActive = true
	if notify {
		view = m.renderViewLocked()
	}
	m.mu.Unlock()
	m.notify(view)
}

func (m *teaModel) handleToolOutputEvent(ev AgentEvent, rendered string) {
	var view string
	notify := m.shouldNotify()
	m.mu.Lock()
	target, matched := m.toolCallGroup.lookup(ev.ToolCallID)
	if matched {
		if target.Full != "" {
			target.Full += "\n" + rendered
		} else {
			target.Full = rendered
		}
	} else {
		m.addIncompleteToolEventLocked(ContentTypeToolOutput, rendered, ev.ToolCallID, ev.IsError)
	}
	if notify {
		view = m.renderViewLocked()
	}
	m.mu.Unlock()
	m.notify(view)
}

func (m *teaModel) handleToolResultEvent(ev AgentEvent, rendered string) {
	var view string
	notify := m.shouldNotify()
	m.mu.Lock()
	target, matched := m.toolCallGroup.lookup(ev.ToolCallID)
	if matched {
		m.updateToolResultStatusLocked(ev.ToolCallID, ev.IsError)
		child := entryFor(ContentTypeToolResult, rendered, m.interactive, ev.ToolCallID)
		child.IsError = ev.IsError
		target.Children = append(target.Children, child)
		m.toolCallGroup.consume(ev.ToolCallID)
	} else {
		m.addIncompleteToolEventLocked(ContentTypeToolResult, rendered, ev.ToolCallID, ev.IsError)
	}
	m.spinnerActive = m.hasRunningToolLocked()
	if notify {
		view = m.renderViewLocked()
	}
	m.mu.Unlock()
	m.notify(view)
}

func (m *teaModel) addIncompleteToolEventLocked(contentType, rendered, toolCallID string, isError bool) {
	out := "[incomplete]"
	if rendered != "" {
		out += "\n" + rendered
	}
	entry := entryFor(contentType, out, m.interactive, toolCallID)
	entry.Incomplete = true
	entry.IsError = isError
	m.accordion.AddStandalone(entry)
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
		Width:       m.renderWidth(),
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
		entry := entryFor(ct, out, m.interactive, "")
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
func (m *teaModel) addEntry(ct string, out string, toolCallID string) {
	var view string
	notify := m.shouldNotify()
	m.mu.Lock()
	if isStreamingRenderContentType(ct) {
		if m.accordion.Len() == 0 {
			m.accordion.Add(entryFor(ct, out, m.interactive, toolCallID))
		} else {
			last := m.accordion.Entries()[m.accordion.Len()-1]
			last.Full = out
			last.Summary = summarizeFirstLine(out, 80)
			last.Collapsed = false
		}
	} else {
		entry := entryFor(ct, out, m.interactive, toolCallID)
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
// mutation. It only fires in non-interactive mode when a callback is wired:
// in interactive (TTY) mode bubbletea owns terminal rendering, so the callback
// is suppressed to avoid double rendering. interactive.go no longer wires
// onUpdate, but the mechanism is retained for other callers that need
// non-interactive streaming.
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
func entryFor(ct, out string, interactive bool, toolCallID string) *AccordionEntry {
	collapsed := interactive && defaultCollapsed(ct)
	return &AccordionEntry{
		ContentType: ct,
		Summary:     summarizeFirstLine(out, 80),
		Full:        out,
		Collapsed:   collapsed,
		Timestamp:   time.Now(),
		ToolCallID:  toolCallID,
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
	if m.activeInteraction != nil {
		return m.handleInteractionKeyLocked(msg)
	}
	if m.steerInputMode {
		return m.handleSteerKeyLocked(msg)
	}
	if m.followUpInputMode {
		return m.handleFollowUpKeyLocked(msg)
	}
	// The help overlay is modal: while open only Esc or '?' close it and
	// all other keys are ignored. It does not affect agent event flow.
	if m.helpOverlay {
		if msg.Type == tea.KeyEsc || (msg.Type == tea.KeyRunes && len(msg.Runes) > 0 && msg.Runes[0] == '?') {
			m.helpOverlay = false
		}
		return nil
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
		case "?":
			// Show the keyboard shortcut help overlay.
			m.helpOverlay = true
			return nil
		case "q":
			return m.quitLocked()
		case "e":
			m.accordion.ExpandAll()
		case "c":
			m.accordion.CollapseAll()
		case "f":
			// Enter follow-up input mode.
			m.followUpInputMode = true
			m.followUpInput = ""
			m.followUpCursor = 0
		}
	}
	return nil
}

// moveInputCursorLeft moves a byte offset to the previous UTF-8 rune boundary.
func moveInputCursorLeft(input string, cursor int) int {
	if cursor <= 0 {
		return 0
	}
	if cursor > len(input) {
		cursor = len(input)
	}
	_, size := utf8.DecodeLastRuneInString(input[:cursor])
	return cursor - size
}

// moveInputCursorRight moves a byte offset to the next UTF-8 rune boundary.
func moveInputCursorRight(input string, cursor int) int {
	if cursor < 0 {
		cursor = 0
	}
	if cursor >= len(input) {
		return len(input)
	}
	_, size := utf8.DecodeRuneInString(input[cursor:])
	return cursor + size
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
			_, size := utf8.DecodeLastRuneInString(m.steerInput[:m.steerCursor])
			m.steerInput = m.steerInput[:m.steerCursor-size] + m.steerInput[m.steerCursor:]
			m.steerCursor -= size
		}
	case tea.KeyLeft:
		m.steerCursor = moveInputCursorLeft(m.steerInput, m.steerCursor)
	case tea.KeyRight:
		m.steerCursor = moveInputCursorRight(m.steerInput, m.steerCursor)
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
			m.steerCursor += utf8.RuneLen(r)
		}
	}
	return nil
}

// handleFollowUpKeyLocked processes a key received while in follow-up input
// mode. The caller must hold m.mu. It returns a tea.Cmd (tea.Quit on Esc, nil
// otherwise).
func (m *teaModel) handleFollowUpKeyLocked(msg tea.KeyMsg) tea.Cmd {
	switch msg.Type {
	case tea.KeyEsc:
		// Esc exits follow-up mode without submitting.
		m.followUpInputMode = false
		m.followUpInput = ""
		m.followUpCursor = 0
	case tea.KeyEnter:
		// Enter submits the follow-up text via the callback and exits
		// follow-up mode.
		input := m.followUpInput
		m.followUpInputMode = false
		m.followUpInput = ""
		m.followUpCursor = 0
		if m.followUpCallback != nil && input != "" {
			cb := m.followUpCallback
			// Run in a goroutine so it does not block the render loop.
			go cb(input)
		}
	case tea.KeyBackspace:
		if m.followUpCursor > 0 {
			_, size := utf8.DecodeLastRuneInString(m.followUpInput[:m.followUpCursor])
			m.followUpInput = m.followUpInput[:m.followUpCursor-size] + m.followUpInput[m.followUpCursor:]
			m.followUpCursor -= size
		}
	case tea.KeyLeft:
		m.followUpCursor = moveInputCursorLeft(m.followUpInput, m.followUpCursor)
	case tea.KeyRight:
		m.followUpCursor = moveInputCursorRight(m.followUpInput, m.followUpCursor)
	case tea.KeyCtrlA:
		m.followUpCursor = 0
	case tea.KeyCtrlE:
		m.followUpCursor = len(m.followUpInput)
	case tea.KeyCtrlW:
		m.deleteFollowUpWordLocked()
	case tea.KeyCtrlU:
		// Ctrl+U clears the follow-up input line.
		m.followUpInput = ""
		m.followUpCursor = 0
	case tea.KeyRunes:
		for _, r := range msg.Runes {
			m.followUpInput = m.followUpInput[:m.followUpCursor] + string(r) + m.followUpInput[m.followUpCursor:]
			m.followUpCursor += utf8.RuneLen(r)
		}
	}
	return nil
}

// finalizeThinking collapses the last thinking entry (if any) and replaces its
// summary with a "thought for Xs" duration string. It is called when a
// non-thinking event arrives. The caller must NOT hold m.mu.
func (m *teaModel) finalizeThinking() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.accordion.entries) - 1; i >= 0; i-- {
		e := m.accordion.entries[i]
		if e.ContentType == ContentTypeThinking && !e.Collapsed && !e.ToolStartTime.IsZero() {
			e.Collapsed = true
			e.ToolDuration = time.Since(e.ToolStartTime)
			e.Summary = fmt.Sprintf("thought for %.1fs", e.ToolDuration.Seconds())
			break
		}
	}
}

// updateToolResultStatusLocked updates only a uniquely associated entry in the
// active assistant group. The caller must hold m.mu.
func (m *teaModel) updateToolResultStatusLocked(toolCallID string, isError bool) {
	target, matched := m.toolCallGroup.lookup(toolCallID)
	if !matched {
		return
	}
	if target.ToolStatus == ToolStatusRunning || target.ToolStatus == ToolStatusPending {
		if !target.ToolStartTime.IsZero() {
			target.ToolDuration = time.Since(target.ToolStartTime)
		}
		if isError {
			target.ToolStatus = ToolStatusError
		} else {
			target.ToolStatus = ToolStatusCompleted
		}
	}
}

// hasRunningToolLocked reports whether any tool_call entry is currently in the
// Running state. The caller must hold m.mu.
func (m *teaModel) hasRunningToolLocked() bool {
	for _, e := range m.accordion.entries {
		if e.ContentType == ContentTypeToolCall && e.ToolStatus == ToolStatusRunning {
			return true
		}
	}
	return false
}

// handleInteractionKeyLocked gives the active modal exclusive keyboard focus.
// The caller must hold m.mu.
func (m *teaModel) handleInteractionKeyLocked(msg tea.KeyMsg) tea.Cmd {
	if m.activeInteraction == nil {
		return nil
	}
	if m.activeInteraction.approval != nil {
		return m.handleApprovalKeyLocked(msg)
	}
	if m.activeInteraction.hitl != nil {
		return m.handleHITLKeyLocked(msg)
	}
	return nil
}

func (m *teaModel) approvalDetailsReviewedLocked() bool {
	current := m.activeInteraction
	if current == nil || current.approval == nil {
		return false
	}
	width := m.width
	if width <= 0 {
		width = 80
	}
	total := walkApprovalDetailLines(current.approval, current.showDiff, approvalDetailWidth(width), nil)
	return approvalDetailsReviewed(current, total)
}

// handleApprovalKeyLocked processes a key while an approval request is active.
// y allows once, n denies, and Esc denies the active request. Reusable approval
// remains unavailable until a trusted scope is supplied. q or Ctrl+C resolves
// every queued waiter and cancels the turn. The caller must hold m.mu.
func (m *teaModel) handleApprovalKeyLocked(msg tea.KeyMsg) tea.Cmd {
	if current := m.activeInteraction; current != nil && current.approval != nil && current.approval.Context != nil {
		if err := current.approval.Context.Err(); err != nil {
			m.cancelInteractionLocked(current.token, err)
			return nil
		}
	}
	switch msg.Type {
	case tea.KeyUp:
		m.scrollApprovalDetailsLocked(-1)
		return nil
	case tea.KeyDown:
		m.scrollApprovalDetailsLocked(1)
		return nil
	case tea.KeyPgUp:
		if rows := approvalDetailViewportRows(m.height); rows > 0 {
			m.scrollApprovalDetailsLocked(-max(1, rows-1))
		}
		return nil
	case tea.KeyPgDown:
		if rows := approvalDetailViewportRows(m.height); rows > 0 {
			m.scrollApprovalDetailsLocked(max(1, rows-1))
		}
		return nil
	case tea.KeyEsc:
		m.resolveApprovalLocked(ApprovalDeny)
	case tea.KeyCtrlC:
		m.cancelInteractionsLocked()
		return m.quitLocked()
	case tea.KeyRunes:
		switch string(msg.Runes) {
		case "y":
			if !m.approvalDetailsReviewedLocked() {
				return nil
			}
			m.resolveApprovalLocked(ApprovalAllow)
		case "n":
			m.resolveApprovalLocked(ApprovalDeny)
		case "d":
			if m.activeInteraction != nil && m.activeInteraction.approval != nil && m.activeInteraction.approval.DiffPreview != "" {
				m.activeInteraction.showDiff = !m.activeInteraction.showDiff
				m.activeInteraction.detailOffset = 0
				m.activeInteraction.reviewedDetailLines = nil
				m.activeInteraction.reviewedDetailCount = 0
			}
		case "a":
			// Reusable approval is unavailable until the caller supplies an
			// authoritative scope that the TUI can display and the policy can
			// enforce. Never infer that scope from the tool name or arguments.
		case "q":
			m.cancelInteractionsLocked()
			return m.quitLocked()
		}
	}
	return nil
}

func (m *teaModel) handleHITLKeyLocked(msg tea.KeyMsg) tea.Cmd {
	current := m.activeInteraction
	if current == nil || current.hitl == nil {
		return nil
	}
	if current.hitl.Context != nil && current.hitl.Context.Err() != nil {
		m.cancelInteractionLocked(current.token, current.hitl.Context.Err())
		return nil
	}
	if msg.Type == tea.KeyEsc {
		m.respondHITLLocked(HITLResponse{Err: context.Canceled})
		m.finishActiveInteractionLocked()
		return nil
	}
	if msg.Type == tea.KeyCtrlC {
		m.cancelInteractionsLocked()
		return m.quitLocked()
	}

	if len(current.hitl.Options) > 0 {
		switch msg.Type {
		case tea.KeyUp:
			if current.selectedOption > 0 {
				current.selectedOption--
			}
		case tea.KeyDown:
			if current.selectedOption+1 < len(current.hitl.Options) {
				current.selectedOption++
			}
		case tea.KeyEnter:
			m.respondHITLLocked(HITLResponse{Answer: current.hitl.Options[current.selectedOption]})
			m.finishActiveInteractionLocked()
		case tea.KeyRunes:
			if len(msg.Runes) == 1 && msg.Runes[0] >= '1' && msg.Runes[0] <= '9' {
				option := int(msg.Runes[0] - '1')
				if option < len(current.hitl.Options) {
					m.respondHITLLocked(HITLResponse{Answer: current.hitl.Options[option]})
					m.finishActiveInteractionLocked()
				}
			}
		}
		return nil
	}

	switch msg.Type {
	case tea.KeyEnter:
		m.respondHITLLocked(HITLResponse{Answer: current.answer})
		m.finishActiveInteractionLocked()
	case tea.KeyBackspace:
		if current.answer != "" {
			_, size := utf8.DecodeLastRuneInString(current.answer)
			current.answer = current.answer[:len(current.answer)-size]
		}
	case tea.KeyRunes:
		current.answer += string(msg.Runes)
	case tea.KeySpace:
		current.answer += " "
	}
	return nil
}

func (m *teaModel) enqueueApprovalLocked(req ApprovalRequest) (uint64, <-chan struct{}) {
	if req.Context != nil && req.Context.Err() != nil {
		respondApproval(req.ResponseCh, ApprovalDeny)
		return 0, nil
	}
	interaction := pendingInteraction{approval: &req, showDiff: true, finished: make(chan struct{})}
	token := m.enqueueInteractionLocked(interaction)
	return token, interaction.finished
}

func (m *teaModel) enqueueHITLLocked(req HITLMessage) (uint64, <-chan struct{}) {
	req.Options = append([]string(nil), req.Options...)
	finished := make(chan struct{})
	interaction := pendingInteraction{hitl: &req, finished: finished}
	token := m.enqueueInteractionLocked(interaction)
	return token, finished
}

func (m *teaModel) enqueueInteractionLocked(interaction pendingInteraction) uint64 {
	m.interactionSequence++
	interaction.token = m.interactionSequence
	if m.activeInteraction == nil {
		m.activeInteraction = &interaction
		return interaction.token
	}
	m.queuedInteractions = append(m.queuedInteractions, interaction)
	return interaction.token
}

func (m *teaModel) finishActiveInteractionLocked() {
	if m.activeInteraction != nil && m.activeInteraction.finished != nil {
		close(m.activeInteraction.finished)
	}
	m.activeInteraction = nil
	if len(m.queuedInteractions) == 0 {
		return
	}
	next := m.queuedInteractions[0]
	m.queuedInteractions = m.queuedInteractions[1:]
	m.activeInteraction = &next
}

func (m *teaModel) resolveApprovalLocked(decision ApprovalDecision) {
	if m.activeInteraction == nil || m.activeInteraction.approval == nil {
		return
	}
	if ctx := m.activeInteraction.approval.Context; ctx != nil && ctx.Err() != nil {
		m.cancelInteractionLocked(m.activeInteraction.token, ctx.Err())
		return
	}
	responseCh := m.activeInteraction.approval.ResponseCh
	respondApproval(responseCh, decision)
	m.finishActiveInteractionLocked()
}

func (m *teaModel) respondHITLLocked(response HITLResponse) {
	if m.activeInteraction == nil || m.activeInteraction.hitl == nil {
		return
	}
	sendHITLResponse(m.activeInteraction.hitl.ResponseCh, response)
}

func sendHITLResponse(responseCh chan HITLResponse, response HITLResponse) {
	if responseCh == nil {
		return
	}
	select {
	case responseCh <- response:
	default:
	}
}

func (m *teaModel) cancelInteractionLocked(token uint64, err error) {
	if err == nil {
		err = context.Canceled
	}
	if current := m.activeInteraction; current != nil && current.token == token {
		m.cancelPendingInteractionLocked(current, err)
		m.finishActiveInteractionLocked()
		return
	}
	for i := range m.queuedInteractions {
		queued := &m.queuedInteractions[i]
		if queued.token != token {
			continue
		}
		m.cancelPendingInteractionLocked(queued, err)
		m.queuedInteractions = append(m.queuedInteractions[:i], m.queuedInteractions[i+1:]...)
		return
	}
}

func (m *teaModel) cancelPendingInteractionLocked(interaction *pendingInteraction, err error) {
	if interaction.approval != nil {
		respondApproval(interaction.approval.ResponseCh, ApprovalDeny)
	}
	if interaction.hitl != nil {
		sendHITLResponse(interaction.hitl.ResponseCh, HITLResponse{Err: err})
	}
	if interaction.finished != nil {
		close(interaction.finished)
		interaction.finished = nil
	}
}

func (m *teaModel) cancelApprovalInteractionsLocked() {
	remaining := make([]pendingInteraction, 0, len(m.queuedInteractions)+1)
	if m.activeInteraction != nil {
		if m.activeInteraction.approval != nil {
			m.cancelPendingInteractionLocked(m.activeInteraction, context.Canceled)
		} else {
			remaining = append(remaining, *m.activeInteraction)
		}
		m.activeInteraction = nil
	}
	for i := range m.queuedInteractions {
		interaction := &m.queuedInteractions[i]
		if interaction.approval != nil {
			m.cancelPendingInteractionLocked(interaction, context.Canceled)
			continue
		}
		remaining = append(remaining, *interaction)
	}
	m.queuedInteractions = nil
	if len(remaining) == 0 {
		return
	}
	m.activeInteraction = &remaining[0]
	if len(remaining) > 1 {
		m.queuedInteractions = append(m.queuedInteractions, remaining[1:]...)
	}
}

func respondApproval(responseCh chan ApprovalResponse, decision ApprovalDecision) {
	if responseCh == nil {
		return
	}
	select {
	case responseCh <- ApprovalResponse{Decision: decision}:
	default:
	}
}

func (m *teaModel) cancelInteractionsLocked() {
	if current := m.activeInteraction; current != nil {
		m.cancelPendingInteractionLocked(current, context.Canceled)
	}
	for i := range m.queuedInteractions {
		queued := &m.queuedInteractions[i]
		m.cancelPendingInteractionLocked(queued, context.Canceled)
	}
	m.activeInteraction = nil
	m.queuedInteractions = nil
}

func (m *teaModel) handleHITLMessage(msg HITLMessage) (tea.Model, tea.Cmd) {
	if msg.Context != nil && msg.Context.Err() != nil {
		sendHITLResponse(msg.ResponseCh, HITLResponse{Err: msg.Context.Err()})
		return m, nil
	}
	m.mu.Lock()
	token, finished := m.enqueueHITLLocked(msg)
	view := ""
	if m.shouldNotify() {
		view = m.renderViewLocked()
	}
	m.mu.Unlock()
	m.notify(view)
	return m, waitForInteractionCancel(msg.Context, token, finished, m.runDone)
}

func waitForInteractionCancel(ctx context.Context, token uint64, finished, runDone <-chan struct{}) tea.Cmd {
	if ctx == nil {
		return nil
	}
	return func() tea.Msg {
		select {
		case <-finished:
			return nil
		case <-ctx.Done():
			return interactionCanceledMsg{token: token, err: ctx.Err()}
		case <-runDone:
			return nil
		}
	}
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
	end := m.steerCursor
	// Skip trailing spaces (traverse by rune to stay on UTF-8 boundaries).
	for end > 0 {
		r, size := utf8.DecodeLastRuneInString(m.steerInput[:end])
		if r != ' ' {
			break
		}
		end -= size
	}
	// Skip word characters (traverse by rune).
	for end > 0 {
		r, size := utf8.DecodeLastRuneInString(m.steerInput[:end])
		if r == ' ' {
			break
		}
		end -= size
	}
	m.steerInput = m.steerInput[:end] + m.steerInput[m.steerCursor:]
	m.steerCursor = end
}

// deleteFollowUpWordLocked deletes the word before the cursor in the follow-up
// input (Ctrl+W behavior). The caller must hold m.mu.
func (m *teaModel) deleteFollowUpWordLocked() {
	if m.followUpCursor == 0 {
		return
	}
	end := m.followUpCursor
	// Skip trailing spaces (traverse by rune to stay on UTF-8 boundaries).
	for end > 0 {
		r, size := utf8.DecodeLastRuneInString(m.followUpInput[:end])
		if r != ' ' {
			break
		}
		end -= size
	}
	// Skip word characters (traverse by rune).
	for end > 0 {
		r, size := utf8.DecodeLastRuneInString(m.followUpInput[:end])
		if r == ' ' {
			break
		}
		end -= size
	}
	m.followUpInput = m.followUpInput[:end] + m.followUpInput[m.followUpCursor:]
	m.followUpCursor = end
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

// resolveColorProfile determines the lipgloss color profile from the
// NO_COLOR/CLICOLOR/CLICOLOR_FORCE environment variables and the TTY status
// of the render stream w. The profile is resolved against w (not os.Stdout)
// so that color detection is consistent with the stream lipgloss renders to.
//
// Precedence (https://bixense.com/clicolors/ and https://no-color.org/):
//   - NO_COLOR set (any value) → Ascii (no color), regardless of other vars.
//   - CLICOLOR=0 → Ascii, unless CLICOLOR_FORCE overrides it.
//   - CLICOLOR_FORCE set (non-zero) → force color even when w is not a TTY.
//   - Otherwise → Ascii when w is not a TTY; auto-detect when it is.
func resolveColorProfile(w io.Writer) termenv.Profile {
	// NO_COLOR (any value) disables all color.
	if os.Getenv("NO_COLOR") != "" {
		return termenv.Ascii
	}

	forced := func() bool {
		v := os.Getenv("CLICOLOR_FORCE")
		return v != "" && v != "0"
	}()

	// CLICOLOR=0 disables color unless CLICOLOR_FORCE overrides it.
	if os.Getenv("CLICOLOR") == "0" && !forced {
		return termenv.Ascii
	}

	// Check if the render stream is a TTY.
	isRenderTTY := false
	if f, ok := w.(*os.File); ok {
		isRenderTTY = term.IsTerminal(f.Fd())
	}

	if !isRenderTTY && !forced {
		return termenv.Ascii
	}

	// Detect color capability from TERM/COLORTERM. When the stream is not a
	// TTY but color is forced, assume TTY so ColorProfile inspects the env
	// vars instead of short-circuiting to Ascii.
	opts := []termenv.OutputOption{}
	if !isRenderTTY {
		opts = append(opts, termenv.WithTTY(true))
	}
	return termenv.NewOutput(w, opts...).ColorProfile()
}
