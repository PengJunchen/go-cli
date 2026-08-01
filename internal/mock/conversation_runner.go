package mock

import (
	"context"
	"fmt"
	"time"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/tracing"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// llmCallOutcome records the outcome of a single model call driven by the
// runner.
type llmCallOutcome struct {
	ToolCalls int
	Err       error
}

// ConversationRunner replays a multi-turn user→LLM→tool interaction directly
// against a llm.BaseChatModel and a MockToolServer (no core.Harness required).
// It wraps every model call in an llm.request span nested under a
// cli.invocation span so trace assertions are meaningful even when the harness
// is absent.
type ConversationRunner struct {
	model         llm.BaseChatModel
	toolServer    *MockToolServer
	traceExporter *MockTraceExporter

	llmCalls []llmCallOutcome
}

// NewConversationRunner creates a runner driving the given model and tool
// server. traceExporter may be nil to disable span emission.
func NewConversationRunner(model llm.BaseChatModel, toolServer *MockToolServer, traceExporter *MockTraceExporter) *ConversationRunner {
	return &ConversationRunner{
		model:         model,
		toolServer:    toolServer,
		traceExporter: traceExporter,
	}
}

// Run replays each user message: for every message it calls model.Generate,
// feeding any returned tool calls through toolServer.Execute and re-calling the
// model with the tool results until the model returns a final non-tool
// response. It records every LLM outcome for assertions.
func (r *ConversationRunner) Run(ctx context.Context, msgs []string) error {
	r.llmCalls = nil

	var invocation tracing.TraceSpan
	var spanCtx = ctx
	if r.traceExporter != nil {
		tracer := tracing.NewTracer("conversation-run", r.traceExporter)
		invocation, spanCtx = tracer.Start(ctx, "cli.invocation", tracing.SpanKindInternal)
		invocation.SetAttributes(tracing.Attribute{Key: "user_messages", Value: len(msgs)})
		defer invocation.End()
	}

	conversation := make([]llm.Message, 0, len(msgs)*2)
	for _, msg := range msgs {
		conversation = append(conversation, llm.Message{Role: llm.RoleUser, Content: msg})

		for {
			var llmSpan tracing.TraceSpan
			modelCtx := spanCtx
			if r.traceExporter != nil {
				llmSpan, modelCtx = tracing.SpanFromContext(spanCtx, "llm.request", tracing.SpanKindClient)
				llmSpan.SetAttributes(tracing.Attribute{Key: "messages_count", Value: len(conversation)})
			}

			resp, err := r.model.Generate(modelCtx, conversation)
			r.llmCalls = append(r.llmCalls, llmCallOutcome{ToolCalls: toolCallCount(resp), Err: err})

			if llmSpan != nil {
				if err != nil {
					llmSpan.SetStatus(tracing.SpanStatusError, err.Error())
				} else {
					llmSpan.SetStatus(tracing.SpanStatusOK, "")
				}
				llmSpan.End()
			}

			if err != nil {
				return fmt.Errorf("model generate: %w", err)
			}

			conversation = append(conversation, *resp)

			if len(resp.ToolCalls) == 0 {
				break // final non-tool response for this user message
			}

			for _, tc := range resp.ToolCalls {
				result, terr := r.toolServer.Execute(modelCtx, tools.ToolCall{
					ID:   tc.ID,
					Name: tc.Name,
					Args: toArgMap(tc.Args),
				})
				content := ""
				if result != nil {
					content = result.Output
				}
				conversation = append(conversation, llm.Message{
					Role:       llm.RoleTool,
					ToolCallID: tc.ID,
					Name:       tc.Name,
					Content:    content,
				})
				if terr != nil {
					return fmt.Errorf("tool %q execute: %w", tc.Name, terr)
				}
			}
		}
	}

	if r.traceExporter != nil {
		invocation.SetStatus(tracing.SpanStatusOK, "")
		invocation.End() // idempotent; the deferred End is a no-op on success
		r.settleSpans()
	}
	return nil
}

// AssertToolCalled fails the test unless the named tool was called at least
// minCount times.
func (r *ConversationRunner) AssertToolCalled(t verify.TestingT, name string, minCount int) {
	t.Helper()
	count := 0
	for _, call := range r.toolServer.CallLog() {
		if call.ToolName == name {
			count++
		}
	}
	if count < minCount {
		t.Fatalf("assertion tool_called: expected %s at least %d times, got %d", name, minCount, count)
	}
}

// AssertNoLLMError fails the test if any model call driven by the runner
// returned an error.
func (r *ConversationRunner) AssertNoLLMError(t verify.TestingT) {
	t.Helper()
	for i, call := range r.llmCalls {
		if call.Err != nil {
			t.Fatalf("assertion no_error: LLM call %d returned error: %v", i, call.Err)
		}
	}
}

// AssertTraceComplete fails the test unless both a cli.invocation and at least
// one llm.request span were emitted. When the runner was constructed without a
// trace exporter the check is skipped.
func (r *ConversationRunner) AssertTraceComplete(t verify.TestingT) {
	t.Helper()
	if r.traceExporter == nil {
		t.Logf("assertion trace_complete: skipped (no trace exporter)")
		return
	}
	r.traceExporter.AssertSpanExists(t, "cli.invocation")
	r.traceExporter.AssertSpanExists(t, "llm.request")
}

// settleSpans waits until all spans created by Run have been asynchronously
// exported, so subsequent assertions are deterministic. Span export happens on
// a background goroutine inside the tracing package; polling is the only
// dependency-free way to wait for it.
func (r *ConversationRunner) settleSpans() {
	// The runner emits one span per LLM call plus the root cli.invocation
	// span. Wait until that many are collected.
	expected := 1 + len(r.llmCalls)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if r.traceExporter.SpanCount() >= expected {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// toArgMap coerces an LLM tool-call argument value into the map form expected
// by the tools contract. It returns nil when args is nil or not a map.
func toArgMap(args any) map[string]any {
	if m, ok := args.(map[string]any); ok {
		return m
	}
	return nil
}
