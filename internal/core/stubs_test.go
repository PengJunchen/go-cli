package core

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tracing"
)

func TestStubZeroValues(t *testing.T) {
	ctx := context.Background()

	turnRes, err := (EinoTurnRunner{}).RunTurn(ctx, Submission{Content: "step"})
	assert.NoError(t, err)
	assert.True(t, turnRes.Success)

	est := (HeuristicTokenEstimator{}).Estimate("0123456789")
	assert.Equal(t, 2, est)

	store := InMemoryApprovalStore{}
	assert.NoError(t, store.Remember(ctx, "bash", true))
	assert.False(t, store.IsAllowed(ctx, "bash"))

	planes, err := (ContextManagerImpl{}).Build(ctx, "s1")
	assert.NoError(t, err)
	assert.Empty(t, planes)

	compacted, err := (UnifiedCompactor{}).Compact(ctx, []AgentMessage{{Role: "user", Content: "x"}}, 10)
	assert.NoError(t, err)
	assert.Len(t, compacted, 1)

	leaves, err := (SessionTreeImpl{}).Leaves(ctx, "root")
	assert.NoError(t, err)
	assert.Empty(t, leaves)

	sess, err := (SessionStoreImpl{}).Load(ctx, "s1")
	assert.NoError(t, err)
	assert.Equal(t, "", sess.ID)
}

func TestStubSlogCallsDoNotPanic(t *testing.T) {
	ctx := context.Background()

	assert.NotPanics(t, func() {
		_, err := (DefaultToolRegistry{}).List(ctx)
		assert.NoError(t, err)
		_, _, err = (DefaultModelProvider{}).Build(ctx, llm.ModelConfig{})
		assert.Error(t, err)
		err = (NoopTraceExporter{}).ExportSpan(ctx, &noopSpanStub{})
		assert.NoError(t, err)
		_, err = (PluginLoaderImpl{}).Load(ctx, "x.so")
		assert.Error(t, err)
		_, err = (DefaultConfigProvider{}).Watch(ctx, "key")
		assert.Error(t, err)
	})

	classifier := SafetyPolicyClassifier{}
	assert.Equal(t, "safety_policy", classifier.Name())
	// String methods log internally.
	assert.Equal(t, "allow", ClassificationAllow.String())
	assert.Equal(t, "user", SubmissionUserMessage.String())
	assert.Equal(t, "discard_oldest", DiscardOldest.String())
}

// noopSpanStub is a minimal tracing.TraceSpan used to exercise the noop
// exporter without building a real span.
type noopSpanStub struct{}

func (noopSpanStub) TraceID() string                       { return "" }
func (noopSpanStub) SpanID() string                        { return "span-1" }
func (noopSpanStub) ParentSpanID() string                  { return "" }
func (noopSpanStub) Name() string                          { return "" }
func (noopSpanStub) StartTime() time.Time                  { return time.Time{} }
func (noopSpanStub) EndTime() time.Time                    { return time.Time{} }
func (noopSpanStub) SetAttributes(...tracing.Attribute)    {}
func (noopSpanStub) AddEvent(string, ...tracing.Attribute) {}
func (noopSpanStub) SetStatus(tracing.SpanStatus, string)  {}
func (noopSpanStub) End()                                  {}
func (noopSpanStub) Context() context.Context              { return context.Background() }

var _ tracing.TraceSpan = (*noopSpanStub)(nil)
