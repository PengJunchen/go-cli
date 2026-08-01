package core

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/tracing"
)

// defaultMaxIterations bounds the number of think -> act -> observe turns a
// LoopAgent performs before giving up, guarding against runaway tool loops.
const defaultMaxIterations = 10

// errNilModel reports that a LoopAgent has no chat model wired up.
var errNilModel = errors.New("core: agent loop has no chat model")

// errMaxIterations reports that a LoopAgent exhausted its iteration budget.
var errMaxIterations = errors.New("core: agent loop exceeded max iterations")

// loopConfig holds the configurable dependencies of a LoopAgent. All
// dependencies are interface types (SCAN-013).
type loopConfig struct {
	model         llm.BaseChatModel
	tools         tools.ToolRegistry
	maxIterations int
}

// LoopOption configures a LoopAgent at construction time.
type LoopOption func(*loopConfig)

// WithLLM sets the chat model the loop drives.
func WithLLM(m llm.BaseChatModel) LoopOption {
	return func(c *loopConfig) { c.model = m }
}

// WithTools sets the tool registry the loop uses to service tool calls.
func WithTools(tr tools.ToolRegistry) LoopOption {
	return func(c *loopConfig) { c.tools = tr }
}

// WithMaxIterations bounds the number of ReAct turns. Non-positive values fall
// back to the default.
func WithMaxIterations(n int) LoopOption {
	return func(c *loopConfig) {
		if n > 0 {
			c.maxIterations = n
		}
	}
}

// LoopAgent is the pure ReAct (think -> act -> observe) loop. It is stateless
// with respect to a session: given a Submission it drives a conversation with
// the injected chat model, servicing any tool calls the model requests, and
// returns the events it fired.
type LoopAgent struct {
	model         llm.BaseChatModel
	tools         tools.ToolRegistry
	maxIterations int
}

var _ AgentLoop = (*LoopAgent)(nil)

// NewLoopAgent builds a LoopAgent from functional options. Missing optional
// dependencies are left nil and reported at Run time so the loop can fail with
// a clear error rather than panic.
func NewLoopAgent(opts ...LoopOption) *LoopAgent {
	cfg := loopConfig{maxIterations: defaultMaxIterations}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.maxIterations <= 0 {
		cfg.maxIterations = defaultMaxIterations
	}
	la := &LoopAgent{
		model:         cfg.model,
		tools:         cfg.tools,
		maxIterations: cfg.maxIterations,
	}
	slog.Info("core.loop.new",
		"max_iterations", la.maxIterations,
		"model_set", la.model != nil,
		"tools_set", la.tools != nil,
	)
	return la
}

// Run executes the ReAct loop for the submission and returns the events fired
// during execution. It emits a "loop.run" span and uses a trace-aware logger.
func (l *LoopAgent) Run(ctx context.Context, submission Submission) ([]AgentEvent, error) {
	span, spanCtx := tracing.SpanFromContext(ctx, "loop.run", tracing.SpanKindInternal)
	defer span.End()
	logger := tracing.NewTraceLogger(span, nil)
	logger.Info("core.loop.run", "type", submission.Type, "iterations", l.maxIterations)

	if l.model == nil {
		span.SetStatus(tracing.SpanStatusError, errNilModel.Error())
		return nil, errNilModel
	}

	var events []AgentEvent
	messages := []llm.Message{{Role: llm.RoleUser, Content: submission.Content}}

	for iter := 0; iter < l.maxIterations; iter++ {
		if err := spanCtx.Err(); err != nil {
			span.SetStatus(tracing.SpanStatusError, err.Error())
			logger.Error("core.loop.canceled", "iteration", iter, "err", err)
			events = append(events, errEvent(err))
			return events, err
		}

		resp, err := l.model.Generate(spanCtx, messages)
		if err != nil {
			span.SetStatus(tracing.SpanStatusError, err.Error())
			logger.Error("core.loop.generate_error", "iteration", iter, "err", err)
			events = append(events, errEvent(err))
			return events, err
		}
		if resp == nil {
			err := errors.New("core: model returned a nil response")
			span.SetStatus(tracing.SpanStatusError, err.Error())
			logger.Error("core.loop.nil_response", "iteration", iter)
			events = append(events, errEvent(err))
			return events, err
		}

		events = append(events, AgentEvent{Kind: "message", Content: resp.Content, Timestamp: time.Now()})
		logger.Info("core.loop.turn",
			"iteration", iter,
			"tool_calls", len(resp.ToolCalls),
			"content", resp.Content,
		)
		messages = append(messages, llm.Message{Role: llm.RoleAssistant, Content: resp.Content, ToolCalls: resp.ToolCalls})

		if len(resp.ToolCalls) == 0 {
			logger.Info("core.loop.finish", "iterations", iter+1, "messages", len(messages))
			return events, nil
		}

		for _, tc := range resp.ToolCalls {
			if err := spanCtx.Err(); err != nil {
				logger.Error("core.loop.canceled", "err", err)
				events = append(events, errEvent(err))
				return events, err
			}
			events = append(events, AgentEvent{Kind: "tool_call", Content: tc.Name, Timestamp: time.Now()})

			resultText, execErr := l.executeTool(spanCtx, toToolsCall(tc))
			if execErr != nil {
				span.SetStatus(tracing.SpanStatusError, execErr.Error())
				logger.Error("core.loop.tool_error", "tool", tc.Name, "err", execErr)
				events = append(events, errEvent(execErr))
				return events, execErr
			}
			events = append(events, AgentEvent{Kind: "tool_result", Content: resultText, Timestamp: time.Now()})
			messages = append(messages, llm.Message{
				Role:       llm.RoleTool,
				ToolCallID: tc.ID,
				Name:       tc.Name,
				Content:    resultText,
			})
		}
	}

	// Iteration budget exhausted.
	span.SetStatus(tracing.SpanStatusError, errMaxIterations.Error())
	logger.Error("core.loop.max_iterations", "max", l.maxIterations)
	events = append(events, errEvent(errMaxIterations))
	return events, errMaxIterations
}

// executeTool looks up the tool and runs its definition against the registry.
func (l *LoopAgent) executeTool(ctx context.Context, call tools.ToolCall) (string, error) {
	if l.tools == nil {
		return "", errors.New("core: agent loop has no tool registry")
	}
	def, err := l.tools.Get(ctx, call.Name)
	if err != nil {
		return "", err
	}
	result, err := def.Execute(ctx, call)
	if err != nil {
		return "", err
	}
	if result == nil {
		return "", nil
	}
	return result.Output, nil
}

// toToolsCall converts an llm.ToolCall (Args is `any`) into a tools.ToolCall
// (Args is `map[string]any`).
func toToolsCall(tc llm.ToolCall) tools.ToolCall {
	call := tools.ToolCall{ID: tc.ID, Name: tc.Name}
	if m, ok := tc.Args.(map[string]any); ok {
		call.Args = m
	}
	return call
}

// errEvent builds a timestamped "error" AgentEvent for the given error.
func errEvent(err error) AgentEvent {
	return AgentEvent{Kind: "error", Content: err.Error(), Timestamp: time.Now()}
}
