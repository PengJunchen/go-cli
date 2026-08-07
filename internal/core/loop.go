package core //exempt:scan009

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/tracing"
)

// defaultMaxIterations bounds the number of think -> act -> observe turns a
// LoopAgent performs before giving up, guarding against runaway tool loops.
// Complex multi-step tasks (install dependencies, write code, run, debug)
// legitimately need many turns, so the default is generous.
const defaultMaxIterations = 200

// maxContinuationAttempts bounds the number of automatic continuation
// requests the loop issues when the model output is truncated by max_tokens
// (finish_reason == "length"). After this many attempts the loop proceeds
// with whatever content has been accumulated and logs a warning.
const maxContinuationAttempts = 3

// errNilModel reports that a LoopAgent has no chat model wired up.
var errNilModel = errors.New("core: agent loop has no chat model")

// errMaxIterations reports that a LoopAgent exhausted its iteration budget.
var errMaxIterations = errors.New("core: agent loop exceeded max iterations")

// loopConfig holds the configurable dependencies of a LoopAgent. All
// dependencies are interface types.
type loopConfig struct {
	model                llm.BaseChatModel
	tools                tools.ToolRegistry
	maxIterations        int
	modelWrapper         ModelWrapper
	executionMode        ExecutionMode
	systemPromptOverride string
	promptBuilder        SystemPromptBuilder
	promptOpts           SystemPromptOptions
	tracer               *tracing.Tracer
	steerCh              chan string
	followUpCh           chan string
	thinkingConfig       *llm.ThinkingConfig
}

// LoopOption configures a LoopAgent at construction time.
type LoopOption func(*loopConfig)

// WithLLM sets the chat model the loop drives.
func WithLLM(m llm.BaseChatModel) LoopOption {
	return func(c *loopConfig) { c.model = m }
}

// WithSystemPrompt overrides the default system prompt the loop sends to the
// model. A non-empty prompt replaces the tool-aware default entirely; an empty
// prompt leaves the default in place. This is how sub-agents receive their own
// role-specific system prompt.
func WithSystemPrompt(prompt string) LoopOption {
	return func(c *loopConfig) { c.systemPromptOverride = prompt }
}

// WithSystemPromptBuilder sets a SystemPromptBuilder that dynamically assembles
// the system prompt at Run time. When set, the builder takes precedence over
// both the default systemPrompt() function and the systemPromptOverride. When
// nil, the loop falls back to the legacy behavior.
func WithSystemPromptBuilder(b SystemPromptBuilder) LoopOption {
	return func(c *loopConfig) { c.promptBuilder = b }
}

// WithSystemPromptOptions sets the options passed to the SystemPromptBuilder at
// Run time. The Tools field is populated automatically from the tool registry;
// callers need only set Cwd, ContextFiles, Skills, CustomPrompt, and
// AppendPrompt.
func WithSystemPromptOptions(opts SystemPromptOptions) LoopOption {
	return func(c *loopConfig) { c.promptOpts = opts }
}

// WithTools sets the tool registry the loop uses to service tool calls.
func WithTools(tr tools.ToolRegistry) LoopOption {
	return func(c *loopConfig) { c.tools = tr }
}

// WithMaxIterations bounds the number of ReAct turns. A value of -1
// disables the iteration limit entirely. Zero falls back to the default.
func WithMaxIterations(n int) LoopOption {
	return func(c *loopConfig) {
		c.maxIterations = n
	}
}

// WithSteeringChannel sets the channel the loop drains for steering
// instructions between LLM iterations. When a steering message is pending,
// it is injected into the conversation as a user message before the next LLM
// call. Steering can only happen between iterations, not during generation,
// because the LLM call is a synchronous blocking operation.
func WithSteeringChannel(ch chan string) LoopOption {
	return func(c *loopConfig) { c.steerCh = ch }
}

// WithFollowUpChannel sets the channel the loop drains for follow-up user
// messages between LLM iterations. Like steering, follow-ups are injected as
// user messages before the next LLM call. Follow-ups are drained after
// steering so the model sees steering context first, then the follow-up.
func WithFollowUpChannel(ch chan string) LoopOption {
	return func(c *loopConfig) { c.followUpCh = ch }
}

// WithThinkingConfig sets the thinking configuration applied to every LLM
// Generate/Stream call. When set, the loop appends llm.WithThinking(cfg) to
// the generation options so the provider can enable reasoning according to the
// configured level. When nil, no thinking option is added.
func WithThinkingConfig(cfg llm.ThinkingConfig) LoopOption {
	return func(c *loopConfig) {
		c.thinkingConfig = &cfg
	}
}

// LoopAgent is the pure ReAct (think -> act -> observe) loop. It is stateless
// with respect to a session: given a Submission it drives a conversation with
// the injected chat model, servicing any tool calls the model requests, and
// returns the events it fired.
type LoopAgent struct {
	model                llm.BaseChatModel
	tools                tools.ToolRegistry
	maxIterations        int
	modelWrapper         ModelWrapper
	executionMode        ExecutionMode
	systemPromptOverride string
	promptBuilder        SystemPromptBuilder
	promptOpts           SystemPromptOptions
	tracer               *tracing.Tracer
	steerCh              chan string
	followUpCh           chan string
	thinkingConfig       *llm.ThinkingConfig
	// toolSearchThreshold is the maximum number of tools to expose to the LLM
	// without filtering. When the tool count exceeds this, tools are
	// dynamically scored and filtered by relevance to the current query.
	// Zero means no filtering (all tools exposed).
	toolSearchThreshold int
	pauseMu             sync.Mutex
	pauseCh             chan struct{}
}

var _ AgentLoop = (*LoopAgent)(nil)

// Pause causes the loop to block at the top of the next iteration until Resume
// is called. It is safe to call from a different goroutine than Run. Calling
// Pause when already paused is a no-op.
func (l *LoopAgent) Pause() {
	l.pauseMu.Lock()
	defer l.pauseMu.Unlock()
	if l.pauseCh == nil {
		l.pauseCh = make(chan struct{})
		slog.Info("core.loop.pause")
	}
}

// Resume unblocks the loop after a Pause. It is safe to call from a different
// goroutine than Run. Calling Resume when not paused is a no-op.
func (l *LoopAgent) Resume() {
	l.pauseMu.Lock()
	defer l.pauseMu.Unlock()
	if l.pauseCh != nil {
		close(l.pauseCh)
		l.pauseCh = nil
		slog.Info("core.loop.resume")
	}
}

// pauseWait blocks until the loop is resumed or the context is canceled. It
// returns nil when resumed, or the context error when canceled.
func (l *LoopAgent) pauseWait(ctx context.Context) error {
	l.pauseMu.Lock()
	pch := l.pauseCh
	l.pauseMu.Unlock()
	if pch == nil {
		return nil
	}
	slog.Info("core.loop.pause_wait")
	select {
	case <-pch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// NewLoopAgent builds a LoopAgent from functional options. Missing optional
// dependencies are left nil and reported at Run time so the loop can fail with
// a clear error rather than panic.
func NewLoopAgent(opts ...LoopOption) *LoopAgent {
	cfg := loopConfig{maxIterations: defaultMaxIterations}
	for _, o := range opts {
		o(&cfg)
	}
	// maxIterations == -1 means unlimited (0 is reserved for "not set",
	// which falls back to the built-in default).
	if cfg.maxIterations == 0 {
		cfg.maxIterations = defaultMaxIterations
	}
	la := &LoopAgent{
		model:                cfg.model,
		tools:                cfg.tools,
		maxIterations:        cfg.maxIterations,
		modelWrapper:         cfg.modelWrapper,
		executionMode:        cfg.executionMode,
		systemPromptOverride: cfg.systemPromptOverride,
		promptBuilder:        cfg.promptBuilder,
		promptOpts:           cfg.promptOpts,
		tracer:               cfg.tracer,
		steerCh:              cfg.steerCh,
		followUpCh:           cfg.followUpCh,
		thinkingConfig:       cfg.thinkingConfig,
	}
	slog.Info("core.loop.new",
		"max_iterations", la.maxIterations,
		"model_set", la.model != nil,
		"tools_set", la.tools != nil,
	)
	return la
}

// WithToolSearchThreshold sets the maximum number of tools exposed to the
// LLM before dynamic filtering kicks in.
func (l *LoopAgent) WithToolSearchThreshold(n int) *LoopAgent {
	l.toolSearchThreshold = n
	return l
}

// Run executes the ReAct loop for the submission and returns the events fired
// during execution. When stream is non-nil, events are sent in real time as
// they happen (streaming mode); otherwise they are collected into the returned
// slice (batch mode, for backward compatibility).
func (l *LoopAgent) Run(ctx context.Context, submission Submission, stream ...EventStream) ([]AgentEvent, error) {
	// Inject the Tracer into the context so that all downstream
	// SpanFromContext calls (middleware, loop internals, tools) create real
	// spans sharing the same trace. When l.tracer is nil, SpanFromContext
	// returns noop spans (zero overhead).
	if l.tracer != nil {
		ctx = l.tracer.ContextWithTracer(ctx)
	}

	span, spanCtx := tracing.SpanFromContext(ctx, "loop.run", tracing.SpanKindInternal)
	defer span.End()
	logger := tracing.NewTraceLogger(span, nil)
	logger.Info("core.loop.run", "type", submission.Type, "iterations", l.maxIterations)

	var es EventStream
	if len(stream) > 0 {
		es = stream[0]
	}

	if l.model == nil {
		span.SetStatus(tracing.SpanStatusError, errNilModel.Error())
		return nil, errNilModel
	}

	// Apply the model wrapper (if any) before the first LLM call. The
	// wrapper may add middleware such as retry, cost tracking, etc.
	model := l.model
	if l.modelWrapper != nil {
		if wrapped := l.modelWrapper(model); wrapped != nil {
			if m, ok := wrapped.(llm.BaseChatModel); ok {
				model = m
				logger.Info("core.loop.model_wrapped")
			}
		}
	}

	// Build tool definitions for the model from the tool registry so the LLM
	// knows what tools it can invoke.
	var toolOpts []llm.Option
	var searchTool *tools.ToolSearchTool
	needToolFiltering := false
	if l.tools != nil {
		defs, listErr := l.tools.List(spanCtx)
		if listErr != nil {
			logger.Warn("core.loop.list_tools_failed", "err", listErr)
		} else if len(defs) > 0 {
			llmTools := make([]llm.ToolDefinition, 0, len(defs))
			for _, d := range defs {
				td := llm.ToolDefinition{
					Name:        d.Name(),
					Description: d.Description(),
				}
				if p, ok := d.(tools.Parameterized); ok {
					td.Parameters = p.Parameters()
				}
				llmTools = append(llmTools, td)
			}
			toolOpts = append(toolOpts, llm.WithTools(llmTools))

			// Dynamic tool filtering: when the tool count exceeds the
			// threshold, score and filter to reduce context bloat.
			if l.toolSearchThreshold > 0 && len(defs) > l.toolSearchThreshold {
				for _, d := range defs {
					if st, ok := d.(*tools.ToolSearchTool); ok {
						searchTool = st
						needToolFiltering = true
						break
					}
				}
			}
		}
	}

	// Apply thinking configuration to every LLM call so the provider can
	// enable reasoning according to the configured level.
	if l.thinkingConfig != nil {
		toolOpts = append(toolOpts, llm.WithThinking(*l.thinkingConfig))
	}

	var events []AgentEvent

	// sendEvent emits an event both to the local events slice and (when
	// streaming) to the EventStream in real time.
	sendEvent := func(ev AgentEvent) {
		events = append(events, ev)
		if es != nil {
			_ = es.Send(ev) //nolint:errcheck
		}
	}

	// Build the conversation from history (if any) plus the current
	// submission. Prior turns must be included or the LLM loses context and
	// cannot answer questions referencing earlier conversation.
	messages := make([]llm.Message, 0, len(submission.History)+2)

	// System prompt: tell the model it can use tools to help the user.
	// When a SystemPromptBuilder is wired, it takes precedence and assembles
	// the prompt dynamically from structured options. Otherwise fall back to
	// the legacy behavior: a non-empty override (e.g. from a sub-agent config)
	// replaces the tool-aware default entirely.
	var sysPrompt string
	if l.promptBuilder != nil {
		opts := l.promptOpts
		if l.tools != nil {
			if defs, listErr := l.tools.List(spanCtx); listErr == nil {
				opts.Tools = defs
			} else {
				logger.Warn("core.loop.list_tools_for_prompt_failed", "err", listErr)
			}
		}
		sysPrompt = l.promptBuilder.Build(spanCtx, opts)
	} else {
		sysPrompt = l.systemPromptOverride
		if sysPrompt == "" {
			sysPrompt = systemPrompt(l.tools)
		}
	}
	messages = append(messages, llm.Message{Role: llm.RoleSystem, Content: sysPrompt})

	for _, hm := range submission.History {
		messages = append(messages, llm.Message{
			Role:          llm.Role(hm.Role),
			Content:       hm.Content,
			ContentBlocks: hm.ContentBlocks,
		})
	}
	if len(messages) == 0 || messages[len(messages)-1].Role != llm.RoleUser {
		messages = append(messages, llm.Message{Role: llm.RoleUser, Content: submission.Content})
	}

	for iter := 0; l.maxIterations < 0 || iter < l.maxIterations; iter++ {
		if err := spanCtx.Err(); err != nil {
			span.SetStatus(tracing.SpanStatusError, err.Error())
			logger.Error("core.loop.canceled", "iteration", iter, "err", err)
			sendEvent(errEvent(err))
			return events, err
		}

		// Block here if the loop has been paused via Pause(). This allows
		// the user to suspend agent execution between LLM iterations.
		if perr := l.pauseWait(spanCtx); perr != nil {
			span.SetStatus(tracing.SpanStatusError, perr.Error())
			logger.Error("core.loop.canceled_during_pause", "iteration", iter, "err", perr)
			sendEvent(errEvent(perr))
			return events, perr
		}

		// Drain any pending steering messages from the steer channel.
		// Steering can only happen between LLM iterations, not during
		// generation, because the LLM call is a synchronous blocking
		// operation. Each steering message is injected as a user message
		// so the model sees it on the next iteration.
		if l.steerCh != nil {
			drainSteerMessages(l.steerCh, &messages, logger)
		}

		// Drain any pending follow-up messages from the follow-up channel.
		// Follow-ups are injected as user messages after steering so the
		// model sees steering context first, then the follow-up.
		if l.followUpCh != nil {
			drainFollowUpMessages(l.followUpCh, &messages, logger)
		}

		// ---- LLM call (streaming) ----
		// Dynamic tool filtering: when the tool count exceeds the threshold,
		// rebuild tool options with only the tools relevant to the current
		// query so the LLM context isn't bloated by irrelevant tool schemas.
		iterOpts := toolOpts
		if needToolFiltering && searchTool != nil {
			query := lastUserQuery(messages)
			if query != "" {
				filtered, ferr := searchTool.TopTools(spanCtx, query, l.toolSearchThreshold)
				if ferr == nil && len(filtered) > 0 {
					iterOpts = buildToolOpts(filtered, l.thinkingConfig)
					logger.Info("core.loop.tool_search_filter",
						"query", query,
						"filtered_tools", len(filtered),
						"threshold", l.toolSearchThreshold,
					)
				}
			}
		}

		resp, err := l.generateWithContinuation(spanCtx, model, messages, iterOpts, es, logger)
		if err != nil {
			span.SetStatus(tracing.SpanStatusError, err.Error())
			logger.Error("core.loop.generate_error", "iteration", iter, "err", err)
			sendEvent(errEvent(err))
			return events, err
		}
		if resp == nil {
			err := errors.New("core: model returned a nil response")
			span.SetStatus(tracing.SpanStatusError, err.Error())
			logger.Error("core.loop.nil_response", "iteration", iter)
			sendEvent(errEvent(err))
			return events, err
		}

		logger.Info("core.loop.turn",
			"iteration", iter,
			"tool_calls", len(resp.ToolCalls),
			"content", resp.Content,
		)
		messages = append(messages, llm.Message{Role: llm.RoleAssistant, Content: resp.Content, ToolCalls: resp.ToolCalls})

		// Emit the complete assistant message as a non-incremental event for
		// every LLM response. Downstream consumers (harness result,
		// lastMessageEvent) depend on it. The TUI skips duplicates when
		// incremental streaming has already rendered the content.
		if resp.Content != "" {
			sendEvent(AgentEvent{Kind: "message", Content: resp.Content, Timestamp: time.Now()})
		}

		if len(resp.ToolCalls) == 0 {
			logger.Info("core.loop.finish", "iterations", iter+1, "messages", len(messages))
			return events, nil
		}

		if l.executionMode == ExecutionModeParallel && len(resp.ToolCalls) > 1 {
			// Parallel mode: emit all tool_call events, execute concurrently,
			// then process results in input order.
			for _, tc := range resp.ToolCalls {
				sendEvent(AgentEvent{Kind: "tool_call", Content: tc.Name, Timestamp: time.Now()})
				logger.Info("core.loop.tool_call", "iteration", iter, "tool", tc.Name, "mode", "parallel")
			}
			results, perr := executeToolsParallel(spanCtx, l.tools, resp.ToolCalls, es)
			for _, res := range results {
				if res.Err != nil {
					logger.Warn("core.loop.tool_error", "tool", res.Name, "err", res.Err)
					res.Output = "Error: " + res.Err.Error()
					if spanCtx.Err() != nil {
						span.SetStatus(tracing.SpanStatusError, res.Err.Error())
						sendEvent(errEvent(res.Err))
						return events, res.Err
					}
				}
				sendEvent(AgentEvent{Kind: "tool_result", Content: res.Output, Timestamp: time.Now()})
				messages = append(messages, llm.Message{
					Role:       llm.RoleTool,
					ToolCallID: res.ID,
					Name:       res.Name,
					Content:    res.Output,
				})
			}
			if perr != nil {
				return events, perr
			}
		} else {
			// Sequential mode: execute one tool at a time.
			for _, tc := range resp.ToolCalls {
				if err := spanCtx.Err(); err != nil {
					logger.Error("core.loop.canceled", "err", err)
					sendEvent(errEvent(err))
					return events, err
				}
				sendEvent(AgentEvent{Kind: "tool_call", Content: tc.Name, Timestamp: time.Now()})
				logger.Info("core.loop.tool_call", "iteration", iter, "tool", tc.Name)

				resultText, execErr := l.executeTool(spanCtx, toToolsCall(tc), es)
				if execErr != nil {
					logger.Warn("core.loop.tool_error", "tool", tc.Name, "err", execErr)
					resultText = "Error: " + execErr.Error()
					if spanCtx.Err() != nil {
						span.SetStatus(tracing.SpanStatusError, execErr.Error())
						sendEvent(errEvent(execErr))
						return events, execErr
					}
				}
				sendEvent(AgentEvent{Kind: "tool_result", Content: resultText, Timestamp: time.Now()})
				messages = append(messages, llm.Message{
					Role:       llm.RoleTool,
					ToolCallID: tc.ID,
					Name:       tc.Name,
					Content:    resultText,
				})
			}
		}
	}

	// Iteration budget exhausted.
	span.SetStatus(tracing.SpanStatusError, errMaxIterations.Error())
	logger.Error("core.loop.max_iterations", "max", l.maxIterations)
	sendEvent(errEvent(errMaxIterations))
	return events, errMaxIterations
}

// streamGenerate calls the LLM in streaming mode. It consumes chunks from
// model.Stream() in real time, emitting incremental "message" events via the
// EventStream so the TUI can render tokens as they arrive. The complete
// response (with accumulated tool calls) is returned.
func (l *LoopAgent) streamGenerate(ctx context.Context, model llm.BaseChatModel, messages []llm.Message, toolOpts []llm.Option, es EventStream, logger *slog.Logger) (*llm.Message, error) {
	ch, err := model.Stream(ctx, messages, toolOpts...)
	if err != nil {
		// Stream failed — propagate the error directly rather than falling
		// back to Generate, because Generate would advance the model's
		// call index a second time and return a different result.
		return nil, err
	}

	var contentBuf strings.Builder
	var toolCalls []llm.ToolCall
	var finishReason string
	gotChunk := false

	for chunk := range ch {
		gotChunk = true
		if chunk.Content != "" {
			contentBuf.WriteString(chunk.Content)
			// Send incremental events only to the EventStream (real-time
			// streaming for TUI consumers), not to the returned events slice.
			// The complete non-incremental "message" event is emitted by the
			// loop once the response finishes.
			if es != nil {
				_ = es.Send(AgentEvent{ //nolint:errcheck
					Kind:        "message",
					Content:     chunk.Content,
					Timestamp:   time.Now(),
					Incremental: true,
				})
			}
		}
		if chunk.Final {
			if len(chunk.ToolCalls) > 0 {
				toolCalls = chunk.ToolCalls
			}
			if chunk.FinishReason != "" {
				finishReason = chunk.FinishReason
			}
		}
	}

	// No chunks at all indicates the model returned an empty response.
	// Return nil so the loop's nil-response guard fires.
	if !gotChunk {
		return nil, nil
	}

	return &llm.Message{
		Role:         llm.RoleAssistant,
		Content:      contentBuf.String(),
		ToolCalls:    toolCalls,
		FinishReason: finishReason,
	}, nil
}

// generateWithContinuation wraps streamGenerate with automatic truncation
// detection and continuation. When the model's finish_reason is "length"
// (output cut off by max_tokens), the partial assistant response is appended
// to the conversation and the model is asked to continue. The continuation
// content (and any tool calls) are merged into the original response. At most
// maxContinuationAttempts retries are issued; if the output is still
// truncated after that, a warning is logged and the accumulated content is
// returned.
func (l *LoopAgent) generateWithContinuation(ctx context.Context, model llm.BaseChatModel, messages []llm.Message, toolOpts []llm.Option, es EventStream, logger *slog.Logger) (*llm.Message, error) {
	resp, err := l.streamGenerate(ctx, model, messages, toolOpts, es, logger)
	if err != nil || resp == nil {
		return resp, err
	}

	for attempt := 0; attempt < maxContinuationAttempts && resp.FinishReason == "length"; attempt++ {
		// Build a continuation conversation: the original messages plus the
		// partial assistant response so the model picks up where it left off.
		contMsgs := make([]llm.Message, len(messages), len(messages)+1)
		copy(contMsgs, messages)
		contMsgs = append(contMsgs, llm.Message{
			Role:         llm.RoleAssistant,
			Content:      resp.Content,
			ToolCalls:    resp.ToolCalls,
			FinishReason: resp.FinishReason,
		})

		logger.Info("core.loop.continuation", "attempt", attempt+1)

		contResp, contErr := l.streamGenerate(ctx, model, contMsgs, toolOpts, es, logger)
		if contErr != nil || contResp == nil {
			break
		}

		// Merge the continuation into the original response.
		resp.Content += contResp.Content
		resp.FinishReason = contResp.FinishReason
		if len(contResp.ToolCalls) > 0 {
			resp.ToolCalls = append(resp.ToolCalls, contResp.ToolCalls...)
		}
	}

	if resp.FinishReason == "length" {
		slog.WarnContext(ctx, "core.loop.truncation_max_attempts",
			"max_attempts", maxContinuationAttempts)
	}

	return resp, nil
}

// eventStreamSink adapts a core.EventStream to satisfy tools.StreamSink.
// It bridges streaming tool output (stdout/stderr lines) into the same
// EventStream the loop uses for all other agent events. Each Send produces a
// "tool_output" AgentEvent carrying the ToolCallID and Stream ("stdout"/
// "stderr") so consumers can associate the line with its originating call.
type eventStreamSink struct {
	es EventStream
}

// Send wraps content as a "tool_output" AgentEvent and forwards it to the
// underlying EventStream.
func (s *eventStreamSink) Send(content, toolCallID, stream string) error {
	return s.es.Send(AgentEvent{
		Kind:       "tool_output",
		Content:    content,
		Timestamp:  time.Now(),
		ToolCallID: toolCallID,
		Stream:     stream,
	})
}

// executeTool looks up the tool and runs its definition against the registry.
// When the tool implements tools.StreamingBashTool and an EventStream is
// provided, it uses ExecuteStreaming to push output lines in real time.
func (l *LoopAgent) executeTool(ctx context.Context, call tools.ToolCall, es EventStream) (string, error) {
	if l.tools == nil {
		return "", errors.New("core: agent loop has no tool registry")
	}
	def, err := l.tools.Get(ctx, call.Name)
	if err != nil {
		return "", err
	}
	// Check if the tool supports streaming output.
	if st, ok := def.(tools.StreamingBashTool); ok && es != nil {
		sink := &eventStreamSink{es: es}
		result, err := st.ExecuteStreaming(ctx, call, sink)
		if err != nil {
			return "", err
		}
		if result == nil {
			return "", nil
		}
		return result.Output, nil
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

// drainSteerMessages non-blockingly drains all pending steering messages from
// ch and appends each as a user message to *msgs. This is how steering
// instructions are injected between LLM iterations: the loop drains the
// channel at the top of each iteration, before calling the LLM.
func drainSteerMessages(ch chan string, msgs *[]llm.Message, logger *slog.Logger) {
	for {
		select {
		case instruction := <-ch:
			*msgs = append(*msgs, llm.Message{Role: llm.RoleUser, Content: instruction})
			logger.Info("core.loop.steer_injected", "instruction", instruction)
		default:
			return
		}
	}
}

// drainFollowUpMessages non-blockingly drains all pending follow-up messages
// from ch and appends each as a user message to *msgs. This mirrors
// drainSteerMessages but is called after it so the model sees steering
// context first, then the follow-up.
func drainFollowUpMessages(ch chan string, msgs *[]llm.Message, logger *slog.Logger) {
	for {
		select {
		case content := <-ch:
			*msgs = append(*msgs, llm.Message{Role: llm.RoleUser, Content: content})
			logger.Info("core.loop.followup_injected", "content_len", len(content))
		default:
			return
		}
	}
}

// lastUserQuery returns the content of the last user message in msgs, or ""
// when there is none.
func lastUserQuery(msgs []llm.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == llm.RoleUser {
			return msgs[i].Content
		}
	}
	return ""
}

// defsToLLMTools converts tool definitions to LLM tool definitions.
func defsToLLMTools(defs []tools.ToolDefinition) []llm.ToolDefinition {
	llmTools := make([]llm.ToolDefinition, 0, len(defs))
	for _, d := range defs {
		td := llm.ToolDefinition{
			Name:        d.Name(),
			Description: d.Description(),
		}
		if p, ok := d.(tools.Parameterized); ok {
			td.Parameters = p.Parameters()
		}
		llmTools = append(llmTools, td)
	}
	return llmTools
}

// buildToolOpts builds LLM generation options from tool definitions and an
// optional thinking config. Used to rebuild tool options per-iteration when
// dynamic tool filtering is active.
func buildToolOpts(defs []tools.ToolDefinition, thinkingCfg *llm.ThinkingConfig) []llm.Option {
	opts := []llm.Option{llm.WithTools(defsToLLMTools(defs))}
	if thinkingCfg != nil {
		opts = append(opts, llm.WithThinking(*thinkingCfg))
	}
	return opts
}

// systemPrompt returns the system instruction that tells the model its role
// and encourages it to use tools when appropriate. When the tool registry is
// nil or empty, a minimal prompt is returned.
func systemPrompt(tr tools.ToolRegistry) string {
	base := `You are a helpful AI assistant embedded in a developer CLI. When the user asks you to perform an action, you MUST use the available tools to accomplish it and persist until the task is fully complete.

Rules:
1. NEVER stop with a statement like "Let me install..." or "I need to..." and then do nothing. If you say you will do something, immediately call a tool to do it in the same turn.
2. When a tool call fails or returns an error, diagnose the cause and try an alternative approach. Do not give up after a single failure.
3. Keep iterating (call tools, observe results, adjust) until the user's request is fully satisfied. Only produce a final text answer with NO tool calls when the task is genuinely complete.
4. Do not guess or fabricate information when a tool can provide the answer.
5. If a skill tool is available and relevant, call it first to obtain expert instructions, then follow those instructions using other tools.`
	if tr == nil {
		return base
	}
	defs, err := tr.List(context.Background())
	if err != nil || len(defs) == 0 {
		return base
	}
	names := make([]string, 0, len(defs))
	for _, d := range defs {
		names = append(names, d.Name())
	}
	return base + "\n\nYou have access to these tools: " + strings.Join(names, ", ") + "."
}
