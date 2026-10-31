//go:build darwin || linux

package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

type gitDiffReadinessFixture struct {
	ready  string
	leader string
	cancel context.CancelFunc
	result chan error
	done   chan struct{}
}

// The child publishes its own PID atomically after it has started. The shell
// waits for that publication before either waiting for cancellation or exiting.
// The long finite watchdog is cleanup insurance, not a way to pass the test.
func newGitDiffReadinessFixture(t *testing.T, startupDelay time.Duration, exitLeader bool) *gitDiffReadinessFixture {
	t.Helper()
	root := t.TempDir()
	fixture := &gitDiffReadinessFixture{
		ready: filepath.Join(root, "child-ready"), leader: filepath.Join(root, "leader"),
		result: make(chan error, 1), done: make(chan struct{}),
	}
	child := fmt.Sprintf("/bin/sleep %.3f\nprintf '%%s' \"$$\" > %s\n/bin/mv %s %s\nexec /bin/sleep 60\n",
		startupDelay.Seconds(), shellQuoteTestArgument(fixture.ready+".tmp"),
		shellQuoteTestArgument(fixture.ready+".tmp"), shellQuoteTestArgument(fixture.ready))
	finish := "wait \"$child\""
	if exitLeader {
		finish = "exit 0"
	}
	installFakeGit(t, t.TempDir(), fmt.Sprintf(`
case "$*" in
  *rev-parse*)
    printf '%%s' "$$" > %s
    /bin/sh -c %s &
    child=$!
    while [ ! -s %s ]; do /bin/sleep 0.01; done
    %s
    ;;
  *) exit 0 ;;
esac
`, shellQuoteTestArgument(fixture.leader), shellQuoteTestArgument(child), shellQuoteTestArgument(fixture.ready), finish))
	ctx, cancel := context.WithCancel(t.Context())
	fixture.cancel = cancel
	// Keep filesystem setup out of the goroutine and the readiness budget.
	git := NewDefaultGitTool(t.TempDir())
	go func() {
		defer close(fixture.done)
		_, err := git.Diff(ctx, GitDiffOptions{})
		fixture.result <- err
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-fixture.done:
		case <-time.After(2 * time.Second):
			if pid, ok := readGitFixturePID(fixture.leader); ok {
				_ = syscall.Kill(-pid, syscall.SIGKILL)
			}
			select {
			case <-fixture.done:
			case <-time.After(2 * time.Second):
				t.Error("fixture Git invocation did not join during cleanup")
			}
		}
		// A failed assertion must not leave the fixture's pipe holder behind.
		if pid, ok := readGitFixturePID(fixture.ready); ok {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	return fixture
}

func readGitFixturePID(path string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	return pid, err == nil && pid > 0
}

func waitGitDiffChildReady(t *testing.T, fixture *gitDiffReadinessFixture) int {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if pid, ready := readGitFixturePID(fixture.ready); ready {
			return pid
		}
		select {
		case <-fixture.done:
			// Recheck atomic publication before classifying an early return.
			if pid, ready := readGitFixturePID(fixture.ready); ready {
				return pid
			}
			t.Fatalf("Git invocation returned before child readiness: %v", <-fixture.result)
		case <-deadline.C:
			t.Fatal("fake Git child did not publish its own PID within the startup readiness budget")
		case <-ticker.C:
		}
	}
}

func assertGitDiffFixtureGone(t *testing.T, fixture *gitDiffReadinessFixture, childPID int) {
	t.Helper()
	assert.ErrorIs(t, syscall.Kill(childPID, 0), syscall.ESRCH, "returned invocation must have stopped its pipe holder")
	leaderPID, ok := readGitFixturePID(fixture.leader)
	require.True(t, ok)
	assert.ErrorIs(t, syscall.Kill(-leaderPID, 0), syscall.ESRCH, "the owned process group must be absent after readers and leader join")
}

func TestDefaultGitDiffCancellationJoinsDescendantHoldingPipes(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	for _, delay := range []time.Duration{0, 1200 * time.Millisecond} {
		t.Run(fmt.Sprintf("startup_%s", delay), func(t *testing.T) {
			fixture := newGitDiffReadinessFixture(t, delay, false)
			childPID := waitGitDiffChildReady(t, fixture)
			require.NoError(t, syscall.Kill(childPID, 0), "the child must be alive before cancellation")
			select {
			case <-fixture.done:
				t.Fatal("live leader/child fixture returned before explicit cancellation")
			default:
			}
			start := time.Now()
			fixture.cancel()
			select {
			case err := <-fixture.result:
				require.ErrorIs(t, err, context.Canceled)
			case <-time.After(1500 * time.Millisecond):
				t.Fatal("Git Diff did not join its owned leader, readers and child within the cancellation budget")
			}
			require.Less(t, time.Since(start), 1500*time.Millisecond)
			assertGitDiffFixtureGone(t, fixture, childPID)
		})
	}
}

func TestDefaultGitDiffLeaderExitAutomaticallyJoinsPipeHoldingDescendant(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	fixture := newGitDiffReadinessFixture(t, 0, true)
	childPID := waitGitDiffChildReady(t, fixture)
	// No cancellation: leader exit itself must trigger the group's cleanup.
	select {
	case err := <-fixture.result:
		require.ErrorIs(t, err, errProcessGroupRemained)
		require.False(t, errors.Is(err, context.Canceled))
		var unknown interface{ OutcomeUnknown() bool }
		require.ErrorAs(t, err, &unknown)
		require.True(t, unknown.OutcomeUnknown())
		var pending PendingExecutionError
		require.False(t, errors.As(err, &pending), "joined resources must not remain pending")
	case <-time.After(2 * time.Second):
		t.Fatal("Git Diff did not automatically join the exited leader's pipe holder")
	}
	assertGitDiffFixtureGone(t, fixture, childPID)
}
