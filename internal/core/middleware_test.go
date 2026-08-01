package core

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/tracing"
)

// orderMiddleware records when its Wrap is called and wraps the next loop in an
// orderLoop that records execution order. It is used to verify onion ordering.
type orderMiddleware struct {
	name      string
	wrapOrder *[]string
	runOrder  *[]string
}

func (m *orderMiddleware) Name() string { return m.name }

func (m *orderMiddleware) Wrap(next AgentLoop) AgentLoop {
	if m.wrapOrder != nil {
		*m.wrapOrder = append(*m.wrapOrder, m.name)
	}
	if m.runOrder == nil {
		return next
	}
	return &orderLoop{name: m.name, next: next, runOrder: m.runOrder}
}

type orderLoop struct {
	name     string
	next     AgentLoop
	runOrder *[]string
}

func (l *orderLoop) Run(ctx context.Context, submission Submission) ([]AgentEvent, error) {
	*l.runOrder = append(*l.runOrder, l.name)
	if l.next == nil {
		return []AgentEvent{}, nil
	}
	return l.next.Run(ctx, submission)
}

func TestMiddlewareChainWrapOrderOnion(t *testing.T) {
	var wrapOrder []string
	m1 := &orderMiddleware{name: "m1", wrapOrder: &wrapOrder}
	m2 := &orderMiddleware{name: "m2", wrapOrder: &wrapOrder}
	m3 := &orderMiddleware{name: "m3", wrapOrder: &wrapOrder}

	base := &stubLoop{}
	chain := NewMiddlewareChain(m1, m2, m3)
	require.NotNil(t, chain.Wrap(base))

	// Wrap is applied last-to-first so the first middleware becomes outermost.
	assert.Equal(t, []string{"m3", "m2", "m1"}, wrapOrder, "wrap application order")
}

func TestMiddlewareChainRunOrderByMiddleware(t *testing.T) {
	var runOrder []string
	m1 := &orderMiddleware{name: "m1", runOrder: &runOrder}
	m2 := &orderMiddleware{name: "m2", runOrder: &runOrder}
	m3 := &orderMiddleware{name: "m3", runOrder: &runOrder}

	baseCount := 0
	base := countingLoop{run: func() { baseCount++ }}
	wrapped := NewMiddlewareChain(m1, m2, m3).Wrap(base)

	_, err := wrapped.Run(context.Background(), Submission{Content: "hi"})
	require.NoError(t, err)

	// Outermost (m1) runs first, then inner ones, then the base loop.
	assert.Equal(t, []string{"m1", "m2", "m3"}, runOrder, "run order is outermost-first")
	assert.Equal(t, 1, baseCount)
}

func TestMiddlewareChainEmptyReturnsBase(t *testing.T) {
	base := &stubLoop{}
	wrapped := NewMiddlewareChain().Wrap(base)
	require.NotNil(t, wrapped)
	_, err := wrapped.Run(context.Background(), Submission{Content: "hi"})
	require.NoError(t, err)
}

func TestModelMiddlewarePassThrough(t *testing.T) {
	mw := NewModelMiddlewareImpl("model-mw")
	assert.Equal(t, "model-mw", mw.Name())
	model := mock.NewMockLLMServer(nil)
	wrapped := mw.WrapModel(model)
	assert.Same(t, model, wrapped)
}

func TestToolMiddlewarePassThrough(t *testing.T) {
	mw := NewToolMiddlewareImpl("tool-mw")
	assert.Equal(t, "tool-mw", mw.Name())
	exec := func(context.Context, tools.ToolCall) (*tools.ToolResult, error) {
		return &tools.ToolResult{Output: "ok"}, nil
	}
	wrapped := mw.WrapToolCall(exec)
	res, err := wrapped(context.Background(), tools.ToolCall{Name: "bash"})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, "ok", res.Output)
}

func TestLoggingMiddlewareEmitsSpanAndPassesThrough(t *testing.T) {
	exporter := mock.NewMockTraceExporter()
	tracer := tracing.NewTracer("trace-mw", exporter)
	_, ctx := tracer.Start(context.Background(), "root", tracing.SpanKindInternal)

	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"MW-01", "logging",
		mock.ConversationTurn{AssistantContent: "hello"},
	))
	loop := NewLoopAgent(WithLLM(model))
	wrapped := NewLoggingMiddleware("audit").Wrap(loop)

	events, err := wrapped.Run(ctx, Submission{Content: "hey"})
	require.NoError(t, err)
	assert.Equal(t, 1, model.CallCount())
	messages := findEvents(events, "message")
	require.Len(t, messages, 1)
	assert.Equal(t, "hello", messages[0])

	assert.Eventually(t, func() bool {
		for _, span := range exporter.Spans() {
			if span.Name == "middleware.audit" {
				return true
			}
		}
		return false
	}, time.Second, 10*time.Millisecond, "middleware span should be exported")
}

func TestHookBackedMiddlewareComposesIntoChain(t *testing.T) {
	base := &stubLoop{}
	mw := &MiddlewareImpl{name: "trace"}
	wrapped := NewMiddlewareChain(mw).Wrap(base)
	require.NotNil(t, wrapped)
	_, err := wrapped.Run(context.Background(), Submission{Content: "hi"})
	require.NoError(t, err)
}

// stubLoop returns a single message event immediately.
type stubLoop struct{}

func (s *stubLoop) Run(context.Context, Submission) ([]AgentEvent, error) {
	return []AgentEvent{{Kind: "message", Content: "ok", Timestamp: time.Now()}}, nil
}

// countingLoop runs a callback each time it executes.
type countingLoop struct{ run func() }

func (c countingLoop) Run(context.Context, Submission) ([]AgentEvent, error) {
	if c.run != nil {
		c.run()
	}
	return []AgentEvent{}, nil
}
