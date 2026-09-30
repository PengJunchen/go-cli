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
