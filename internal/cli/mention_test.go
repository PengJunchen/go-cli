package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMentionExpander_ExpandsExistingFile verifies that @foo.txt is replaced
// with an inline <file> XML block containing the file contents.
func TestMentionExpander_ExpandsExistingFile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "foo.txt"), []byte("hello world"), 0644))

	e := NewMentionExpander(dir, 0)
	result, files, totalBytes, err := e.Expand(context.Background(), "review @foo.txt please")
	require.NoError(t, err)

	assert.Contains(t, result, `<file path="foo.txt"><contents>hello world</contents></file>`)
	assert.Contains(t, result, "review ")
	assert.Contains(t, result, " please")
	assert.Equal(t, []string{"foo.txt"}, files)
	assert.Equal(t, len("hello world"), totalBytes)
}

// TestMentionExpander_NonexistentPathLeftAsIs verifies that @path tokens
// pointing to non-existent files are preserved verbatim.
func TestMentionExpander_NonexistentPathLeftAsIs(t *testing.T) {
	e := NewMentionExpander(t.TempDir(), 0)
	result, files, totalBytes, err := e.Expand(context.Background(), "check @nope.txt")
	require.NoError(t, err)

	assert.Equal(t, "check @nope.txt", result)
	assert.Empty(t, files)
	assert.Equal(t, 0, totalBytes)
}

// TestMentionExpander_EmailNotExpanded verifies that email addresses are not
// matched as @-mentions (the word char before @ prevents matching).
func TestMentionExpander_EmailNotExpanded(t *testing.T) {
	e := NewMentionExpander(t.TempDir(), 0)
	result, files, _, err := e.Expand(context.Background(), "email me@example.com")
	require.NoError(t, err)

	assert.Equal(t, "email me@example.com", result)
	assert.Empty(t, files)
}

// TestMentionExpander_TruncatesLargeFile verifies that files exceeding
// maxBytes are truncated with a notice appended.
func TestMentionExpander_TruncatesLargeFile(t *testing.T) {
	dir := t.TempDir()
	content := strings.Repeat("A", 20)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "big.txt"), []byte(content), 0644))

	e := NewMentionExpander(dir, 10) // maxBytes = 10
	result, files, totalBytes, err := e.Expand(context.Background(), "@big.txt")
	require.NoError(t, err)

	assert.Contains(t, result, "[truncated at 10 bytes]")
	assert.Contains(t, result, strings.Repeat("A", 10))
	assert.NotContains(t, result, strings.Repeat("A", 20))
	assert.Equal(t, []string{"big.txt"}, files)
	// totalBytes counts the returned content including truncation notice
	assert.Greater(t, totalBytes, 10)
}

// TestMentionExpander_MultipleFiles verifies that multiple @-mentions in a
// single input are all expanded.
func TestMentionExpander_MultipleFiles(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("AAA"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.txt"), []byte("BBB"), 0644))

	e := NewMentionExpander(dir, 0)
	result, files, totalBytes, err := e.Expand(context.Background(), "compare @a.txt and @b.txt")
	require.NoError(t, err)

	assert.Contains(t, result, `<file path="a.txt"><contents>AAA</contents></file>`)
	assert.Contains(t, result, `<file path="b.txt"><contents>BBB</contents></file>`)
	assert.Equal(t, []string{"a.txt", "b.txt"}, files)
	assert.Equal(t, 6, totalBytes)
}

// TestMentionExpander_EmailWithDomainFile verifies that me@example.com is not
// expanded even when a file named "example.com" exists in the working dir.
func TestMentionExpander_EmailWithDomainFile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "example.com"), []byte("domain content"), 0644))

	e := NewMentionExpander(dir, 0)
	result, files, _, err := e.Expand(context.Background(), "contact me@example.com")
	require.NoError(t, err)

	assert.Equal(t, "contact me@example.com", result)
	assert.Empty(t, files)
}

// TestMentionExpander_AbsolutePath verifies that absolute @/path tokens are
// resolved correctly regardless of cwd.
func TestMentionExpander_AbsolutePath(t *testing.T) {
	dir := t.TempDir()
	absFile := filepath.Join(dir, "abs.txt")
	require.NoError(t, os.WriteFile(absFile, []byte("absolute content"), 0644))

	// Use a different cwd to prove the absolute path is not joined with cwd.
	e := NewMentionExpander("/some/other/dir", 0)
	result, files, _, err := e.Expand(context.Background(), "@"+absFile)
	require.NoError(t, err)

	assert.Contains(t, result, `<file path="`+absFile+`"><contents>absolute content</contents></file>`)
	assert.Equal(t, []string{absFile}, files)
}

// TestMentionExpander_BinaryFilePlaceholder verifies that files containing null
// bytes (binary) are replaced with a placeholder instead of inlining content.
func TestMentionExpander_BinaryFilePlaceholder(t *testing.T) {
	dir := t.TempDir()
	binData := []byte{0x89, 0x50, 0x4E, 0x47, 0x00, 0x0D, 0x0A} // PNG-like header with null byte
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bin.dat"), binData, 0644))

	e := NewMentionExpander(dir, 0)
	result, files, _, err := e.Expand(context.Background(), "@bin.dat")
	require.NoError(t, err)

	assert.Contains(t, result, "[binary file,")
	assert.Contains(t, result, "bytes]")
	assert.NotContains(t, result, string(binData))
	assert.Equal(t, []string{"bin.dat"}, files)
}
