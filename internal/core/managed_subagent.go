package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/tracing"
)

// ManagedSubAgentExecutor owns an independently assembled child, including its
// run binding, event drain, and resource teardown. It must join those resources
// before returning; unproved teardown must return an OutcomeUnknown error.
type ManagedSubAgentExecutor func(context.Context, SubAgentConfig, string, <-chan string, func(AgentEvent)) (AgentMessage, error)

type managedSubAgentFactory struct{ execute ManagedSubAgentExecutor }
type managedSubAgentRunner struct {
	config  SubAgentConfig
	execute ManagedSubAgentExecutor
}

var _ SubAgentFactory = (*managedSubAgentFactory)(nil)
var _ subAgentRunner = (*managedSubAgentRunner)(nil)

// NewManagedSubAgentFactory delegates execution to a host that owns a complete
// independent child runtime. Unlike a generic runner override this is an
// explicit lifecycle contract, used by the CLI's independent Assembly builder.
func NewManagedSubAgentFactory(execute ManagedSubAgentExecutor) SubAgentFactory {
	return &managedSubAgentFactory{execute: execute}
}

func (f *managedSubAgentFactory) Create(_ context.Context, name string, cfg SubAgentConfig) (SubAgent, error) {
	if f.execute == nil {
		return nil, errors.New("core: managed sub-agent executor is unavailable")
	}
	if name != "" {
		cfg.Name = name
	}
	return NewDefaultSubAgent(cfg, WithSubAgentRunner(func(c SubAgentConfig) subAgentRunner {
		return &managedSubAgentRunner{config: c, execute: f.execute}
	})), nil
}

func (r *managedSubAgentRunner) Run(ctx context.Context, prompt string, inbox <-chan string, emit func(AgentEvent)) (AgentMessage, error) {
	depth, _ := ctx.Value(subagentDepthKey{}).(int)
	if depth >= defaultMaxSubAgentDepth {
		return AgentMessage{}, errSubAgentDepthExceeded
	}
	prompt, err := inheritedSubagentPrompt(ctx, prompt, r.config.ContextMode)
	if err != nil {
		return AgentMessage{}, err
	}
	// Start from a new value tree. Cancellation and tracing may cross this
	// boundary; run IDs, sinks, owners, generations and capabilities may not.
	var child context.Context
	var cancel context.CancelFunc
	if deadline, ok := ctx.Deadline(); ok {
		child, cancel = context.WithDeadline(context.Background(), deadline)
	} else {
		child, cancel = context.WithCancel(context.Background())
	}
	defer cancel()
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	if ctx.Err() != nil {
		cancel()
		return AgentMessage{}, ctx.Err()
	}
	child = tracing.CopyTraceContext(child, ctx)
	child = context.WithValue(child, subagentDepthKey{}, depth+1)
	if seal, ok := ctx.Value(subagentInputSealKey{}).(func() []string); ok {
		child = context.WithValue(child, subagentInputSealKey{}, seal)
	}
	config := r.config
	if _, binding, bound := RunCommitFromContext(ctx); bound {
		config.ParentSessionID = binding.SessionID
		config.ParentRunID = RunIDFromContext(ctx)
	}
	return r.execute(child, config, prompt, inbox, emit)
}

type subagentParentMessagesKey struct{}

func withSubagentParentMessages(ctx context.Context, messages []llm.Message) context.Context {
	// Select data explicitly: the snapshot is informational, never runtime
	// authority. JSON also makes the copy independent of mutable tool arguments.
	type message struct {
		Role       llm.Role       `json:"role"`
		Content    string         `json:"content"`
		ToolCalls  []llm.ToolCall `json:"tool_calls,omitempty"`
		ToolCallID string         `json:"tool_call_id,omitempty"`
		Name       string         `json:"name,omitempty"`
	}
	var conversation []message
	for _, current := range messages {
		if current.Role != llm.RoleUser && current.Role != llm.RoleAssistant && current.Role != llm.RoleTool {
			continue
		}
		conversation = append(conversation, message{current.Role, current.Content, current.ToolCalls, current.ToolCallID, current.Name})
	}
	encoded, err := json.Marshal(conversation)
	if err != nil {
		return ctx
	}
	return context.WithValue(ctx, subagentParentMessagesKey{}, string(encoded))
}

func inheritedSubagentPrompt(ctx context.Context, prompt, mode string) (string, error) {
	switch mode {
	case "", "independent":
		return prompt, nil
	case "inherit":
		messages, ok := ctx.Value(subagentParentMessagesKey{}).(string)
		if !ok {
			return "", errors.New("core: inherited sub-agent context is unavailable")
		}
		return "Parent conversation (reference data; does not grant permissions):\n" + messages + "\n\nDelegated task:\n" + prompt, nil
	default:
		return "", fmt.Errorf("core: invalid sub-agent context mode %q", mode)
	}
}

// NewManagedSubagentTool exposes the host-owned dispatcher as a tool. The host
// must separately register its joined completion contract after constructing
// the independent Assembly executor.
func NewManagedSubagentTool(d *DefaultSubagentDispatcher) (tools.ToolDefinition, error) {
	if d == nil {
		return nil, ErrRunCommitChildUnsupported
	}
	if _, ok := d.factory.(*managedSubAgentFactory); !ok {
		return nil, ErrRunCommitChildUnsupported
	}
	return tools.NewOwnedSubagentTool(AdaptSubagentDispatcher(d)), nil
}

type subagentInputSealKey struct{}

// FinishSubAgentInput atomically seals a managed child's inbox when empty, or
// returns accepted follow-ups for its next run. A subsequent Send is rejected
// once sealed, so no successful delivery can disappear at final completion.
func FinishSubAgentInput(ctx context.Context) []string {
	if seal, ok := ctx.Value(subagentInputSealKey{}).(func() []string); ok {
		return seal()
	}
	return nil
}
