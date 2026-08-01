package core

import (
	"context"
	"log/slog"
	"sync"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// agentConfig holds the configurable state of an AgentImpl.
type agentConfig struct {
	history []AgentMessage
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
	name    string
	loop    AgentLoop
	mu      sync.Mutex
	history []AgentMessage
	events  []AgentEvent
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
		name:    name,
		loop:    loop,
		history: append([]AgentMessage{}, cfg.history...),
	}
	slog.Info("core.agent.new", "name", name, "history", len(a.history))
	return a
}

// Name returns the agent identifier.
func (a *AgentImpl) Name() string { return a.name }

// Run appends the submission to the agent's history, executes the loop, records
// the events it fired, and returns the final result. It is thread-safe. Success
// is derived from the loop error: a successful run yields Success == true.
func (a *AgentImpl) Run(ctx context.Context, submission Submission) (Result, error) {
	span, spanCtx := tracing.SpanFromContext(ctx, "agent.run", tracing.SpanKindInternal)
	defer span.End()
	logger := tracing.NewTraceLogger(span, nil)
	logger.Info("core.agent.run", "name", a.name, "type", submission.Type, "content", submission.Content)

	a.mu.Lock()
	a.history = append(a.history, AgentMessage{Role: "user", Content: submission.Content})
	a.mu.Unlock()

	events, err := a.loop.Run(spanCtx, submission)
	if err != nil {
		span.SetStatus(tracing.SpanStatusError, err.Error())
	}

	evs := events
	if evs == nil {
		evs = []AgentEvent{}
	}
	finalMsg := lastMessageEvent(evs)

	a.mu.Lock()
	a.events = evs
	if finalMsg != "" {
		a.history = append(a.history, AgentMessage{Role: "assistant", Content: finalMsg})
	}
	a.mu.Unlock()

	logger.Info("core.agent.done", "name", a.name, "events", len(evs), "success", err == nil)
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

// lastMessageEvent returns the content of the final non-empty "message" event,
// or the empty string if none exists.
func lastMessageEvent(events []AgentEvent) string {
	final := ""
	for _, ev := range events {
		if ev.Kind == "message" && ev.Content != "" {
			final = ev.Content
		}
	}
	return final
}
