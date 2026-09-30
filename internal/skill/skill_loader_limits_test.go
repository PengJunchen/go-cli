package skill

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSkillLoaderHonorsLimitsAndCancel(t *testing.T) {
	t.Run("single file byte limit", func(t *testing.T) {
		path := writeLimitedSkill(t, t.TempDir(), "large.md", testMaxSkillFileBytes+1)
		defs, err := NewYAMLSkillLoader().LoadDir(context.Background(), filepath.Dir(path))
		require.ErrorIs(t, err, ErrSkillResourceLimit)
		assert.Nil(t, defs, "resource failure must not expose earlier definitions")
	})

	t.Run("candidate file count", func(t *testing.T) {
		root := t.TempDir()
		for i := 0; i <= testMaxSkillCandidateFiles; i++ {
			writeLimitedSkill(t, root, fmt.Sprintf("skill-%03d.md", i), int64(len(skillFixture)))
		}
		defs, err := NewYAMLSkillLoader().LoadDir(context.Background(), root)
		require.ErrorIs(t, err, ErrSkillResourceLimit)
		assert.Nil(t, defs)
	})

	t.Run("walk entry count", func(t *testing.T) {
		root := t.TempDir()
		for i := 0; i <= testMaxSkillWalkEntries; i++ {
			require.NoError(t, os.WriteFile(filepath.Join(root, fmt.Sprintf("data-%04d.txt", i)), nil, 0o600))
		}
		defs, err := NewYAMLSkillLoader().LoadDir(context.Background(), root)
		require.ErrorIs(t, err, ErrSkillResourceLimit)
		assert.Nil(t, defs)
	})

	t.Run("directory depth", func(t *testing.T) {
		root := t.TempDir()
		parts := make([]string, testMaxSkillDirectoryDepth+1)
		for i := range parts {
			parts[i] = "d"
		}
		require.NoError(t, os.MkdirAll(filepath.Join(append([]string{root}, parts...)...), 0o700))
		defs, err := NewYAMLSkillLoader().LoadDir(context.Background(), root)
		require.ErrorIs(t, err, ErrSkillResourceLimit)
		assert.Nil(t, defs)
	})

	t.Run("total source bytes", func(t *testing.T) {
		root := t.TempDir()
		for i := 0; i < 16; i++ {
			writeLimitedSkill(t, root, fmt.Sprintf("skill-%02d.md", i), testMaxSkillFileBytes)
		}
		writeLimitedSkill(t, root, "skill-over.md", 1)
		defs, err := NewYAMLSkillLoader().LoadDir(context.Background(), root)
		require.ErrorIs(t, err, ErrSkillResourceLimit)
		assert.Nil(t, defs)
	})

	t.Run("cancellation while walking", func(t *testing.T) {
		root := t.TempDir()
		for i := 0; i < 32; i++ {
			require.NoError(t, os.WriteFile(filepath.Join(root, fmt.Sprintf("data-%02d.txt", i)), nil, 0o600))
		}
		ctx := newCancelOnErrContext(8)
		defs, err := NewYAMLSkillLoader().LoadDir(ctx, root)
		require.ErrorIs(t, err, context.Canceled)
		assert.Nil(t, defs)
	})

	t.Run("cancellation during file scan", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "many-lines.md")
		content := "---\nname: scanning\n---\n" + strings.Repeat("line\n", 20_000)
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
		ctx := newCancelOnErrContext(12)
		def, err := NewYAMLSkillLoader().Load(ctx, path)
		require.ErrorIs(t, err, context.Canceled)
		assert.Nil(t, def)
	})

	t.Run("candidate symlink", func(t *testing.T) {
		root := t.TempDir()
		writeLimitedSkill(t, root, "a-valid.md", int64(len(skillFixture)))
		outside := writeLimitedSkill(t, t.TempDir(), "outside.md", int64(len(skillFixture)))
		require.NoError(t, os.Symlink(outside, filepath.Join(root, "z-linked.md")))
		defs, err := NewYAMLSkillLoader().LoadDir(context.Background(), root)
		require.ErrorIs(t, err, ErrSkillUnsafeSource)
		assert.Nil(t, defs)
	})

	t.Run("candidate directory is non-regular", func(t *testing.T) {
		root := t.TempDir()
		writeLimitedSkill(t, root, "a-valid.md", int64(len(skillFixture)))
		require.NoError(t, os.Mkdir(filepath.Join(root, "z-not-a-file.md"), 0o700))
		defs, err := NewYAMLSkillLoader().LoadDir(context.Background(), root)
		require.ErrorIs(t, err, ErrSkillUnsafeSource)
		assert.Nil(t, defs)
	})
}

func TestSkillLoadSourcesEnforcesAggregateBoundsBeforeReturningDefinitions(t *testing.T) {
	loader := NewYAMLSkillLoader().(*YAMLSkillLoader)
	sources := make([]SkillSourceFile, 0, testMaxSkillCandidateFiles+1)
	for i := 0; i <= testMaxSkillCandidateFiles; i++ {
		sources = append(sources, SkillSourceFile{
			RelativePath: fmt.Sprintf("skill-%03d.md", i),
			Content:      []byte(skillFixture),
		})
	}
	defs, err := loader.LoadSources(context.Background(), sources)
	require.ErrorIs(t, err, ErrSkillResourceLimit)
	assert.Nil(t, defs)

	tooLarge := []SkillSourceFile{{RelativePath: "large.md", Content: []byte(strings.Repeat("x", testMaxSkillFileBytes+1))}}
	defs, err = loader.LoadSources(context.Background(), tooLarge)
	require.ErrorIs(t, err, ErrSkillResourceLimit)
	assert.Nil(t, defs)

	large := []byte(skillFixture + strings.Repeat("x", testMaxSkillFileBytes-len(skillFixture)))
	tooMuch := make([]SkillSourceFile, 0, 17)
	for i := 0; i < 16; i++ {
		tooMuch = append(tooMuch, SkillSourceFile{RelativePath: fmt.Sprintf("large-%02d.md", i), Content: large})
	}
	tooMuch = append(tooMuch, SkillSourceFile{RelativePath: "over.md", Content: []byte("x")})
	defs, err = loader.LoadSources(context.Background(), tooMuch)
	require.ErrorIs(t, err, ErrSkillResourceLimit)
	assert.Nil(t, defs)
}

func TestSkillLoadSourcesRejectsUncleanAndDuplicatePathsWithoutPartialDefinitions(t *testing.T) {
	loader := NewYAMLSkillLoader().(*YAMLSkillLoader)
	for _, tt := range []struct {
		name    string
		sources []SkillSourceFile
	}{
		{
			name: "path escapes source directory",
			sources: []SkillSourceFile{
				{RelativePath: "valid.md", Content: []byte(skillFixture)},
				{RelativePath: "../escape.md", Content: []byte(skillFixture)},
			},
		},
		{
			name: "duplicate source path",
			sources: []SkillSourceFile{
				{RelativePath: "duplicate.md", Content: []byte(skillFixture)},
				{RelativePath: "duplicate.md", Content: []byte(skillFixture)},
			},
		},
		{
			name: "non-candidate extension",
			sources: []SkillSourceFile{
				{RelativePath: "valid.md", Content: []byte(skillFixture)},
				{RelativePath: "payload.txt", Content: []byte(skillFixture)},
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			defs, err := loader.LoadSources(context.Background(), tt.sources)
			require.ErrorIs(t, err, ErrSkillUnsafeSource)
			assert.Nil(t, defs)
		})
	}
}

func TestSkillLoadSourcesCancellationDoesNotReturnPartialDefinitions(t *testing.T) {
	loader := NewYAMLSkillLoader().(*YAMLSkillLoader)
	content := "---\nname: scanning\n---\n" + strings.Repeat("line\n", 20_000)
	ctx := newCancelOnErrContext(12)
	defs, err := loader.LoadSources(ctx, []SkillSourceFile{{RelativePath: "many-lines.md", Content: []byte(content)}})
	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, defs)
}

const skillFixture = "---\nname: bounded\n---\nbody\n"

const (
	testMaxSkillFileBytes      = 1 << 20
	testMaxSkillCandidateFiles = 128
	testMaxSkillWalkEntries    = 4096
	testMaxSkillDirectoryDepth = 128
)

func writeLimitedSkill(t *testing.T, root, name string, size int64) string {
	t.Helper()
	path := filepath.Join(root, name)
	content := []byte(skillFixture)
	if size > int64(len(content)) {
		content = append(content, []byte(strings.Repeat("x", int(size)-len(content)))...)
	}
	require.NoError(t, os.WriteFile(path, content, 0o600))
	return path
}

type cancelOnErrContext struct {
	context.Context
	cancel   context.CancelFunc
	checks   int
	cancelAt int
}

func newCancelOnErrContext(cancelAt int) *cancelOnErrContext {
	base, cancel := context.WithCancel(context.Background())
	return &cancelOnErrContext{Context: base, cancel: cancel, cancelAt: cancelAt}
}

func (c *cancelOnErrContext) Err() error {
	c.checks++
	if c.checks >= c.cancelAt {
		c.cancel()
	}
	return c.Context.Err()
}
