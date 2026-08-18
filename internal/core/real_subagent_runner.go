package core

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"
)

// subagentDepthKey is the context key used to propagate the sub-agent
// recursion depth from a parent runner to its children. Each level of
// recursion increments the depth by one so that deeply nested delegations
// can be bounded.
type subagentDepthKey struct{}

// defaultMaxSubAgentDepth is the maximum recursion depth for sub-agent
// delegation. A sub-agent at this depth may not dispatch further sub-agents.
const defaultMaxSubAgentDepth = 3

// errSubAgentDepthExceeded is returned when a sub-agent dispatch would exceed
// the configured maximum recursion depth.
var errSubAgentDepthExceeded = fmt.Errorf("sub-agent recursion depth limit (%d) exceeded", defaultMaxSubAgentDepth)

// realSubAgentRunner implements subAgentRunner by constructing a fresh
// LoopAgent -> AgentImpl -> HarnessImpl stack per Run call. Unlike the
// simulated runner it calls the real LLM and produces genuine responses.
type realSubAgentRunner struct {
	model          llm.BaseChatModel
	registry       *llm.ProviderRegistry
	modelName      string
	tools          tools.ToolRegistry
	allowedTools   []string
	maxIter        int
	systemPrompt   string
	opts           []LoopOption
	depth          int
	maxDepth       int
	compactionHook CompactionHook
}

var _ subAgentRunner = (*realSubAgentRunner)(nil)

// Run creates an independent agent stack, submits the prompt, drains events
// via emit, and returns the final assistant message. It enforces the
// recursion depth limit: if the depth propagated through the context meets or
// exceeds maxDepth, the run is aborted with errSubAgentDepthExceeded.
func (r *realSubAgentRunner) Run(ctx context.Context, prompt string, inbox <-chan string, emit func(AgentEvent)) (AgentMessage, error) {
	// Read the recursion depth from the context (default 0 for top-level).
	depth := r.depth
	if d, ok := ctx.Value(subagentDepthKey{}).(int); ok && d > depth {
		depth = d
	}

	// Enforce the recursion depth limit before spawning the sub-agent.
	maxDepth := r.maxDepth
	if maxDepth <= 0 {
		maxDepth = defaultMaxSubAgentDepth
	}
	if depth >= maxDepth {
		slog.Warn("core.subagent.real_runner.depth_exceeded",
			"depth", depth,
			"max_depth", maxDepth,
		)
		emit(AgentEvent{Kind: "error", Content: errSubAgentDepthExceeded.Error()})
		return AgentMessage{}, fmt.Errorf("core: %w", errSubAgentDepthExceeded)
	}

	// Propagate depth+1 to any child sub-agents dispatched by this run.
	ctx = context.WithValue(ctx, subagentDepthKey{}, depth+1)

	// Drain any pending inbox messages before starting the agent so follow-up
	// messages delivered before Run are visible as "user" events.
	if err := pumpInbox(ctx, inbox, emit); err != nil {
		return AgentMessage{}, err
	}

	// Resolve the model: when a model-name override is set and a registry is
	// available, build a model from the registry; otherwise fall back to the
	// parent model.
	model := r.model
	if r.modelName != "" && r.registry != nil {
		m, cleanup, err := r.registry.GetModel(ctx, r.modelName, llm.ModelConfig{Model: r.modelName})
		if err != nil {
			return AgentMessage{}, err
		}
		if cleanup != nil {
			defer cleanup()
		}
		model = m
	}

	loopOpts := []LoopOption{
		WithLLM(model),
	}
	// The sub-agent's system prompt (resolved by the dispatcher) overrides the
	// LoopAgent's tool-aware default so the sub-agent adopts its delegated role.
	if r.systemPrompt != "" {
		loopOpts = append(loopOpts, WithSystemPrompt(r.systemPrompt))
	}
	tr := r.tools
	if tr != nil && len(r.allowedTools) > 0 {
		tr = newFilteredToolRegistry(tr, r.allowedTools)
	}
	if tr != nil {
		loopOpts = append(loopOpts, WithTools(tr))
	}
	if r.maxIter > 0 {
		loopOpts = append(loopOpts, WithMaxIterations(r.maxIter))
	}
	loopOpts = append(loopOpts, r.opts...)

	loop := NewLoopAgent(loopOpts...)
	agentOpts := []AgentOption{}
	if r.compactionHook != nil {
		agentOpts = append(agentOpts, WithCompactionHook(r.compactionHook))
	} else {
		agentOpts = append(agentOpts, WithCompactionHook(defaultSubAgentCompactionHook))
	}
	agent := NewAgentImpl("subagent", loop, agentOpts...)
	h := NewHarnessImpl(agent, WithEventBuffer(32))

	stream, err := h.Submit(ctx, prompt)
	if err != nil {
		return AgentMessage{}, err
	}

	for ev := range stream.Events() {
		emit(ev)
	}

	final, runErr := stream.Result()
	if runErr != nil {
		slog.Warn("core.subagent.real_runner.error", "err", runErr)
		return final, runErr
	}

	slog.Info("core.subagent.real_runner.complete",
		"content_len", len(final.Content),
	)
	return final, nil
}

// subagentMaxHistory is the maximum number of messages a sub-agent's history
// may grow to before the default compaction hook trims it.
const subagentMaxHistory = 40

// defaultSubAgentCompactionHook is the CompactionHook injected into every
// sub-agent's AgentImpl. It trims the history to the most recent
// subagentMaxHistory messages so a long-running sub-agent does not consume
// unbounded context. The first message (system prompt) is always preserved.
func defaultSubAgentCompactionHook(_ context.Context, messages []AgentMessage) ([]AgentMessage, error) {
	if len(messages) <= subagentMaxHistory {
		return messages, nil
	}
	// Keep the first message (typically the system prompt) and the most
	// recent messages up to the limit.
	keep := subagentMaxHistory - 1
	compacted := make([]AgentMessage, 0, subagentMaxHistory)
	compacted = append(compacted, messages[0])
	compacted = append(compacted, messages[len(messages)-keep:]...)
	return compacted, nil
}

// NewRealSubAgentRunnerFactory returns a SubAgentRunnerFactory that produces
// realSubAgentRunner instances. The model and tool registry are captured in
// the closure so every sub-agent gets the same LLM and tool set as the parent.
func NewRealSubAgentRunnerFactory(model llm.BaseChatModel, registry *llm.ProviderRegistry, tr tools.ToolRegistry, opts ...LoopOption) SubAgentRunnerFactory {
	return func(cfg SubAgentConfig) subAgentRunner {
		maxIter := cfg.MaxTurns
		if maxIter <= 0 {
			maxIter = defaultMaxIterations
		}
		return &realSubAgentRunner{
			model:        model,
			registry:     registry,
			modelName:    cfg.Model,
			tools:        tr,
			allowedTools: cfg.Tools,
			maxIter:      maxIter,
			systemPrompt: cfg.SystemPrompt,
			opts:         opts,
			maxDepth:     defaultMaxSubAgentDepth,
		}
	}
}

// NewRealSubAgentFactory creates a SubAgentFactory that spawns real
// LoopAgent-backed sub-agents instead of the simulated runner. This is the
// exported entry point for packages outside core (e.g. cli) to register a
// real sub-agent factory via RegisterSubAgentFactory.
func NewRealSubAgentFactory(model llm.BaseChatModel, registry *llm.ProviderRegistry, tr tools.ToolRegistry, opts ...LoopOption) SubAgentFactory {
	return NewSubAgentFactory(WithSubAgentRunnerFactory(
		NewRealSubAgentRunnerFactory(model, registry, tr, opts...),
	))
}

// filteredToolRegistry wraps a tools.ToolRegistry and only exposes tools whose
// names appear in the allowed list. Register is a no-op (the filtered registry
// is read-only). It is used by the sub-agent runner to restrict the tool set
// based on SubAgentConfig.Tools.
type filteredToolRegistry struct {
	inner   tools.ToolRegistry
	allowed map[string]bool
}

var _ tools.ToolRegistry = (*filteredToolRegistry)(nil)

func newFilteredToolRegistry(inner tools.ToolRegistry, allowed []string) *filteredToolRegistry {
	m := make(map[string]bool, len(allowed))
	for _, name := range allowed {
		m[name] = true
	}
	return &filteredToolRegistry{inner: inner, allowed: m}
}

func (r *filteredToolRegistry) Register(_ context.Context, _ tools.ToolDefinition) error {
	return nil // read-only: silently ignore
}

func (r *filteredToolRegistry) Get(ctx context.Context, name string) (tools.ToolDefinition, error) {
	if !r.allowed[name] {
		return nil, tools.ErrToolNotFound
	}
	return r.inner.Get(ctx, name)
}

func (r *filteredToolRegistry) List(ctx context.Context) ([]tools.ToolDefinition, error) {
	all, err := r.inner.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]tools.ToolDefinition, 0, len(all))
	for _, def := range all {
		if r.allowed[def.Name()] {
			out = append(out, def)
		}
	}
	return out, nil
}
