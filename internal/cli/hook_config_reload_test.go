package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/approval"
	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/mock"
)

func TestDynamicSkillOnlyReloadKeepsVerifiedJSONHookExecutable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, os.Chmod(root, 0700))
	t.Chdir(root)
	cfg := newAssembleTestConfig(t)
	cfg.Git.WorkDir, cfg.Runtime.Dynamic, cfg.Skill.Progressive = root, true, true
	cfg.Skill.Dir = ".go-cli/skills"
	cfg.MCP = config.MCPConfig{Loading: "lazy", Servers: []config.MCPServerConfig{}}
	disabled := false
	cfg.Extensions.Enabled = &disabled
	cfg.Commands.Dir = filepath.Join(root, ".go-cli", "commands")
	require.NoError(t, os.MkdirAll(cfg.Commands.Dir, 0700))
	cfg.Memory.StorePath = filepath.Join(root, "memory.jsonl")
	cfg.Sandbox.AllowedPaths = []string{root}
	data, err := json.Marshal(cfg)
	require.NoError(t, err)
	configPath := filepath.Join(root, ".go-cli.json")
	require.NoError(t, os.WriteFile(configPath, data, 0600))
	skillPath := filepath.Join(root, ".go-cli", "skills", "reload-check", "SKILL.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(skillPath), 0700))
	writeSkill := func(version string) {
		t.Helper()
		require.NoError(t, os.WriteFile(skillPath, []byte("---\nname: reload-check\ndescription: "+version+"\n---\nInstructions "+version), 0600))
	}
	writeSkill("one")
	hooks := []byte(" \n" + `{"hooks":[{"event":"pre_tool_use","command":"cat >/dev/null; printf 'HOOK_STABLE\\n' >> hook-events","timeout":"1s"}]}`)
	hookPath := filepath.Join(root, ".go-cli", "hooks.yaml")
	require.NoError(t, os.WriteFile(hookPath, hooks, 0600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "input.txt"), []byte("verified input"), 0600))
	storeDir := t.TempDir()
	require.NoError(t, os.Chmod(storeDir, 0700))
	manager := approval.NewDefaultTrustManager(approval.NewFileTrustStore(filepath.Join(storeDir, "trust.json")))
	frozen, ok := manager.(approval.FrozenProjectSourceTrust)
	require.True(t, ok)
	grant := func() {
		t.Helper()
		captured, err := config.LoadTrustedWithManager(ctx, manager)
		require.NoError(t, err)
		require.NotNil(t, captured.CapturedProjectSourcesForGrant())
		require.NoError(t, frozen.TrustProjectSourceSnapshot(ctx, captured.CapturedProjectSourcesForGrant()))
	}
	load := func(loadCtx context.Context) (*config.Config, error) {
		loaded, err := config.LoadTrustedWithManager(loadCtx, manager)
		if err != nil {
			return nil, err
		}
		// Keep the complete loader-minted source/capture provenance while
		// executing only the explicit temporary project's public settings.
		// Copy no private fields and create no replacement trust proof.
		overlay := loaded.ProjectOverlayForVerifiedSources()
		if overlay == nil {
			return nil, errors.New("fixture project needs trust")
		}
		target, source := reflect.ValueOf(loaded).Elem(), reflect.ValueOf(overlay).Elem()
		for index := range target.NumField() {
			if target.Type().Field(index).IsExported() {
				target.Field(index).Set(source.Field(index))
			}
		}
		return loaded, nil
	}
	grant()
	cfg, err = load(ctx)
	require.NoError(t, err)
	ctx = WithRuntimeConfigLoader(ctx, load)
	assembly, err := AssembleAgent(ctx, cfg, "openai", "test-model", io.Discard, WithApproveMode(ApproveAuto))
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)
	assembly.LoopAgent.WithToolSearchThreshold(0)
	model := mock.NewMockLLMServer(mock.NewConversationTemplate("json-hooks-reload", "read",
		mock.ConversationTurn{AssistantToolCalls: []mock.ExpectedToolCall{{ID: "first-read", Name: "read", Args: map[string]any{"path": "input.txt"}}}},
		mock.ConversationTurn{AssistantContent: "first complete"},
		mock.ConversationTurn{AssistantToolCalls: []mock.ExpectedToolCall{{ID: "next-read", Name: "read", Args: map[string]any{"path": "input.txt"}}}},
		mock.ConversationTurn{AssistantContent: "next complete"}))
	assembly.LoopAgent.SetModel(model)
	run := func() string {
		t.Helper()
		runCtx, lease, err := assembly.dynamicRuntime.AcquireRun(ctx)
		require.NoError(t, err)
		defer lease.Release()
		plan := runCtx.Value(runtimePlanKey{}).(*runtimePlan)
		require.NotNil(t, plan.interceptor)
		require.Len(t, plan.hooks.Hooks(), 1)
		_, err = assembly.LoopAgent.RunCanonical(runCtx, core.Submission{Content: "read input.txt"})
		require.NoError(t, err)
		metadata, _ := core.RunGenerationMetadataFromContext(runCtx)
		return metadata.GenerationID
	}
	before := run()
	writeSkill("two")
	grant()
	require.NoError(t, assembly.ReloadCapabilities(ctx), "unchanged verified JSON hooks must remain accepted after a skill-only grant")
	require.NotEqual(t, before, run())
	actual, err := os.ReadFile(hookPath)
	require.NoError(t, err)
	require.Equal(t, hooks, actual)
	actual, err = os.ReadFile(configPath)
	require.NoError(t, err)
	require.Equal(t, data, actual)
	actual, err = os.ReadFile(filepath.Join(root, "hook-events"))
	require.NoError(t, err)
	require.Equal(t, 2, strings.Count(string(actual), "HOOK_STABLE"))
}

func TestShellHookConfigFormatsKeepRuntimeSemanticGuards(t *testing.T) {
	for _, input := range []string{
		`{"hooks":[{"event":"pre_tool_use","command":"true","timeout":"1s"}]}`,
		"hooks:\n  - event: pre_tool_use\n    command: \"true\"\n    timeout: 1s\n",
	} {
		declarations, err := parseShellHooksConfig([]byte(input))
		require.NoError(t, err)
		require.Len(t, declarations.Hooks, 1)
		require.NoError(t, validateRuntimeHooks(&config.Config{Hooks: declarations}))
		for _, invalid := range []config.HookConfig{
			{Event: "unknown", Command: "true"},
			{Event: "post_tool_use", Command: "true"},
			{Event: "pre_tool_use", Command: "true", Timeout: "0s"},
			{Event: "pre_tool_use", Command: "true", Timeout: "61s"},
		} {
			require.Error(t, validateRuntimeHooks(&config.Config{Hooks: config.HooksConfig{Hooks: []config.HookConfig{invalid}}}))
		}
	}
	_, err := parseShellHooksConfig([]byte(`{"hooks":`))
	require.Error(t, err)
}
