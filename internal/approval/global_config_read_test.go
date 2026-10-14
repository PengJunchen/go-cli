package approval

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadGlobalConfigFileReadsPrivateRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	want := []byte(`{"servers":[]}`)
	require.NoError(t, os.WriteFile(path, want, 0o600))

	got, err := ReadGlobalConfigFile(path)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestReadGlobalConfigFileRejectsPrivateSymlinkTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.json")
	path := filepath.Join(dir, "mcp.json")
	require.NoError(t, os.WriteFile(target, []byte(`{"servers":[]}`), 0o600))
	require.NoError(t, os.Symlink(target, path))

	_, err := ReadGlobalConfigFile(path)
	assert.Error(t, err, "global MCP sources must not follow a symlink, even when its target is private")
}

func TestReadGlobalConfigFileRejectsSymlinkSwapBeforeOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	target := filepath.Join(dir, "attacker.json")
	saved := filepath.Join(dir, "validated.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"servers":[{"name":"validated"}]}`), 0o600))
	require.NoError(t, os.WriteFile(target, []byte(`{"servers":[{"name":"attacker"}]}`), 0o600))

	_, err := readGlobalConfigFileWithOps(path, globalConfigReadOps{
		lstat: os.Lstat,
		open: func(openPath string) (*os.File, error) {
			if renameErr := os.Rename(openPath, saved); renameErr != nil {
				return nil, renameErr
			}
			if symlinkErr := os.Symlink(target, openPath); symlinkErr != nil {
				return nil, symlinkErr
			}
			return os.Open(openPath)
		},
	})
	assert.Error(t, err, "opened bytes must come from the same inode that passed the initial path check")
}

func TestReadGlobalConfigFileRejectsPathSwapAfterOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	target := filepath.Join(dir, "attacker.json")
	saved := filepath.Join(dir, "validated.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"servers":[{"name":"validated"}]}`), 0o600))
	require.NoError(t, os.WriteFile(target, []byte(`{"servers":[{"name":"attacker"}]}`), 0o600))

	lstatCalls := 0
	_, err := readGlobalConfigFileWithOps(path, globalConfigReadOps{
		lstat: func(checkPath string) (os.FileInfo, error) {
			lstatCalls++
			if lstatCalls == 2 {
				if renameErr := os.Rename(checkPath, saved); renameErr != nil {
					return nil, renameErr
				}
				if symlinkErr := os.Symlink(target, checkPath); symlinkErr != nil {
					return nil, symlinkErr
				}
			}
			return os.Lstat(checkPath)
		},
		open: os.Open,
	})
	assert.Error(t, err, "the path must still name the opened inode after the read")
}
