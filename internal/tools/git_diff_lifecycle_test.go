package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultGitDiffCancellationJoinsDescendantHoldingPipes(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("managed process groups are supported only on Darwin and Linux")
	}

	binDir := t.TempDir()
	started := filepath.Join(t.TempDir(), "child-started")
	installFakeGit(t, binDir, fmt.Sprintf(`
case "$*" in
  *rev-parse*)
    /bin/sleep 3 &
    child=$!
    printf '%%s' "$child" > %q
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
`, started))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := NewDefaultGitTool(t.TempDir()).Diff(ctx, GitDiffOptions{})
		result <- err
	}()

	require.Eventually(t, func() bool {
		_, err := os.Stat(started)
		return err == nil
	}, time.Second, 5*time.Millisecond, "fake git must start its finite descendant before cancellation")
	start := time.Now()
	cancel()

	var err error
	select {
	case err = <-result:
	case <-time.After(1500 * time.Millisecond):
		select {
		case err = <-result:
			t.Errorf("Git Diff waited for the descendant pipe after cancellation: %v", err)
		case <-time.After(3 * time.Second):
			t.Fatal("Git Diff did not return after its finite descendant exited")
		}
		return
	}

	require.Error(t, err)
	require.True(t, errors.Is(err, context.Canceled), "cancellation must remain in the error chain: %v", err)
	require.Less(t, time.Since(start), 1500*time.Millisecond, "return only after the owned leader, readers, and process group are joined")
}

func installFakeGit(t *testing.T, binDir, body string) {
	t.Helper()
	path := filepath.Join(binDir, "git")
	content := "#!/bin/sh\nset -eu\n" + body
	require.NoError(t, os.WriteFile(path, []byte(content), 0o700))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

type lifecycleTestGitTool struct {
	GitTool
	output string
	err    error
	calls  int
}

func (g *lifecycleTestGitTool) Diff(context.Context, GitDiffOptions) (string, error) {
	g.calls++
	return g.output, g.err
}

type lifecycleTestDiffGenerator struct {
	err   error
	calls int
}

func (g *lifecycleTestDiffGenerator) Generate(context.Context, string, string, string) (string, error) {
	g.calls++
	return "", g.err
}

func TestGitDiffGeneratorDoesNotFallbackForUnresolvedHelper(t *testing.T) {
	pendingDone := make(chan struct{})
	cases := []struct {
		name string
		err  error
	}{
		{name: "pending process", err: pendingProcessErrorWithDone(errors.New("helper still active"), true, pendingDone)},
		{name: "containment unknown", err: newUnknownExecutionError(errors.Join(errProcessContainmentUnconfirmed, errors.New("helper escaped its group")))},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			git := &lifecycleTestGitTool{err: testCase.err}
			fallback := &lifecycleTestDiffGenerator{}
			generator := NewGitDiffGenerator(git, fallback)

			_, err := generator.Generate(context.Background(), "old", "new", "file.txt")
			require.Error(t, err)
			require.ErrorIs(t, err, testCase.err)
			require.Equal(t, 1, git.calls)
			require.Zero(t, fallback.calls, "a pending or containment-unproven helper must stop the fallback chain")
		})
	}
}

func TestGitDiffGeneratorDoesNotStartFallbackAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	git := &lifecycleTestGitTool{}
	fallback := &lifecycleTestDiffGenerator{}
	generator := NewGitDiffGenerator(git, fallback)

	_, err := generator.Generate(ctx, "old", "new", "file.txt")
	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, git.calls, "a canceled operation must not start a Git helper")
	assert.Zero(t, fallback.calls, "a canceled operation must not start a fallback helper")
}

func TestWriteAndEditDoNotMutateWhenDiffLifecycleIsUnresolved(t *testing.T) {
	pendingDone := make(chan struct{})
	cases := []struct {
		name        string
		err         error
		wantPending bool
	}{
		{name: "pending process", err: pendingProcessErrorWithDone(errors.New("helper still active"), true, pendingDone), wantPending: true},
		{name: "containment unknown", err: newUnknownExecutionError(errors.Join(errProcessContainmentUnconfirmed, errors.New("helper escaped its group")))},
	}
	for _, testCase := range cases {
		for _, toolName := range []string{"write", "edit"} {
			t.Run(testCase.name+"/"+toolName, func(t *testing.T) {
				root := t.TempDir()
				path := filepath.Join(root, "existing.txt")
				require.NoError(t, os.WriteFile(path, []byte("old content\n"), 0o600))
				diff := &lifecycleTestDiffGenerator{err: testCase.err}

				var err error
				switch toolName {
				case "write":
					tool := NewWriteTool(WithWriteWorkdir(root), WithOverwrite(true), WithDiffGenerator(diff))
					_, err = tool.Execute(context.Background(), ToolCall{Args: map[string]any{
						"path": "existing.txt", "content": "new content\n",
					}})
				case "edit":
					tool := NewEditFileTool(WithEditDiffGenerator(diff))
					_, err = tool.Execute(context.Background(), ToolCall{Args: map[string]any{
						"file_path": path, "old_string": "old", "new_string": "new",
					}})
				}

				require.Error(t, err, "unresolved diff execution must be visible to the mutation caller")
				if testCase.wantPending {
					var pending PendingExecutionError
					require.ErrorAs(t, err, &pending)
				} else {
					require.ErrorIs(t, err, errProcessContainmentUnconfirmed)
				}
				require.Equal(t, 1, diff.calls)
				actual, readErr := os.ReadFile(path)
				require.NoError(t, readErr)
				require.Equal(t, "old content\n", string(actual), "the file must remain untouched while helper completion is unproven")
			})
		}
	}
}

func TestPreparedMutationQueueWaitsForNestedPendingProcess(t *testing.T) {
	queue := NewDefaultFileMutationQueue().(*DefaultFileMutationQueue) //nolint:errcheck
	defer queue.Close()
	pendingDone := make(chan struct{})
	pendingErr := pendingProcessErrorWithDone(errors.New("nested helper pending"), true, pendingDone)
	invocation := newQueuedMutationTestInvocation(t, pendingErr)
	resultCh, err := queue.enqueuePrepared(context.Background(), FileMutation{
		FilePath: filepath.Join(t.TempDir(), "queued.txt"), Operation: "write", Content: "new", ToolName: "write",
	}, invocation)
	require.NoError(t, err)

	result := <-resultCh
	require.ErrorIs(t, result.Error, pendingErr)
	signal := invocation.CompletionSignal()
	require.NotNil(t, signal.Done())
	select {
	case <-signal.Done():
		t.Fatal("queue completion closed before the nested helper's ExecutionDone")
	default:
	}
	assert.Empty(t, signal.Proof())

	close(pendingDone)
	require.Eventually(t, func() bool {
		select {
		case <-signal.Done():
			return true
		default:
			return false
		}
	}, time.Second, 5*time.Millisecond)
	assert.Equal(t, CompletionQueuedJobJoined, signal.Proof(), "the queue proof follows the real nested resource receipt")
}

func TestPreparedMutationQueueInvalidatesProofForUnknownContainment(t *testing.T) {
	queue := NewDefaultFileMutationQueue().(*DefaultFileMutationQueue) //nolint:errcheck
	defer queue.Close()
	unknownErr := newUnknownExecutionError(errors.Join(errProcessContainmentUnconfirmed, errors.New("escaped helper")))
	invocation := newQueuedMutationTestInvocation(t, unknownErr)
	resultCh, err := queue.enqueuePrepared(context.Background(), FileMutation{
		FilePath: filepath.Join(t.TempDir(), "queued.txt"), Operation: "write", Content: "new", ToolName: "write",
	}, invocation)
	require.NoError(t, err)

	result := <-resultCh
	require.ErrorIs(t, result.Error, unknownErr)
	signal := invocation.CompletionSignal()
	require.Eventually(t, func() bool {
		select {
		case <-signal.Done():
			return true
		default:
			return false
		}
	}, time.Second, 5*time.Millisecond)
	assert.Empty(t, signal.Proof(), "a joined queue job cannot upgrade unknown process containment to a positive proof")
}

func TestPreparedMutationQueueKeepsContainmentFailureStickyUntilNestedJoin(t *testing.T) {
	queue := NewDefaultFileMutationQueue().(*DefaultFileMutationQueue) //nolint:errcheck
	defer queue.Close()
	pendingDone := make(chan struct{})
	containmentErr := pendingProcessErrorWithDone(errors.Join(errProcessContainmentUnconfirmed, errors.New("forced pipe close")), true, pendingDone)
	invocation := newQueuedMutationTestInvocation(t, containmentErr)
	resultCh, err := queue.enqueuePrepared(context.Background(), FileMutation{
		FilePath: filepath.Join(t.TempDir(), "queued.txt"), Operation: "write", Content: "new", ToolName: "write",
	}, invocation)
	require.NoError(t, err)

	result := <-resultCh
	require.ErrorIs(t, result.Error, containmentErr)
	signal := invocation.CompletionSignal()
	select {
	case <-signal.Done():
		t.Fatal("queue completion closed before the nested helper's ExecutionDone")
	default:
	}
	assert.Empty(t, signal.Proof(), "containment failure must invalidate proof before waiting for local reader cleanup")

	close(pendingDone)
	require.Eventually(t, func() bool {
		select {
		case <-signal.Done():
			return true
		default:
			return false
		}
	}, time.Second, 5*time.Millisecond)
	assert.Empty(t, signal.Proof(), "local resource join cannot turn forced-close containment into a positive proof")
}

func TestRegisterDefaultsManagedGitDiffUsesJoinedQueuedMutationProof(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("managed process groups are supported only on Darwin and Linux")
	}
	binDir := t.TempDir()
	installFakeGit(t, binDir, `
case "$*" in
  *rev-parse*) printf 'true\n'; exit 0 ;;
  *diff*) printf 'synthetic git diff failure\n' >&2; exit 1 ;;
  *) exit 0 ;;
esac
`)

	for _, toolName := range []string{"write", "edit"} {
		t.Run(toolName, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			path := filepath.Join(root, "existing.txt")
			require.NoError(t, os.WriteFile(path, []byte("old content\n"), 0o600))
			generator := NewGitDiffGenerator(NewDefaultGitTool(root), NewUnifiedDiffGenerator(0, false))
			base := NewDefaultToolRegistry().(*DefaultToolRegistry) //nolint:errcheck
			require.NoError(t, RegisterDefaults(ctx, base,
				WithRegisteredBuiltinWhitelist([]string{toolName}),
				WithRegisteredDiffGenerator(generator),
			))
			registry := NewMiddlewareToolRegistry(base)
			queue := NewDefaultFileMutationQueue().(*DefaultFileMutationQueue) //nolint:errcheck
			defer queue.Close()
			configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{MutationQueue: queue})

			args := map[string]any{}
			switch toolName {
			case "write":
				args = map[string]any{"path": path, "content": "appended\n", "append": true}
			case "edit":
				args = map[string]any{"file_path": path, "old_string": "old", "new_string": "new"}
			}
			invocation, err := registry.PrepareToolCall(ctx, ToolCall{Name: toolName, Args: args})
			require.NoError(t, err)
			result, err := invocation.Dispatch(ctx)
			require.NoError(t, err, "a normally joined Git helper error may use the unified fallback")
			require.NotNil(t, result)
			assert.NotEmpty(t, result.Metadata["diff"], "the joined helper failure should retain the unified preview")

			want := "new content\n"
			if toolName == "write" {
				want = "old content\nappended\n"
			}
			actual, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, want, string(actual), "the ordinary joined helper failure preserves the normal mutation semantics")

			signal := invocation.CompletionSignal()
			require.NotNil(t, signal.Done(), "the exact default GitDiff registration must have a queued-job completion signal")
			require.Eventually(t, func() bool {
				select {
				case <-signal.Done():
					return true
				default:
					return false
				}
			}, time.Second, 5*time.Millisecond)
			assert.Equal(t, CompletionQueuedJobJoined, signal.Proof(), "Git helper completion is covered by the outer mutation queue")
		})
	}
}

func newQueuedMutationTestInvocation(t *testing.T, executionErr error) *preparedInvocation {
	t.Helper()
	fence := NewDispatchFence()
	call, err := freezeToolCall(ToolCall{Name: "write", Args: map[string]any{"path": "queued.txt", "content": "new"}})
	require.NoError(t, err)
	return newPreparedInvocation(preparedInvocationConfig{
		fence: fence, generations: fence.Snapshot(), call: call,
		completionProof: CompletionQueuedJobJoined,
		execute:         func(context.Context, ToolCall) (*ToolResult, error) { return nil, executionErr },
	})
}
