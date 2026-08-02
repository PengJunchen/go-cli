//go:build mock

package mock

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/skill"
)

// ---------------------------------------------------------------------------
// MockSkillLoader
// ---------------------------------------------------------------------------

func TestNewMockSkillLoaderReturnsNonNil(t *testing.T) {
	l := NewMockSkillLoader()
	require.NotNil(t, l)
}

func TestMockSkillLoaderDefaultLoadReturnsNil(t *testing.T) {
	l := NewMockSkillLoader()
	def, err := l.Load(context.Background(), "/some/path")
	require.NoError(t, err)
	assert.Nil(t, def, "default Load should return nil definition")
}

func TestMockSkillLoaderDefaultLoadDirReturnsNil(t *testing.T) {
	l := NewMockSkillLoader()
	defs, err := l.LoadDir(context.Background(), "/some/dir")
	require.NoError(t, err)
	assert.Nil(t, defs, "default LoadDir should return nil definitions")
}

func TestMockSkillLoaderWithLoadOption(t *testing.T) {
	wantDef := skill.NewSkill("custom-skill", skill.WithCategory("coding"))
	l := NewMockSkillLoader(WithLoad(func(_ context.Context, path string) (*skill.SkillDefinition, error) {
		if path == "/skills/custom.md" {
			base := skill.SkillDefinition(wantDef)
			return &base, nil
		}
		return nil, nil
	}))

	def, err := l.Load(context.Background(), "/skills/custom.md")
	require.NoError(t, err)
	require.NotNil(t, def)
	assert.Equal(t, "custom-skill", (*def).Name())
}

func TestMockSkillLoaderWithLoadError(t *testing.T) {
	wantErr := errors.New("load error")
	l := NewMockSkillLoader(WithLoad(func(_ context.Context, _ string) (*skill.SkillDefinition, error) {
		return nil, wantErr
	}))

	def, err := l.Load(context.Background(), "/fail")
	assert.Nil(t, def)
	assert.ErrorIs(t, err, wantErr)
}

func TestMockSkillLoaderWithLoadDirOption(t *testing.T) {
	d1 := skill.NewSkill("s1", skill.WithCategory("coding"))
	d2 := skill.NewSkill("s2", skill.WithCategory("writing"))
	l := NewMockSkillLoader(WithLoadDir(func(_ context.Context, dir string) ([]*skill.SkillDefinition, error) {
		if dir == "/skills" {
			b1 := skill.SkillDefinition(d1)
			b2 := skill.SkillDefinition(d2)
			return []*skill.SkillDefinition{&b1, &b2}, nil
		}
		return nil, nil
	}))

	defs, err := l.LoadDir(context.Background(), "/skills")
	require.NoError(t, err)
	require.Len(t, defs, 2)
	assert.Equal(t, "s1", (*defs[0]).Name())
	assert.Equal(t, "s2", (*defs[1]).Name())
}

func TestMockSkillLoaderWithLoadDirError(t *testing.T) {
	wantErr := errors.New("dir error")
	l := NewMockSkillLoader(WithLoadDir(func(_ context.Context, _ string) ([]*skill.SkillDefinition, error) {
		return nil, wantErr
	}))

	defs, err := l.LoadDir(context.Background(), "/bad-dir")
	assert.Nil(t, defs)
	assert.ErrorIs(t, err, wantErr)
}

func TestMockSkillLoaderLoadCount(t *testing.T) {
	l := NewMockSkillLoader()
	assert.Equal(t, 0, l.LoadCount())

	_, _ = l.Load(context.Background(), "/a")
	assert.Equal(t, 1, l.LoadCount())

	_, _ = l.Load(context.Background(), "/b")
	_, _ = l.Load(context.Background(), "/c")
	assert.Equal(t, 3, l.LoadCount())
}

func TestMockSkillLoaderLoadDirCalls(t *testing.T) {
	l := NewMockSkillLoader()
	assert.Empty(t, l.LoadDirCalls())

	_, _ = l.LoadDir(context.Background(), "/dir1")
	assert.Equal(t, []string{"/dir1"}, l.LoadDirCalls())

	_, _ = l.LoadDir(context.Background(), "/dir2")
	calls := l.LoadDirCalls()
	assert.Equal(t, []string{"/dir1", "/dir2"}, calls)

	// Verify the returned slice is a copy (mutating it should not affect the loader).
	calls[0] = "mutated"
	assert.Equal(t, "/dir1", l.LoadDirCalls()[0], "LoadDirCalls should return a copy")
}

func TestMockSkillLoaderConcurrentAccess(t *testing.T) {
	l := NewMockSkillLoader(WithLoad(func(_ context.Context, path string) (*skill.SkillDefinition, error) {
		return nil, nil
	}))

	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines * 2)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			_, _ = l.Load(context.Background(), "/path")
		}()
	}
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			_, _ = l.LoadDir(context.Background(), "/dir")
		}()
	}

	wg.Wait()
	assert.Equal(t, goroutines, l.LoadCount())
	assert.Equal(t, goroutines, len(l.LoadDirCalls()))
}

// ---------------------------------------------------------------------------
// MockSkillRegistry
// ---------------------------------------------------------------------------

func TestNewMockSkillRegistryReturnsNonNil(t *testing.T) {
	r := NewMockSkillRegistry()
	require.NotNil(t, r)
}

func TestMockSkillRegistryRegisterAndGet(t *testing.T) {
	r := NewMockSkillRegistry()
	s := skill.NewSkill("my-skill", skill.WithDescription("does things"))

	err := r.Register(context.Background(), s)
	require.NoError(t, err)
	assert.Equal(t, 1, r.RegisterCount())

	got, ok := r.Get(context.Background(), "my-skill")
	assert.True(t, ok, "Get should find registered skill")
	assert.Equal(t, "my-skill", got.Name())
	assert.Equal(t, "does things", got.Description())
}

func TestMockSkillRegistryGetMissing(t *testing.T) {
	r := NewMockSkillRegistry()
	_, ok := r.Get(context.Background(), "nope")
	assert.False(t, ok, "Get should not find unregistered skill")
}

func TestMockSkillRegistryRegisterOverwrites(t *testing.T) {
	r := NewMockSkillRegistry()
	v1 := skill.NewSkill("s", skill.WithDescription("v1"))
	v2 := skill.NewSkill("s", skill.WithDescription("v2"))

	require.NoError(t, r.Register(context.Background(), v1))
	require.NoError(t, r.Register(context.Background(), v2))

	got, ok := r.Get(context.Background(), "s")
	assert.True(t, ok)
	assert.Equal(t, "v2", got.Description(), "last Register should win")
	assert.Equal(t, 2, r.RegisterCount())
}

func TestMockSkillRegistryListAll(t *testing.T) {
	r := NewMockSkillRegistry()
	s1 := skill.NewSkill("a", skill.WithCategory("coding"))
	s2 := skill.NewSkill("b", skill.WithCategory("writing"))

	require.NoError(t, r.Register(context.Background(), s1))
	require.NoError(t, r.Register(context.Background(), s2))

	all := r.List(context.Background())
	assert.Len(t, all, 2)
}

func TestMockSkillRegistryListByCategory(t *testing.T) {
	r := NewMockSkillRegistry()
	s1 := skill.NewSkill("a", skill.WithCategory("coding"))
	s2 := skill.NewSkill("b", skill.WithCategory("writing"))
	s3 := skill.NewSkill("c", skill.WithCategory("coding"))

	require.NoError(t, r.Register(context.Background(), s1))
	require.NoError(t, r.Register(context.Background(), s2))
	require.NoError(t, r.Register(context.Background(), s3))

	coding := r.List(context.Background(), "coding")
	assert.Len(t, coding, 2, "should only return skills with category 'coding'")

	writing := r.List(context.Background(), "writing")
	assert.Len(t, writing, 1)

	other := r.List(context.Background(), "nonexistent")
	assert.Empty(t, other)
}

func TestMockSkillRegistryMatch(t *testing.T) {
	r := NewMockSkillRegistry()
	s1 := skill.NewSkill("refactor", skill.WithCategory("coding"))
	s2 := skill.NewSkill("translate", skill.WithCategory("writing"))

	require.NoError(t, r.Register(context.Background(), s1))
	require.NoError(t, r.Register(context.Background(), s2))

	matched := r.Match(context.Background(), "refactor")
	require.Len(t, matched, 1)
	assert.Equal(t, "refactor", matched[0].Name())

	noMatch := r.Match(context.Background(), "deploy")
	assert.Empty(t, noMatch)
}

func TestMockSkillRegistryUnregister(t *testing.T) {
	r := NewMockSkillRegistry()
	s := skill.NewSkill("to-remove", skill.WithCategory("coding"))

	require.NoError(t, r.Register(context.Background(), s))
	assert.Equal(t, 1, r.RegisterCount())

	err := r.Unregister(context.Background(), "to-remove")
	require.NoError(t, err)
	assert.Equal(t, 1, r.UnregisterCount())

	_, ok := r.Get(context.Background(), "to-remove")
	assert.False(t, ok, "skill should be removed after Unregister")
}

func TestMockSkillRegistryUnregisterMissing(t *testing.T) {
	r := NewMockSkillRegistry()
	err := r.Unregister(context.Background(), "never-existed")
	require.NoError(t, err, "Unregister should not error on missing key")
	assert.Equal(t, 1, r.UnregisterCount())
}

func TestMockSkillRegistryConcurrentAccess(t *testing.T) {
	r := NewMockSkillRegistry()
	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines * 3)

	// Concurrent Register
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			s := skill.NewSkill("skill-"+string(rune(i)), skill.WithCategory("cat"))
			_ = r.Register(context.Background(), s)
		}(i)
	}
	// Concurrent Get
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			_, _ = r.Get(context.Background(), "skill-0")
		}()
	}
	// Concurrent List
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			_ = r.List(context.Background())
		}()
	}

	wg.Wait()
	// Test passes if no race condition is detected.
}
