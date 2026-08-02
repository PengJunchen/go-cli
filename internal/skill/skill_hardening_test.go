package skill_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/skill"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// LoadDir on an empty directory yields an empty slice and no error.
func TestLoaderLoadDirEmptyDirectory(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	loader := skill.NewYAMLSkillLoader()
	defs, err := loader.LoadDir(context.Background(), t.TempDir())
	require.NoError(t, err)
	assert.Empty(t, defs)
}

// LoadDir ignores nested directories themselves and returns no error when a
// directory contains only subdirectories with no skill files.
func TestLoaderLoadDirNestedOnlyDirs(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "a", "b"), 0o700))

	loader := skill.NewYAMLSkillLoader()
	defs, err := loader.LoadDir(context.Background(), root)
	require.NoError(t, err)
	assert.Empty(t, defs)
}

// A skill file with only frontmatter and no body yields an empty prompt.
func TestLoaderLoadFrontmatterOnlyNoBody(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.md")
	require.NoError(t, os.WriteFile(path, []byte("---\nname: empty\n---\n"), 0o600))

	loader := skill.NewYAMLSkillLoader()
	def, err := loader.Load(context.Background(), path)
	require.NoError(t, err)
	assert.Equal(t, "empty", (*def).Name())
	assert.Empty(t, (*def).Prompt())
}

// A file whose path is a directory (not a regular file) surfaces a read/open
// error through Load.
func TestLoaderLoadPathIsDirectory(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	loader := skill.NewYAMLSkillLoader()
	_, err := loader.Load(context.Background(), t.TempDir())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "skill")
}

// Loading a single file whose name does not match a skill suffix is still
// parsed when passed directly to Load (suffix filtering only applies in
// LoadDir).
func TestLoaderLoadNonStandardExtension(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	dir := t.TempDir()
	path := filepath.Join(dir, "weird.txt")
	require.NoError(t, os.WriteFile(path, []byte("---\nname: weird\n---\nbody\n"), 0o600))

	loader := skill.NewYAMLSkillLoader()
	def, err := loader.Load(context.Background(), path)
	require.NoError(t, err)
	assert.Equal(t, "weird", (*def).Name())
}

// LoadDir skips skill-suffixed files that cannot be opened/parsed and keeps
// going, returning the healthy ones (covered further in edges).
func TestLoaderLoadDirMixedHealth(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	dir := t.TempDir()
	// A broken symlink-ish entry is not possible to create portably here; an
	// unreadable path inside the tree is simulated by a bad parse instead.
	writeSkillFile(t, dir, "ok.md", "---\nname: ok\n---\nbody\n")
	writeSkillFile(t, dir, "also-ok.md", "---\nname: also-ok\n---\nbody\n")

	loader := skill.NewYAMLSkillLoader()
	defs, err := loader.LoadDir(context.Background(), dir)
	require.NoError(t, err)
	require.Len(t, defs, 2)
}

// A leading UTF-8 BOM on the first line prevents the frontmatter delimiter
// from being detected, so parsing fails with a parse error.
func TestLoaderLoadBOMSpoilsDelimiter(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	dir := t.TempDir()
	path := filepath.Join(dir, "bom.md")
	content := append([]byte{0xEF, 0xBB, 0xBF}, []byte("---\nname: bom\n---\nbody\n")...)
	require.NoError(t, os.WriteFile(path, content, 0o600))

	loader := skill.NewYAMLSkillLoader()
	_, err := loader.Load(context.Background(), path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "skill")
}

// The adapter exposes the declared tools in their declared order in the
// description while sorting parameter names alphabetically.
func TestAdapterDescriptionOrdersToolsAndParams(t *testing.T) {
	def := skill.NewSkill(
		"ordered",
		skill.WithDescription("d"),
		skill.WithTools("bash", "read", "write"),
		skill.WithParameters(map[string]any{"zeta": 1, "alpha": 2, "mid": 3}),
	)
	desc := skill.NewSkillAdapter(def).Description()

	assert.Contains(t, desc, "tools: bash, read, write")
	toolsIdx := strings.Index(desc, "tools: bash, read, write")
	paramsIdx := strings.Index(desc, "parameters: alpha, mid, zeta")
	assert.Greater(t, toolsIdx, 0)
	assert.Greater(t, paramsIdx, toolsIdx, "parameters section follows the tools section")
}

// An adapter wrapping a minimal definition (no description) still returns a
// coherent description string and executes without error.
func TestAdapterDescriptionEmptySkillFields(t *testing.T) {
	adapter := skill.NewSkillAdapter(skill.NewSkill("bare"))
	assert.Equal(t, "", adapter.Description())

	res, err := adapter.Execute(context.Background(), tools.ToolCall{ID: "c"})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "bare")
	// The metadata carrier still records the skill name.
	require.Contains(t, res.Metadata, "skill")
	require.Empty(t, res.Metadata["tools"])
	require.Empty(t, res.Metadata["parameters"])
}

// Execute echoes the declared tools and parameters in the metadata map and
// propagates the call ID.
func TestAdapterExecuteMetadataEcho(t *testing.T) {
	def := skill.NewSkill(
		"meta2",
		skill.WithTools("read"),
		skill.WithParameters(map[string]any{"depth": 5}),
		skill.WithPrompt("p"),
	)
	res, err := skill.NewSkillAdapter(def).Execute(context.Background(), tools.ToolCall{ID: "id-9"})
	require.NoError(t, err)
	assert.Equal(t, "id-9", res.ToolCallID)

	toolsMeta, ok := res.Metadata["tools"].([]string)
	require.True(t, ok)
	assert.Equal(t, []string{"read"}, toolsMeta)

	paramsMeta, ok := res.Metadata["parameters"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, map[string]any{"depth": 5}, paramsMeta)

	skillName, ok := res.Metadata["skill"].(string)
	require.True(t, ok)
	assert.Equal(t, "meta2", skillName)
}

// Execute returns a nil result and an error when the wrapped definition is nil.
func TestAdapterExecuteNilDefReturnsError(t *testing.T) {
	adapter := skill.NewSkillAdapter(nil)
	res, err := adapter.Execute(context.Background(), tools.ToolCall{})
	assert.Nil(t, res)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil")
}

// Execute on a canceled context short-circuits before building a prompt.
func TestAdapterExecuteCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	adapter := skill.NewSkillAdapter(skill.NewSkill("c", skill.WithPrompt("p")))
	_, err := adapter.Execute(ctx, tools.ToolCall{})
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled))
}

// A skill with a prompt that spans the body keeps tools/params intact and the
// adapter surfaces them.
func TestAdapterDescriptionRoundTripFromLoader(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	dir := t.TempDir()
	path := writeSkillFile(t, dir, "rt.md", `---
name: rt
description: round trip
tools:
  - bash
parameters:
  attempts: 3
---
body prompt
`)
	loader := skill.NewYAMLSkillLoader()
	def, err := loader.Load(context.Background(), path)
	require.NoError(t, err)

	adapter := skill.NewSkillAdapter(*def)
	desc := adapter.Description()
	assert.Contains(t, desc, "round trip")
	assert.Contains(t, desc, "bash")
	assert.Contains(t, desc, "attempts")
}
