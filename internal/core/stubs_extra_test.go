package core

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tracing"
)

func TestHeuristicTokenEstimatorEstimate(t *testing.T) {
	est := HeuristicTokenEstimator{}
	tests := []struct {
		name string
		text string
		want int
	}{
		{"empty", "", 0},
		{"short", "abcd", 1},
		{"exact", "abcdefgh", 2},
		{"truncates", "abc", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, est.Estimate(tt.text))
		})
	}
}

func TestHeuristicTokenEstimatorEstimateMessages(t *testing.T) {
	est := HeuristicTokenEstimator{}
	msgs := []AgentMessage{
		{Role: "user", Content: "abcdefgh"},
		{Role: "assistant", Content: "abcd"},
		{Role: "system", Content: ""},
	}
	// (8/4) + (4/4) + (0/4) = 2 + 1 + 0
	assert.Equal(t, 3, est.EstimateMessages(msgs))
}

func TestHeuristicTokenEstimatorEstimateMessagesEmpty(t *testing.T) {
	assert.Equal(t, 0, (HeuristicTokenEstimator{}).EstimateMessages(nil))
}

func TestSessionStoreImplStubs(t *testing.T) {
	ctx := context.Background()
	store := SessionStoreImpl{}
	require.NoError(t, store.Save(ctx, Session{ID: "s1"}))

	sess, err := store.Load(ctx, "s1")
	require.NoError(t, err)
	assert.Equal(t, "", sess.ID)

	list, err := store.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, list)
}

func TestSessionTreeImplStubs(t *testing.T) {
	ctx := context.Background()
	tree := SessionTreeImpl{}
	require.NoError(t, tree.Create(ctx, "root"))
	require.NoError(t, tree.Branch(ctx, "root", "child"))

	leaves, err := tree.Leaves(ctx, "root")
	require.NoError(t, err)
	assert.Empty(t, leaves)
}

func TestInMemoryApprovalStoreStubs(t *testing.T) {
	ctx := context.Background()
	store := InMemoryApprovalStore{}
	require.NoError(t, store.Remember(ctx, "bash", true))
	assert.False(t, store.IsAllowed(ctx, "bash"))
	assert.False(t, store.IsAllowed(ctx, "anything"))
}

func TestSafetyPolicyClassifierAllowsAll(t *testing.T) {
	classifier := SafetyPolicyClassifier{}
	assert.Equal(t, "safety_policy", classifier.Name())
	for _, tool := range []string{"bash", "write_file", "read_file"} {
		assert.Equal(t, ClassificationAllow, classifier.Classify(context.Background(), tool))
	}
}

func TestDefaultModelProviderStubs(t *testing.T) {
	ctx := context.Background()
	p := DefaultModelProvider{}
	assert.Equal(t, "default", p.Name())
	assert.Nil(t, p.Models())

	model, cleanup, err := p.Build(ctx, llm.ModelConfig{Model: "m", APIKey: "k"})
	require.ErrorIs(t, err, errModelUnsupported)
	assert.Nil(t, model)
	require.NotNil(t, cleanup)
	cleanup() // must be nil-safe to call
}

func TestDefaultToolRegistryStubs(t *testing.T) {
	ctx := context.Background()
	reg := DefaultToolRegistry{}
	require.NoError(t, reg.Register(ctx, testTool{name: "t"}))

	def, err := reg.Get(ctx, "t")
	require.ErrorIs(t, err, errToolUnknown)
	assert.Nil(t, def)

	list, err := reg.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, list)
}

func TestNoopTraceExporterStub(t *testing.T) {
	ctx := context.Background()
	exp := NoopTraceExporter{}
	require.NoError(t, exp.ExportSpan(ctx, &noopSpanStub{}))
	require.NoError(t, exp.Shutdown(ctx))
}

func TestDefaultConfigProviderStubs(t *testing.T) {
	ctx := context.Background()
	cp := DefaultConfigProvider{}
	assert.Equal(t, "default", cp.Name())
	require.ErrorIs(t, cp.Load(ctx, "k", nil), errConfigUnsupported)

	ch, err := cp.Watch(ctx, "k")
	require.ErrorIs(t, err, errConfigUnsupported)
	assert.Nil(t, ch)
}

func TestPluginLoaderStub(t *testing.T) {
	ctx := context.Background()
	ext, err := (PluginLoaderImpl{}).Load(ctx, "path.so")
	require.ErrorIs(t, err, errPluginsUnsupported)
	assert.Nil(t, ext)
}

func TestDefaultStubErrorIsolations(t *testing.T) {
	// The four sentinel stub errors must be distinct so callers can switch.
	all := []error{errPluginsUnsupported, errToolUnknown, errModelUnsupported, errConfigUnsupported}
	seen := map[string]bool{}
	for _, e := range all {
		assert.False(t, seen[e.Error()], "duplicate error %q", e.Error())
		seen[e.Error()] = true
		assert.NotNil(t, e)
	}
}

func TestNoopSpanStubContext(t *testing.T) {
	s := &noopSpanStub{}
	assert.Equal(t, context.Background(), s.Context())
	assert.Equal(t, "span-1", s.SpanID())
}

var _ tracing.TraceExporter = (*NoopTraceExporter)(nil)
