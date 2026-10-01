package core

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/tools"
)

type subAgentModelBuilderContextKey struct{}

func newRealSubAgentRunnerWithBuilder(t *testing.T, parent llm.BaseChatModel, builder SubAgentModelBuilder, modelName string) *realSubAgentRunner {
	t.Helper()

	factory := NewRealSubAgentFactoryWithModelBuilder(parent, builder, tools.NewDefaultToolRegistry())
	defaultFactory, ok := factory.(*DefaultSubAgentFactory)
	require.True(t, ok)
	runnerFactory := defaultFactory.runnerFactory
	require.NotNil(t, runnerFactory)
	runner, ok := runnerFactory(SubAgentConfig{Model: modelName}).(*realSubAgentRunner)
	require.True(t, ok)
	return runner
}

func TestRealSubAgentModelBuilderEmptyOverrideInheritsParent(t *testing.T) {
	parent := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"System", "parent-model",
		mock.ConversationTurn{AssistantContent: "parent response"},
	))
	var buildCalls atomic.Int32
	runner := newRealSubAgentRunnerWithBuilder(t, parent, func(context.Context, string) (llm.BaseChatModel, func(), error) {
		buildCalls.Add(1)
		return nil, nil, errors.New("builder must not run for an empty override")
	}, " \t ")

	final, err := runner.Run(context.Background(), "child prompt", nil, func(AgentEvent) {})
	require.NoError(t, err)
	assert.Equal(t, "parent response", final.Content)
	assert.Zero(t, buildCalls.Load())
	assert.Equal(t, 1, parent.CallCount())
}

func TestRealSubAgentModelBuilderUsesRunContextAndTrimmedOverride(t *testing.T) {
	parent := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"System", "parent-model",
		mock.ConversationTurn{AssistantContent: "parent response"},
	))
	child := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"System", "child-model",
		mock.ConversationTurn{AssistantContent: "child response"},
	))
	ctx := context.WithValue(context.Background(), subAgentModelBuilderContextKey{}, "run-context-marker")
	var buildCalls atomic.Int32
	var receivedModel string
	var receivedContext any
	runner := newRealSubAgentRunnerWithBuilder(t, parent, func(gotCtx context.Context, modelName string) (llm.BaseChatModel, func(), error) {
		buildCalls.Add(1)
		receivedModel = modelName
		receivedContext = gotCtx.Value(subAgentModelBuilderContextKey{})
		return child, nil, nil
	}, "  child-model \t")

	final, err := runner.Run(ctx, "child prompt", nil, func(AgentEvent) {})
	require.NoError(t, err)
	assert.Equal(t, "child response", final.Content)
	assert.Equal(t, int32(1), buildCalls.Load())
	assert.Equal(t, "child-model", receivedModel)
	assert.Equal(t, "run-context-marker", receivedContext)
	assert.Zero(t, parent.CallCount(), "an override must not call the parent model")
	assert.Equal(t, 1, child.CallCount())
}

func TestRealSubAgentModelBuilderMissingBuilderFailsClosed(t *testing.T) {
	parent := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"System", "parent-model",
		mock.ConversationTurn{AssistantContent: "must not run"},
	))
	runner := newRealSubAgentRunnerWithBuilder(t, parent, nil, "child-model")

	_, err := runner.Run(context.Background(), "child prompt", nil, func(AgentEvent) {})
	require.ErrorIs(t, err, ErrSubAgentModelBuilderUnavailable)
	assert.Zero(t, parent.CallCount())
}

func TestRealSubAgentModelBuilderNilModelFailsClosed(t *testing.T) {
	parent := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"System", "parent-model",
		mock.ConversationTurn{AssistantContent: "must not run"},
	))
	runner := newRealSubAgentRunnerWithBuilder(t, parent, func(context.Context, string) (llm.BaseChatModel, func(), error) {
		return nil, nil, nil
	}, "child-model")

	_, err := runner.Run(context.Background(), "child prompt", nil, func(AgentEvent) {})
	require.ErrorIs(t, err, errSubAgentModelBuilderReturnedNil)
	assert.Equal(t, "core: sub-agent model resolution failed", err.Error())
	assert.Zero(t, parent.CallCount())
}

func TestRealSubAgentModelBuilderErrorIsSafeAndRetainsCause(t *testing.T) {
	parent := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"System", "parent-model",
		mock.ConversationTurn{AssistantContent: "must not run"},
	))
	buildCause := errors.New("api-key-canary endpoint?token=endpoint-canary body=response-canary")
	var cleanups atomic.Int32
	runner := newRealSubAgentRunnerWithBuilder(t, parent, func(context.Context, string) (llm.BaseChatModel, func(), error) {
		return nil, func() { cleanups.Add(1) }, buildCause
	}, "child-model")

	_, err := runner.Run(context.Background(), "child prompt", nil, func(AgentEvent) {})
	require.ErrorIs(t, err, buildCause)
	assert.Equal(t, "core: sub-agent model resolution failed", err.Error())
	assert.NotContains(t, err.Error(), "api-key-canary")
	assert.NotContains(t, err.Error(), "endpoint-canary")
	assert.NotContains(t, err.Error(), "response-canary")
	assert.Equal(t, int32(1), cleanups.Load(), "cleanup returned with an error is still owned by Run")
	assert.Zero(t, parent.CallCount())
}

func TestRealSubAgentModelBuilderGuardsRunBeforeResolution(t *testing.T) {
	parent := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"System", "parent-model",
		mock.ConversationTurn{AssistantContent: "must not run"},
	))
	var buildCalls atomic.Int32
	runner := newRealSubAgentRunnerWithBuilder(t, parent, func(context.Context, string) (llm.BaseChatModel, func(), error) {
		buildCalls.Add(1)
		return parent, nil, nil
	}, "child-model")

	durableCtx := WithRunCommit(context.Background(), inertRunCommitSink{}, RunCommitBinding{
		SessionID:   "parent-session",
		WorkspaceID: "workspace-1",
	})
	_, durableErr := runner.Run(durableCtx, "child prompt", nil, func(AgentEvent) {})
	require.ErrorIs(t, durableErr, ErrRunCommitChildUnsupported)

	depthCtx := context.WithValue(context.Background(), subagentDepthKey{}, defaultMaxSubAgentDepth)
	_, depthErr := runner.Run(depthCtx, "child prompt", nil, func(AgentEvent) {})
	require.ErrorIs(t, depthErr, errSubAgentDepthExceeded)

	assert.Zero(t, buildCalls.Load(), "durable and depth guards precede model resolution")
	assert.Zero(t, parent.CallCount())
}

type joinedSubAgentModel struct {
	started     chan struct{}
	release     chan struct{}
	joined      chan struct{}
	callOnce    atomic.Bool
	releaseOnce sync.Once
	joinedOnce  sync.Once
}

func (m *joinedSubAgentModel) unblock() {
	m.releaseOnce.Do(func() { close(m.release) })
}

func (m *joinedSubAgentModel) Generate(context.Context, []llm.Message, ...llm.Option) (*llm.Message, error) {
	return &llm.Message{Role: llm.RoleAssistant, Content: "child response"}, nil
}

func (m *joinedSubAgentModel) Stream(ctx context.Context, _ []llm.Message, _ ...llm.Option) (<-chan llm.MessageChunk, error) {
	chunks := make(chan llm.MessageChunk)
	go func() {
		defer close(chunks)
		defer m.joinedOnce.Do(func() { close(m.joined) })
		if m.callOnce.CompareAndSwap(false, true) {
			close(m.started)
		}
		select {
		case <-m.release:
		case <-ctx.Done():
			return
		}
		select {
		case chunks <- llm.MessageChunk{Role: llm.RoleAssistant, Content: "child response"}:
		case <-ctx.Done():
			return
		}
		select {
		case chunks <- llm.MessageChunk{Role: llm.RoleAssistant, Final: true, FinishReason: "stop"}:
		case <-ctx.Done():
		}
	}()
	return chunks, nil
}

func TestRealSubAgentModelBuilderCleanupWaitsForJoinedChild(t *testing.T) {
	child := &joinedSubAgentModel{started: make(chan struct{}), release: make(chan struct{}), joined: make(chan struct{})}
	t.Cleanup(child.unblock)
	var cleanups atomic.Int32
	runner := newRealSubAgentRunnerWithBuilder(t, nil, func(context.Context, string) (llm.BaseChatModel, func(), error) {
		return child, func() {
			select {
			case <-child.joined:
			default:
				assert.Fail(t, "model cleanup ran before the child stream joined")
			}
			cleanups.Add(1)
		}, nil
	}, "child-model")

	type runResult struct {
		message AgentMessage
		err     error
	}
	done := make(chan runResult, 1)
	go func() {
		message, err := runner.Run(context.Background(), "child prompt", nil, func(AgentEvent) {})
		done <- runResult{message: message, err: err}
	}()

	select {
	case <-child.started:
	case <-time.After(5 * time.Second):
		t.Fatal("child model stream did not start")
	}
	assert.Zero(t, cleanups.Load(), "cleanup must remain owned while the child stream is active")
	child.unblock()

	select {
	case outcome := <-done:
		require.NoError(t, outcome.err)
		assert.Equal(t, "child response", outcome.message.Content)
	case <-time.After(5 * time.Second):
		t.Fatal("child runner did not finish after stream release")
	}
	assert.Equal(t, int32(1), cleanups.Load())
}

func TestRealSubAgentModelBuilderCleanupRunsAfterCanceledChildJoins(t *testing.T) {
	child := &joinedSubAgentModel{started: make(chan struct{}), release: make(chan struct{}), joined: make(chan struct{})}
	t.Cleanup(child.unblock)
	var cleanups atomic.Int32
	runner := newRealSubAgentRunnerWithBuilder(t, nil, func(context.Context, string) (llm.BaseChatModel, func(), error) {
		return child, func() {
			select {
			case <-child.joined:
			default:
				assert.Fail(t, "model cleanup ran before the canceled child stream joined")
			}
			cleanups.Add(1)
		}, nil
	}, "child-model")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := runner.Run(ctx, "child prompt", nil, func(AgentEvent) {})
		done <- err
	}()
	select {
	case <-child.started:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("child model stream did not start")
	}
	cancel()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("canceled child runner did not finish")
	}
	assert.Equal(t, int32(1), cleanups.Load())
}
