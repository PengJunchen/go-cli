package cli

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
)

func TestAssembledCLIDefaultPromptDisclosesGuardPolicy(t *testing.T) {
	assembly, err := AssembleAgent(context.Background(), newAssembleTestConfig(t), "openai", "test-model", io.Discard)
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)

	prompt := assembly.PromptBuilder.Build(context.Background(), core.SystemPromptOptions{
		AppendPrompt: "USER_APPEND_SENTINEL",
	})
	require.Equal(t, 1, strings.Count(prompt, cliSensitiveDataPromptPolicy))
	require.Less(t, strings.Index(prompt, cliSensitiveDataPromptPolicy), strings.Index(prompt, "USER_APPEND_SENTINEL"))
}

func TestCLIDefaultPromptPolicyPreservesCustomAndAppendSemantics(t *testing.T) {
	ctx := context.Background()
	coreBuilder := core.NewDefaultSystemPromptBuilder()
	options := core.SystemPromptOptions{AppendPrompt: "USER_APPEND_SENTINEL"}
	original := options

	guarded := newCLIDefaultPromptBuilder(true)
	prompt := guarded.Build(ctx, options)
	require.Equal(t, 1, strings.Count(prompt, cliSensitiveDataPromptPolicy))
	require.Less(t, strings.Index(prompt, cliSensitiveDataPromptPolicy), strings.Index(prompt, "USER_APPEND_SENTINEL"))
	require.Equal(t, original.AppendPrompt, options.AppendPrompt, "Build must not mutate caller options")
	require.Equal(t, prompt, guarded.Build(ctx, options), "repeated builds must not duplicate the policy")

	custom := core.SystemPromptOptions{CustomPrompt: "CUSTOM_PROMPT_SENTINEL", AppendPrompt: "CUSTOM_APPEND_SENTINEL"}
	require.Equal(t, coreBuilder.Build(ctx, custom), guarded.Build(ctx, custom))
	require.NotContains(t, guarded.Build(ctx, custom), cliSensitiveDataPromptPolicy)

	unguarded := newCLIDefaultPromptBuilder(false)
	require.Equal(t, coreBuilder.Build(ctx, options), unguarded.Build(ctx, options))
	require.Equal(t, coreBuilder.Build(ctx, options), core.NewDefaultSystemPromptBuilder().Build(ctx, options))
}

func TestFinalPIIRepairEligibilityUsesCurrentEffectivePrompt(t *testing.T) {
	ctx := context.Background()
	assert.True(t, finalPIIGuardRepairEligible(ctx, true, false, ""))
	assert.False(t, finalPIIGuardRepairEligible(ctx, true, false, "CUSTOM_STATIC_PROMPT"))
	assert.False(t, finalPIIGuardRepairEligible(ctx, false, false, ""), "unguarded CLI must not opt in")

	defaultPlan := &runtimePlan{promptOptions: core.SystemPromptOptions{CustomPrompt: ""}}
	dynamicDefault := context.WithValue(ctx, runtimePlanKey{}, defaultPlan)
	assert.True(t, finalPIIGuardRepairEligible(dynamicDefault, true, true, ""))

	customPlan := &runtimePlan{promptOptions: core.SystemPromptOptions{CustomPrompt: "CUSTOM_DYNAMIC_PROMPT"}}
	dynamicCustom := context.WithValue(ctx, runtimePlanKey{}, customPlan)
	assert.False(t, finalPIIGuardRepairEligible(dynamicCustom, true, true, ""), "dynamic default-to-custom must turn repair off for this run")
	assert.False(t, finalPIIGuardRepairEligible(ctx, true, true, ""), "a missing dynamic runtime plan must fail closed")

	implicitRuntime := newAssembleTestConfig(t)
	implicitRuntime.Skill.Progressive = true
	require.False(t, implicitRuntime.Runtime.Dynamic)
	requiresSnapshot := dynamicRuntimeEnabled(implicitRuntime)
	require.True(t, requiresSnapshot)
	assert.True(t, finalPIIGuardRepairEligible(dynamicDefault, true, requiresSnapshot, ""))
	assert.False(t, finalPIIGuardRepairEligible(dynamicCustom, true, requiresSnapshot, ""), "an implicit runtime mode must follow its current custom prompt too")
}

func TestDynamicCLIPromptPolicyTracksReloadAndRuntimeBudget(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	cfg := newAssembleTestConfig(t)
	cfg.Runtime.Dynamic = true
	cfg.Runtime.MaxPromptBytes = 64 << 10
	cfg.Agent.AppendSystemPrompt = "DEFAULT_APPEND_SENTINEL"
	current := cfg
	ctx := WithRuntimeConfigLoader(context.Background(), func(context.Context) (*config.Config, error) {
		return current, nil
	})
	assembly, err := AssembleAgent(ctx, cfg, "openai", "test-model", io.Discard)
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)

	buildPrompt := func() string {
		runCtx, lease, acquireErr := assembly.dynamicRuntime.AcquireRun(ctx)
		require.NoError(t, acquireErr)
		defer lease.Release()
		return assembly.PromptBuilder.Build(runCtx, core.SystemPromptOptions{})
	}
	prompt := buildPrompt()
	require.Equal(t, 1, strings.Count(prompt, cliSensitiveDataPromptPolicy))
	require.Less(t, strings.Index(prompt, cliSensitiveDataPromptPolicy), strings.Index(prompt, "DEFAULT_APPEND_SENTINEL"))

	custom := *cfg
	custom.Agent.SystemPrompt = "CUSTOM_PROMPT_SENTINEL"
	custom.Agent.AppendSystemPrompt = "CUSTOM_APPEND_SENTINEL"
	current = &custom
	require.NoError(t, assembly.ReloadCapabilities(ctx))
	prompt = buildPrompt()
	require.Contains(t, prompt, "CUSTOM_PROMPT_SENTINEL")
	require.Contains(t, prompt, "CUSTOM_APPEND_SENTINEL")
	require.NotContains(t, prompt, cliSensitiveDataPromptPolicy)

	defaultAgain := custom
	defaultAgain.Agent.SystemPrompt = ""
	defaultAgain.Agent.AppendSystemPrompt = "DEFAULT_APPEND_AGAIN_SENTINEL"
	current = &defaultAgain
	require.NoError(t, assembly.ReloadCapabilities(ctx))
	prompt = buildPrompt()
	require.Equal(t, 1, strings.Count(prompt, cliSensitiveDataPromptPolicy))
	require.Less(t, strings.Index(prompt, cliSensitiveDataPromptPolicy), strings.Index(prompt, "DEFAULT_APPEND_AGAIN_SENTINEL"))

	basePrompt := core.NewDefaultSystemPromptBuilder().Build(ctx, core.SystemPromptOptions{})
	_, err = validateRuntimeRequestBudget(ctx, newCLIDefaultPromptBuilder(true), core.SystemPromptOptions{}, len(basePrompt))
	require.ErrorContains(t, err, "byte budget")
	bounded := &runtimePromptBuilder{inner: newCLIDefaultPromptBuilder(true), maxBytes: len(basePrompt)}
	require.Contains(t, bounded.Build(ctx, core.SystemPromptOptions{}), "Capability prompt exceeds the configured")
}
