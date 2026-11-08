//go:build darwin || linux

package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func editCall(path string) ToolCall {
	return ToolCall{Name: "edit", Args: map[string]any{"file_path": path, "old_string": "old", "new_string": "new"}}
}

func TestEditStableSymlinkAndSnapshot(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	require.NoError(t, os.Mkdir(realDir, 0700))
	require.NoError(t, os.Symlink(realDir, filepath.Join(root, "link")))
	path := filepath.Join(root, "link", "file")
	require.NoError(t, os.WriteFile(path, []byte("prefix old suffix\n"), 0751))
	tracker := NewFileTracker()
	tool := NewEditFileTool(WithEditPathWhitelist([]string{root}), WithEditFileTracker(tracker), WithEditDiffGenerator(terraEditDiff{}))
	result, err := tool.Execute(context.Background(), editCall(path))
	require.NoError(t, err)
	require.Equal(t, "preview", result.Metadata["diff"])
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "prefix new suffix\n", string(content))
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0751), info.Mode().Perm())
	checkpoints := tracker.ListCheckpoints()
	require.Len(t, checkpoints, 1)
	require.Equal(t, os.FileMode(0751), checkpoints[0].Mode.Perm())
	backup, ok := tracker.BackupContent(checkpoints[0].ID)
	require.True(t, ok)
	require.Equal(t, "prefix old suffix\n", string(backup))
	backup[0] = 'X'
	again, ok := tracker.BackupContent(checkpoints[0].ID)
	require.True(t, ok)
	require.Equal(t, "prefix old suffix\n", string(again))
	matches, err := filepath.Glob(filepath.Join(realDir, ".write-*"))
	require.NoError(t, err)
	require.Empty(t, matches)
}

func TestEditParentDescriptorDoesNotFollowSwappedPath(t *testing.T) {
	root := t.TempDir()
	allowed := filepath.Join(root, "allowed")
	parent := filepath.Join(allowed, "sub")
	outside := filepath.Join(root, "outside")
	moved := filepath.Join(root, "moved-parent")
	require.NoError(t, os.MkdirAll(parent, 0700))
	require.NoError(t, os.Mkdir(outside, 0700))
	path := filepath.Join(parent, "file")
	require.NoError(t, os.WriteFile(path, []byte("prefix old suffix\n"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(outside, "file"), []byte("outside-original\n"), 0600))
	target, err := captureEditTarget(path, NewPathWhitelist([]string{allowed}))
	require.NoError(t, err)
	defer target.close()
	calls := 0
	ctx := context.WithValue(context.Background(), preparedEffectStartKey{}, preparedEffectStartGuard(func(start func() error) error {
		calls++
		if calls == 2 {
			if err := os.Rename(parent, moved); err != nil {
				return err
			}
			if err := os.Symlink(outside, parent); err != nil {
				return err
			}
		}
		return start()
	}))
	require.NoError(t, target.write(ctx, []byte("prefix new suffix\n")))
	require.Equal(t, 2, calls)
	outsideContent, err := os.ReadFile(filepath.Join(outside, "file"))
	require.NoError(t, err)
	require.Equal(t, "outside-original\n", string(outsideContent))
	movedContent, err := os.ReadFile(filepath.Join(moved, "file"))
	require.NoError(t, err)
	require.Equal(t, "prefix new suffix\n", string(movedContent))
	link, err := os.Lstat(parent)
	require.NoError(t, err)
	require.NotZero(t, link.Mode()&os.ModeSymlink)
	for _, dir := range []string{moved, outside} {
		matches, err := filepath.Glob(filepath.Join(dir, ".write-*"))
		require.NoError(t, err)
		require.Empty(t, matches)
	}
}

func TestEditFinalFenceRejectsAndCleansTemporary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file")
	require.NoError(t, os.WriteFile(path, []byte("prefix old suffix\n"), 0600))
	tracker := NewFileTracker()
	calls := 0
	ctx := context.WithValue(context.Background(), preparedEffectStartKey{}, preparedEffectStartGuard(func(start func() error) error {
		calls++
		if calls == 2 {
			return ErrDispatchFenceChanged
		}
		return start()
	}))
	result, err := NewEditFileTool(WithEditFileTracker(tracker)).Execute(ctx, editCall(path))
	require.ErrorIs(t, err, ErrDispatchFenceChanged)
	require.Nil(t, result)
	require.Equal(t, 2, calls)
	// The fence changed after the first validation and backup.
	require.Len(t, tracker.ListCheckpoints(), 1)
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "prefix old suffix\n", string(content))
	matches, err := filepath.Glob(filepath.Join(dir, ".write-*"))
	require.NoError(t, err)
	require.Empty(t, matches)
}

func TestEditLateConflictRetainsEarlierSnapshotAndCleansTemporary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file")
	require.NoError(t, os.WriteFile(path, []byte("prefix old suffix\n"), 0600))
	tracker := NewFileTracker()
	calls := 0
	ctx := context.WithValue(context.Background(), preparedEffectStartKey{}, preparedEffectStartGuard(func(start func() error) error {
		calls++
		if err := start(); err != nil {
			return err
		}
		if calls == 1 {
			return os.WriteFile(path, []byte("late external change"), 0600)
		}
		return nil
	}))
	result, err := NewEditFileTool(WithEditFileTracker(tracker)).Execute(ctx, editCall(path))
	require.Nil(t, result)
	assertEditConflict(t, err, dir)
	require.Equal(t, 1, calls)
	checkpoints := tracker.ListCheckpoints()
	require.Len(t, checkpoints, 1)
	backup, ok := tracker.BackupContent(checkpoints[0].ID)
	require.True(t, ok)
	require.Equal(t, "prefix old suffix\n", string(backup))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "late external change", string(data))
}

func TestEditRevalidationIOErrorFailsClosed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file")
	require.NoError(t, os.WriteFile(path, []byte("prefix old suffix\n"), 0600))
	target, err := captureEditTarget(path, NewPathWhitelist(nil))
	require.NoError(t, err)
	concrete := target.(*anchoredEditTarget)
	require.NoError(t, concrete.parent.Close())
	err = target.revalidate()
	require.ErrorIs(t, err, unix.EBADF)
	require.NotErrorIs(t, err, ErrEditConflict)
	require.ErrorIs(t, target.write(context.Background(), []byte("new")), unix.EBADF)
	_ = target.close()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "prefix old suffix\n", string(data))
}

func TestEditFIFORejectsWithoutBlocking(t *testing.T) {
	if phase := os.Getenv("GO_CLI_EDIT_FIFO_PHASE"); phase != "" {
		path := os.Getenv("GO_CLI_EDIT_FIFO_PATH")
		tracker := NewFileTracker()
		called := false
		diff := terraEditDiff{mutate: func() error {
			called = true
			if phase == "preview" {
				if err := os.Remove(path); err != nil {
					return err
				}
				return unix.Mkfifo(path, 0600)
			}
			return nil
		}}
		result, err := NewEditFileTool(WithEditFileTracker(tracker), WithEditDiffGenerator(diff)).Execute(context.Background(), editCall(path))
		require.Error(t, err)
		require.Nil(t, result)
		require.Equal(t, phase == "preview", called)
		require.Empty(t, tracker.ListCheckpoints())
		info, statErr := os.Lstat(path)
		require.NoError(t, statErr)
		require.NotZero(t, info.Mode()&os.ModeNamedPipe)
		matches, globErr := filepath.Glob(filepath.Join(filepath.Dir(path), ".write-*"))
		require.NoError(t, globErr)
		require.Empty(t, matches)
		return
	}
	for _, phase := range []string{"initial", "preview"} {
		t.Run(phase, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "fifo")
			if phase == "initial" {
				require.NoError(t, unix.Mkfifo(path, 0600))
			} else {
				require.NoError(t, os.WriteFile(path, []byte("prefix old suffix\n"), 0600))
			}
			binary, err := os.Executable()
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, binary, "-test.run=^TestEditFIFORejectsWithoutBlocking$", "-test.timeout=4s")
			cmd.Env = append(os.Environ(), "GO_CLI_EDIT_FIFO_PHASE="+phase, "GO_CLI_EDIT_FIFO_PATH="+path)
			output, runErr := cmd.CombinedOutput() // CommandContext kills the child; CombinedOutput waits for it.
			require.NoError(t, ctx.Err(), "FIFO child exceeded deadline and was killed and joined: %s", output)
			require.NoError(t, runErr, "%s", output)
		})
	}
}

func TestEditConflictThroughQueueAndPreparedDispatch(t *testing.T) {
	for _, mode := range []string{"queue", "prepared"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "file")
			require.NoError(t, os.WriteFile(path, []byte("prefix old suffix\n"), 0600))
			tracker := NewFileTracker()
			diff := terraEditDiff{mutate: func() error { return os.WriteFile(path, []byte("external content"), 0600) }}
			queue := NewDefaultFileMutationQueue(WithMutationFileTracker(tracker), WithMutationDiffGenerator(diff)).(*DefaultFileMutationQueue)
			defer closeQueue(t, queue)
			var result *ToolResult
			var err error
			if mode == "queue" {
				wrapped := WithMutationQueue(queue, func(context.Context, ToolCall) (*ToolResult, error) { return nil, errors.New("unexpected next") })
				result, err = wrapped(context.Background(), editCall(path))
			} else {
				// A managed Git preview exercises the trusted prepared completion
				// path and changes the real target while that preview is running.
				witnessPath := filepath.Join(dir, "preview-branch")
				t.Setenv("GO_CLI_EDIT_PREPARED_TARGET", path)
				t.Setenv("GO_CLI_EDIT_PREPARED_WITNESS", witnessPath)
				require.Nil(t, tracker.snapshotMgr, "fixture does not configure a Git snapshot manager")
				installFakeGit(t, t.TempDir(), `
while [ "$#" -gt 0 ]; do
  case "$1" in
    -c) [ "$#" -ge 2 ] || exit 2; shift 2 ;;
    --no-optional-locks) shift ;;
    *) break ;;
  esac
done
[ "$#" -gt 0 ] || exit 2
case "$1" in
  rev-parse) printf 'true\n' ;;
  diff) printf 'external content' > "$GO_CLI_EDIT_PREPARED_TARGET"; printf 'diff\n' > "$GO_CLI_EDIT_PREPARED_WITNESS"; printf 'preview\n' ;;
  *) printf 'unexpected git fixture command\n' >&2; exit 2 ;;
esac
`)
				generator := NewGitDiffGenerator(NewDefaultGitTool(dir), NewUnifiedDiffGenerator(0, false))
				base := NewDefaultToolRegistry().(*DefaultToolRegistry)
				require.NoError(t, RegisterDefaults(context.Background(), base, WithRegisteredBuiltinWhitelist([]string{"edit"}), WithRegisteredFileTracker(tracker), WithRegisteredDiffGenerator(generator)))
				registry := NewMiddlewareToolRegistry(base)
				configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{MutationQueue: queue})
				call := editCall(path)
				call.ID = "edit-conflict"
				invocation, prepErr := registry.PrepareToolCall(context.Background(), call)
				require.NoError(t, prepErr)
				result, err = invocation.Dispatch(context.Background())
				signal := invocation.CompletionSignal()
				require.NotNil(t, signal.Done())
				select {
				case <-signal.Done():
				case <-time.After(3 * time.Second):
					t.Fatal("queue worker completion did not close")
				}
				require.Equal(t, CompletionQueuedJobJoined, signal.Proof())
				witness, witnessErr := os.ReadFile(witnessPath)
				require.NoError(t, witnessErr, "managed Git preview diff branch did not run")
				require.Equal(t, "diff\n", string(witness))
			}
			require.Nil(t, result)
			assertEditConflict(t, err, dir)
			data, readErr := os.ReadFile(path)
			require.NoError(t, readErr)
			require.Equal(t, "external content", string(data))
			require.Empty(t, tracker.ListCheckpoints(), "Edit checkpoint list")
		})
	}
}

type editUnknownPreviewError struct{ cause error }

func (e editUnknownPreviewError) Error() string      { return e.cause.Error() }
func (e editUnknownPreviewError) Unwrap() error      { return e.cause }
func (editUnknownPreviewError) OutcomeUnknown() bool { return true }

func TestEditUnresolvedPreviewPreservesErrorAndFile(t *testing.T) {
	cause := errors.New("preview sentinel")
	done := make(chan struct{})
	defer close(done)
	for _, previewErr := range []error{
		&pendingExecutionErrorStub{cause: cause, done: done, unknown: false},
		editUnknownPreviewError{cause: cause},
		fmt.Errorf("wrapped: %w", context.Canceled),
	} {
		t.Run(fmt.Sprintf("%T", previewErr), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "file")
			require.NoError(t, os.WriteFile(path, []byte("prefix old suffix\n"), 0600))
			tracker := NewFileTracker()
			result, err := NewEditFileTool(WithEditFileTracker(tracker), WithEditDiffGenerator(terraEditDiff{previewErr: previewErr})).Execute(context.Background(), editCall(path))
			require.Nil(t, result)
			require.ErrorIs(t, err, previewErr)
			if !errors.Is(previewErr, context.Canceled) {
				require.ErrorIs(t, err, cause)
			}
			content, readErr := os.ReadFile(path)
			require.NoError(t, readErr)
			require.Equal(t, "prefix old suffix\n", string(content))
			require.Empty(t, tracker.ListCheckpoints())
			matches, globErr := filepath.Glob(filepath.Join(dir, ".write-*"))
			require.NoError(t, globErr)
			require.Empty(t, matches)
		})
	}
}

func TestEditConflictLogExcludesContents(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	defer slog.SetDefault(previous)
	path := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(path, []byte("prefix old suffix\n"), 0600))
	_, err := NewEditFileTool(WithEditDiffGenerator(terraEditDiff{mutate: func() error { return os.WriteFile(path, []byte("external-secret"), 0600) }})).Execute(context.Background(), editCall(path))
	require.ErrorIs(t, err, ErrEditConflict)
	logs := output.String()
	require.Contains(t, logs, "edit.conflict")
	require.Contains(t, logs, `"success":false`)
	require.False(t, strings.Contains(logs, "prefix old suffix") || strings.Contains(logs, "external-secret"), "%s", logs)
}
