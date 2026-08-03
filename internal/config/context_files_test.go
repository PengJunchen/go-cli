package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewContextFileLoaderDefaults(t *testing.T) {
	l := NewContextFileLoader()
	assert.Equal(t, []string{"AGENTS.md", "CLAUDE.md", "SYSTEM.md"}, l.files)
}

func TestContextFileLoaderLoadAllFound(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("agents body"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("claude body"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "SYSTEM.md"), []byte("system body"), 0o644))

	l := NewContextFileLoader()
	files, err := l.Load(context.Background(), dir)
	require.NoError(t, err)
	require.Len(t, files, 3)

	assert.Equal(t, "agents body", files[0].Content)
	assert.Equal(t, 0, files[0].Priority)
	assert.Equal(t, "claude body", files[1].Content)
	assert.Equal(t, 1, files[1].Priority)
	assert.Equal(t, "system body", files[2].Content)
	assert.Equal(t, 2, files[2].Priority)
}

func TestContextFileLoaderLoadSkipsMissing(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("only claude"), 0o644))

	l := NewContextFileLoader()
	files, err := l.Load(context.Background(), dir)
	require.NoError(t, err)
	require.Len(t, files, 1)
	assert.Equal(t, "only claude", files[0].Content)
}

func TestContextFileLoaderLoadEmptyDir(t *testing.T) {
	dir := t.TempDir()
	l := NewContextFileLoader()
	files, err := l.Load(context.Background(), dir)
	require.NoError(t, err)
	assert.Empty(t, files)
}

func TestContextFileLoaderMergePriorityOrder(t *testing.T) {
	l := NewContextFileLoader()
	files := []ContextFile{
		{Path: "SYSTEM.md", Content: "system", Priority: 2},
		{Path: "AGENTS.md", Content: "agents", Priority: 0},
		{Path: "CLAUDE.md", Content: "claude", Priority: 1},
	}
	merged := l.Merge(files)
	assert.Equal(t, "agents\n\nclaude\n\nsystem", merged)
}

func TestContextFileLoaderMergeSkipsEmpty(t *testing.T) {
	l := NewContextFileLoader()
	files := []ContextFile{
		{Path: "AGENTS.md", Content: "agents", Priority: 0},
		{Path: "CLAUDE.md", Content: "  \n  ", Priority: 1},
		{Path: "SYSTEM.md", Content: "system", Priority: 2},
	}
	merged := l.Merge(files)
	assert.Equal(t, "agents\n\nsystem", merged)
}

func TestContextFileLoaderWithFiles(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "CUSTOM.md"), []byte("custom"), 0o644))

	l := NewContextFileLoader().WithFiles([]string{"CUSTOM.md"})
	files, err := l.Load(context.Background(), dir)
	require.NoError(t, err)
	require.Len(t, files, 1)
	assert.Equal(t, "custom", files[0].Content)
}
