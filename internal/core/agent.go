package core

import (
	"context"
	"log/slog"
	"sync"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// agentConfig holds the configurable state of an AgentImpl.
type agentConfig struct {
	history        []AgentMessage
	compactionHook CompactionHook
}

// AgentOption configures an AgentImpl at construction time.
type AgentOption func(*agentConfig)

// WithHistory presets the agent's initial message history.
func WithHistory(history []AgentMessage) AgentOption {
	return func(c *agentConfig) { c.history = append(c.history, history...) }
}

// eventSource is satisfied by Agent implementations that can expose the events
// produced by the most recent Run call. The Harness relies on it to stream
// events to consumers.
type eventSource interface {
	// Events returns a copy of the events produced by the most recent Run.
	Events() []AgentEvent
}

var _ eventSource = (*AgentImpl)(nil)

// AgentImpl is a stateful wrapper around an AgentLoop. It maintains message
// history across Run calls and records its name so the runtime can address it.
type AgentImpl struct {
	name           string
	loop           AgentLoop
	mu             sync.Mutex
	history        []AgentMessage
	events         []AgentEvent
	compactionHook CompactionHook
	state          AgentState
}

var _ Agent = (*AgentImpl)(nil)

// NewAgentImpl builds an AgentImpl bound to the given loop. A nil loop causes a
// panic because an agent with no loop cannot execute. The name defaults to
// "default" when empty, and the supplied AgentOptions are applied before the
// agent is returned.
func NewAgentImpl(name string, loop AgentLoop, opts ...AgentOption) *AgentImpl {
	if loop == nil {
		panic("core: agent requires a non-nil AgentLoop")
	}
	if name == "" {
		name = "default"
	}
	cfg := agentConfig{}
	for _, o := range opts {
		o(&cfg)
	}
	a := &AgentImpl{
		name:           name,
		loop:           loop,
		history:        append([]AgentMessage{}, cfg.history...),
		compactionHook: cfg.compactionHook,
		state:          StateCreated,
	}
	// Advance the lifecycle from Created to Initialized now that the agent is
	// fully constructed and ready to accept Run calls.
	a.transitionState(StateInitialized)
	slog.Info("core.agent.new", "name", name, "history", len(a.history))
	return a
}

// Name returns the agent identifier.
func (a *AgentImpl) Name() string { return a.name }

// State returns the current lifecycle state of the agent. It is thread-safe.
func (a *AgentImpl) State() AgentState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.state
}

// transitionState validates and applies a state transition. It is advisory:
// when the transition is invalid it logs a warning but still updates the
// state, preserving backward compatibility with callers that drive the agent
// through runs the state machine does not model.
func (a *AgentImpl) transitionState(to AgentState) {
	a.mu.Lock()
	defer a.mu.Unlock()
	from := a.state
	if err := assertTransition(from, to); err != nil {
		slog.Warn("core.agent.invalid_state_transition",
			"name", a.name, "from", from, "to", to, "err", err)
	}
	a.state = to
}

// Run appends the submission to the agent's history, executes the loop, records
// the events it fired, and returns the final result. It is thread-safe. Success
// is derived from the loop error: a successful run yields Success == true.
func (a *AgentImpl) Run(ctx context.Context, submission Submission, stream ...EventStream) (Result, error) {
	span, spanCtx := tracing.SpanFromContext(ctx, "agent.run", tracing.SpanKindInternal)
	defer span.End()
	logger := tracing.NewTraceLogger(span, nil)
	logger.Info("core.agent.run", "name", a.name, "type", submission.Type, "content", submission.Content)

	// Mark the agent as running before dispatching to the loop. The state
	// machine is advisory: an invalid transition logs a warning but does not
	// abort the run, preserving backward compatibility.
	a.transitionState(StateRunning)

	a.mu.Lock()
	a.history = append(a.history, AgentMessage{Role: "user", Content: submission.Content})
	historyCopy := append([]AgentMessage{}, a.history...)
	a.mu.Unlock()

	// Propagate the full history to the loop so the LLM receives the complete
	// conversation context (prior user/assistant turns).
	submission.History = historyCopy

	events, err := a.loop.Run(spanCtx, submission, stream...)
	if err != nil {
		span.SetStatus(tracing.SpanStatusError, err.Error())
	}

	evs := events
	if evs == nil {
		evs = []AgentEvent{}
	}
	lastAssistant := lastAssistantMessage(evs)

	a.mu.Lock()
	a.events = evs
	if lastAssistant != nil {
		a.history = append(a.history, AgentMessage{
			Role:      "assistant",
			Content:   lastAssistant.Content,
			Usage:     lastAssistant.Usage,
			ToolCalls: lastAssistant.ToolCalls,
		})
	}
	a.mu.Unlock()

	// Apply compaction hook if set: the hook may trim or summarize the
	// history to keep it within the token budget.
	if a.compactionHook != nil {
		a.mu.Lock()
		historyCopy := append([]AgentMessage{}, a.history...)
		a.mu.Unlock()

		compacted, compErr := a.compactionHook(spanCtx, historyCopy)
		if compErr != nil {
			logger.Warn("core.agent.compaction_failed", "err", compErr)
		} else {
			a.mu.Lock()
			a.history = compacted
			a.mu.Unlock()
			logger.Info("core.agent.compacted", "before", len(historyCopy), "after", len(compacted))
		}
	}

	logger.Info("core.agent.done", "name", a.name, "events", len(evs), "success", err == nil)

	// Advance the lifecycle to a terminal state based on the run outcome.
	if err != nil {
		a.transitionState(StateError)
	} else {
		a.transitionState(StateStopped)
	}

	finalMsg := ""
	if lastAssistant != nil {
		finalMsg = lastAssistant.Content
	}
	return Result{Message: finalMsg, Success: err == nil}, err
}

// Events returns a copy of the events produced by the most recent Run call.
func (a *AgentImpl) Events() []AgentEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]AgentEvent{}, a.events...)
}

// Messages returns a copy of the accumulated message history.
func (a *AgentImpl) Messages() []AgentMessage {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]AgentMessage{}, a.history...)
}

// ClearHistory resets the agent's conversation history. It is used by the
// /clear slash command.
func (a *AgentImpl) ClearHistory() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.history = nil
	slog.Info("core.agent.history_cleared", "name", a.name)
}

// SetHistory replaces the agent's conversation history. It is used by the
// /resume slash command to restore a previous session's context.
func (a *AgentImpl) SetHistory(messages []AgentMessage) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.history = append([]AgentMessage{}, messages...)
	slog.Info("core.agent.history_set", "name", a.name, "messages", len(messages))
}

// Compact manually triggers the compaction hook on the current history.
// It is used by the /compact slash command. When no compaction hook is
// configured, it is a no-op.
func (a *AgentImpl) Compact(ctx context.Context) error {
	if a.compactionHook == nil {
		return nil
	}
	a.mu.Lock()
	historyCopy := append([]AgentMessage{}, a.history...)
	a.mu.Unlock()

	compacted, err := a.compactionHook(ctx, historyCopy)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.history = compacted
	a.mu.Unlock()
	slog.Info("core.agent.manual_compact", "name", a.name, "before", len(historyCopy), "after", len(compacted))
	return nil
}

// lastMessageEvent returns the content of the final "message" event, or the
// empty string if none exists. It matches by Kind only so that pure tool call
// responses (empty Content) are still returned.
func lastMessageEvent(events []AgentEvent) string {
	final := ""
	for _, ev := range events {
		if ev.Kind == "message" {
			final = ev.Content
		}
	}
	return final
}

// lastAssistantMessage returns a pointer to the final "message" event, or nil
// when no message event exists. It matches by Kind only so that pure tool call
// responses (empty Content but non-empty ToolCalls) are returned. The caller
// can then access Content, Usage, and ToolCalls from the single event.
func lastAssistantMessage(events []AgentEvent) *AgentEvent {
	var last *AgentEvent
	for i := range events {
		if events[i].Kind == "message" {
			last = &events[i]
		}
	}
	return last
}
