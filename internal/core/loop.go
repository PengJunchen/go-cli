package core //exempt:scan009

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/pengjunchen/go-cli/internal/compaction"
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
// (finish_reason == "length"). If that budget is exhausted, the incomplete
// candidate is rejected rather than returned as a complete assistant turn.
const maxContinuationAttempts = 3

// errNilModel reports that a LoopAgent has no chat model wired up.
var errNilModel = errors.New("core: agent loop has no chat model")

// ErrLoopDetected is returned when an active tool attempt reaches the
// configured repeated-action threshold with terminate disposition.
var ErrLoopDetected = errors.New("core: active loop detected")

// errMaxIterations reports that a LoopAgent exhausted its iteration budget.
var errMaxIterations = errors.New("core: agent loop exceeded max iterations")

var errCurrentSubmissionNotPreserved = errors.New("core: compaction did not preserve the current submission")

// loopConfig holds the configurable dependencies of a LoopAgent. All
// dependencies are interface types.
type loopConfig struct {
	model                   llm.BaseChatModel
	providerAttemptRecorder func(llm.ProviderAttemptResult) error
	providerRetryPolicy     llm.RetryPolicy
	tools                   tools.ToolRegistry
	maxIterations           int
	modelWrapper            ModelWrapper
	executionMode           ExecutionMode
	toolInterceptors        []ToolInterceptor
	systemPromptOverride    string
	promptBuilder           SystemPromptBuilder
	promptOpts              SystemPromptOptions
	tracer                  *tracing.Tracer
	steerCh                 chan string
	followUpCh              chan string
	thinkingConfig          *llm.ThinkingConfig
	midTurnCompactor        *compaction.MidTurnCompact
	compactor               compaction.Compactor
	estimator               compaction.TokenEstimator
	maxTokens               int
}

// LoopOption configures a LoopAgent at construction time.
type LoopOption func(*loopConfig)

// WithProviderAttemptRecorder installs the run-scoped sink for immutable
// provider results produced by resolver-built models.
func WithProviderAttemptRecorder(record func(llm.ProviderAttemptResult) error) LoopOption {
	return func(c *loopConfig) { c.providerAttemptRecorder = record }
}

// WithProviderRetryPolicy installs the policy used by the Loop-owned provider
// operation scheduler. Active middleware dispatches only the candidate chosen
// by Core and does not make retry decisions.
func WithProviderRetryPolicy(policy llm.RetryPolicy) LoopOption {
	return func(c *loopConfig) { c.providerRetryPolicy = policy }
}

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

// WithToolInterceptor adds an interceptor scoped to the LoopAgent being
// constructed. A nil interceptor is ignored.
func WithToolInterceptor(f ToolInterceptor) LoopOption {
	return func(c *loopConfig) {
		if f != nil {
			c.toolInterceptors = append(c.toolInterceptors, f)
		}
	}
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
	model                   llm.BaseChatModel
	providerAttemptRecorder func(llm.ProviderAttemptResult) error
	providerRetryPolicy     llm.RetryPolicy
	tools                   tools.ToolRegistry
	maxIterations           int
	modelWrapper            ModelWrapper
	executionMode           ExecutionMode
	toolInterceptors        []ToolInterceptor
	systemPromptOverride    string
	promptBuilder           SystemPromptBuilder
	promptOpts              SystemPromptOptions
	tracer                  *tracing.Tracer
	steerCh                 chan string
	followUpCh              chan string
	thinkingConfig          *llm.ThinkingConfig
	midTurnCompactor        *compaction.MidTurnCompact
	compactor               compaction.Compactor
	estimator               compaction.TokenEstimator
	maxTokens               int
	// toolSearchThreshold is the maximum number of tools to expose to the LLM
	// without filtering. When the tool count exceeds this, tools are
	// dynamically scored and filtered by relevance to the current query.
	// Zero means no filtering (all tools exposed).
	toolSearchThreshold int
	// mu protects runtime mutable fields (model, midTurnCompactor, compactor,
	// estimator, maxTokens, toolSearchThreshold) from data races between
	// concurrent setter calls and Run.
	mu      sync.RWMutex
	pauseMu sync.Mutex
	pauseCh chan struct{}

	// toolDefsCache caches the LLM tool definitions so they are not
	// rebuilt on every Run call. The cache is invalidated when the tool
	// registry version changes. When the registry does not support
	// versioning (Version() returns 0), the cache is always considered
	// valid after the first build.
	toolDefsCache   []llm.ToolDefinition
	toolDefsVersion int
	toolDefsMu      sync.RWMutex
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
		model:                   cfg.model,
		providerAttemptRecorder: cfg.providerAttemptRecorder,
		providerRetryPolicy:     cfg.providerRetryPolicy,
		tools:                   cfg.tools,
		maxIterations:           cfg.maxIterations,
		modelWrapper:            cfg.modelWrapper,
		executionMode:           cfg.executionMode,
		toolInterceptors:        append([]ToolInterceptor(nil), cfg.toolInterceptors...),
		systemPromptOverride:    cfg.systemPromptOverride,
		promptBuilder:           cfg.promptBuilder,
		promptOpts:              cfg.promptOpts,
		tracer:                  cfg.tracer,
		steerCh:                 cfg.steerCh,
		followUpCh:              cfg.followUpCh,
		thinkingConfig:          cfg.thinkingConfig,
		midTurnCompactor:        cfg.midTurnCompactor,
		compactor:               cfg.compactor,
		estimator:               cfg.estimator,
		maxTokens:               cfg.maxTokens,
	}
	slog.Info("core.loop.new",
		"max_iterations", la.maxIterations,
		"model_set", la.model != nil,
		"tools_set", la.tools != nil,
	)
	return la
}

func (l *LoopAgent) newPreToolCallEvent(toolName, toolCallID string, args map[string]any) *PreToolCallEvent {
	l.mu.RLock()
	interceptors := append([]ToolInterceptor(nil), l.toolInterceptors...)
	l.mu.RUnlock()

	return &PreToolCallEvent{
		ToolName:     toolName,
		ToolCallID:   toolCallID,
		Args:         args,
		interceptors: interceptors,
	}
}

// WithToolSearchThreshold sets the maximum number of tools exposed to the
// LLM before dynamic filtering kicks in.
func (l *LoopAgent) WithToolSearchThreshold(n int) *LoopAgent {
	l.mu.Lock()
	l.toolSearchThreshold = n
	l.mu.Unlock()
	return l
}

// buildToolDefinitions returns the cached LLM tool definitions, rebuilding
// them only when the tool registry version has changed. When the registry
// does not support versioning (Version() returns 0), the cache is always
// considered valid after the first build.
func (l *LoopAgent) buildToolDefinitions(ctx context.Context) ([]llm.ToolDefinition, error) {
	if l.tools == nil {
		return nil, nil
	}

	version := 0
	if v, ok := l.tools.(interface{ Version() int }); ok {
		version = v.Version()
	}

	l.toolDefsMu.RLock()
	cached := l.toolDefsCache
	cachedVersion := l.toolDefsVersion
	l.toolDefsMu.RUnlock()

	if cached != nil && version == cachedVersion {
		return cached, nil
	}

	defs, err := l.tools.List(ctx)
	if err != nil {
		return nil, err
	}

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

	l.toolDefsMu.Lock()
	l.toolDefsCache = llmTools
	l.toolDefsVersion = version
	l.toolDefsMu.Unlock()

	return llmTools, nil
}

// SetMidTurnCompaction wires the mid-turn compaction guard post-construction.
// This is used when the compactor components are created after NewLoopAgent
// (e.g. in the assemble layer where the compactor factory runs later).
func (l *LoopAgent) SetMidTurnCompaction(mtc *compaction.MidTurnCompact, compactor compaction.Compactor, estimator compaction.TokenEstimator, maxTokens int) {
	l.mu.Lock()
	l.midTurnCompactor = mtc
	l.compactor = compactor
	l.estimator = estimator
	l.maxTokens = maxTokens
	l.mu.Unlock()
}

// SetModel replaces the LLM model at runtime. The next Run call will use the
// new model. This is used by the /model slash command to switch models
// without restarting the process.
func (l *LoopAgent) SetModel(m llm.BaseChatModel) {
	l.mu.Lock()
	l.model = m
	l.mu.Unlock()
}

// Run executes the ReAct loop for the submission and returns the events fired
// during execution. When stream is non-nil, events are sent in real time as
// they happen (streaming mode); otherwise they are collected into the returned
// slice (batch mode, for backward compatibility).
func (l *LoopAgent) Run(ctx context.Context, submission Submission, stream ...EventStream) ([]AgentEvent, error) {
	result, err := l.RunCanonical(ctx, submission, stream...)
	return result.Events, err
}

// RunCanonical executes one ReAct run and returns a typed transcript delta
// owned by this invocation. Prior submission history remains model context
// only and is never copied into Transcript.
func (l *LoopAgent) RunCanonical(ctx context.Context, submission Submission, stream ...EventStream) (runResult CanonicalRunResult, runErr error) {
	events := make([]AgentEvent, 0)
	transcript := []TranscriptEntry{{Message: AgentMessage{
		Role:      string(llm.RoleUser),
		Content:   submission.Content,
		InputKind: InputKindUser,
	}}}

	// Snapshot runtime mutable fields under a read lock to avoid data races
	// with concurrent setter calls (SetModel, SetMidTurnCompaction,
	// WithToolSearchThreshold). The local copies are used throughout Run.
	l.mu.RLock()
	model := l.model
	toolSearchThreshold := l.toolSearchThreshold
	midTurnCompactor := l.midTurnCompactor
	compactor := l.compactor
	estimator := l.estimator
	maxTokens := l.maxTokens
	l.mu.RUnlock()

	// Inject the Tracer into the context so that all downstream
	// SpanFromContext calls (middleware, loop internals, tools) create real
	// spans sharing the same trace. When l.tracer is nil, SpanFromContext
	// returns noop spans (zero overhead).
	if l.tracer != nil {
		ctx = l.tracer.ContextWithTracer(ctx)
	}

	span, spanCtx := tracing.SpanFromContext(ctx, "loop.run", tracing.SpanKindInternal)
	defer span.End()
	spanCtx = llm.WithRunOperationFactory(spanCtx, &providerOperationFactory{
		record:      l.providerAttemptRecorder,
		retryPolicy: l.providerRetryPolicy,
	})
	logger := tracing.NewTraceLogger(span, nil)
	logger.Info("core.loop.run", "type", submission.Type, "iterations", l.maxIterations)

	var es EventStream
	if len(stream) > 0 {
		es = stream[0]
	}
	consumer, commitErr := newRunCommitConsumer(spanCtx)
	if commitErr != nil {
		span.SetStatus(tracing.SpanStatusError, commitErr.Error())
		return CanonicalRunResult{Transcript: transcript}, commitErr
	}
	if consumer == nil && midTurnCompactor != nil {
		midTurnCompactor = midTurnCompactor.NewRun()
	}
	if consumer != nil {
		spanCtx = withRunCommitLoopScope(spanCtx)
	}
	terminalEligible := false
	if consumer != nil {
		input := pendingRunInput(submission.History, submission.Content, consumer)
		submission.History = input.history
		transcript[0].Message = input.message
		provisional := agentMessagesToTurnItems(submission.History)
		for i := range provisional {
			if provisional[i].Runtime.Source.Kind == llm.SourcePendingRunInput {
				provisional[i].Runtime.Source = consumer.transcriptMetadata(0, provisional[i].Runtime.ReceiptOutcome).Source
			}
		}
		if commitErr = validateDurableRequestSource(provisional, consumer.source, consumer.runID); commitErr != nil {
			span.SetStatus(tracing.SpanStatusError, commitErr.Error())
			return CanonicalRunResult{Transcript: transcript}, commitErr
		}
		if commitErr = consumer.commitInput(0, transcript[0].Message); commitErr != nil {
			span.SetStatus(tracing.SpanStatusError, commitErr.Error())
			markRunOutcomeUnknown(spanCtx)
			return CanonicalRunResult{Transcript: transcript}, commitErr
		}
		transcript[0].Message.Runtime = consumer.transcriptMetadata(0, transcript[0].Message.ReceiptOutcome)
		terminalEligible = true
		defer func() {
			if !terminalEligible {
				return
			}
			runErr = normalizeLoopDetectedCause(spanCtx, runErr)
			terminalErr := consumer.commitTerminal(runResult.Transcript, consumer.projections, terminalOutcome(runErr))
			if terminalErr != nil {
				markRunOutcomeUnknown(spanCtx)
				runErr = errors.Join(runErr, terminalErr)
				runResult.Events = append(runResult.Events, errEvent(terminalErr))
			}
		}()
		defer func() {
			if errors.Is(context.Cause(spanCtx), ErrLoopDetected) {
				runErr = normalizeLoopDetectedCause(spanCtx, runErr)
			}
		}()
	}

	if model == nil {
		span.SetStatus(tracing.SpanStatusError, errNilModel.Error())
		return CanonicalRunResult{Transcript: transcript}, errNilModel
	}

	// Apply the model wrapper (if any) before the first LLM call. The
	// wrapper may add middleware such as retry, cost tracking, etc.
	if l.modelWrapper != nil {
		if wrapped := l.modelWrapper(model); wrapped != nil {
			if m, ok := wrapped.(llm.BaseChatModel); ok {
				model = m
				logger.Info("core.loop.model_wrapped")
			}
		}
	}

	// Build tool definitions for the model from the tool registry so the LLM
	// knows what tools it can invoke. The raw tool definitions are cached in
	// toolDefs for the duration of this Run so that subsequent paths (tool
	// filtering, prompt builder, system prompt) reuse the same snapshot
	// instead of calling List again. buildToolDefinitions has its own
	// version-based cache and is left untouched.
	var toolDefs []tools.ToolDefinition
	var toolOpts []llm.Option
	var searchTool tools.TopToolsProvider
	needToolFiltering := false
	if l.tools != nil {
		if defs, err := l.tools.List(spanCtx); err == nil {
			toolDefs = defs
		} else {
			logger.Warn("core.loop.list_tools_failed", "err", err)
		}

		llmTools, listErr := l.buildToolDefinitions(spanCtx)
		if listErr != nil {
			logger.Warn("core.loop.list_tools_failed", "err", listErr)
		} else if len(llmTools) > 0 {
			toolOpts = append(toolOpts, llm.WithTools(llmTools))

			// Dynamic tool filtering: when the tool count exceeds the
			// threshold, score and filter to reduce context bloat.
			if toolSearchThreshold > 0 && len(llmTools) > toolSearchThreshold {
				for _, d := range toolDefs {
					if st, ok := d.(tools.TopToolsProvider); ok {
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

	eventCoordinates := agentEventCoordinates{runID: RunIDFromContext(spanCtx)}

	// sendEvent emits an event both to the local events slice and (when
	// streaming) to the EventStream in real time.
	sendEvent := func(ev AgentEvent) {
		ev = stampAgentEvent(eventCoordinates, ev)
		events = append(events, ev)
		if es != nil {
			_ = es.Send(ev) //nolint:errcheck
		}
	}
	sendToolResultEvent := func(content string, isError bool, toolCallID, outcome string, sourceTruncated *bool) {
		sendEvent(AgentEvent{
			Kind: "tool_result", Content: content, Timestamp: time.Now(), IsError: isError,
			ToolCallID: toolCallID, ReceiptOutcome: outcome,
			SourceTruncated: cloneOptionalBool(sourceTruncated),
		})
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
			opts.Tools = toolDefs
		}
		sysPrompt = l.promptBuilder.Build(spanCtx, opts)
	} else {
		sysPrompt = l.systemPromptOverride
		if sysPrompt == "" {
			sysPrompt = systemPromptFromDefs(toolDefs)
		}
	}
	messages = append(messages, llm.Message{
		Role: llm.RoleSystem, Content: sysPrompt,
		Runtime: llm.RuntimeMetadata{Source: llm.SourceRef{Kind: llm.SourceEphemeral}},
	})

	for _, hm := range submission.History {
		runtime := hm.Runtime
		if consumer != nil && runtime.Source.Kind == llm.SourcePendingRunInput {
			runtime = consumer.transcriptMetadata(0, hm.ReceiptOutcome)
		}
		if runtime.ReceiptOutcome == "" {
			runtime.ReceiptOutcome = hm.ReceiptOutcome
		}
		msg := llm.Message{
			Role:          llm.Role(hm.Role),
			Content:       hm.Content,
			ContentBlocks: hm.ContentBlocks,
			Runtime:       runtime,
		}
		// Forward ToolCalls from assistant messages so the LLM can
		// correlate tool calls with tool results across turns.
		if len(hm.ToolCalls) > 0 {
			msg.ToolCalls = hm.ToolCalls
		}
		// Forward ToolCallID and tool name on tool-result messages so
		// the LLM can match results to their originating calls.
		if hm.ToolCallID != "" {
			msg.ToolCallID = hm.ToolCallID
			msg.Name = hm.ToolName
		}
		messages = append(messages, msg)
	}
	if len(messages) == 0 || messages[len(messages)-1].Role != llm.RoleUser || messages[len(messages)-1].Content != submission.Content {
		message := llm.Message{Role: llm.RoleUser, Content: submission.Content}
		if consumer != nil {
			message.Runtime = consumer.transcriptMetadata(0, "")
		}
		messages = append(messages, message)
	}
	currentSubmissionIndex := len(messages) - 1

	// emitCanceledToolResults synthesizes error tool_result messages for any
	// tool calls in the last assistant message that lack a matching
	// tool_result, and emits corresponding events. This keeps the message
	// history complete when the loop is canceled mid-execution so
	// subsequent LLM requests don't reject the conversation.
	emitCanceledToolResults := func(canceledErr error) {
		var synthCalls []llm.ToolCall
		messages, synthCalls = synthesizeCanceledToolResults(messages, canceledErr)
		errMsg := "tool call canceled"
		if canceledErr != nil {
			errMsg = canceledErr.Error()
		}
		for _, tc := range synthCalls {
			appendTranscriptToolReceipt(&transcript, tc, "Error: "+errMsg, ReceiptOutcomeInterrupted, consumer)
			sendToolResultEvent("Error: "+errMsg, true, tc.ID, ReceiptOutcomeInterrupted, nil)
		}
	}

	for iter := 0; l.maxIterations < 0 || iter < l.maxIterations; iter++ {
		if err := spanCtx.Err(); err != nil {
			span.SetStatus(tracing.SpanStatusError, err.Error())
			logger.Error("core.loop.canceled", "iteration", iter, "err", err)
			sendEvent(errEvent(err))
			return CanonicalRunResult{Events: events, Transcript: transcript}, err
		}

		// Block here if the loop has been paused via Pause(). This allows
		// the user to suspend agent execution between LLM iterations.
		if perr := l.pauseWait(spanCtx); perr != nil {
			span.SetStatus(tracing.SpanStatusError, perr.Error())
			logger.Error("core.loop.canceled_during_pause", "iteration", iter, "err", perr)
			sendEvent(errEvent(perr))
			return CanonicalRunResult{Events: events, Transcript: transcript}, perr
		}

		// Drain any pending steering messages from the steer channel.
		// Steering can only happen between LLM iterations, not during
		// generation, because the LLM call is a synchronous blocking
		// operation. Each steering message is injected as a user message
		// so the model sees it on the next iteration.
		if l.steerCh != nil {
			for _, instruction := range drainSteerMessages(l.steerCh, &messages, logger) {
				entry := TranscriptEntry{Message: AgentMessage{Role: string(llm.RoleUser), Content: instruction, InputKind: InputKindSteering}}
				entry.Message.Runtime = consumer.transcriptMetadata(uint64(len(transcript)), "")
				if consumer != nil {
					if commitErr := consumer.commitInput(uint64(len(transcript)), entry.Message); commitErr != nil {
						span.SetStatus(tracing.SpanStatusError, commitErr.Error())
						sendEvent(errEvent(commitErr))
						return CanonicalRunResult{Events: events, Transcript: transcript}, commitErr
					}
				}
				messages[len(messages)-1].Runtime = entry.Message.Runtime
				transcript = append(transcript, entry)
			}
		}

		// Drain any pending follow-up messages from the follow-up channel.
		// Follow-ups are injected as user messages after steering so the
		// model sees steering context first, then the follow-up.
		if l.followUpCh != nil {
			for _, content := range drainFollowUpMessages(l.followUpCh, &messages, logger) {
				entry := TranscriptEntry{Message: AgentMessage{Role: string(llm.RoleUser), Content: content, InputKind: InputKindFollowUp}}
				entry.Message.Runtime = consumer.transcriptMetadata(uint64(len(transcript)), "")
				if consumer != nil {
					if commitErr := consumer.commitInput(uint64(len(transcript)), entry.Message); commitErr != nil {
						span.SetStatus(tracing.SpanStatusError, commitErr.Error())
						sendEvent(errEvent(commitErr))
						return CanonicalRunResult{Events: events, Transcript: transcript}, commitErr
					}
				}
				messages[len(messages)-1].Runtime = entry.Message.Runtime
				transcript = append(transcript, entry)
			}
		}

		if consumer != nil {
			compacted, compactErr := preflightDurableRequest(spanCtx, messages, consumer, compactor, estimator, maxTokens)
			if compactErr != nil {
				span.SetStatus(tracing.SpanStatusError, compactErr.Error())
				sendEvent(errEvent(compactErr))
				return CanonicalRunResult{Events: events, Transcript: transcript}, compactErr
			}
			messages = compacted
		} else if midTurnCompactor != nil && compactor != nil && estimator != nil && maxTokens > 0 {
			compacted, compactedSubmissionIndex, compactResult, compactErr := preflightNonDurableRequest(
				spanCtx, messages, currentSubmissionIndex, midTurnCompactor, compactor, estimator, maxTokens,
			)
			if compactErr != nil {
				logger.Warn("core.loop.midturn_compact_error", "err", compactErr)
				span.SetStatus(tracing.SpanStatusError, compactErr.Error())
				sendEvent(errEvent(compactErr))
				return CanonicalRunResult{Events: events, Transcript: transcript}, compactErr
			}
			if compactResult.Triggered {
				messagesBefore := len(messages)
				messages = compacted
				currentSubmissionIndex = compactedSubmissionIndex
				logger.Info("core.loop.midturn_compact",
					"reason", compactResult.Reason.String(),
					"messages_before", messagesBefore,
					"messages_after", len(messages),
				)
			}
		}

		// ---- LLM call (streaming) ----
		// Dynamic tool filtering: when the tool count exceeds the threshold,
		// rebuild tool options with only the tools relevant to the current
		// query so the LLM context isn't bloated by irrelevant tool schemas.
		iterOpts := toolOpts
		if needToolFiltering && searchTool != nil {
			query := lastUserQuery(messages)
			if query != "" {
				filtered, ferr := searchTool.TopTools(spanCtx, query, toolSearchThreshold)
				if ferr == nil && len(filtered) > 0 {
					iterOpts = buildToolOpts(filtered, l.thinkingConfig)
					logger.Info("core.loop.tool_search_filter",
						"query", query,
						"filtered_tools", len(filtered),
						"threshold", toolSearchThreshold,
					)
				}
			}
		}

		assistantIndex := uint64(len(transcript))
		assistantOrdinal := uint64(iter + 1)
		iterationCoordinates := agentEventCoordinates{runID: RunIDFromContext(spanCtx)}
		if iterationCoordinates.runID != "" {
			iterationCoordinates.canonicalTranscriptIndex = &assistantIndex
		}
		eventCoordinates = iterationCoordinates
		iterationCtx := withAgentEventCoordinates(spanCtx, iterationCoordinates)
		resp, err := l.generateWithContinuationAndCompaction(iterationCtx, model, messages, iterOpts, es, logger, consumer, compactor, estimator, maxTokens)
		if err != nil {
			var providerErr *llm.ProviderError
			if errors.As(err, &providerErr) && providerErr != nil {
				span.SetAttributes(
					tracing.Attribute{Key: "error_type", Value: string(providerErr.ErrorType)},
					tracing.Attribute{Key: "http_status_code", Value: providerErr.StatusCode},
				)
			}
			span.SetStatus(tracing.SpanStatusError, err.Error())
			logger.Error("core.loop.generate_error", "iteration", iter, "err", err)
			sendEvent(errEvent(err))
			return CanonicalRunResult{Events: events, Transcript: transcript}, err
		}
		if resp == nil {
			err := errors.New("core: model returned a nil response")
			span.SetStatus(tracing.SpanStatusError, err.Error())
			logger.Error("core.loop.nil_response", "iteration", iter)
			sendEvent(errEvent(err))
			return CanonicalRunResult{Events: events, Transcript: transcript}, err
		}
		if err := validateCompleteToolCalls(resp.ToolCalls); err != nil {
			span.SetStatus(tracing.SpanStatusError, err.Error())
			sendEvent(errEvent(err))
			return CanonicalRunResult{Events: events, Transcript: transcript}, err
		}
		if len(resp.ToolCalls) > 0 && iterationCoordinates.runID != "" {
			iterationCoordinates.assistantTranscriptOrdinal = &assistantOrdinal
			eventCoordinates = iterationCoordinates
			iterationCtx = withAgentEventCoordinates(iterationCtx, iterationCoordinates)
		}
		assistantEntry, err := transcriptAssistantEntry(resp)
		if err != nil {
			span.SetStatus(tracing.SpanStatusError, err.Error())
			sendEvent(errEvent(err))
			return CanonicalRunResult{Events: events, Transcript: transcript}, err
		}
		assistantEntry.Message.Runtime = consumer.transcriptMetadata(assistantIndex, "")
		transcript = append(transcript, assistantEntry)

		logger.Info("core.loop.turn",
			"iteration", iter,
			"tool_calls", len(resp.ToolCalls),
			"content", resp.Content,
		)
		messages = append(messages, llm.Message{
			Role: llm.RoleAssistant, Content: resp.Content, ToolCalls: resp.ToolCalls,
			Runtime: assistantEntry.Message.Runtime,
		})

		// Emit the complete assistant message as a non-incremental event for
		// every LLM response. Downstream consumers (harness result,
		// lastMessageEvent) depend on it. The TUI skips duplicates when
		// incremental streaming has already rendered the content. Pure tool
		// call responses (empty content but non-empty ToolCalls) must also
		// emit a message event so they enter history and subsequent turns
		// retain tool call context.
		if resp.Content != "" || len(resp.ToolCalls) > 0 {
			sendEvent(AgentEvent{Kind: "message", Content: resp.Content, Timestamp: time.Now(), Usage: resp.Usage, ToolCalls: resp.ToolCalls})
		}

		if len(resp.ToolCalls) == 0 {
			logger.Info("core.loop.finish", "iterations", iter+1, "messages", len(messages))
			return CanonicalRunResult{Events: events, Transcript: transcript}, nil
		}
		if l.executionMode == ExecutionModeParallel && len(resp.ToolCalls) > 1 {
			// Parallel mode: emit pre-tool-call events, then execute
			// non-canceled tools concurrently, matching results by
			// ToolCallID (not positional index) so that out-of-order
			// completion does not cause mismatched results.
			var activeCalls []llm.ToolCall
			canceledCalls := make(map[string]bool)
			for _, tc := range resp.ToolCalls {
				preEv := l.newPreToolCallEvent(tc.Name, tc.ID, toToolsCall(tc).Args)
				sendEvent(AgentEvent{Kind: EventKindPreToolCall, Timestamp: time.Now(), PreToolCall: preEv})
				if preEv.IsCancelled() {
					logger.Info("core.loop.tool_canceled", "tool", tc.Name, "id", tc.ID)
					sendEvent(AgentEvent{Kind: "tool_canceled", Content: tc.Name, Timestamp: time.Now(), ToolCallID: tc.ID})
					canceledCalls[tc.ID] = true
					continue
				}
				activeCalls = append(activeCalls, tc)
				sendEvent(AgentEvent{Kind: "tool_call", Content: tc.Name, Timestamp: time.Now(), ToolCallID: tc.ID})
				logger.Info("core.loop.tool_call", "iteration", iter, "tool", tc.Name, "mode", "parallel")
			}
			if consumer != nil {
				startIndex := len(transcript)
				commitErr := consumer.executeParallelGroup(iterationCtx, l.tools, assistantOrdinal, assistantEntry.Message, resp.ToolCalls, canceledCalls, es, &transcript, &messages)
				for index := startIndex; index < len(transcript); index++ {
					entry := transcript[index]
					message := entry.Message
					if message.Role != string(llm.RoleTool) {
						continue
					}
					sendToolResultEvent(message.Content, message.ReceiptOutcome != ReceiptOutcomeSuccess, message.ToolCallID, message.ReceiptOutcome, entry.SourceTruncated)
				}
				if commitErr != nil {
					span.SetStatus(tracing.SpanStatusError, commitErr.Error())
					sendEvent(errEvent(commitErr))
					return CanonicalRunResult{Events: events, Transcript: transcript}, commitErr
				}
			} else {
				var parallelErr error
				var unknownToolErr error
				resultsByID := make(map[string]ParallelToolResult, len(activeCalls))
				if len(activeCalls) > 0 {
					results, execErr := executeToolsParallel(iterationCtx, l.tools, activeCalls, es)
					parallelErr = execErr
					matched, matchErr := matchToolResultsByID(activeCalls, results)
					if matchErr != nil {
						parallelErr = errors.Join(parallelErr, matchErr)
					} else {
						for _, res := range matched {
							resultsByID[res.ID] = res
						}
					}
				}
				for _, tc := range resp.ToolCalls {
					if canceledCalls[tc.ID] {
						const canceledOutput = "Tool call canceled by interceptor"
						sendToolResultEvent(canceledOutput, true, tc.ID, ReceiptOutcomeInterrupted, nil)
						appendToolReceipt(&messages, &transcript, tc, canceledOutput, ReceiptOutcomeInterrupted, consumer)
						continue
					}
					res, ok := resultsByID[tc.ID]
					if !ok {
						missingErr := fmt.Errorf("core: missing or uncorrelated result for tool call %q", tc.ID)
						parallelErr = errors.Join(parallelErr, missingErr)
						unknownToolErr = errors.Join(unknownToolErr, markUnknownToolOutcome(missingErr))
						output := "Error: " + missingErr.Error()
						sendToolResultEvent(output, true, tc.ID, ReceiptOutcomeUnknown, nil)
						appendToolReceipt(&messages, &transcript, tc, output, ReceiptOutcomeUnknown, consumer)
						continue
					}
					if res.Err != nil {
						logger.Warn("core.loop.tool_error", "tool", res.Name, "err", res.Err)
						res.Output = "Error: " + res.Err.Error()
						res.IsError = true
					}
					outcome := toolReceiptOutcome(iterationCtx, res.Err, res.IsError)
					sendToolResultEvent(res.Output, res.IsError, res.ID, outcome, nil)
					appendToolReceipt(&messages, &transcript, tc, res.Output, outcome, consumer)
					if outcome == ReceiptOutcomeUnknown && unknownToolErr == nil {
						unknownToolErr = markUnknownToolOutcome(res.Err)
					}
				}
				if parallelErr != nil || unknownToolErr != nil {
					runErr := errors.Join(parallelErr, unknownToolErr)
					span.SetStatus(tracing.SpanStatusError, runErr.Error())
					sendEvent(errEvent(runErr))
					return CanonicalRunResult{Events: events, Transcript: transcript}, runErr
				}
			}
		} else {
			// Sequential mode: execute one tool at a time.
			for callIndex, tc := range resp.ToolCalls {
				if err := spanCtx.Err(); err != nil {
					logger.Error("core.loop.canceled", "err", err)
					if consumer != nil {
						for _, undispatched := range resp.ToolCalls[callIndex:] {
							const skippedOutput = "Tool call canceled before dispatch"
							if commitErr := consumer.commitKnownCall(assistantOrdinal, assistantEntry.Message, "batch-"+newClaimID(), undispatched, uint64(len(transcript)), skippedOutput, ReceiptOutcomeInterrupted, &transcript, &messages); commitErr != nil {
								span.SetStatus(tracing.SpanStatusError, commitErr.Error())
								sendEvent(errEvent(commitErr))
								return CanonicalRunResult{Events: events, Transcript: transcript}, commitErr
							}
							sendToolResultEvent(skippedOutput, true, undispatched.ID, ReceiptOutcomeInterrupted, nil)
						}
						sendEvent(errEvent(err))
						return CanonicalRunResult{Events: events, Transcript: transcript}, err
					}
					emitCanceledToolResults(err)
					sendEvent(errEvent(err))
					return CanonicalRunResult{Events: events, Transcript: transcript}, err
				}

				// Emit a pre-tool-call event so external interceptors
				// can cancel the tool call before it executes.
				preEv := l.newPreToolCallEvent(tc.Name, tc.ID, toToolsCall(tc).Args)
				sendEvent(AgentEvent{Kind: EventKindPreToolCall, Timestamp: time.Now(), PreToolCall: preEv})
				if preEv.IsCancelled() {
					logger.Info("core.loop.tool_canceled", "tool", tc.Name, "id", tc.ID)
					sendEvent(AgentEvent{Kind: "tool_canceled", Content: tc.Name, Timestamp: time.Now(), ToolCallID: tc.ID, ReceiptOutcome: ReceiptOutcomeInterrupted})
					if consumer != nil {
						const canceledOutput = "Tool call canceled by interceptor"
						if commitErr := consumer.commitKnownCall(assistantOrdinal, assistantEntry.Message, "batch-"+newClaimID(), tc, uint64(len(transcript)), canceledOutput, ReceiptOutcomeInterrupted, &transcript, &messages); commitErr != nil {
							span.SetStatus(tracing.SpanStatusError, commitErr.Error())
							sendEvent(errEvent(commitErr))
							return CanonicalRunResult{Events: events, Transcript: transcript}, commitErr
						}
					} else {
						appendToolReceipt(&messages, &transcript, tc, "Tool call canceled by interceptor", ReceiptOutcomeInterrupted, consumer)
					}
					continue
				}

				sendEvent(AgentEvent{Kind: "tool_call", Content: tc.Name, Timestamp: time.Now(), ToolCallID: tc.ID})
				logger.Info("core.loop.tool_call", "iteration", iter, "tool", tc.Name)

				if consumer != nil {
					result, commitErr := consumer.executeSequentialCall(iterationCtx, l.tools, assistantOrdinal, assistantEntry.Message, tc, es, &transcript, &messages)
					sendToolResultEvent(result.output, result.isError, tc.ID, result.outcome, result.sourceTruncated)
					if commitErr != nil {
						if !hasUnknownToolOutcome(commitErr) && callIndex+1 < len(resp.ToolCalls) && (errors.Is(commitErr, context.Canceled) || errors.Is(commitErr, context.DeadlineExceeded) || errors.Is(commitErr, ErrRunCommitPreparedInvocationUnavailable) || errors.Is(commitErr, ErrRunCommitChildUnsupported) || errors.Is(commitErr, ErrRunCommitStreamingUnsupported)) {
							for _, undispatched := range resp.ToolCalls[callIndex+1:] {
								const skippedOutput = "Tool call not dispatched because a prior tool call stopped the group"
								if suffixErr := consumer.commitKnownCall(assistantOrdinal, assistantEntry.Message, "batch-"+newClaimID(), undispatched, uint64(len(transcript)), skippedOutput, ReceiptOutcomeInterrupted, &transcript, &messages); suffixErr != nil {
									commitErr = errors.Join(commitErr, suffixErr)
									break
								}
								sendToolResultEvent(skippedOutput, true, undispatched.ID, ReceiptOutcomeInterrupted, nil)
							}
						}
						span.SetStatus(tracing.SpanStatusError, commitErr.Error())
						sendEvent(errEvent(commitErr))
						return CanonicalRunResult{Events: events, Transcript: transcript}, commitErr
					}
					continue
				}
				resultText, isErr, execErr := l.executeTool(iterationCtx, toToolsCall(tc), es)
				isError := isErr
				if execErr != nil {
					logger.Warn("core.loop.tool_error", "tool", tc.Name, "err", execErr)
					resultText = "Error: " + execErr.Error()
					isError = true
				}
				outcome := toolReceiptOutcome(iterationCtx, execErr, isError)
				sendToolResultEvent(resultText, isError, tc.ID, outcome, nil)
				appendToolReceipt(&messages, &transcript, tc, resultText, outcome, consumer)
				if outcome == ReceiptOutcomeUnknown {
					for _, undispatched := range resp.ToolCalls[callIndex+1:] {
						skippedOutput := "Tool call not dispatched because a prior tool outcome is unknown"
						if spanCtx.Err() != nil {
							skippedOutput = "Tool call canceled because a prior tool outcome is unknown"
						}
						sendToolResultEvent(skippedOutput, true, undispatched.ID, ReceiptOutcomeInterrupted, nil)
						appendToolReceipt(&messages, &transcript, undispatched, skippedOutput, ReceiptOutcomeInterrupted, consumer)
					}
					unknownErr := markUnknownToolOutcome(execErr)
					span.SetStatus(tracing.SpanStatusError, unknownErr.Error())
					sendEvent(errEvent(unknownErr))
					return CanonicalRunResult{Events: events, Transcript: transcript}, unknownErr
				}
				if execErr != nil && spanCtx.Err() != nil {
					span.SetStatus(tracing.SpanStatusError, execErr.Error())
					emitCanceledToolResults(execErr)
					sendEvent(errEvent(execErr))
					return CanonicalRunResult{Events: events, Transcript: transcript}, execErr
				}
				if callIndex+1 < len(resp.ToolCalls) && spanCtx.Err() != nil {
					cancelErr := spanCtx.Err()
					emitCanceledToolResults(cancelErr)
					sendEvent(errEvent(cancelErr))
					return CanonicalRunResult{Events: events, Transcript: transcript}, cancelErr
				}
			}
		}

	}

	// Iteration budget exhausted.
	span.SetStatus(tracing.SpanStatusError, errMaxIterations.Error())
	logger.Error("core.loop.max_iterations", "max", l.maxIterations)
	sendEvent(errEvent(errMaxIterations))
	return CanonicalRunResult{Events: events, Transcript: transcript}, errMaxIterations
}

func preflightDurableRequest(
	ctx context.Context,
	messages []llm.Message,
	consumer *runCommitConsumer,
	compactor compaction.Compactor,
	estimator compaction.TokenEstimator,
	maxTokens int,
) ([]llm.Message, error) {
	items := messagesToTurnItems(messages)
	if err := validateDurableRequestSource(items, consumer.source, consumer.runID); err != nil {
		return nil, err
	}
	if maxTokens <= 0 || estimator == nil || compaction.EstimateTokens(items, estimator) <= maxTokens {
		return messages, nil
	}
	if err := compaction.ValidateCompactionEligible(items); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDurableCompactionUnsupported, err)
	}
	if consumer.source == nil || compactor == nil {
		return nil, ErrRunCommitCompactionProjectionUnavailable
	}
	currentInput, err := captureDurableCurrentInput(items, consumer.source, consumer.runID)
	if err != nil {
		return nil, err
	}
	result, err := compactor.Compact(ctx, cloneCompactionTurnItems(items), maxTokens, estimator)
	if err != nil {
		return nil, err
	}
	projection, err := validateDurableCompactionResult(items, result, consumer.source, consumer.runID)
	if err != nil {
		return nil, err
	}
	if err := validateDurableCurrentInputPreserved(currentInput, result.Items); err != nil {
		return nil, err
	}
	if err := consumer.commitCompaction(projection); err != nil {
		markRunOutcomeUnknown(ctx)
		return nil, err
	}
	return turnItemsToMessages(result.Items), nil
}

func preflightNonDurableRequest(
	ctx context.Context,
	messages []llm.Message,
	currentSubmissionIndex int,
	midTurnCompactor *compaction.MidTurnCompact,
	compactor compaction.Compactor,
	estimator compaction.TokenEstimator,
	maxTokens int,
) ([]llm.Message, int, compaction.CompactResult, error) {
	if currentSubmissionIndex < 0 || currentSubmissionIndex >= len(messages) {
		return nil, currentSubmissionIndex, compaction.CompactResult{}, errCurrentSubmissionNotPreserved
	}
	items := messagesToTurnItems(messages)
	items, err := compaction.NormalizeToolCallGroups(items)
	if err != nil {
		return nil, currentSubmissionIndex, compaction.CompactResult{}, err
	}
	currentSubmissionID := items[currentSubmissionIndex].ID
	currentSubmission := cloneLLMMessage(messages[currentSubmissionIndex])
	compacted, result, err := midTurnCompactor.CompactIfNeeded(ctx, items, maxTokens, estimator, withIsolatedCompactorInput(compactor))
	if err != nil {
		return nil, currentSubmissionIndex, result, err
	}
	if !result.Triggered {
		return messages, currentSubmissionIndex, result, nil
	}
	compactedItems, err := compaction.NormalizeToolCallGroups(compacted.Items)
	if err != nil {
		return nil, currentSubmissionIndex, result, err
	}
	compactedSubmissionIndex := -1
	for index, item := range compactedItems {
		if item.ID != currentSubmissionID {
			continue
		}
		if compactedSubmissionIndex >= 0 {
			return nil, currentSubmissionIndex, result, errCurrentSubmissionNotPreserved
		}
		compactedSubmissionIndex = index
	}
	if compactedSubmissionIndex < 0 {
		return nil, currentSubmissionIndex, result, errCurrentSubmissionNotPreserved
	}
	compactedMessages := turnItemsToMessages(compactedItems)
	if !reflect.DeepEqual(currentSubmission, compactedMessages[compactedSubmissionIndex]) {
		return nil, currentSubmissionIndex, result, errCurrentSubmissionNotPreserved
	}
	return compactedMessages, compactedSubmissionIndex, result, nil
}

type pendingInput struct {
	history []AgentMessage
	message AgentMessage
}

func pendingRunInput(history []AgentMessage, content string, consumer *runCommitConsumer) pendingInput {
	history = cloneAgentMessages(history)
	priorCount := uint64(0)
	if consumer.source != nil {
		priorCount = uint64(len(consumer.source.SourceEntryIDs))
	}
	runID := consumer.runID
	index := -1
	if len(history) > 0 {
		last := history[len(history)-1]
		if last.Role == string(llm.RoleUser) && last.Content == content {
			switch last.Runtime.Source.Kind {
			case llm.SourcePendingRunInput:
				index = len(history) - 1
			case llm.SourceNone:
				if last.InputKind == InputKindUser {
					index = len(history) - 1
				}
			}
		}
	}
	if index < 0 {
		message := AgentMessage{Role: string(llm.RoleUser), Content: content, InputKind: InputKindUser}
		history = append(history, message)
		index = len(history) - 1
	}
	message := history[index]
	message.Runtime.Source = llm.SourceRef{
		Kind: llm.SourcePendingRunInput, RunID: runID, PriorSourceCount: priorCount,
	}
	message.Runtime.ReceiptOutcome = message.ReceiptOutcome
	history[index] = message
	return pendingInput{history: history, message: message}
}

func transcriptAssistantEntry(resp *llm.Message) (TranscriptEntry, error) {
	entry := AgentMessage{Role: string(llm.RoleAssistant), Content: resp.Content}
	if resp.Usage != nil {
		usage := *resp.Usage
		entry.Usage = &usage
	}
	if resp.ContentBlocks != nil {
		entry.ContentBlocks = append([]llm.ContentBlock(nil), resp.ContentBlocks...)
		for i := range entry.ContentBlocks {
			if resp.ContentBlocks[i].ImageURL != nil {
				imageURL := *resp.ContentBlocks[i].ImageURL
				entry.ContentBlocks[i].ImageURL = &imageURL
			}
		}
	}
	if len(resp.ToolCalls) > 0 {
		encoded, err := json.Marshal(resp.ToolCalls)
		if err != nil {
			return TranscriptEntry{}, fmt.Errorf("core: clone assistant tool calls for transcript: %w", err)
		}
		if err := json.Unmarshal(encoded, &entry.ToolCalls); err != nil {
			return TranscriptEntry{}, fmt.Errorf("core: decode cloned assistant tool calls: %w", err)
		}
	}
	return TranscriptEntry{Message: entry}, nil
}

func validateCompleteToolCalls(calls []llm.ToolCall) error {
	if len(calls) > guardedStreamMaxToolCalls {
		return fmt.Errorf("core: response contains more than %d tool calls", guardedStreamMaxToolCalls)
	}
	seen := make(map[string]struct{}, len(calls))
	var payloadBytes int
	for _, call := range calls {
		if call.ID == "" || call.Name == "" {
			return errors.New("core: tool call is missing its ID or name")
		}
		if _, exists := seen[call.ID]; exists {
			return fmt.Errorf("core: response contains duplicate tool call ID %q", call.ID)
		}
		seen[call.ID] = struct{}{}
		if call.Args != nil {
			if _, ok := call.Args.(map[string]any); !ok {
				return fmt.Errorf("core: tool call %q has invalid arguments", call.ID)
			}
		}
		encoded, err := json.Marshal(call)
		if err != nil {
			return fmt.Errorf("core: encode tool call %q: %w", call.ID, err)
		}
		if len(encoded) > guardedStreamMaxBytes-payloadBytes {
			return fmt.Errorf("core: tool calls exceed %d-byte payload limit", guardedStreamMaxBytes)
		}
		payloadBytes += len(encoded)
	}
	return nil
}

func appendToolReceipt(messages *[]llm.Message, transcript *[]TranscriptEntry, call llm.ToolCall, content, outcome string, consumer *runCommitConsumer) {
	metadata := consumer.transcriptMetadata(uint64(len(*transcript)), outcome)
	*messages = append(*messages, llm.Message{
		Role:       llm.RoleTool,
		ToolCallID: call.ID,
		Name:       call.Name,
		Content:    content,
		Runtime:    metadata,
	})
	appendTranscriptToolReceipt(transcript, call, content, outcome, consumer)
}

func appendTranscriptToolReceipt(transcript *[]TranscriptEntry, call llm.ToolCall, content, outcome string, consumer *runCommitConsumer) {
	*transcript = append(*transcript, TranscriptEntry{Message: AgentMessage{
		Role:           string(llm.RoleTool),
		Content:        content,
		ToolCallID:     call.ID,
		ToolName:       call.Name,
		ReceiptOutcome: outcome,
		Runtime:        consumer.transcriptMetadata(uint64(len(*transcript)), outcome),
	}})
}

func toolReceiptOutcome(ctx context.Context, err error, isError bool) string {
	if err != nil {
		if hasUnknownToolOutcome(err) {
			return ReceiptOutcomeUnknown
		}
		var outcome interface{ OutcomeUnknown() bool }
		if errors.As(err, &outcome) {
			if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return ReceiptOutcomeInterrupted
			}
		}
		if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return ReceiptOutcomeUnknown
		}
		return ReceiptOutcomeError
	}
	if isError {
		return ReceiptOutcomeError
	}
	return ReceiptOutcomeSuccess
}

func hasUnknownToolOutcome(err error) bool {
	var outcome interface{ OutcomeUnknown() bool }
	return errors.As(err, &outcome) && outcome.OutcomeUnknown()
}

// messagesToTurnItems converts []llm.Message to []compaction.TurnItem for the
// mid-turn compaction pipeline. Tool-result messages are mapped to the
// TurnItem.ToolResult field so compactors (e.g. MicroCompactor) can replace
// them with placeholders.
func messagesToTurnItems(msgs []llm.Message) []compaction.TurnItem {
	canonical := make([]AgentMessage, len(msgs))
	finishReasons := make([]string, len(msgs))
	for i, message := range msgs {
		canonical[i] = AgentMessage{
			Role:           string(message.Role),
			Content:        message.Content,
			ContentBlocks:  message.ContentBlocks,
			Usage:          message.Usage,
			ToolCalls:      message.ToolCalls,
			ToolCallID:     message.ToolCallID,
			ToolName:       message.Name,
			ReceiptOutcome: message.Runtime.ReceiptOutcome,
			Runtime:        message.Runtime,
		}
		finishReasons[i] = message.FinishReason
	}
	return agentMessagesToTurnItemsWithFinish(canonical, finishReasons)
}

func cloneLLMMessage(message llm.Message) llm.Message {
	cloned := cloneAgentMessage(AgentMessage{
		Role: string(message.Role), Content: message.Content, ContentBlocks: message.ContentBlocks,
		Usage: message.Usage, ToolCalls: message.ToolCalls, ToolCallID: message.ToolCallID,
		ToolName: message.Name, Runtime: message.Runtime,
	})
	message.ContentBlocks = cloned.ContentBlocks
	message.Usage = cloned.Usage
	message.ToolCalls = cloned.ToolCalls
	return message
}

// agentMessagesToTurnItems retains source metadata while preparing the
// canonical Agent history for a durable request preflight.
func agentMessagesToTurnItems(messages []AgentMessage) []compaction.TurnItem {
	return agentMessagesToTurnItemsWithFinish(messages, nil)
}

func agentMessagesToTurnItemsWithFinish(messages []AgentMessage, finishReasons []string) []compaction.TurnItem {
	canonical := cloneAgentMessages(messages)
	items := make([]compaction.TurnItem, len(canonical))
	for i, message := range canonical {
		item := compaction.TurnItem{
			ID:             fmt.Sprintf("msg-%d", i),
			Role:           string(message.Role),
			ContentBlocks:  message.ContentBlocks,
			Usage:          message.Usage,
			ToolCalls:      message.ToolCalls,
			ToolCallID:     message.ToolCallID,
			ToolName:       message.ToolName,
			ReceiptOutcome: message.ReceiptOutcome,
			Runtime:        message.Runtime,
		}
		if i < len(finishReasons) {
			item.FinishReason = finishReasons[i]
		}
		if message.Role == compaction.RoleTool {
			item.ToolResult = message.Content
		} else {
			item.Content = message.Content
		}
		if message.Role == compaction.RoleUser {
			sourceKind := message.Runtime.Source.Kind
			if sourceKind == llm.SourceRunSummary {
				item.IsCompaction = true
			} else if sourceKind == llm.SourcePriorPublished {
				if raw, ok := compaction.UnwrapUntrustedSummaryContext(item.Content); ok {
					item.Content = raw
					item.IsCompaction = true
				}
			}
			if item.IsCompaction {
				if raw, ok := compaction.UnwrapUntrustedSummaryContext(item.Content); ok {
					item.Content = raw
				}
			}
		}
		items[i] = item
	}
	return items
}

// turnItemsToMessages converts compacted items back to model history. Summary
// markers are projected as untrusted user content exactly once; runtime source
// coordinates remain private to this invocation and are not provider JSON.
func turnItemsToMessages(items []compaction.TurnItem) []llm.Message {
	messages := make([]llm.Message, len(items))
	for i, item := range items {
		message := llm.Message{
			Role:          llm.Role(item.Role),
			ContentBlocks: item.ContentBlocks,
			ToolCalls:     item.ToolCalls,
			ToolCallID:    item.ToolCallID,
			Name:          item.ToolName,
			Usage:         item.Usage,
			FinishReason:  item.FinishReason,
			Runtime:       item.Runtime,
		}
		if message.Runtime.ReceiptOutcome == "" {
			message.Runtime.ReceiptOutcome = item.ReceiptOutcome
		}
		if item.IsCompaction {
			content := item.Content
			if content == "" {
				content = "[compacted]"
			}
			if raw, ok := compaction.UnwrapUntrustedSummaryContext(content); ok {
				content = raw
			}
			message.Role = llm.RoleUser
			message.Content = compaction.UntrustedSummaryContext(content)
			message.ContentBlocks = nil
			message.ToolCalls = nil
			message.ToolCallID = ""
			message.Name = ""
			message.Usage = nil
			message.FinishReason = ""
		} else if item.Role == compaction.RoleTool {
			message.Content = item.ToolResult
		} else {
			message.Content = item.Content
		}
		messages[i] = message
	}
	return messages
}

// streamGenerate calls the LLM in streaming mode. It consumes chunks from
// model.Stream() in real time, emitting incremental "message" events via the
// EventStream so the TUI can render tokens as they arrive. The complete
// response (with accumulated tool calls) is returned.
func (l *LoopAgent) streamGenerate(ctx context.Context, model llm.BaseChatModel, messages []llm.Message, toolOpts []llm.Option, es EventStream) (*llm.Message, error) {
	return l.streamGenerateWithBudget(ctx, model, messages, toolOpts, es, &guardedStreamBudget{}, true)
}

func (l *LoopAgent) streamGenerateWithBudget(ctx context.Context, model llm.BaseChatModel, messages []llm.Message, toolOpts []llm.Option, es EventStream, budget *guardedStreamBudget, publishIncremental bool) (*llm.Message, error) {
	streamCtx, cancel := context.WithCancelCause(ctx)
	ch, err := model.Stream(streamCtx, messages, toolOpts...)
	if err != nil {
		cause := context.Cause(ctx)
		if cause == nil {
			cancel(llm.NewLocalStreamAbortCause(err))
		} else {
			cancel(cause)
		}
		if ch != nil {
			for range ch {
			}
		}
		// Stream failed — propagate the error directly rather than falling
		// back to Generate, because Generate would advance the model's
		// call index a second time and return a different result.
		return nil, err
	}
	if ch == nil {
		err = fmt.Errorf("streamGenerate: nil channel from model")
		cancel(llm.NewLocalStreamAbortCause(err))
		return nil, err
	}
	abortAndDrain := func(failure error) {
		cause := context.Cause(ctx)
		if cause == nil {
			cancel(llm.NewLocalStreamAbortCause(failure))
		} else {
			cancel(cause)
		}
		for range ch {
		}
	}

	var contentBuf strings.Builder
	var toolCalls []llm.ToolCall
	var finishReason string
	var usage *llm.Usage
	finalSeen := false

	for {
		var chunk llm.MessageChunk
		var ok bool
		select {
		case <-streamCtx.Done():
			cause := context.Cause(ctx)
			if cause == nil {
				cause = context.Cause(streamCtx)
			}
			if cause == nil {
				cause = streamCtx.Err()
			}
			abortAndDrain(cause)
			return nil, fmt.Errorf("core: stream canceled: %w", cause)
		case chunk, ok = <-ch:
		}
		if !ok {
			break
		}
		if chunk.Error != nil {
			abortAndDrain(chunk.Error)
			return nil, chunk.Error
		}
		if ctx.Err() != nil {
			cause := context.Cause(ctx)
			if cause == nil {
				cause = ctx.Err()
			}
			abortAndDrain(cause)
			return nil, fmt.Errorf("core: stream canceled: %w", cause)
		}
		if finalSeen {
			err = errors.New("core: stream emitted data after its final chunk")
			abortAndDrain(err)
			return nil, err
		}
		if budget != nil {
			if err := budget.accountChunk(chunk); err != nil {
				abortAndDrain(err)
				return nil, err
			}
		}
		if !chunk.Final && len(chunk.ToolCalls) > 0 {
			err = errors.New("core: stream emitted tool calls before the final chunk")
			abortAndDrain(err)
			return nil, err
		}
		if chunk.Content != "" {
			contentBuf.WriteString(chunk.Content)
			// Send incremental events only to the EventStream (real-time
			// streaming for TUI consumers), not to the returned events slice.
			// The complete non-incremental "message" event is emitted by the
			// loop once the response finishes.
			if publishIncremental && es != nil {
				if budget != nil {
					budget.visible = true
				}
				_ = es.Send(stampAgentEvent(agentEventCoordinatesFromContext(ctx), AgentEvent{ //nolint:errcheck
					Kind:        "message",
					Content:     chunk.Content,
					Timestamp:   time.Now(),
					Incremental: true,
				}))
			}
		}
		if chunk.Final {
			finalSeen = true
			if len(chunk.ToolCalls) > 0 {
				toolCalls = chunk.ToolCalls
			}
			if chunk.FinishReason != "" {
				finishReason = chunk.FinishReason
			}
		}
		// Track the last non-nil Usage so it propagates to the returned
		// Message. Providers set Usage on the Final chunk.
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
	}

	if ctx.Err() != nil {
		cause := context.Cause(ctx)
		if cause == nil {
			cause = ctx.Err()
		}
		abortAndDrain(cause)
		return nil, fmt.Errorf("core: stream canceled: %w", cause)
	}
	if !finalSeen {
		err = errors.New("core: stream ended without a final chunk")
		abortAndDrain(err)
		return nil, err
	}

	response := &llm.Message{
		Role:         llm.RoleAssistant,
		Content:      contentBuf.String(),
		ToolCalls:    toolCalls,
		FinishReason: finishReason,
		Usage:        usage,
	}
	if budget != nil {
		candidateBytes, sizeErr := candidatePayloadBytes(response)
		if sizeErr != nil {
			cancel(llm.NewLocalStreamAbortCause(sizeErr))
			return nil, sizeErr
		}
		if candidateBytes > guardedStreamMaxBytes {
			err = fmt.Errorf("core: response segment exceeds %d-byte candidate limit", guardedStreamMaxBytes)
			cancel(llm.NewLocalStreamAbortCause(err))
			return nil, err
		}
	}
	cancel(nil)
	return response, nil
}

// generateWithContinuation wraps streamGenerate with automatic truncation
// detection and continuation. When the model's finish_reason is "length"
// (output cut off by max_tokens), the partial assistant response is appended
// to the conversation and the model is asked to continue. The continuation
// content is merged into the original response. Tool calls from a truncated
// response are partial (JSON args cut off) and are dropped before execution;
// only tool calls from a non-truncated (finish_reason != "length") response
// are kept. At most maxContinuationAttempts retries are issued; if the output
// is still truncated after that, the candidate is rejected and no complete
// assistant turn is returned.
func (l *LoopAgent) generateWithContinuation(ctx context.Context, model llm.BaseChatModel, messages []llm.Message, toolOpts []llm.Option, es EventStream, logger *slog.Logger) (*llm.Message, error) {
	return l.generateWithContinuationAndCompaction(ctx, model, messages, toolOpts, es, logger, nil, nil, nil, 0)
}

func (l *LoopAgent) generateWithContinuationAndCompaction(
	ctx context.Context,
	model llm.BaseChatModel,
	messages []llm.Message,
	toolOpts []llm.Option,
	es EventStream,
	logger *slog.Logger,
	consumer *runCommitConsumer,
	compactor compaction.Compactor,
	estimator compaction.TokenEstimator,
	maxTokens int,
) (*llm.Message, error) {
	guard, guarded := model.(CompleteResponseGuard)
	if guarded {
		if guard.MaxCandidateBytes() != guardedStreamMaxBytes {
			return nil, fmt.Errorf("core: complete-response guard must advertise the %d-byte candidate limit", guardedStreamMaxBytes)
		}
		ctx = llm.WithLoopOwnedContinuation(ctx)
	}
	mainCtx, operationActive, operationErr := providerOperationContext(ctx, model, llm.OperationMainRequest)
	if operationErr != nil {
		return nil, operationErr
	}
	resp, messages, budget, err := l.runMainProviderOperation(
		mainCtx, model, messages, toolOpts, es, guarded, operationActive,
		consumer, compactor, estimator, maxTokens,
	)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, errors.New("core: provider operation returned a nil response")
	}

	// Lazily create a continuation span only when the response is truncated.
	// The span is a child of the loop.run span (ctx carries it). When tracing
	// is disabled, SpanFromContext returns a noop span with zero overhead.
	var contSpan tracing.TraceSpan
	var droppedCount int
	var attemptCount int
	if resp.FinishReason == "length" {
		contSpan, _ = tracing.SpanFromContext(ctx, "loop.continuation", tracing.SpanKindInternal)
	}
	defer func() {
		if contSpan == nil {
			return
		}
		contSpan.SetAttributes(
			tracing.Attribute{Key: "attempts", Value: attemptCount},
			tracing.Attribute{Key: "partial_tool_calls_dropped", Value: droppedCount},
			tracing.Attribute{Key: "finish_reason", Value: resp.FinishReason},
		)
		if resp.FinishReason == "length" {
			contSpan.SetStatus(tracing.SpanStatusError,
				"max continuation attempts exceeded with truncated output")
		}
		contSpan.End()
	}()

	for attempt := 0; attempt < maxContinuationAttempts && resp.FinishReason == "length"; attempt++ {
		attemptCount = attempt + 1

		// When the response is truncated by max_tokens, any tool calls in it
		// are partial (JSON args cut off, fields missing) and must not be
		// executed. Drop them before building the continuation request.
		if resp.FinishReason == "length" && len(resp.ToolCalls) > 0 {
			logger.Warn("partial_tool_calls_dropped", "count", len(resp.ToolCalls))
			if contSpan != nil {
				contSpan.AddEvent("partial_tool_calls_dropped",
					tracing.Attribute{Key: "count", Value: len(resp.ToolCalls)})
			}
			droppedCount += len(resp.ToolCalls)
			resp.ToolCalls = nil
		}

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

		contCtx := llm.WithContinuationNoReplay(ctx)
		contCtx, _, operationErr = providerOperationContext(contCtx, model, llm.OperationContinuation)
		if operationErr != nil {
			return nil, operationErr
		}
		contResp, contErr := l.streamGenerateWithBudget(contCtx, model, contMsgs, toolOpts, es, budget, !guarded)
		if contErr != nil {
			return nil, fmt.Errorf("core: continuation %d failed: %w", attempt+1, contErr)
		}
		if contResp == nil {
			return nil, fmt.Errorf("core: continuation %d returned a nil response", attempt+1)
		}

		// Merge the continuation into the original response. Content is
		// concatenated; tool calls are replaced (not appended) because any
		// previous tool calls were partial and have been dropped.
		resp.Content += contResp.Content
		resp.FinishReason = contResp.FinishReason
		resp.ToolCalls = contResp.ToolCalls
		if contResp.Usage != nil {
			if resp.Usage == nil {
				resp.Usage = contResp.Usage
			} else {
				resp.Usage.InputTokens += contResp.Usage.InputTokens
				resp.Usage.OutputTokens += contResp.Usage.OutputTokens
				resp.Usage.TotalTokens += contResp.Usage.TotalTokens
			}
		}
		candidateBytes, candidateErr := candidatePayloadBytes(resp)
		if candidateErr != nil {
			return nil, candidateErr
		}
		if candidateBytes > guardedStreamMaxBytes {
			return nil, fmt.Errorf("core: complete response exceeds %d-byte candidate limit", guardedStreamMaxBytes)
		}
	}

	// A still-truncated response is not a complete assistant turn and must not
	// be exposed as a successful answer or dispatched as a tool call.
	if resp.FinishReason == "length" {
		slog.WarnContext(ctx, "core.loop.truncation_max_attempts",
			"max_attempts", maxContinuationAttempts)
		return nil, fmt.Errorf("core: response remains truncated after %d continuation attempts", maxContinuationAttempts)
	}

	candidateBytes, candidateErr := candidatePayloadBytes(resp)
	if candidateErr != nil {
		return nil, candidateErr
	}
	if candidateBytes > guardedStreamMaxBytes {
		return nil, fmt.Errorf("core: complete response exceeds %d-byte candidate limit", guardedStreamMaxBytes)
	}
	if guarded {
		if err := validateCompleteToolCalls(resp.ToolCalls); err != nil {
			return nil, fmt.Errorf("core: reject invalid guarded tool candidate: %w", err)
		}
		checked, checkErr := guard.CheckCandidate(ctx, resp.Content)
		if checkErr != nil {
			return nil, fmt.Errorf("core: check complete response: %w", checkErr)
		}
		if checked == nil {
			return nil, errors.New("core: complete-response guard returned nil result")
		}
		if checked.Sanitized != "" {
			resp.Content = checked.Sanitized
		} else if !checked.Allowed {
			return nil, errors.New("core: complete-response guard denied candidate")
		}
		if len(resp.Content) > guard.MaxCandidateBytes() {
			return nil, fmt.Errorf("core: guarded response exceeds %d-byte candidate limit", guard.MaxCandidateBytes())
		}
		candidateBytes, candidateErr = candidatePayloadBytes(resp)
		if candidateErr != nil {
			return nil, candidateErr
		}
		if candidateBytes > guard.MaxCandidateBytes() {
			return nil, fmt.Errorf("core: guarded response payload exceeds %d-byte candidate limit", guard.MaxCandidateBytes())
		}
		if es != nil && resp.Content != "" {
			budget.visible = true
			_ = es.Send(stampAgentEvent(agentEventCoordinatesFromContext(ctx), AgentEvent{Kind: "message", Content: resp.Content, Timestamp: time.Now(), Incremental: true})) //nolint:errcheck
		}
	}

	return resp, nil
}

func (l *LoopAgent) runMainProviderOperation(
	ctx context.Context,
	model llm.BaseChatModel,
	messages []llm.Message,
	toolOpts []llm.Option,
	es EventStream,
	guarded bool,
	operationActive bool,
	consumer *runCommitConsumer,
	compactor compaction.Compactor,
	estimator compaction.TokenEstimator,
	maxTokens int,
) (*llm.Message, []llm.Message, *guardedStreamBudget, error) {
	currentMessages := messages
	var response *llm.Message
	var successfulBudget *guardedStreamBudget
	runSchedule := func(operationCtx context.Context, dispatchModel llm.BaseChatModel) error {
		for {
			attemptBudget := &guardedStreamBudget{}
			candidate, callErr := l.streamGenerateWithBudget(operationCtx, dispatchModel, currentMessages, toolOpts, es, attemptBudget, !guarded)
			if callErr == nil {
				if candidate == nil {
					return errors.New("core: provider returned a nil response")
				}
				response = candidate
				successfulBudget = attemptBudget
				return nil
			}

			recovered, compactedMessages, recoveryBudget, recoveredOK, recoveryErr := l.recoverOverflowAttempt(
				operationCtx, dispatchModel, currentMessages, toolOpts, es, attemptBudget, guarded, operationActive,
				consumer, compactor, estimator, maxTokens, callErr,
			)
			if recoveryErr != nil {
				return errors.Join(callErr, recoveryErr)
			}
			if recoveredOK {
				if recovered == nil {
					return errors.New("core: overflow recovery returned a nil response")
				}
				response = recovered
				currentMessages = compactedMessages
				successfulBudget = recoveryBudget
				return nil
			}
			if !operationActive || attemptBudget.visible || llm.ContinuationNoReplay(operationCtx) {
				return callErr
			}

			authority, ok := llm.OperationDispatchAuthorityFromContext(operationCtx)
			if !ok {
				return callErr
			}
			owner, ok := authority.(*providerOperationAuthority)
			if !ok {
				return callErr
			}
			proposal, backoff, planned := owner.nextCandidateProposal(operationCtx, callErr)
			if !planned {
				return callErr
			}
			if waitErr := waitProviderRetry(operationCtx, backoff); waitErr != nil {
				return errors.Join(callErr, waitErr)
			}
			operationCtx = llm.WithCandidateProposal(operationCtx, proposal)
		}
	}

	var runErr error
	if operationActive {
		if runner, ok := model.(llm.ProviderOperationRunner); ok {
			runErr = runner.RunProviderOperation(ctx, runSchedule)
		} else {
			runErr = runSchedule(ctx, model)
		}
	} else {
		runErr = runSchedule(ctx, model)
	}
	if runErr != nil {
		return nil, currentMessages, nil, runErr
	}
	return response, currentMessages, successfulBudget, nil
}

func waitProviderRetry(ctx context.Context, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (l *LoopAgent) recoverOverflowAttempt(
	ctx context.Context,
	model llm.BaseChatModel,
	messages []llm.Message,
	toolOpts []llm.Option,
	es EventStream,
	failedBudget *guardedStreamBudget,
	guarded bool,
	operationActive bool,
	consumer *runCommitConsumer,
	compactor compaction.Compactor,
	estimator compaction.TokenEstimator,
	maxTokens int,
	callErr error,
) (*llm.Message, []llm.Message, *guardedStreamBudget, bool, error) {
	if !guarded || !operationActive || failedBudget == nil || failedBudget.visible || failedBudget.bytes != 0 || failedBudget.chunks != 0 {
		return nil, nil, nil, false, nil
	}
	attempt, finished := llm.FinishedProviderAttemptFromError(callErr)
	if !finished || attempt.Failure != llm.FailureOverflow || attempt.AttemptID == "" {
		return nil, nil, nil, false, nil
	}
	authority, ok := llm.OperationDispatchAuthorityFromContext(ctx)
	if !ok {
		return nil, nil, nil, false, nil
	}
	recoveryOwner, ok := authority.(llm.OverflowRecoveryAuthority)
	if !ok {
		return nil, nil, nil, false, nil
	}
	var candidate llm.ResolvedCandidateHandle
	for _, resolved := range llm.ProviderCandidatesFromModel(model) {
		if resolved != nil && resolved.CandidateID() == attempt.Identity.Candidate && resolved.Identity() == attempt.Identity {
			candidate = resolved
			break
		}
	}
	if candidate == nil {
		return nil, nil, nil, false, nil
	}
	if err := recoveryOwner.BeginOverflowRecovery(ctx, attempt.AttemptID); err != nil {
		return nil, nil, nil, false, nil
	}

	compacted, err := compactOverflowRequest(ctx, messages, consumer, compactor, estimator, maxTokens)
	if err != nil {
		return nil, nil, nil, false, err
	}
	proposal := llm.CandidateProposal{
		OperationID: authority.ID(),
		Candidate:   candidate,
		Reason:      llm.CandidateOverflow,
		Antecedent: llm.CandidateAntecedent{
			Kind:      llm.AntecedentFinishedOverflow,
			AttemptID: attempt.AttemptID,
		},
	}
	owner, ok := authority.(*providerOperationAuthority)
	if !ok {
		return nil, nil, nil, false, errors.New("core: overflow recovery requires the Loop-owned dispatch authority")
	}
	if err := owner.planCandidateProposal(proposal); err != nil {
		return nil, nil, nil, false, err
	}
	recoveryCtx := llm.WithCandidateProposal(ctx, proposal)
	recoveryBudget := &guardedStreamBudget{}
	response, err := l.streamGenerateWithBudget(recoveryCtx, model, compacted, toolOpts, es, recoveryBudget, false)
	if err != nil {
		return nil, nil, nil, false, err
	}
	if response == nil {
		return nil, nil, nil, false, errors.New("core: overflow recovery returned a nil response")
	}
	return response, compacted, recoveryBudget, true, nil
}

func compactOverflowRequest(
	ctx context.Context,
	messages []llm.Message,
	consumer *runCommitConsumer,
	compactor compaction.Compactor,
	estimator compaction.TokenEstimator,
	maxTokens int,
) ([]llm.Message, error) {
	if compactor == nil || estimator == nil || maxTokens <= 0 {
		return nil, errors.New("core: overflow recovery requires a configured compactor, estimator, and token budget")
	}
	items := messagesToTurnItems(messages)
	if consumer != nil {
		if consumer.source == nil {
			return nil, ErrRunCommitCompactionProjectionUnavailable
		}
		if err := validateDurableRequestSource(items, consumer.source, consumer.runID); err != nil {
			return nil, err
		}
	}
	if err := compaction.ValidateCompactionEligible(items); err != nil {
		return nil, fmt.Errorf("core: overflow recovery cannot compact this request: %w", err)
	}
	var currentInput compaction.TurnItem
	if consumer != nil {
		var err error
		currentInput, err = captureDurableCurrentInput(items, consumer.source, consumer.runID)
		if err != nil {
			return nil, err
		}
	}
	result, err := compactor.Compact(ctx, cloneCompactionTurnItems(items), maxTokens, estimator)
	if err != nil {
		return nil, fmt.Errorf("core: compact overflow request: %w", err)
	}
	if reflect.DeepEqual(items, result.Items) {
		return nil, errors.New("core: overflow compactor made no progress")
	}
	if consumer != nil {
		projection, err := validateDurableCompactionResult(items, result, consumer.source, consumer.runID)
		if err != nil {
			return nil, err
		}
		if err := validateDurableCurrentInputPreserved(currentInput, result.Items); err != nil {
			return nil, err
		}
		if err := consumer.commitCompaction(projection); err != nil {
			markRunOutcomeUnknown(ctx)
			return nil, err
		}
	}
	return turnItemsToMessages(result.Items), nil
}

func providerOperationContext(ctx context.Context, model llm.BaseChatModel, kind llm.ProviderOperationKind) (context.Context, bool, error) {
	candidates := llm.ProviderCandidatesFromModel(model)
	if len(candidates) == 0 {
		return ctx, false, nil
	}
	factory, ok := llm.RunOperationFactoryFromContext(ctx)
	if !ok {
		return nil, false, llm.ErrOperationAuthorityRequired
	}
	authority, err := factory.NewOperation(ctx, kind, defaultProviderOperationAttempts, candidates)
	if err != nil {
		return nil, false, fmt.Errorf("core: create provider operation: %w", err)
	}
	return llm.WithOperationDispatchAuthority(ctx, authority), true, nil
}

// eventStreamSink adapts a core.EventStream to satisfy tools.StreamSink.
// It bridges streaming tool output (stdout/stderr lines) into the same
// EventStream the loop uses for all other agent events. Each Send produces a
// "tool_output" AgentEvent carrying the ToolCallID and Stream ("stdout"/
// "stderr") so consumers can associate the line with its originating call.
type eventStreamSink struct {
	es          EventStream
	coordinates agentEventCoordinates
}

// Send wraps content as a "tool_output" AgentEvent and forwards it to the
// underlying EventStream.
func (s *eventStreamSink) Send(content, toolCallID, stream string) error {
	return s.es.Send(stampAgentEvent(s.coordinates, AgentEvent{
		Kind:       "tool_output",
		Content:    content,
		Timestamp:  time.Now(),
		ToolCallID: toolCallID,
		Stream:     stream,
	}))
}

// executeTool looks up the tool and runs its definition against the registry.
// When the tool implements tools.StreamingBashTool and an EventStream is
// provided, it uses ExecuteStreaming to push output lines in real time. The
// returned isError flag is propagated from ToolResult.IsError so callers can
// detect tool errors via a structured marker instead of string matching.
func (l *LoopAgent) executeTool(ctx context.Context, call tools.ToolCall, es EventStream) (string, bool, error) {
	return executeSingleTool(ctx, l.tools, call, es)
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
func drainSteerMessages(ch chan string, msgs *[]llm.Message, logger *slog.Logger) []string {
	var consumed []string
	for {
		select {
		case instruction, ok := <-ch:
			if !ok {
				return consumed
			}
			*msgs = append(*msgs, llm.Message{Role: llm.RoleUser, Content: instruction})
			consumed = append(consumed, instruction)
			logger.Info("core.loop.steer_injected", "instruction", instruction)
		default:
			return consumed
		}
	}
}

// drainFollowUpMessages non-blockingly drains all pending follow-up messages
// from ch and appends each as a user message to *msgs. This mirrors
// drainSteerMessages but is called after it so the model sees steering
// context first, then the follow-up.
func drainFollowUpMessages(ch chan string, msgs *[]llm.Message, logger *slog.Logger) []string {
	var consumed []string
	for {
		select {
		case content, ok := <-ch:
			if !ok {
				return consumed
			}
			*msgs = append(*msgs, llm.Message{Role: llm.RoleUser, Content: content})
			consumed = append(consumed, content)
			logger.Info("core.loop.followup_injected", "content_len", len(content))
		default:
			return consumed
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

// systemPromptFromDefs returns the system instruction that tells the model
// its role and encourages it to use tools when appropriate, using pre-fetched
// tool definitions so no additional List call is needed. When defs is empty,
// a minimal prompt is returned.
func systemPromptFromDefs(defs []tools.ToolDefinition) string {
	base := `You are a helpful AI assistant embedded in a developer CLI. When the user asks you to perform an action, you MUST use the available tools to accomplish it and persist until the task is fully complete.

Rules:
1. NEVER stop with a statement like "Let me install..." or "I need to..." and then do nothing. If you say you will do something, immediately call a tool to do it in the same turn.
2. When a tool call fails or returns an error, diagnose the cause and try an alternative approach. Do not give up after a single failure.
3. Keep iterating (call tools, observe results, adjust) until the user's request is fully satisfied. Only produce a final text answer with NO tool calls when the task is genuinely complete.
4. Do not guess or fabricate information when a tool can provide the answer.
5. If a skill tool is available and relevant, call it first to obtain expert instructions, then follow those instructions using other tools.`
	if len(defs) == 0 {
		return base
	}
	names := make([]string, 0, len(defs))
	for _, d := range defs {
		names = append(names, d.Name())
	}
	return base + "\n\nYou have access to these tools: " + strings.Join(names, ", ") + "."
}

// systemPrompt returns the system instruction that tells the model its role
// and encourages it to use tools when appropriate. When the tool registry is
// nil or empty, a minimal prompt is returned.
//
//nolint:unused
func systemPrompt(ctx context.Context, tr tools.ToolRegistry) string {
	if tr == nil {
		return systemPromptFromDefs(nil)
	}
	defs, err := tr.List(ctx)
	if err != nil {
		return systemPromptFromDefs(nil)
	}
	return systemPromptFromDefs(defs)
}
