package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/approval"
	"github.com/pengjunchen/go-cli/internal/capability"
	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/mock"
)

func TestDynamicRuntimeReloadPinsPromptAndRejectsColdConfiguration(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	cfg := newAssembleTestConfig(t)
	cfg.Runtime.Dynamic = true
	cfg.Skill.Progressive = true
	cfg.Agent.SystemPrompt = "old prompt"
	current := cfg
	ctx := WithRuntimeConfigLoader(context.Background(), func(context.Context) (*config.Config, error) { return current, nil })
	assembly, err := AssembleAgent(ctx, cfg, "openai", "test-model", io.Discard, WithApproveMode(ApproveAuto))
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)
	runCtx, lease, err := assembly.dynamicRuntime.AcquireRun(ctx)
	require.NoError(t, err)
	require.Contains(t, assembly.PromptBuilder.Build(runCtx, core.SystemPromptOptions{}), "old prompt")
	newer := *cfg
	newer.Agent.SystemPrompt = "new prompt"
	current = &newer
	deadline, cancel := context.WithTimeout(ctx, time.Millisecond)
	defer cancel()
	require.ErrorIs(t, assembly.ReloadCapabilities(deadline), context.DeadlineExceeded)
	require.Contains(t, assembly.PromptBuilder.Build(runCtx, core.SystemPromptOptions{}), "old prompt")
	lease.Release()
	require.NoError(t, assembly.ReloadCapabilities(ctx))
	runCtx, lease, err = assembly.dynamicRuntime.AcquireRun(ctx)
	require.NoError(t, err)
	require.Contains(t, assembly.PromptBuilder.Build(runCtx, core.SystemPromptOptions{}), "new prompt")
	lease.Release()
	beforeCold, err := assembly.CapabilityStatus(ctx)
	require.NoError(t, err)
	for _, field := range []struct {
		name   string
		change func(*config.Config)
	}{
		{name: "session", change: func(next *config.Config) { next.Session.StorePath = "changed-store" }},
		{name: "dynamic", change: func(next *config.Config) { next.Runtime.Dynamic = false }},
		{name: "profile", change: func(next *config.Config) { next.Runtime.Profile = "edge" }},
		{name: "server-budget", change: func(next *config.Config) { next.Runtime.MaxMCPServers = 4 }},
	} {
		t.Run(field.name, func(t *testing.T) {
			changed := newer
			field.change(&changed)
			current = &changed
			require.ErrorIs(t, assembly.ReloadCapabilities(ctx), capability.ErrRestartRequired)
			status, err := assembly.CapabilityStatus(ctx)
			require.NoError(t, err)
			require.Equal(t, beforeCold.Generation, status.Generation)
		})
	}
}

func TestProgressiveSkillInstructionsReachRealLoopOnlyAfterLoad(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(t.TempDir())
	dir := filepath.Join(home, ".config", "go-cli", "skills", "writing")
	require.NoError(t, os.MkdirAll(dir, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: writing\ndescription: Write well\n---\nPRIVATE_SKILL_BODY"), 0600))
	cfg := newAssembleTestConfig(t)
	cfg.Skill = config.SkillConfig{Progressive: true}
	assembly, err := AssembleAgent(context.Background(), cfg, "openai", "test-model", io.Discard, WithApproveMode(ApproveAuto))
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)
	server := mock.NewMockLLMServer(mock.NewConversationTemplate("skill-progressive", "skill", mock.ConversationTurn{AssistantToolCalls: []mock.ExpectedToolCall{{ID: "skill", Name: "skill_load", Args: map[string]any{"name": "writing"}}}}, mock.ConversationTurn{AssistantContent: "done"}))
	assembly.LoopAgent.SetModel(server)
	runCtx, lease, err := assembly.dynamicRuntime.AcquireRun(context.Background())
	require.NoError(t, err)
	defer lease.Release()
	_, err = assembly.LoopAgent.Run(runCtx, core.Submission{Content: "Write something"})
	require.NoError(t, err)
	calls := server.CallLog()
	require.Len(t, calls, 2)
	require.NotContains(t, calls[0].Messages[0].Content, "PRIVATE_SKILL_BODY")
	found := false
	for _, message := range calls[1].Messages {
		found = found || strings.Contains(message.Content, "PRIVATE_SKILL_BODY")
	}
	require.True(t, found)
}

func TestProgressiveMCPActivationRefreshesNextRequestSchema(t *testing.T) {
	peer := newMCPGenerationPeer(t, []string{"lookup"}, "alive", false, false, false)
	data := []byte(`{"servers":[{"name":"lazy","url":"` + peer.server.URL + `"}]}`)
	_, _, _, cfg := setupMCPGenerationProject(t, data)
	cfg.MCP.Loading = "lazy"
	cfg.Provider = newAssembleTestConfig(t).Provider
	assembly, err := AssembleAgent(context.Background(), cfg, "openai", "test-model", io.Discard, WithApproveMode(ApproveAuto))
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)
	require.Zero(t, peer.requests.Load(), "lazy bootstrap must not dial")
	server := mock.NewMockLLMServer(mock.NewConversationTemplate("mcp-progressive", "mcp", mock.ConversationTurn{AssistantToolCalls: []mock.ExpectedToolCall{{ID: "discover", Name: "mcp_discover", Args: map[string]any{"server": "lazy"}}}}, mock.ConversationTurn{AssistantToolCalls: []mock.ExpectedToolCall{{ID: "call", Name: "mcp__lazy__lookup", Args: map[string]any{}}}}, mock.ConversationTurn{AssistantContent: "done"}))
	assembly.LoopAgent.SetModel(server)
	runCtx, lease, err := assembly.dynamicRuntime.AcquireRun(context.Background())
	require.NoError(t, err)
	defer lease.Release()
	_, err = assembly.LoopAgent.Run(runCtx, core.Submission{Content: "Look it up"})
	require.NoError(t, err)
	calls := server.CallLog()
	require.Len(t, calls, 3)
	options := llm.GenerationOptions{}
	for _, option := range calls[1].Options {
		option(&options)
	}
	found := false
	for _, tool := range options.Tools {
		found = found || tool.Name == "mcp__lazy__lookup"
	}
	require.True(t, found)
	require.EqualValues(t, 1, peer.lists.Load())
	require.EqualValues(t, 1, peer.calls.Load())
}

func TestDynamicRuntimeReloadReplacesHookPlanWithoutMutatingPinnedPlan(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	cfg := newAssembleTestConfig(t)
	cfg.Runtime.Dynamic = true
	cfg.Hooks.Hooks = []config.HookConfig{{Event: "pre_tool_use", Command: "printf old", Timeout: "1s"}}
	current := cfg
	ctx := WithRuntimeConfigLoader(context.Background(), func(context.Context) (*config.Config, error) { return current, nil })
	assembly, err := AssembleAgent(ctx, cfg, "openai", "test-model", io.Discard, WithApproveMode(ApproveAuto))
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)
	runCtx, lease, err := assembly.dynamicRuntime.AcquireRun(ctx)
	require.NoError(t, err)
	defer lease.Release()
	oldMetadata, _ := core.RunGenerationMetadataFromContext(runCtx)
	oldPlan := runCtx.Value(runtimePlanKey{}).(*runtimePlan)
	require.NotNil(t, oldPlan.interceptor)
	require.Len(t, oldPlan.hooks.Hooks(), 1)
	lease.Release()
	next := *cfg
	next.Hooks.Hooks = nil
	current = &next
	require.NoError(t, assembly.ReloadCapabilities(ctx))
	runCtx, lease, err = assembly.dynamicRuntime.AcquireRun(ctx)
	require.NoError(t, err)
	defer lease.Release()
	newMetadata, _ := core.RunGenerationMetadataFromContext(runCtx)
	require.NotEqual(t, oldMetadata.GenerationID, newMetadata.GenerationID, "global hook declarations participate in generation identity")
	newPlan := runCtx.Value(runtimePlanKey{}).(*runtimePlan)
	require.Nil(t, newPlan.interceptor)
	require.Empty(t, newPlan.hooks.Hooks())
	require.NotNil(t, oldPlan.interceptor, "published plan remains immutable")
}

func TestDynamicRuntimeRejectedPromptBudgetRetainsPublishedGeneration(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	cfg := newAssembleTestConfig(t)
	cfg.Runtime.Dynamic = true
	cfg.Agent.SystemPrompt = "A complete prompt plan"
	current := cfg
	ctx := WithRuntimeConfigLoader(context.Background(), func(context.Context) (*config.Config, error) { return current, nil })
	assembly, err := AssembleAgent(ctx, cfg, "openai", "test-model", io.Discard, WithApproveMode(ApproveAuto))
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)
	before, err := assembly.CapabilityStatus(ctx)
	require.NoError(t, err)
	rejected := *cfg
	rejected.Runtime.MaxPromptBytes = 8
	current = &rejected
	require.ErrorContains(t, assembly.ReloadCapabilities(ctx), "byte budget")
	after, err := assembly.CapabilityStatus(ctx)
	require.NoError(t, err)
	require.Equal(t, before.Generation, after.Generation)
	runCtx, lease, err := assembly.dynamicRuntime.AcquireRun(ctx)
	require.NoError(t, err)
	defer lease.Release()
	require.Contains(t, assembly.PromptBuilder.Build(runCtx, core.SystemPromptOptions{}), "A complete prompt plan")
}

func TestDynamicRuntimePromptRenderingUsesUpdatedPlanBudget(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	cfg := newAssembleTestConfig(t)
	cfg.Runtime.Dynamic = true
	cfg.Runtime.MaxPromptBytes = 64 << 10
	cfg.Agent.SystemPrompt = "small"
	current := cfg
	ctx := WithRuntimeConfigLoader(context.Background(), func(context.Context) (*config.Config, error) { return current, nil })
	assembly, err := AssembleAgent(ctx, cfg, "openai", "test-model", io.Discard, WithApproveMode(ApproveAuto))
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)
	next := *cfg
	next.Runtime.MaxPromptBytes = 256 << 10
	next.Agent.SystemPrompt = strings.Repeat("expanded prompt ", 8000)
	current = &next
	require.NoError(t, assembly.ReloadCapabilities(ctx))
	runCtx, lease, err := assembly.dynamicRuntime.AcquireRun(ctx)
	require.NoError(t, err)
	defer lease.Release()
	require.Contains(t, assembly.PromptBuilder.Build(runCtx, core.SystemPromptOptions{}), next.Agent.SystemPrompt)
}

func TestProgressiveRuntimeCanDiscoverFirstCapabilitiesAfterEmptyStartup(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(t.TempDir())
	cfg := newAssembleTestConfig(t)
	cfg.Skill.Progressive = true
	cfg.MCP.Loading = "lazy"
	assembly, err := AssembleAgent(context.Background(), cfg, "openai", "test-model", io.Discard, WithApproveMode(ApproveAuto))
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)
	_, err = assembly.ToolRegistry.Get(context.Background(), "skill_load")
	require.NoError(t, err)
	_, err = assembly.ToolRegistry.Get(context.Background(), "mcp_discover")
	require.NoError(t, err)
	skillDir := filepath.Join(home, ".config", "go-cli", "skills", "first")
	require.NoError(t, os.MkdirAll(skillDir, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: first\ndescription: First added skill\n---\nFIRST_BODY"), 0600))
	peer := newMCPGenerationPeer(t, []string{"lookup"}, "alive", false, false, false)
	require.NoError(t, os.WriteFile(filepath.Join(home, ".config", "go-cli", "mcp.json"), []byte(`{"servers":[{"name":"first","url":"`+peer.server.URL+`"}]}`), 0600))
	require.NoError(t, assembly.ReloadCapabilities(context.Background()))
	status, err := assembly.CapabilityStatus(context.Background())
	require.NoError(t, err)
	require.Len(t, status.Skills, 1)
	require.Len(t, status.MCP, 1)
	require.Zero(t, peer.requests.Load())
	server := mock.NewMockLLMServer(mock.NewConversationTemplate("first-capabilities", "first", mock.ConversationTurn{AssistantToolCalls: []mock.ExpectedToolCall{{ID: "skill", Name: "skill_load", Args: map[string]any{"name": "first"}}}}, mock.ConversationTurn{AssistantToolCalls: []mock.ExpectedToolCall{{ID: "discover", Name: "mcp_discover", Args: map[string]any{"server": "first"}}}}, mock.ConversationTurn{AssistantToolCalls: []mock.ExpectedToolCall{{ID: "call", Name: "mcp__first__lookup", Args: map[string]any{}}}}, mock.ConversationTurn{AssistantContent: "done"}))
	assembly.LoopAgent.SetModel(server)
	runCtx, lease, err := assembly.dynamicRuntime.AcquireRun(context.Background())
	require.NoError(t, err)
	defer lease.Release()
	_, err = assembly.LoopAgent.Run(runCtx, core.Submission{Content: "Use the new capabilities"})
	require.NoError(t, err)
	require.EqualValues(t, 1, peer.calls.Load())
}

func TestNamedMCPRefreshRejectsChangedProjectGrantBeforeReconnect(t *testing.T) {
	peer := newMCPGenerationPeer(t, []string{"lookup"}, "alive", false, false, false)
	data := []byte(`{"servers":[{"name":"lazy","url":"` + peer.server.URL + `"}]}`)
	root, _, manager, cfg := setupMCPGenerationProject(t, data)
	cfg.MCP.Loading = "lazy"
	cfg.Provider = newAssembleTestConfig(t).Provider
	current := cfg
	ctx := WithRuntimeConfigLoader(context.Background(), func(context.Context) (*config.Config, error) { return current, nil })
	assembly, err := AssembleAgent(ctx, cfg, "openai", "test-model", io.Discard, WithApproveMode(ApproveAuto))
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)
	require.NoError(t, assembly.LoadMCPServer(ctx, "lazy"))
	requests := peer.requests.Load()
	before, err := assembly.CapabilityStatus(ctx)
	require.NoError(t, err)
	require.NoError(t, manager.RevokeTrust(ctx, root))
	trustMCPGenerationProjectSnapshot(t, root, manager)
	fresh, err := config.LoadTrustedWithManager(ctx, manager)
	require.NoError(t, err)
	fresh.MCP.Loading = "lazy"
	fresh.Provider = cfg.Provider
	current = fresh
	require.ErrorIs(t, assembly.RefreshMCPServer(ctx, "lazy"), capability.ErrRestartRequired)
	require.Equal(t, requests, peer.requests.Load(), "a fresh grant must not be transplanted to the old server owner")
	after, err := assembly.CapabilityStatus(ctx)
	require.NoError(t, err)
	require.Equal(t, before.Generation, after.Generation)
}

func TestProgressiveMCPRejectsOversizedCandidateBeforePublication(t *testing.T) {
	for _, refresh := range []bool{false, true} {
		t.Run(map[bool]string{false: "first-activation", true: "named-refresh"}[refresh], func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Chdir(t.TempDir())
			oldPeer := newMCPGenerationPeer(t, []string{"lookup"}, "old-executor", false, false, false)
			largeSchema := map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string", "description": strings.Repeat("large schema ", 20000)}}}
			candidatePeer := newMCPGenerationPeer(t, []string{"lookup"}, "candidate-executor", false, false, false, largeSchema)
			path := filepath.Join(home, ".config", "go-cli", "mcp.json")
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
			writeServer := func(url string) {
				require.NoError(t, os.WriteFile(path, []byte(`{"servers":[{"name":"lazy","url":"`+url+`"}]}`), 0600))
			}
			if refresh {
				writeServer(oldPeer.server.URL)
			} else {
				writeServer(candidatePeer.server.URL)
			}
			cfg := newAssembleTestConfig(t)
			cfg.MCP.Loading = "lazy"
			cfg.Runtime.MaxPromptBytes = 64 << 10
			assembly, err := AssembleAgent(context.Background(), cfg, "openai", "test-model", io.Discard, WithApproveMode(ApproveAuto))
			require.NoError(t, err)
			t.Cleanup(assembly.Cleanup)
			if refresh {
				require.NoError(t, assembly.LoadMCPServer(context.Background(), "lazy"))
				writeServer(candidatePeer.server.URL)
			}
			before, err := assembly.CapabilityStatus(context.Background())
			require.NoError(t, err)
			owner := assembly.progressiveMCP.owner
			sourceID := assembly.progressiveMCP.source.id + "/server/lazy"
			owner.mu.Lock()
			var oldCurrent *assemblyMCPGeneration
			if state := owner.sources[sourceID]; state != nil {
				oldCurrent = state.current
			}
			owner.mu.Unlock()
			if refresh {
				err = assembly.RefreshMCPServer(context.Background(), "lazy")
			} else {
				err = assembly.LoadMCPServer(context.Background(), "lazy")
			}
			require.ErrorContains(t, err, "budget exhausted")
			after, err := assembly.CapabilityStatus(context.Background())
			require.NoError(t, err)
			require.Equal(t, before.Generation, after.Generation)
			require.Equal(t, before.CatalogRevision, after.CatalogRevision)
			owner.mu.Lock()
			var nextCurrent *assemblyMCPGeneration
			if state := owner.sources[sourceID]; state != nil {
				nextCurrent = state.current
			}
			stagedCount := len(owner.staged)
			owner.mu.Unlock()
			require.True(t, oldCurrent == nextCurrent, "a rejected candidate must not replace the source owner")
			require.Zero(t, stagedCount)
			require.EqualValues(t, 1, candidatePeer.disconnects.Load(), "the rejected connection is closed before returning")
			require.Zero(t, candidatePeer.calls.Load())
			if refresh {
				server := mock.NewMockLLMServer(mock.NewConversationTemplate("old-after-rejection", "old", mock.ConversationTurn{AssistantToolCalls: []mock.ExpectedToolCall{{ID: "old", Name: "mcp__lazy__lookup", Args: map[string]any{}}}}, mock.ConversationTurn{AssistantContent: "done"}))
				assembly.LoopAgent.SetModel(server)
				runCtx, lease, acquireErr := assembly.dynamicRuntime.AcquireRun(context.Background())
				require.NoError(t, acquireErr)
				defer lease.Release()
				_, err = assembly.LoopAgent.Run(runCtx, core.Submission{Content: "Use the old server"})
				require.NoError(t, err)
				require.EqualValues(t, 1, oldPeer.calls.Load())
			} else {
				_, err = assembly.ToolRegistry.Get(context.Background(), "mcp__lazy__lookup")
				require.Error(t, err)
			}
		})
	}
}

func TestDynamicRuntimeReloadRejectsLoadedMCPSourceIdentityChange(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(t.TempDir())
	peer := newMCPGenerationPeer(t, []string{"lookup"}, "alive", false, false, false)
	data := []byte(`{"servers":[{"name":"lazy","url":"` + peer.server.URL + `"}]}`)
	path := filepath.Join(home, ".config", "go-cli", "mcp.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
	require.NoError(t, os.WriteFile(path, data, 0600))
	manager := approval.NewDefaultTrustManager(approval.NewInMemoryTrustStore())
	cfg, err := config.LoadTrustedWithManager(context.Background(), manager)
	require.NoError(t, err)
	cfg.MCP.Loading = "lazy"
	cfg.Provider = newAssembleTestConfig(t).Provider
	cfg.Git.WorkDir = t.TempDir()
	current := cfg
	ctx := WithRuntimeConfigLoader(context.Background(), func(context.Context) (*config.Config, error) { return current, nil })
	assembly, err := AssembleAgent(ctx, cfg, "openai", "test-model", io.Discard, WithApproveMode(ApproveAuto))
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)
	require.NoError(t, assembly.LoadMCPServer(ctx, "lazy"))
	before, err := assembly.CapabilityStatus(ctx)
	require.NoError(t, err)
	requests := peer.requests.Load()
	globalPath := filepath.Join(home, ".go-cli", "config.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(globalPath), 0700))
	require.NoError(t, os.WriteFile(globalPath, append(append([]byte(`{"mcp":`), data...), '}'), 0600))
	fresh, err := config.LoadTrustedWithManager(ctx, manager)
	require.NoError(t, err)
	fresh.MCP.Loading = cfg.MCP.Loading
	fresh.Provider = cfg.Provider
	fresh.Git = cfg.Git
	current = fresh
	for _, action := range []struct {
		name string
		run  func() error
	}{
		{name: "reload", run: func() error { return assembly.ReloadCapabilities(ctx) }},
		{name: "named-refresh", run: func() error { return assembly.RefreshMCPServer(ctx, "lazy") }},
	} {
		t.Run(action.name, func(t *testing.T) {
			require.ErrorIs(t, action.run(), capability.ErrRestartRequired)
			require.Equal(t, requests, peer.requests.Load())
			after, err := assembly.CapabilityStatus(ctx)
			require.NoError(t, err)
			require.Equal(t, before.Generation, after.Generation)
			require.Equal(t, before.CatalogRevision, after.CatalogRevision)
		})
	}
}

func TestDynamicRuntimeEagerMCPSourcesRequireRestartWithoutBackgroundReload(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(t.TempDir())
	oldPeer := newMCPGenerationPeer(t, []string{"lookup"}, "old-eager", false, false, false)
	newPeer := newMCPGenerationPeer(t, []string{"lookup"}, "new-eager", false, false, false)
	path := filepath.Join(home, ".config", "go-cli", "mcp.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
	writeSource := func(url string) {
		require.NoError(t, os.WriteFile(path, []byte(`{"servers":[{"name":"eager","url":"`+url+`"}]}`), 0600))
	}
	writeSource(oldPeer.server.URL)
	cfg := newAssembleTestConfig(t)
	cfg.Runtime.Dynamic = true
	current := cfg
	ctx := WithRuntimeConfigLoader(context.Background(), func(context.Context) (*config.Config, error) { return current, nil })
	assembly, err := AssembleAgent(ctx, cfg, "openai", "test-model", io.Discard, WithApproveMode(ApproveAuto))
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)
	_, err = assembly.ToolRegistry.Get(ctx, "mcp__eager__lookup")
	require.NoError(t, err)
	assembly.mcpResources.mu.Lock()
	reloaders := len(assembly.mcpResources.reloaders)
	assembly.mcpResources.mu.Unlock()
	require.Zero(t, reloaders, "dynamic + eager cannot replace a pinned registry in the background")
	before, err := assembly.CapabilityStatus(ctx)
	require.NoError(t, err)
	for _, change := range []struct {
		name string
		run  func()
	}{
		{name: "unchanged-source", run: func() {}},
		{name: "same-path-new-bytes", run: func() { writeSource(newPeer.server.URL) }},
		{name: "removed-source", run: func() { require.NoError(t, os.Remove(path)) }},
		{name: "new-inline-config", run: func() {
			next := *cfg
			next.MCP.Servers = []config.MCPServerConfig{{Name: "eager", URL: newPeer.server.URL}}
			current = &next
		}},
	} {
		t.Run(change.name, func(t *testing.T) {
			change.run()
			require.ErrorIs(t, assembly.ReloadCapabilities(ctx), capability.ErrRestartRequired)
			after, err := assembly.CapabilityStatus(ctx)
			require.NoError(t, err)
			require.Equal(t, before.Generation, after.Generation)
			require.Equal(t, before.CatalogRevision, after.CatalogRevision)
			require.Zero(t, newPeer.requests.Load())
		})
	}
	server := mock.NewMockLLMServer(mock.NewConversationTemplate("eager-still-old", "old", mock.ConversationTurn{AssistantToolCalls: []mock.ExpectedToolCall{{ID: "old", Name: "mcp__eager__lookup", Args: map[string]any{}}}}, mock.ConversationTurn{AssistantContent: "done"}))
	assembly.LoopAgent.SetModel(server)
	runCtx, lease, err := assembly.dynamicRuntime.AcquireRun(ctx)
	require.NoError(t, err)
	defer lease.Release()
	_, err = assembly.LoopAgent.Run(runCtx, core.Submission{Content: "Use the original eager server"})
	require.NoError(t, err)
	require.EqualValues(t, 1, oldPeer.calls.Load())
	require.Zero(t, newPeer.calls.Load())
}
