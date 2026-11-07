//go:build darwin || linux

package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPreparedDefaultMutationCancellationDuringGitDiffPreservesFile(t *testing.T) {
	for _, toolName := range []string{"write", "edit"} {
		t.Run(toolName, func(t *testing.T) {
			testPreparedMutationCancellationDuringGitDiff(t, toolName, false)
		})
	}
}

func TestPreparedDefaultMutationCancellationDuringStoppedGitDiffPreservesFile(t *testing.T) {
	testPreparedMutationCancellationDuringGitDiff(t, "write", true)
}

func testPreparedMutationCancellationDuringGitDiff(t *testing.T, toolName string, stopLeader bool) {
	t.Helper()
	root := t.TempDir()
	started := filepath.Join(root, "git-child-started")
	reaped := filepath.Join(root, "git-child-reaped")
	stop := ""
	if stopLeader {
		// The shell stops itself, so the test never signals a potentially reused PID.
		stop = `kill -STOP "$$"`
	}
	installFakeGit(t, t.TempDir(), fmt.Sprintf(`
case "$*" in
  *rev-parse*)
    reaped=%s
    /bin/sleep 10 &
    child=$!
    trap 'kill "$child" 2>/dev/null || true; wait "$child" 2>/dev/null || true; printf done > "$reaped"; exit 143' TERM INT
    printf '%%s %%s\n' "$$" "$child" > %s
    /bin/mv %s %s
    %s
    wait "$child"
    ;;
  *) exit 0 ;;
esac
`, shellQuoteTestArgument(reaped), shellQuoteTestArgument(started+".tmp"),
		shellQuoteTestArgument(started+".tmp"), shellQuoteTestArgument(started), stop))
	path := filepath.Join(root, "existing.txt")
	const original = "old content\n"
	require.NoError(t, os.WriteFile(path, []byte(original), 0o600))
	base := NewDefaultToolRegistry().(*DefaultToolRegistry) //nolint:errcheck
	generator := NewGitDiffGenerator(NewDefaultGitTool(root), NewUnifiedDiffGenerator(0, false))
	require.NoError(t, RegisterDefaults(context.Background(), base,
		WithRegisteredBuiltinWhitelist([]string{toolName}), WithRegisteredDiffGenerator(generator)))
	registry := NewMiddlewareToolRegistry(base)
	queue := NewDefaultFileMutationQueue().(*DefaultFileMutationQueue) //nolint:errcheck
	configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{MutationQueue: queue})
	args := map[string]any{"path": path, "content": "appended\n", "append": true}
	if toolName == "edit" {
		args = map[string]any{"file_path": path, "old_string": "old", "new_string": "new"}
	}
	ctx, cancel := context.WithCancel(context.Background())
	watchCtx, cancelWatch := context.WithCancel(context.Background())
	var expected gitCancelExpected
	var watcherDone <-chan struct{}
	t.Cleanup(func() {
		cancel()
		if watcherDone != nil {
			select {
			case <-watcherDone:
			case <-time.After(5 * time.Second):
				t.Error("completion watcher did not finish before fixture cleanup")
				cancelWatch()
				select {
				case <-watcherDone:
				case <-time.After(2 * time.Second):
					t.Error("completion watcher did not stop")
				}
			}
		}
		cancelWatch()
		if expected.host == "" || expected.leader.pid <= 0 || expected.child.pid <= 0 {
			t.Log("fixture identities were not fully captured; process-group cleanup is unproven")
		}
		cleanupGitCancelGroup(t, expected)
		closed := make(chan error, 1)
		go func() { closed <- queue.Close() }()
		select {
		case err := <-closed:
			assert.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Error("mutation queue did not close during fixture cleanup")
		}
	})
	invocation, err := registry.PrepareToolCall(ctx, ToolCall{Name: toolName, Args: args})
	require.NoError(t, err)
	returned := make(chan error, 1)
	startupDeadline := time.Now().Add(3 * time.Second)
	go func() {
		_, dispatchErr := invocation.Dispatch(ctx)
		returned <- dispatchErr
	}()
	clock := gitCancelSystemClock()
	leaderPID, childPID, err := awaitGitCancelPIDs(startupDeadline, returned, clock, func() ([]byte, os.FileMode, error) {
		return readGitCancelPIDFile(started)
	})
	require.NoError(t, err)
	expected.host, err = currentHostGeneration()
	require.NoError(t, err)
	expected.leader, err = captureGitCancelIdentity(leaderPID)
	require.NoError(t, err)
	expected.child, err = captureGitCancelIdentity(childPID)
	require.NoError(t, err)
	expected.target = original
	require.NotEqual(t, expected.leader.pid, expected.child.pid)
	require.Equal(t, expected.leader.pid, expected.leader.pgid)
	require.Equal(t, expected.leader.pgid, expected.child.pgid)
	t.Logf("captured host=%s leader=%+v child=%+v", expected.host, expected.leader, expected.child)
	if stopLeader {
		stopCtx, stopCancel := context.WithDeadline(ctx, startupDeadline)
		defer stopCancel()
		for {
			require.NoError(t, gitCancelCheckReturned(returned))
			if !clock.now().Before(startupDeadline) {
				t.Fatal("fake Git leader did not reach the kernel's stopped state within the startup deadline")
			}
			stopped, stopErr := gitCancelLeaderStopped(stopCtx, expected.leader)
			require.NoError(t, gitCancelCheckReturned(returned))
			require.NoError(t, stopErr)
			if !clock.now().Before(startupDeadline) {
				t.Fatal("fake Git leader did not reach the kernel's stopped state within the startup deadline")
			}
			if stopped {
				t.Logf("captured fake Git leader %d is kernel-stopped", expected.leader.pid)
				break
			}
			delay := startupDeadline.Sub(clock.now())
			if delay <= 0 {
				continue
			}
			if delay > 5*time.Millisecond {
				delay = 5 * time.Millisecond
			}
			if dispatchErr, returnedEarly := clock.wait(delay, returned); returnedEarly {
				require.NoError(t, gitCancelUnexpectedReturn(dispatchErr))
			}
		}
	}
	signal := invocation.CompletionSignal()
	done, err := awaitGitCancelDone(startupDeadline, returned, clock, signal.Done)
	require.NoError(t, err)
	ready := make(chan struct{})
	observed := make(chan gitCancelSample, 1)
	finished := make(chan struct{})
	watcherDone = finished
	go func() {
		defer close(finished)
		close(ready)
		select {
		case <-done:
			observed <- sampleGitCancelDone(expected, signal)
		case <-watchCtx.Done():
		}
	}()
	<-ready
	select {
	case <-done:
		t.Fatal("queue job completed before cancellation")
	default:
	}
	host, err := currentHostGeneration()
	require.NoError(t, err)
	require.Equal(t, expected.host, host)
	require.NoError(t, checkGitCancelCapturedIdentity(expected.leader))
	require.NoError(t, checkGitCancelCapturedIdentity(expected.child))
	require.NoError(t, gitCancelCheckReturned(returned))
	cancel()
	select {
	case err = <-returned:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("prepared dispatch did not return after cancellation")
	}
	// Dispatch may return before the queued executor. The pre-armed watcher
	// samples both identities and the whole group immediately after observing Done.
	var sample gitCancelSample
	select {
	case sample = <-observed:
	case <-time.After(5 * time.Second):
		t.Fatal("accepted queue job did not join its Git helper")
	}
	t.Logf("first post-Done observation at %s: leader pid=%d birth=%q pgid=%d identityErr=%v kill0=%v; child pid=%d birth=%q pgid=%d identityErr=%v kill0=%v; group=%d kill0=%v",
		sample.observedAt.Format(time.RFC3339Nano), sample.leader.pid, sample.leader.birth,
		sample.leader.observedPGID, sample.leader.birthErr, sample.leader.killErr,
		sample.child.pid, sample.child.birth, sample.child.observedPGID, sample.child.birthErr,
		sample.child.killErr, sample.pgid, sample.groupErr)
	contents, err := os.ReadFile(path)
	require.NoError(t, err)
	sample.target = string(contents)
	assert.Equal(t, original, sample.target, "canceled Git preview must not permit a late file mutation")
	assert.Equal(t, CompletionQueuedJobJoined, sample.proof)
	require.NoError(t, checkGitCancelSample(expected, sample), "fake Git process quiescence must be independently proven")
	_, markerErr := os.Stat(reaped)
	if stopLeader {
		require.ErrorIs(t, markerErr, os.ErrNotExist, "the forced-cleanup control must demonstrate the old marker oracle would fail")
	} else if markerErr != nil && !errors.Is(markerErr, os.ErrNotExist) {
		require.NoError(t, markerErr)
	}
	t.Logf("fake Git trap marker present: %t", markerErr == nil)
}
