package cli

import (
	"bytes"
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
)

// mockAgentLoop is a minimal AgentLoop for testing middleware.
type mockAgentLoop struct {
	called bool
}

func (m *mockAgentLoop) Run(_ context.Context, _ core.Submission, _ ...core.EventStream) ([]core.AgentEvent, error) {
	m.called = true
	return []core.AgentEvent{{Kind: "message", Content: "mock-response"}}, nil
}

// TestMiddlewareChainExecuted verifies that when MiddlewareChain is applied
// with LoggingMiddleware, the middleware emits log lines around the loop's
// Run call and the base loop executes.
func TestMiddlewareChainExecuted(t *testing.T) {
	// Capture slog output to verify LoggingMiddleware emits logs.
	var logBuf bytes.Buffer
	handler := slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(handler))
	defer slog.SetDefault(oldLogger)

	base := &mockAgentLoop{}
	wrapped := core.NewMiddlewareChain(
		core.NewLoggingMiddleware("interactive"),
	).Wrap(base)

	require.NotNil(t, wrapped)

	events, err := wrapped.Run(context.Background(), core.Submission{Content: "test"})
	require.NoError(t, err)
	assert.NotEmpty(t, events)
	assert.True(t, base.called, "base loop should have been called")

	// Verify LoggingMiddleware emitted log lines around the Run call.
	logOutput := logBuf.String()
	assert.Contains(t, logOutput, "core.logging_middleware.run")
	assert.Contains(t, logOutput, "core.logging_middleware.done")
	assert.Contains(t, logOutput, "interactive")
}

type canonicalAssemblyTestLoop struct{ calls int }

func (*canonicalAssemblyTestLoop) Run(context.Context, core.Submission, ...core.EventStream) ([]core.AgentEvent, error) {
	return nil, nil
}

func (l *canonicalAssemblyTestLoop) RunCanonical(_ context.Context, submission core.Submission, _ ...core.EventStream) (core.CanonicalRunResult, error) {
	l.calls++
	return core.CanonicalRunResult{
		Events: []core.AgentEvent{{Kind: "message", Content: "assembled answer"}},
		Transcript: []core.TranscriptEntry{
			{Message: core.AgentMessage{Role: "user", Content: submission.Content, InputKind: core.InputKindUser}},
			{Message: core.AgentMessage{Role: "assistant", Content: "assembled answer"}},
		},
	}, nil
}

func TestAssembleMiddlewarePreservesCanonicalLoop(t *testing.T) {
	base := &canonicalAssemblyTestLoop{}
	state := &assembleState{
		ac:        assembleConfig{agentName: "canonical-assembly"},
		rc:        &config.Config{},
		loop:      base,
		loopAgent: core.NewLoopAgent(),
		hookChain: core.NewHookChain(),
	}

	state.assembleMiddleware()

	canonical, ok := state.loop.(core.CanonicalLoop)
	require.True(t, ok, "the default assembled logging/hook middleware chain must preserve RunCanonical")
	result, err := canonical.RunCanonical(context.Background(), core.Submission{Content: "request"})
	require.NoError(t, err)
	assert.Equal(t, 1, base.calls)
	require.Len(t, result.Transcript, 2)
	assert.Equal(t, "assembled answer", result.Transcript[1].Message.Content)
}
