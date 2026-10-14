package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/approval"
	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/tools"
)

func TestRegisterSkillToolsRejectsUnverifiedProjectDirectory(t *testing.T) {
	root := t.TempDir()
	skillDir := filepath.Join(root, ".go-cli", "skills")
	writeProjectSkill(t, filepath.Join(skillDir, "untrusted", "SKILL.md"), projectSkillContent("untrusted", "untrusted prompt"))
	t.Chdir(root)

	registry := tools.NewDeferredToolRegistryAdapter(tools.NewDefaultToolRegistry())
	config := &config.Config{Skill: config.SkillConfig{Dir: skillDir}}

	infos := registerSkillTools(context.Background(), config, registry)
	assert.Empty(t, infos)
	registered, err := registry.List(context.Background())
	require.NoError(t, err)
	assert.Empty(t, registered)
}

func TestRegisterSkillToolsRejectsExternalDirectory(t *testing.T) {
	root := t.TempDir()
	externalSkills := filepath.Join(t.TempDir(), "skills")
	writeProjectSkill(t, filepath.Join(externalSkills, "external.md"), projectSkillContent("external", "external prompt"))
	t.Chdir(root)

	registry := tools.NewDeferredToolRegistryAdapter(tools.NewDefaultToolRegistry())
	config := &config.Config{Skill: config.SkillConfig{Dir: externalSkills}}

	infos := registerSkillTools(context.Background(), config, registry)
	assert.Empty(t, infos)
	registered, err := registry.List(context.Background())
	require.NoError(t, err)
	assert.Empty(t, registered)
}

func TestRegisterSkillToolsUsesVerifiedSnapshotAfterDiskChanges(t *testing.T) {
	root := t.TempDir()
	skillDir := filepath.Join(root, ".go-cli", "skills")
	originalPath := filepath.Join(skillDir, "original", "SKILL.md")
	writeProjectSkill(t, originalPath, `---
name: approved-skill
description: captured description
tools:
  - read
parameters:
  mode: captured-default
parameter_schema: |
  {"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}
---
approved prompt
`)
	writeProjectSkill(t, filepath.Join(skillDir, "flat.md"), projectSkillContent("flat-skill", "flat prompt"))
	writeProjectSkill(t, filepath.Join(skillDir, "schema.yaml"), projectSkillContent("yaml-skill", "yaml prompt"))
	writeProjectSkill(t, filepath.Join(skillDir, "schema-yml.yml"), projectSkillContent("yml-skill", "yml prompt"))
	writeProjectSkill(t, filepath.Join(root, ".go-cli.yaml"), "skill:\n  dir: .go-cli/skills\n")
	t.Chdir(root)
	t.Setenv("HOME", t.TempDir())

	ctx := context.Background()
	manager := approval.NewDefaultTrustManager(approval.NewInMemoryTrustStore())
	uncurated, err := config.LoadTrustedWithManager(ctx, manager)
	require.NoError(t, err)
	snapshot := uncurated.CapturedProjectSourcesForGrant()
	require.NotNil(t, snapshot)
	frozen, ok := manager.(approval.FrozenProjectSourceTrust)
	require.True(t, ok)
	require.NoError(t, frozen.TrustProjectSourceSnapshot(ctx, snapshot))
	trusted, err := config.LoadTrustedWithManager(ctx, manager)
	require.NoError(t, err)
	require.NotNil(t, trusted.VerifiedProjectSources())

	writeProjectSkill(t, originalPath, projectSkillContent("changed-skill", "changed prompt"))
	writeProjectSkill(t, filepath.Join(skillDir, "added", "SKILL.md"), projectSkillContent("added-skill", "added prompt"))

	registry := tools.NewDeferredToolRegistryAdapter(tools.NewDefaultToolRegistry())
	infos := registerSkillTools(ctx, trusted, registry)
	require.Len(t, infos, 4)
	skillNames := make([]string, 0, len(infos))
	for _, info := range infos {
		skillNames = append(skillNames, info.Name)
	}
	assert.ElementsMatch(t, []string{"approved-skill", "flat-skill", "yaml-skill", "yml-skill"}, skillNames)

	tool, err := registry.Get(ctx, "approved-skill")
	require.NoError(t, err)
	parameterized, ok := tool.(tools.Parameterized)
	require.True(t, ok)
	assert.Equal(t, map[string]any{
		"type":                 "object",
		"properties":           map[string]any{"query": map[string]any{"type": "string"}},
		"required":             []any{"query"},
		"additionalProperties": false,
	}, parameterized.Parameters())
	result, err := tool.Execute(ctx, tools.ToolCall{ID: "skill-test", Name: tool.Name(), Args: map[string]any{"query": "captured argument"}})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "approved prompt")
	assert.Contains(t, result.Output, `{"query":"captured argument"}`)
	assert.NotContains(t, result.Output, "changed prompt")
	assert.NotContains(t, result.Output, "added prompt")
	assert.Equal(t, []string{"read"}, result.Metadata["tools"])
	assert.Equal(t, map[string]any{"mode": "captured-default"}, result.Metadata["parameters"])
	assert.Equal(t, map[string]any{"query": "captured argument"}, result.Metadata["arguments"])

	flatTool, err := registry.Get(ctx, "flat-skill")
	require.NoError(t, err)
	flatResult, err := flatTool.Execute(ctx, tools.ToolCall{ID: "flat-skill-test", Name: flatTool.Name()})
	require.NoError(t, err)
	assert.Contains(t, flatResult.Output, "flat prompt")

	current, err := config.LoadTrustedWithManager(ctx, manager)
	require.NoError(t, err)
	assert.Nil(t, current.VerifiedProjectSources(), "the next live source verification rejects the disk changes")
	assert.Equal(t, config.ProjectSourceTrustChanged, current.ProjectSourceTrustStatus)
}

func TestProjectSkillToolStaysRevokedUntilFreshAssembly(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	skillDir := filepath.Join(root, ".go-cli", "skills")
	writeProjectSkill(t, filepath.Join(skillDir, "review", "SKILL.md"), projectSkillContent("project-review", "approved project instructions"))
	writeProjectSkill(t, filepath.Join(root, ".go-cli.yaml"), "skill:\n  dir: .go-cli/skills\n")
	t.Chdir(root)
	home := t.TempDir()
	t.Setenv("HOME", home)
	trustDir := filepath.Join(home, ".config", "go-cli")
	require.NoError(t, os.MkdirAll(trustDir, 0o700))
	trustPath := filepath.Join(trustDir, "trust.json")
	manager := approval.NewDefaultTrustManager(approval.NewFileTrustStore(trustPath))
	uncurated, err := config.LoadTrustedWithManager(ctx, manager)
	require.NoError(t, err)
	snapshot := uncurated.CapturedProjectSourcesForGrant()
	require.NotNil(t, snapshot)
	frozen, ok := manager.(approval.FrozenProjectSourceTrust)
	require.True(t, ok)
	require.NoError(t, frozen.TrustProjectSourceSnapshot(ctx, snapshot))
	trusted, err := config.LoadTrustedWithManager(ctx, manager)
	require.NoError(t, err)
	require.NotNil(t, trusted.VerifiedProjectSources())

	oldRegistry := newPreparedProjectSkillRegistry(ctx, t, trusted)
	call := tools.ToolCall{ID: "skill-before-revoke", Name: "project-review"}
	prepared, err := oldRegistry.PrepareToolCall(ctx, call)
	require.NoError(t, err)
	result, err := prepared.Dispatch(ctx)
	require.NoError(t, err)
	assert.Contains(t, result.Output, "approved project instructions")

	stalePrepared, err := oldRegistry.PrepareToolCall(ctx, tools.ToolCall{ID: "skill-after-revoke", Name: "project-review"})
	require.NoError(t, err)

	revoker := approval.NewDefaultTrustManager(approval.NewFileTrustStore(trustPath))
	require.NoError(t, revoker.RevokeTrust(ctx, snapshot.CanonicalRoot()))
	_, err = stalePrepared.Dispatch(ctx)
	require.Error(t, err, "the already assembled project skill must reject the revoked generation")
	assert.False(t, stalePrepared.DispatchCommitted())

	grantor := approval.NewDefaultTrustManager(approval.NewFileTrustStore(trustPath))
	grant, ok := grantor.(approval.FrozenProjectSourceTrust)
	require.True(t, ok)
	require.NoError(t, grant.TrustProjectSourceSnapshot(ctx, snapshot))
	freshConfig, err := config.LoadTrustedWithManager(ctx, grantor)
	require.NoError(t, err)
	require.NotNil(t, freshConfig.VerifiedProjectSources())
	newRegistry := newPreparedProjectSkillRegistry(ctx, t, freshConfig)
	newCall := tools.ToolCall{ID: "skill-after-new-grant", Name: "project-review"}
	newInvocation, err := newRegistry.PrepareToolCall(ctx, newCall)
	require.NoError(t, err)
	newResult, err := newInvocation.Dispatch(ctx)
	require.NoError(t, err, "a fresh assembly may bind the newly granted source generation")
	assert.Contains(t, newResult.Output, "approved project instructions")

	_, err = stalePrepared.Dispatch(ctx)
	require.Error(t, err, "regranting must not repair the old skill adapter's generation")
	assert.False(t, stalePrepared.DispatchCommitted())
}

func newPreparedProjectSkillRegistry(ctx context.Context, t *testing.T, cfg *config.Config) *tools.MiddlewareToolRegistry {
	t.Helper()
	base := tools.NewDefaultToolRegistry()
	deferred := tools.NewDeferredToolRegistryAdapter(base)
	registry := tools.NewMiddlewareToolRegistry(deferred)
	require.NoError(t, registry.SetPreparedToolStages(tools.PreparedToolStages{}))
	require.NoError(t, registry.SetPreparedToolPolicy(func(_ context.Context, request tools.PreparedPolicyRequest) (tools.PreparedPolicyDecision, error) {
		return tools.PreparedPolicyDecision{Allowed: true, Generations: request.Generations}, nil
	}))
	require.Len(t, registerSkillTools(ctx, cfg, deferred), 1)
	return registry
}

func TestRegisterSkillToolsRejectsProjectDirectorySwappedToUserRoot(t *testing.T) {
	root := t.TempDir()
	skillDir := filepath.Join(root, ".go-cli", "skills")
	writeProjectSkill(t, filepath.Join(skillDir, "approved", "SKILL.md"), projectSkillContent("approved", "approved prompt"))
	writeProjectSkill(t, filepath.Join(root, ".go-cli.yaml"), "skill:\n  dir: .go-cli/skills\n")

	home := t.TempDir()
	t.Setenv("HOME", home)
	userSkillDir := filepath.Join(home, ".config", "go-cli", "skills")
	writeProjectSkill(t, filepath.Join(userSkillDir, "substituted.md"), projectSkillContent("substituted", "substituted prompt"))
	t.Chdir(root)

	ctx := context.Background()
	manager := approval.NewDefaultTrustManager(approval.NewInMemoryTrustStore())
	uncurated, err := config.LoadTrustedWithManager(ctx, manager)
	require.NoError(t, err)
	snapshot := uncurated.CapturedProjectSourcesForGrant()
	require.NotNil(t, snapshot)
	frozen := manager.(approval.FrozenProjectSourceTrust)
	require.NoError(t, frozen.TrustProjectSourceSnapshot(ctx, snapshot))
	trusted, err := config.LoadTrustedWithManager(ctx, manager)
	require.NoError(t, err)
	require.NotNil(t, trusted.VerifiedProjectSources())

	require.NoError(t, os.RemoveAll(skillDir))
	if err := os.Symlink(userSkillDir, skillDir); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	registry := tools.NewDeferredToolRegistryAdapter(tools.NewDefaultToolRegistry())
	infos := registerSkillTools(ctx, trusted, registry)
	assert.Empty(t, infos)
	registered, err := registry.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, registered)
}

func TestRegisterSkillToolsAllowsCanonicalUserDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := t.TempDir()
	t.Chdir(root)
	userSkillDir := filepath.Join(home, ".config", "go-cli", "skills")
	writeProjectSkill(t, filepath.Join(userSkillDir, "personal.md"), projectSkillContent("personal", "personal prompt"))

	registry := tools.NewDeferredToolRegistryAdapter(tools.NewDefaultToolRegistry())
	config := &config.Config{Skill: config.SkillConfig{Dir: userSkillDir}}

	infos := registerSkillTools(context.Background(), config, registry)
	require.Len(t, infos, 1)
	assert.Equal(t, "personal", infos[0].Name)
}

func writeProjectSkill(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

func projectSkillContent(name, prompt string) string {
	return "---\nname: " + name + "\ndescription: " + name + " description\nprompt: " + prompt + "\n---\n"
}
