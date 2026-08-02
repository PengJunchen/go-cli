package core

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// SubAgentState describes the lifecycle state of a sub-agent as it moves from
// creation through execution to a terminal condition.
type SubAgentState string

const (
	// SubAgentIdle is a sub-agent that has been created but not yet run.
	SubAgentIdle SubAgentState = "idle"
	// SubAgentRunning is a sub-agent currently executing its sub-task.
	SubAgentRunning SubAgentState = "running"
	// SubAgentWaiting is a sub-agent that has yielded pending a Send/Interrupt.
	SubAgentWaiting SubAgentState = "waiting"
	// SubAgentCompleted is a sub-agent that finished its sub-task successfully.
	SubAgentCompleted SubAgentState = "completed"
	// SubAgentFailed is a sub-agent whose sub-task terminated with an error.
	SubAgentFailed SubAgentState = "failed"
	// SubAgentInterrupted is a sub-agent that was stopped by an Interrupt call.
	SubAgentInterrupted SubAgentState = "interrupted"
)

// String returns the textual state identifier.
func (s SubAgentState) String() string { return string(s) }

// SubAgentConfig carries the construction-time settings for a SubAgent.
type SubAgentConfig struct {
	// Name identifies the sub-agent.
	Name string
	// SystemPrompt is the system prompt bound to the sub-task harness.
	SystemPrompt string
	// Tools lists the tool names available to the sub-task.
	Tools []string
	// Model is the chat model the sub-task uses.
	Model string
	// MaxTurns bounds the sub-task turn loop.
	MaxTurns int
}

// SubAgent is the Phase 4 contract for a delegated sub-task. It runs a prompt
// through an independent harness, streams AgentEvents, and supports sending
// follow-up messages, interrupting, and waiting for the final result.
type SubAgent interface {
	// Name returns the sub-agent identifier.
	Name() string
	// Run starts executing the sub-task and returns its event stream.
	Run(ctx context.Context, prompt string) (<-chan AgentEvent, error)
	// Send delivers a follow-up message to the running sub-agent.
	Send(ctx context.Context, msg string) error
	// Interrupt stops the running sub-agent.
	Interrupt(ctx context.Context) error
	// Wait blocks until the sub-task finishes and returns the final message.
	Wait(ctx context.Context) (AgentMessage, error)
}

// subAgentRunner is the harness-ish seam a DefaultSubAgent drives. It executes
// the sub-task turn loop, emitting AgentEvents through emit and returning the
// final AgentMessage. It must honor ctx cancellation so that interrupting or
// canceling the parent cancels the running sub-task.
type subAgentRunner interface {
	Run(ctx context.Context, prompt string, inbox <-chan string, emit func(AgentEvent)) (AgentMessage, error)
}

// subAgentRunnerFactory builds a runner from a SubAgentConfig. It is the
// pluggable harness constructor seam (defaults to a simulated in-process
// runner).
type subAgentRunnerFactory func(cfg SubAgentConfig) subAgentRunner

// eventBufferSize caps how many events a sub-agent buffers.
const eventBufferSize = 32

// inboxBufferSize caps how many unread Send messages a running sub-agent holds.
const inboxBufferSize = 16

// defaultSubAgentMaxTurns bounds the simulated runner's turn count when the
// config leaves MaxTurns unset.
const defaultSubAgentMaxTurns = 1

// DefaultSubAgent is the default SubAgent implementation. It runs a sub-task
// through an INDEPENDENT run (a fresh subAgentRunner per Run), streaming
// core.AgentEvent values and returning a final core.AgentMessage.
//
// The real Harness/layer wiring is intentionally stubbed: rather than
// constructing the full AgentLoop -> AgentImpl -> HarnessImpl stack (which
// would require a model provider, tool registry and system prompt plumbing),
// it drives a self-contained in-process runner that still honors
// cancellation / interrupt / send semantics and emits the required
// subagent.spawn / subagent.run spans. The seam is pluggable via
// WithSubAgentRunner so a real harness can be dropped in later.
type DefaultSubAgent struct {
	config SubAgentConfig

	mu            sync.Mutex
	state         SubAgentState
	interrupted   bool
	received      []string
	events        chan AgentEvent
	inbox         chan string
	done          chan struct{}
	result        AgentMessage
	doneErr       error
	cancel        context.CancelFunc
	runnerFactory subAgentRunnerFactory
}

var _ SubAgent = (*DefaultSubAgent)(nil)

// SubAgentOption configures a DefaultSubAgent at construction time.
type SubAgentOption func(*DefaultSubAgent)

// WithSubAgentRunner overrides the runner factory the sub-agent drives. It is
// the pluggable harness constructor; a nil factory uses the simulated runner.
func WithSubAgentRunner(factory subAgentRunnerFactory) SubAgentOption {
	return func(s *DefaultSubAgent) { s.runnerFactory = factory }
}

// NewDefaultSubAgent builds a DefaultSubAgent bound to the given config. A
// blank name is replaced with "subagent".
func NewDefaultSubAgent(config SubAgentConfig, opts ...SubAgentOption) *DefaultSubAgent {
	if config.Name == "" {
		config.Name = "subagent"
	}
	s := &DefaultSubAgent{
		config:        config,
		state:         SubAgentIdle,
		done:          make(chan struct{}),
		events:        make(chan AgentEvent, eventBufferSize),
		inbox:         make(chan string, inboxBufferSize),
		runnerFactory: simulatedRunnerFactory,
	}
	for _, o := range opts {
		o(s)
	}
	if s.runnerFactory == nil {
		s.runnerFactory = simulatedRunnerFactory
	}
	slog.Info("core.subagent.new",
		"name", s.config.Name,
		"model", s.config.Model,
		"tools", len(s.config.Tools),
		"max_turns", s.config.MaxTurns,
	)
	return s
}

// Name returns the sub-agent identifier.
func (s *DefaultSubAgent) Name() string { return s.config.Name }

// State returns the current lifecycle state.
func (s *DefaultSubAgent) State() SubAgentState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Received returns a copy of the messages delivered via Send so far.
func (s *DefaultSubAgent) Received() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.received...)
}

// Run starts executing the prompt in an independent run and returns a channel
// of AgentEvents. It emits a subagent.spawn span (INTERNAL) with
// subagent_name / prompt_length attributes and starts a goroutine that emits a
// subagent.run span (INTERNAL) with subagent_name / status attributes until it
// reaches a terminal state. The run derives a child context from ctx so that
// canceling the parent (or Interrupt) cancels the sub-task.
func (s *DefaultSubAgent) Run(ctx context.Context, prompt string) (<-chan AgentEvent, error) {
	s.mu.Lock()
	if s.state != SubAgentIdle {
		s.mu.Unlock()
		return nil, fmt.Errorf("core: sub-agent %q already ran (state %s)", s.config.Name, s.state)
	}
	s.state = SubAgentRunning
	s.mu.Unlock()

	span, spanCtx := tracing.SpanFromContext(ctx, "subagent.spawn", tracing.SpanKindInternal)
	span.SetAttributes(
		tracing.Attribute{Key: "subagent_name", Value: s.config.Name},
		tracing.Attribute{Key: "prompt_length", Value: len(prompt)},
	)
	defer span.End()
	logger := tracing.NewTraceLogger(span, nil)
	logger.Info("core.subagent.run", "name", s.config.Name, "prompt_len", len(prompt))

	// Derive an independent, cancellable child context from the (trace-aware)
	// spawn context so the sub-task inherits the trace while remaining
	// independently cancellable.
	subCtx, cancel := context.WithCancel(spanCtx)

	s.mu.Lock()
	s.cancel = cancel
	events := s.events
	inbox := s.inbox
	s.mu.Unlock()

	go s.runAndFinalize(subCtx, prompt, events, inbox)

	return events, nil
}

// runAndFinalize drives the runner, streams its events to the out channel, and
// finalizes state once the runner returns or the context is canceled.
func (s *DefaultSubAgent) runAndFinalize(ctx context.Context, prompt string, out chan<- AgentEvent, inbox <-chan string) {
	span, spanCtx := tracing.SpanFromContext(ctx, "subagent.run", tracing.SpanKindInternal)
	span.SetAttributes(
		tracing.Attribute{Key: "subagent_name", Value: s.config.Name},
	)

	emit := func(ev AgentEvent) {
		select {
		case out <- ev:
		case <-ctx.Done():
		}
	}

	runner := s.runnerFactory(s.config)
	result, err := runner.Run(spanCtx, prompt, inbox, emit)

	s.mu.Lock()
	interrupted := s.interrupted
	s.mu.Unlock()

	status, finalErr := s.finalizeOutcome(err, interrupted)
	span.SetAttributes(tracing.Attribute{Key: "status", Value: status.String()})
	if finalErr != nil {
		span.SetStatus(tracing.SpanStatusError, finalErr.Error())
	} else {
		span.SetStatus(tracing.SpanStatusOK, "")
	}
	span.End()

	s.mu.Lock()
	s.result = result
	s.doneErr = finalErr
	s.mu.Unlock()
	close(out)
	close(s.done)

	slog.Info("core.subagent.done", "name", s.config.Name, "state", status)
}

// finalizeOutcome maps a runner error and the interrupted flag to the terminal
// state and the error reported by Wait.
func (s *DefaultSubAgent) finalizeOutcome(err error, interrupted bool) (SubAgentState, error) {
	switch {
	case interrupted:
		s.setStateLocked(SubAgentInterrupted)
		return SubAgentInterrupted, context.Canceled
	case err != nil:
		s.setStateLocked(SubAgentFailed)
		return SubAgentFailed, err
	default:
		s.setStateLocked(SubAgentCompleted)
		return SubAgentCompleted, nil
	}
}

func (s *DefaultSubAgent) setStateLocked(state SubAgentState) {
	s.mu.Lock()
	s.state = state
	s.mu.Unlock()
}

// Send records and delivers a follow-up message to the running sub-agent.
// Messages are recorded immediately under lock so they remain observable even
// if the runner has already terminated.
func (s *DefaultSubAgent) Send(_ context.Context, msg string) error {
	s.mu.Lock()
	s.received = append(s.received, msg)
	s.mu.Unlock()

	// Best-effort delivery: if the inbox buffer is full or the sub-task has
	// ended, the message is still recorded above.
	s.mu.Lock()
	inbox := s.inbox
	s.mu.Unlock()
	select {
	case inbox <- msg:
	default:
	}
	return nil
}

// Interrupt stops the running sub-agent: it flags the interrupt, cancels the
// sub-task's context, sets the state to Interrupted, and logs the event.
func (s *DefaultSubAgent) Interrupt(ctx context.Context) error {
	s.mu.Lock()
	s.interrupted = true
	cancel := s.cancel
	s.mu.Unlock()

	slog.InfoContext(ctx, "core.subagent.interrupt", "name", s.config.Name)

	if cancel != nil {
		cancel()
	}
	return nil
}

// Wait blocks until the sub-task finishes and returns the final AgentMessage.
// It reports the terminal error when the sub-task failed or was interrupted.
func (s *DefaultSubAgent) Wait(ctx context.Context) (AgentMessage, error) {
	s.mu.Lock()
	done := s.done
	s.mu.Unlock()

	select {
	case <-done:
		s.mu.Lock()
		res := s.result
		err := s.doneErr
		s.mu.Unlock()
		return res, err
	case <-ctx.Done():
		return AgentMessage{}, ctx.Err()
	}
}

// simulatedRunnerFactory returns the default in-process runner for the config.
func simulatedRunnerFactory(cfg SubAgentConfig) subAgentRunner {
	return &simulatedSubAgentRunner{maxTurns: cfg.MaxTurns}
}

// simulatedSubAgentRunner is the default subAgentRunner. It simulates a short
// ReAct-style turn loop in-process, emitting an initial "user" event, draining
// any inbox messages, and producing "message" events until it reaches a
// terminal response. It honors ctx cancellation.
type simulatedSubAgentRunner struct {
	maxTurns int
}

// Compile-time assertion that the simulated runner is the default
// implementation of the subAgentRunner interface.
var _ subAgentRunner = (*simulatedSubAgentRunner)(nil)

// Run executes the simulated turn loop.
func (r *simulatedSubAgentRunner) Run(ctx context.Context, prompt string, inbox <-chan string, emit func(AgentEvent)) (AgentMessage, error) {
	maxTurns := r.maxTurns
	if maxTurns <= 0 {
		maxTurns = defaultSubAgentMaxTurns
	}
	emit(AgentEvent{Kind: "user", Content: prompt, Timestamp: time.Now()})

	final := ""
	for turn := 0; turn < maxTurns; turn++ {
		if err := ctx.Err(); err != nil {
			emit(errEvent(err))
			return AgentMessage{Role: "assistant", Content: final}, err
		}
		if err := pumpInbox(ctx, inbox, emit); err != nil {
			emit(errEvent(err))
			return AgentMessage{Role: "assistant", Content: final}, err
		}
		final = fmt.Sprintf("response-%d", turn+1)
		emit(AgentEvent{Kind: "message", Content: final, Timestamp: time.Now()})
	}
	return AgentMessage{Role: "assistant", Content: final}, nil
}

// pumpInbox forwards any pending inbox messages to the event stream as "user"
// events, returning the context error if the sub-task was canceled.
func pumpInbox(ctx context.Context, inbox <-chan string, emit func(AgentEvent)) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case msg, ok := <-inbox:
			if !ok {
				return nil
			}
			emit(AgentEvent{Kind: "user", Content: msg, Timestamp: time.Now()})
		default:
			return nil
		}
	}
}
