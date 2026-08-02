package skill_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/skill"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// Load returns a wrapped open error when the file does not exist.
func TestYAMLLoaderLoadMissingFile(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	loader := skill.NewYAMLSkillLoader()
	_, err := loader.Load(context.Background(), filepath.Join(t.TempDir(), "missing.md"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "skill")
}

// LoadDir with a bad top-level path surfaces a scan error.
func TestYAMLLoaderLoadDirBadRoot(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	loader := skill.NewYAMLSkillLoader()
	_, err := loader.LoadDir(context.Background(), filepath.Join(t.TempDir(), "no-such-dir"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "skill")
}

// LoadDir skips files that fail to parse and still returns the good ones.
func TestYAMLLoaderLoadDirSkipsBadFiles(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	dir := t.TempDir()
	writeSkillFile(t, dir, "good.md", "---\nname: good\n---\nbody good\n")
	writeSkillFile(t, dir, "bad.md", "no frontmatter here\n")

	loader := skill.NewYAMLSkillLoader()
	defs, err := loader.LoadDir(context.Background(), dir)
	require.NoError(t, err)
	require.Len(t, defs, 1)
	assert.Equal(t, "good", (*defs[0]).Name())
}

// LoadDir ignores directories' own entries and respects the .yaml/.yml suffixes.
func TestYAMLLoaderLoadDirYamlFormats(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	dir := t.TempDir()
	writeSkillFile(t, dir, "a.yaml", "---\nname: a\n---\n")
	writeSkillFile(t, dir, "b.yml", "---\nname: b\n---\n")

	loader := skill.NewYAMLSkillLoader()
	defs, err := loader.LoadDir(context.Background(), dir)
	require.NoError(t, err)
	assert.Len(t, defs, 2)
}

// Registry Match returns nil for an empty/whitespace hint.
func TestDefaultRegistryMatchEmptyHint(t *testing.T) {
	reg := skill.NewDefaultSkillRegistry()
	ctx := context.Background()
	require.NoError(t, reg.Register(ctx, skill.NewSkill("x", skill.WithDescription("d"))))
	assert.Nil(t, reg.Match(ctx, ""))
	assert.Nil(t, reg.Match(ctx, "   "))
}

// Registry List copy cannot be mutated through the caller.
func TestDefaultRegistryListReturnsCopy(t *testing.T) {
	reg := skill.NewDefaultSkillRegistry()
	ctx := context.Background()
	require.NoError(t, reg.Register(ctx, skill.NewSkill("a", skill.WithCategory("c1"))))

	list := reg.List(ctx)
	require.Len(t, list, 1)
	list[0] = nil // mutation must not affect the registry

	again := reg.List(ctx)
	require.Len(t, again, 1)
	assert.Equal(t, "a", again[0].Name())
}

// Registry category filtering with overlapping categories is deduplicated.
func TestDefaultRegistryListByCategoryDedup(t *testing.T) {
	reg := skill.NewDefaultSkillRegistry()
	ctx := context.Background()
	require.NoError(t, reg.Register(ctx, skill.NewSkill("shared", skill.WithCategory("coding"))))
	require.NoError(t, reg.Register(ctx, skill.NewSkill("other", skill.WithCategory("writing"))))

	got := reg.List(ctx, "coding", "coding") // repeated + matching category
	require.Len(t, got, 1)
	assert.Equal(t, "shared", got[0].Name())
}

// Registry match ties are broken by name for stable ordering.
func TestDefaultRegistryMatchStableOrderByScore(t *testing.T) {
	reg := skill.NewDefaultSkillRegistry()
	ctx := context.Background()
	require.NoError(t, reg.Register(ctx, skill.NewSkill("b-doc", skill.WithDescription("write docs"))))
	require.NoError(t, reg.Register(ctx, skill.NewSkill("a-doc", skill.WithDescription("write docs"))))

	got := reg.Match(ctx, "write docs")
	require.Len(t, got, 2)
	// Both score 2 (description match); tie-break orders by name ascending.
	assert.Equal(t, "a-doc", got[0].Name())
	assert.Equal(t, "b-doc", got[1].Name())
}

// Registry overall concurrent Register/Get/List/Match/Unregister.
func TestDefaultRegistryConcurrent(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	reg := skill.NewDefaultSkillRegistry()

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			name := string(rune('a' + g))
			require.NoError(t, reg.Register(ctx, skill.NewSkill(name, skill.WithCategory("cat"))))
			_, _ = reg.Get(ctx, name)
			_ = reg.List(ctx)
			_ = reg.Match(ctx, name)
			err := reg.Unregister(ctx, name)
			_ = err
		}(g)
	}
	wg.Wait()
	assert.Empty(t, reg.List(ctx))
}

// Adapter Description omits tool/parameter sections when none are declared.
func TestSkillAdapterDescriptionWithoutToolsOrParams(t *testing.T) {
	adapter := skill.NewSkillAdapter(skill.NewSkill("minimal", skill.WithDescription("just text")))
	desc := adapter.Description()
	assert.Equal(t, "just text", desc)
	assert.NotContains(t, desc, "tools:")
	assert.NotContains(t, desc, "parameters:")
}

// Adapter Execute with a nil def fails cleanly.
func TestSkillAdapterExecuteNilDef(t *testing.T) {
	adapter := skill.NewSkillAdapter(nil)
	_, err := adapter.Execute(context.Background(), tools.ToolCall{ID: "c"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil")
}

// Process-wide loader/registry registries default and restore custom values.
func TestSkillProcessRegistries(t *testing.T) {
	origLoader, origRegistry := skill.GetSkillLoader(), skill.GetSkillRegistry()

	// Lazy defaults.
	assert.NotNil(t, skill.GetSkillLoader())
	assert.NotNil(t, skill.GetSkillRegistry())

	customLoader := &stubLoader{}
	customReg := &stubRegistry{}
	skill.RegisterSkillLoader(customLoader)
	skill.RegisterSkillRegistry(customReg)
	assert.Equal(t, customLoader, skill.GetSkillLoader())
	assert.Equal(t, customReg, skill.GetSkillRegistry())

	// Restore.
	skill.RegisterSkillLoader(origLoader)
	skill.RegisterSkillRegistry(origRegistry)
}

// Process registry concurrent access under the race detector.
func TestSkillProcessRegistriesConcurrent(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	var wg sync.WaitGroup
	for g := 0; g < 6; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			if g%2 == 0 {
				skill.RegisterSkillLoader(nil)
				skill.RegisterSkillRegistry(nil)
			} else {
				skill.RegisterSkillLoader(&stubLoader{})
				skill.RegisterSkillRegistry(&stubRegistry{})
			}
			_ = skill.GetSkillLoader()
			_ = skill.GetSkillRegistry()
		}(g)
	}
	wg.Wait()
}

type stubLoader struct{}

func (s *stubLoader) Load(ctx context.Context, _ string) (*skill.SkillDefinition, error) {
	return nil, errors.New("stub load")
}

func (s *stubLoader) LoadDir(context.Context, string) ([]*skill.SkillDefinition, error) {
	return nil, nil
}

type stubRegistry struct{}

func (s *stubRegistry) Register(context.Context, skill.SkillDefinition) error { return nil }
func (s *stubRegistry) Get(context.Context, string) (skill.SkillDefinition, bool) {
	return nil, false
}
func (s *stubRegistry) List(context.Context, ...string) []skill.SkillDefinition { return nil }
func (s *stubRegistry) Match(context.Context, string) []skill.SkillDefinition   { return nil }
func (s *stubRegistry) Unregister(context.Context, string) error                { return nil }
