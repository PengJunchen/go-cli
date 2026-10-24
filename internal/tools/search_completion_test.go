package tools

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type unreviewedSearchRegistry struct{ ToolRegistry }

var errUnreviewedSearchList = errors.New("custom listing failed")

func (unreviewedSearchRegistry) List(context.Context) ([]ToolDefinition, error) {
	return nil, errUnreviewedSearchList
}

type overriddenDefaultSearchRegistry struct{ *DefaultToolRegistry }

func (*overriddenDefaultSearchRegistry) List(context.Context) ([]ToolDefinition, error) {
	return nil, errUnreviewedSearchList
}

type detachedSearchRegistry struct {
	ToolRegistry
	release <-chan struct{}
	done    chan struct{}
}

func (r detachedSearchRegistry) List(context.Context) ([]ToolDefinition, error) {
	go func() {
		<-r.release
		close(r.done)
	}()
	return nil, nil
}

func TestPreparedToolSearchCompletionJoinsReviewedRegistry(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name      string
		registry  ToolRegistry
		wantError bool
	}{
		{name: "default", registry: NewDefaultToolRegistry()},
		{name: "deferred", registry: NewDeferredToolRegistryAdapter(NewDefaultToolRegistry())},
		{name: "nil dependency", wantError: true},
		{name: "unconfigured middleware list error", registry: NewMiddlewareToolRegistry(NewDefaultToolRegistry()), wantError: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			base := NewDefaultToolRegistry().(*DefaultToolRegistry) //nolint:errcheck
			registry := NewMiddlewareToolRegistry(base)
			configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{})
			require.NoError(t, registry.RegisterTrustedControl(ctx, NewToolSearchTool(testCase.registry)))
			invocation, err := registry.PrepareToolCall(ctx, ToolCall{Name: "tool_search", Args: map[string]any{"query": "read"}})
			require.NoError(t, err)
			_, err = invocation.Dispatch(ctx)
			if testCase.wantError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			signal := invocation.CompletionSignal()
			require.NotNil(t, signal.Done(), "synchronous discovery must prove its actual local pipeline joined")
			select {
			case <-signal.Done():
			default:
				t.Fatal("search returned before its local pipeline joined")
			}
			assert.Equal(t, CompletionInlinePipelineJoined, signal.Proof())
			assert.Equal(t, []TrustedToolEffect{EffectControl}, invocation.EffectSet().Values())
		})
	}
}

func TestPreparedToolSearchDoesNotTrustCustomRegistryCompletion(t *testing.T) {
	ctx := context.Background()
	base := NewDefaultToolRegistry().(*DefaultToolRegistry) //nolint:errcheck
	registry := NewMiddlewareToolRegistry(base)
	configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{})
	require.NoError(t, registry.RegisterTrustedControl(ctx, NewToolSearchTool(unreviewedSearchRegistry{})))
	invocation, err := registry.PrepareToolCall(ctx, ToolCall{Name: "tool_search"})
	require.NoError(t, err)
	_, err = invocation.Dispatch(ctx)
	require.ErrorIs(t, err, errUnreviewedSearchList)
	assert.Empty(t, invocation.CompletionSignal().Proof(), "an arbitrary List implementation cannot manufacture join authority")
}

func TestPreparedToolSearchDoesNotAttestDetachedCustomListing(t *testing.T) {
	ctx := context.Background()
	release, done := make(chan struct{}), make(chan struct{})
	base := NewDefaultToolRegistry().(*DefaultToolRegistry) //nolint:errcheck
	registry := NewMiddlewareToolRegistry(base)
	configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{})
	require.NoError(t, registry.RegisterTrustedControl(ctx, NewToolSearchTool(detachedSearchRegistry{release: release, done: done})))
	invocation, err := registry.PrepareToolCall(ctx, ToolCall{Name: "tool_search"})
	require.NoError(t, err)
	_, err = invocation.Dispatch(ctx)
	require.NoError(t, err)
	assert.Empty(t, invocation.CompletionSignal().Proof(), "returning a listing does not join arbitrary detached resources")
	close(release)
	<-done
	assert.Empty(t, invocation.CompletionSignal().Proof(), "an external resource finishing later cannot invent completion authority")
}

func TestToolSearchReportsOnlyReviewedSynchronousReturns(t *testing.T) {
	for _, category := range []string{"", "invalid"} {
		var proof PreparedCompletionProof
		var done <-chan struct{}
		ctx := withPreparedCompletionReporter(context.Background(), func(value PreparedCompletionProof, joined <-chan struct{}) { proof, done = value, joined })
		_, _ = NewToolSearchTool(NewDefaultToolRegistry()).Execute(ctx, ToolCall{Args: map[string]any{"category": category}})
		require.NotNil(t, done)
		assert.Equal(t, CompletionInlinePipelineJoined, proof)
		select {
		case <-done:
		default:
			t.Fatal("inline completion report must be closed")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var canceledProof PreparedCompletionProof
	var canceledDone <-chan struct{}
	ctx = withPreparedCompletionReporter(ctx, func(proof PreparedCompletionProof, done <-chan struct{}) { canceledProof, canceledDone = proof, done })
	_, err := NewToolSearchTool(NewDefaultToolRegistry()).Execute(ctx, ToolCall{})
	require.ErrorIs(t, err, context.Canceled)
	require.NotNil(t, canceledDone)
	assert.Equal(t, CompletionInlinePipelineJoined, canceledProof)
	select {
	case <-canceledDone:
	default:
		t.Fatal("canceled synchronous search must join before returning")
	}
}

func TestToolSearchSynchronousRegistryRejectsUnknownBoundaries(t *testing.T) {
	var nilDefault *DefaultToolRegistry
	var nilMiddleware *MiddlewareToolRegistry
	var nilDeferred *DeferredToolRegistryAdapter
	cycle := NewMiddlewareToolRegistry(nil)
	cycle.inner = cycle
	for _, registry := range []ToolRegistry{
		nilDefault, nilMiddleware, nilDeferred, cycle,
		NewMiddlewareToolRegistry(nil), NewDeferredToolRegistryAdapter(nil),
		&overriddenDefaultSearchRegistry{NewDefaultToolRegistry().(*DefaultToolRegistry)}, //nolint:errcheck
	} {
		assert.False(t, synchronousToolSearchRegistry(registry), "unreviewed boundary %T must not manufacture join authority", registry)
	}
}
