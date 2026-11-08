//go:build darwin || linux

package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

type terraEditDiff struct {
	mutate     func() error
	previewErr error
}

func (d terraEditDiff) Generate(_ context.Context, old, updated, _ string) (string, error) {
	if old != "prefix old suffix\n" || updated != "prefix new suffix\n" {
		return "", fmt.Errorf("unexpected preview inputs")
	}
	if d.mutate != nil {
		if err := d.mutate(); err != nil {
			return "", err
		}
	}
	return "preview", d.previewErr
}

func assertEditConflict(t *testing.T, err error, dir string) {
	t.Helper()
	if !errors.Is(err, ErrEditConflict) {
		t.Errorf("expected ErrEditConflict, got %v", err)
	}
	var conflict *EditConflictError
	if !errors.As(err, &conflict) {
		t.Errorf("expected EditConflictError, got %v", err)
	}
	matches, globErr := filepath.Glob(filepath.Join(dir, ".write-*"))
	require.NoError(t, globErr)
	require.Empty(t, matches)
}

func TestEditPreviewConflictRegression(t *testing.T) {
	t.Run("unchanged_control", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "file.txt")
		require.NoError(t, os.WriteFile(path, []byte("prefix old suffix\n"), 0600))
		ft := NewFileTracker()
		tool := NewEditFileTool(WithEditDiffGenerator(terraEditDiff{}), WithEditFileTracker(ft))
		result, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{"file_path": path, "old_string": "old", "new_string": "new"}})
		got, rerr := os.ReadFile(path)
		require.NoError(t, rerr)
		t.Logf("control err=%v result=%v final=%q checkpoints=%d", err, result != nil, got, len(ft.ListCheckpoints()))
		require.NoError(t, err)
		require.Equal(t, "prefix new suffix\n", string(got))
		require.Len(t, ft.ListCheckpoints(), 1)
	})
	for _, ordinaryErr := range []bool{false, true} {
		name := "changed_content"
		if ordinaryErr {
			name = "changed_content_preview_error"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "file.txt")
			external := "prefix old suffix external\n"
			require.NoError(t, os.WriteFile(path, []byte("prefix old suffix\n"), 0600))
			ft := NewFileTracker()
			diff := terraEditDiff{mutate: func() error { return os.WriteFile(path, []byte(external), 0600) }}
			if ordinaryErr {
				diff.previewErr = errors.New("preview unavailable")
			}
			tool := NewEditFileTool(WithEditDiffGenerator(diff), WithEditFileTracker(ft))
			result, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{"file_path": path, "old_string": "old", "new_string": "new"}})
			got, rerr := os.ReadFile(path)
			require.NoError(t, rerr)
			t.Logf("external=%q final=%q err=%v result=%v checkpoints=%d", external, got, err, result != nil, len(ft.ListCheckpoints()))
			assertEditConflict(t, err, dir)
			if string(got) != external {
				t.Error("external content was overwritten")
			}
			if len(ft.ListCheckpoints()) != 0 {
				t.Error("conflict created an edit backup")
			}
		})
	}
	t.Run("same_bytes_replacement", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "file.txt")
		initial := "prefix old suffix\n"
		require.NoError(t, os.WriteFile(path, []byte(initial), 0600))
		before, err := os.Stat(path)
		require.NoError(t, err)
		ft := NewFileTracker()
		replacementHadDifferentIdentity := false
		diff := terraEditDiff{mutate: func() error {
			f, e := os.CreateTemp(dir, "replacement-")
			if e != nil {
				return e
			}
			name := f.Name()
			if e = f.Close(); e != nil {
				return e
			}
			if e = os.WriteFile(name, []byte(initial), 0600); e != nil {
				return e
			}
			if e = os.Rename(name, path); e != nil {
				return e
			}
			afterReplacement, e := os.Stat(path)
			if e != nil {
				return e
			}
			replacementHadDifferentIdentity = !os.SameFile(before, afterReplacement)
			return nil
		}}
		tool := NewEditFileTool(WithEditDiffGenerator(diff), WithEditFileTracker(ft))
		result, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{"file_path": path, "old_string": "old", "new_string": "new"}})
		got, rerr := os.ReadFile(path)
		require.NoError(t, rerr)
		after, rerr := os.Stat(path)
		require.NoError(t, rerr)
		t.Logf("final=%q err=%v result=%v replacement_different_inode=%v initial_inode_same_final=%v checkpoints=%d", got, err, result != nil, replacementHadDifferentIdentity, os.SameFile(before, after), len(ft.ListCheckpoints()))
		if !replacementHadDifferentIdentity {
			t.Error("replacement fixture did not change identity")
		}
		assertEditConflict(t, err, dir)
		if string(got) != initial {
			t.Error("replacement was overwritten")
		}
		if len(ft.ListCheckpoints()) != 0 {
			t.Error("conflict created an edit backup")
		}
	})
	t.Run("mode_change", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "file.txt")
		require.NoError(t, os.WriteFile(path, []byte("prefix old suffix\n"), 0600))
		ft := NewFileTracker()
		tool := NewEditFileTool(WithEditDiffGenerator(terraEditDiff{mutate: func() error { return os.Chmod(path, 0640) }}), WithEditFileTracker(ft))
		result, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{"file_path": path, "old_string": "old", "new_string": "new"}})
		got, rerr := os.ReadFile(path)
		require.NoError(t, rerr)
		info, rerr := os.Stat(path)
		require.NoError(t, rerr)
		t.Logf("final=%q mode=%#o err=%v result=%v checkpoints=%d", got, info.Mode().Perm(), err, result != nil, len(ft.ListCheckpoints()))
		assertEditConflict(t, err, dir)
		require.Equal(t, os.FileMode(0640), info.Mode().Perm())
		if string(got) != "prefix old suffix\n" {
			t.Error("changed-mode file overwritten")
		}
		if len(ft.ListCheckpoints()) != 0 {
			t.Error("conflict created an edit backup")
		}
	})
	t.Run("canceled_before_backup", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "file.txt")
		require.NoError(t, os.WriteFile(path, []byte("prefix old suffix\n"), 0600))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ft := NewFileTracker()
		tool := NewEditFileTool(WithEditDiffGenerator(terraEditDiff{mutate: func() error { cancel(); return nil }}), WithEditFileTracker(ft))
		result, err := tool.Execute(ctx, ToolCall{Args: map[string]any{"file_path": path, "old_string": "old", "new_string": "new"}})
		got, rerr := os.ReadFile(path)
		require.NoError(t, rerr)
		t.Logf("final=%q err=%v canceled=%v result=%v checkpoints=%d", got, err, errors.Is(err, context.Canceled), result != nil, len(ft.ListCheckpoints()))
		if !errors.Is(err, context.Canceled) {
			t.Error("missing cancellation")
		}
		if string(got) != "prefix old suffix\n" {
			t.Error("canceled edit mutated file")
		}
		if len(ft.ListCheckpoints()) != 0 {
			t.Error("canceled edit created a backup")
		}
	})
	t.Run("final_symlink_swap", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "file.txt")
		outside := filepath.Join(dir, "outside.txt")
		require.NoError(t, os.WriteFile(path, []byte("prefix old suffix\n"), 0600))
		require.NoError(t, os.WriteFile(outside, []byte("outside-original\n"), 0600))
		ft := NewFileTracker()
		diff := terraEditDiff{mutate: func() error {
			if err := os.Remove(path); err != nil {
				return err
			}
			return os.Symlink(outside, path)
		}}
		tool := NewEditFileTool(WithEditDiffGenerator(diff), WithEditFileTracker(ft))
		result, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{"file_path": path, "old_string": "old", "new_string": "new"}})
		outsideBytes, readErr := os.ReadFile(outside)
		require.NoError(t, readErr)
		info, statErr := os.Lstat(path)
		require.NoError(t, statErr)
		isSymlink := info.Mode()&os.ModeSymlink != 0
		t.Logf("outside=%q link_remains=%v err=%v result=%v checkpoints=%d", outsideBytes, isSymlink, err, result != nil, len(ft.ListCheckpoints()))
		assertEditConflict(t, err, dir)
		if !isSymlink {
			t.Error("external symlink was replaced")
		}
		if string(outsideBytes) != "outside-original\n" {
			t.Error("symlink target overwritten")
		}
		if len(ft.ListCheckpoints()) != 0 {
			t.Error("final symlink conflict created a backup")
		}
	})
	t.Run("intermediate_symlink_escape", func(t *testing.T) {
		dir := t.TempDir()
		allowed := filepath.Join(dir, "allowed")
		outside := filepath.Join(dir, "outside")
		sub := filepath.Join(allowed, "sub")
		require.NoError(t, os.MkdirAll(sub, 0700))
		require.NoError(t, os.MkdirAll(outside, 0700))
		path := filepath.Join(sub, "file.txt")
		outsideFile := filepath.Join(outside, "file.txt")
		require.NoError(t, os.WriteFile(path, []byte("prefix old suffix\n"), 0600))
		require.NoError(t, os.WriteFile(outsideFile, []byte("outside-original\n"), 0600))
		ft := NewFileTracker()
		diff := terraEditDiff{mutate: func() error {
			if err := os.Rename(sub, filepath.Join(allowed, "original-sub")); err != nil {
				return err
			}
			return os.Symlink(outside, sub)
		}}
		tool := NewEditFileTool(WithEditPathWhitelist([]string{allowed}), WithEditDiffGenerator(diff), WithEditFileTracker(ft))
		result, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{"file_path": path, "old_string": "old", "new_string": "new"}})
		got, rerr := os.ReadFile(outsideFile)
		require.NoError(t, rerr)
		t.Logf("outside_final=%q err=%v result=%v checkpoints=%d", got, err, result != nil, len(ft.ListCheckpoints()))
		assertEditConflict(t, err, sub)
		if string(got) != "outside-original\n" {
			t.Error("outside file overwritten")
		}
		if len(ft.ListCheckpoints()) != 0 {
			t.Error("escape created an edit backup")
		}
	})
}
