package core

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
)

func TestAuxiliaryOperationsHaveIndependentIdentityAndNoAgentRun(t *testing.T) {
	parent, cancel := context.WithCancel(withRunID(context.Background(), "completed-agent-run"))
	ctx := WithAuxiliaryOperations(parent, nil, nil)
	require.Empty(t, RunIDFromContext(ctx))
	_, _, bound := RunCommitFromContext(ctx)
	require.False(t, bound)
	factory, ok := llm.RunOperationFactoryFromContext(ctx)
	require.True(t, ok)
	candidate := requestEnvelopeCandidate(t, llm.ModelInfo{Name: "auxiliary", ContextWindow: 128000})
	first, err := factory.NewOperation(ctx, llm.OperationSummary, 1, []llm.ResolvedCandidateHandle{candidate})
	require.NoError(t, err)
	second, err := factory.NewOperation(ctx, llm.OperationSummary, 1, []llm.ResolvedCandidateHandle{candidate})
	require.NoError(t, err)
	require.NotEqual(t, first.ID(), second.ID())
	cancel()
	require.ErrorIs(t, ctx.Err(), context.Canceled)
}
