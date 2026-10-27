package core

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/tracing"
)

func TestManagedSubAgentCopiesConversationButNoParentRunAuthority(t *testing.T) {
	ctx := WithRunCommit(context.Background(), &consumerTestSink{}, RunCommitBinding{SessionID: "parent-physical", WorkspaceID: "workspace", OwnerGeneration: "parent-owner"})
	ctx = withRunID(ctx, "parent-run")
	ctx = withRunCommitLoopScope(ctx)
	ctx = withRunWorkerTracker(ctx, &runOwner{})
	ctx = WithRunGenerationMetadata(ctx, RunGenerationMetadata{GenerationID: "parent-generation"})
	ctx = WithRunToolRegistry(ctx, tools.NewDefaultToolRegistry())
	tracer := tracing.NewTracer("parent-trace", nil)
	ctx = tracer.ContextWithTracer(ctx)
	ctx = withSubagentParentMessages(ctx, []llm.Message{
		{Role: llm.RoleSystem, Content: "parent-system-must-not-copy"},
		{Role: llm.RoleUser, Content: "current-parent-conversation", Runtime: llm.RuntimeMetadata{Source: llm.SourceRef{RunID: "parent-run"}}},
	})
	called := false
	runner := &managedSubAgentRunner{config: SubAgentConfig{ContextMode: "inherit"}, execute: func(child context.Context, _ SubAgentConfig, prompt string, _ <-chan string, _ func(AgentEvent)) (AgentMessage, error) {
		called = true
		_, _, bound := RunCommitFromContext(child)
		require.False(t, bound)
		require.Empty(t, RunIDFromContext(child))
		require.Nil(t, runOwnerFromContext(child))
		require.Nil(t, runToolRegistry(child, nil))
		_, generation := RunGenerationMetadataFromContext(child)
		require.False(t, generation)
		require.Same(t, tracer, tracing.TracerFromContext(child))
		require.Contains(t, prompt, "current-parent-conversation")
		require.NotContains(t, prompt, "parent-system-must-not-copy")
		require.NotContains(t, prompt, "parent-run")
		require.NotContains(t, prompt, "parent-owner")
		return AgentMessage{Role: "assistant", Content: "actual child"}, nil
	}}
	final, err := runner.Run(ctx, "delegated task", nil, func(AgentEvent) {})
	require.NoError(t, err)
	require.True(t, called)
	require.Equal(t, "actual child", final.Content)
}

type managedUnknownOutcome struct{}

func (managedUnknownOutcome) Error() string        { return "child dispatched with unknown outcome" }
func (managedUnknownOutcome) OutcomeUnknown() bool { return true }

func TestManagedSubAgentInterruptAndDispatcherPreserveUnknownOutcome(t *testing.T) {
	for _, dispatch := range []bool{false, true} {
		started := make(chan struct{})
		factory := NewManagedSubAgentFactory(func(ctx context.Context, _ SubAgentConfig, _ string, _ <-chan string, _ func(AgentEvent)) (AgentMessage, error) {
			close(started)
			<-ctx.Done()
			return AgentMessage{}, managedUnknownOutcome{}
		})
		ctx, cancel := context.WithCancel(t.Context())
		if dispatch {
			done := make(chan error, 1)
			go func() {
				_, err := NewDefaultSubagentDispatcher(factory).Dispatch(ctx, SubagentTask{ID: "unknown", Prompt: "task"})
				done <- err
			}()
			<-started
			cancel()
			err := <-done
			var unknown interface{ OutcomeUnknown() bool }
			require.True(t, errors.As(err, &unknown))
			require.True(t, unknown.OutcomeUnknown())
		} else {
			sub, err := factory.Create(ctx, "unknown", SubAgentConfig{})
			require.NoError(t, err)
			events, err := sub.Run(ctx, "task")
			require.NoError(t, err)
			drained := make(chan struct{})
			go func() {
				defer close(drained)
				for range events {
				}
			}()
			<-started
			require.NoError(t, sub.Interrupt(t.Context()))
			_, err = sub.Wait(t.Context())
			<-drained
			require.ErrorIs(t, err, context.Canceled)
			var unknown interface{ OutcomeUnknown() bool }
			require.True(t, errors.As(err, &unknown))
			require.True(t, unknown.OutcomeUnknown())
			cancel()
		}
	}
}

func TestManagedSubAgentRequiresContextAndBoundsNestedDepth(t *testing.T) {
	called := 0
	runner := &managedSubAgentRunner{config: SubAgentConfig{ContextMode: "inherit"}, execute: func(context.Context, SubAgentConfig, string, <-chan string, func(AgentEvent)) (AgentMessage, error) {
		called++
		return AgentMessage{}, nil
	}}
	_, err := runner.Run(context.Background(), "task", nil, func(AgentEvent) {})
	require.ErrorContains(t, err, "context is unavailable")
	require.Zero(t, called)
	runner.config.ContextMode = "independent"
	_, err = runner.Run(context.WithValue(context.Background(), subagentDepthKey{}, defaultMaxSubAgentDepth), "task", nil, func(AgentEvent) {})
	require.ErrorIs(t, err, errSubAgentDepthExceeded)
	require.Zero(t, called)
	runner.config.ContextMode = "invalid"
	_, err = runner.Run(context.Background(), "task", nil, func(AgentEvent) {})
	require.ErrorContains(t, err, "invalid")
	require.Zero(t, called)
}

func TestOrdinaryFactoryCannotAcquireManagedSubagentCompletion(t *testing.T) {
	_, err := NewManagedSubagentTool(NewDefaultSubagentDispatcher(NewSubAgentFactory()))
	require.ErrorIs(t, err, ErrRunCommitChildUnsupported)
}

func TestManagedSubAgentNestedDispatchPropagatesDepth(t *testing.T) {
	var factory SubAgentFactory
	runs := 0
	factory = NewManagedSubAgentFactory(func(ctx context.Context, _ SubAgentConfig, _ string, _ <-chan string, _ func(AgentEvent)) (AgentMessage, error) {
		runs++
		result, err := NewDefaultSubagentDispatcher(factory).Dispatch(ctx, SubagentTask{ID: "nested", Prompt: "continue nesting"})
		return AgentMessage{Role: "assistant", Content: result.Content}, err
	})
	_, err := NewDefaultSubagentDispatcher(factory).Dispatch(t.Context(), SubagentTask{ID: "root-child", Prompt: "start nesting"})
	require.ErrorIs(t, err, errSubAgentDepthExceeded)
	require.Equal(t, defaultMaxSubAgentDepth, runs)
}
