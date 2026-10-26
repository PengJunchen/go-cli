package cli

import (
	"context"
	"io"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/mock"
)

func TestReviewFailedHookCandidateRetainsOldGeneration(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	cfg := newAssembleTestConfig(t)
	cfg.Runtime.Dynamic = true
	cfg.Agent.SystemPrompt = "retained prompt"
	current := cfg
	ctx := WithRuntimeConfigLoader(context.Background(), func(context.Context) (*config.Config, error) { return current, nil })
	assembly, err := AssembleAgent(ctx, cfg, "openai", "test-model", io.Discard, WithApproveMode(ApproveAuto))
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)
	before, err := assembly.CapabilityStatus(ctx)
	require.NoError(t, err)
	invalid := *cfg
	invalid.Agent.SystemPrompt = "candidate must not leak"
	invalid.Hooks.Hooks = []config.HookConfig{{Event: "unsupported_event", Command: "printf ignored"}}
	current = &invalid
	require.Error(t, assembly.ReloadCapabilities(ctx))
	after, err := assembly.CapabilityStatus(ctx)
	require.NoError(t, err)
	require.Equal(t, before.Generation, after.Generation)
	runCtx, lease, err := assembly.dynamicRuntime.AcquireRun(ctx)
	require.NoError(t, err)
	defer lease.Release()
	prompt := assembly.PromptBuilder.Build(runCtx, core.SystemPromptOptions{})
	require.Contains(t, prompt, "retained prompt")
	require.NotContains(t, prompt, "candidate must not leak")
}

func TestReviewUnverifiedProjectReloadCannotAdoptChangedCapture(t *testing.T) {
	_, path, manager, cfg := setupMCPGenerationProject(t, []byte(`{"servers":[]}`))
	cfg.MCP.Loading = "lazy"
	cfg.Provider = newAssembleTestConfig(t).Provider
	ctx := WithRuntimeConfigLoader(context.Background(), func(ctx context.Context) (*config.Config, error) {
		fresh, err := config.LoadTrustedWithManager(ctx, manager)
		if fresh != nil {
			fresh.MCP.Loading = "lazy"
			fresh.Provider = cfg.Provider
		}
		return fresh, err
	})
	assembly, err := AssembleAgent(ctx, cfg, "openai", "test-model", io.Discard, WithApproveMode(ApproveAuto))
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)
	before, err := assembly.CapabilityStatus(ctx)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, []byte(`{"servers":[{"name":"unverified","url":"http://invalid.test"}]}`), 0600))
	require.Error(t, assembly.ReloadCapabilities(ctx))
	after, err := assembly.CapabilityStatus(ctx)
	require.NoError(t, err)
	require.Equal(t, before.Generation, after.Generation)
	require.Empty(t, after.MCP)
}

func TestReviewLazyMCPRemainsCallableAcrossOwnedRuns(t *testing.T) {
	peer := newMCPGenerationPeer(t, []string{"lookup"}, "alive", false, false, false)
	_, _, _, cfg := setupMCPGenerationProject(t, []byte(`{"servers":[{"name":"two-runs","url":"`+peer.server.URL+`"}]}`))
	cfg.MCP.Loading = "lazy"
	cfg.Provider = newAssembleTestConfig(t).Provider
	assembly, err := AssembleAgent(context.Background(), cfg, "openai", "test-model", io.Discard, WithApproveMode(ApproveAuto))
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)
	model := mock.NewMockLLMServer(mock.NewConversationTemplate("owned-two-runs", "mcp", mock.ConversationTurn{AssistantToolCalls: []mock.ExpectedToolCall{{ID: "discover", Name: "mcp_discover", Args: map[string]any{"server": "two-runs"}}}}, mock.ConversationTurn{AssistantToolCalls: []mock.ExpectedToolCall{{ID: "first", Name: "mcp__two-runs__lookup", Args: map[string]any{}}}}, mock.ConversationTurn{AssistantContent: "first done"}, mock.ConversationTurn{AssistantToolCalls: []mock.ExpectedToolCall{{ID: "second", Name: "mcp__two-runs__lookup", Args: map[string]any{}}}}, mock.ConversationTurn{AssistantContent: "second done"}))
	assembly.LoopAgent.SetModel(model)
	for _, message := range []string{"activate and call", "call again"} {
		stream, err := assembly.Harness.Submit(context.Background(), message)
		require.NoError(t, err)
		for range stream.Events() {
		}
		_, err = stream.Result()
		require.NoError(t, err)
	}
	require.EqualValues(t, 1, peer.lists.Load())
	require.EqualValues(t, 2, peer.calls.Load())
}
