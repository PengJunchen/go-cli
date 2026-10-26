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
)

type searchCompletionReport struct {
	proof PreparedCompletionProof
	done  <-chan struct{}
}

func TestSearchHelperEscapedPipeWriter(t *testing.T) {
	pidPath := os.Getenv("SEARCH_HELPER_ESCAPED_PIPE_PID")
	if pidPath == "" {
		return
	}
	if _, err := syscall.Setsid(); err != nil {
		t.Fatalf("escape helper process group: %v", err)
	}
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatalf("write escaped helper PID: %v", err)
	}
	time.Sleep(time.Hour)
}

func TestGrepForcedPipeCloseStopsFallbackAndWithholdsProof(t *testing.T) {
	workdir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(workdir, "match.txt"), []byte("needle\n"), 0o600))
	helperDir := t.TempDir()
	pidPath := filepath.Join(t.TempDir(), "rg-escaped.pid")
	writeFailingHelperWithEscapedPipeWriter(t, helperDir, "rg", pidPath)
	t.Setenv("PATH", helperDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SEARCH_HELPER_ESCAPED_PIPE_PID", pidPath)

	reported := make(chan searchCompletionReport, 2)
	ctx := withPreparedCompletionReporter(context.Background(), func(proof PreparedCompletionProof, done <-chan struct{}) {
		reported <- searchCompletionReport{proof: proof, done: done}
	})
	tool := NewGrepTool(WithGrepWorkdir(workdir))
	result, err := tool.Execute(ctx, ToolCall{Args: map[string]any{"pattern": "needle"}})
	pid := waitForChildPIDFile(t, pidPath)
	assert.NoError(t, syscall.Kill(pid, 0), "escaped helper must still be observable when Grep returns")
	require.Error(t, err)
	assert.Empty(t, result.Output, "an uncontained helper failure must not return pure-Go fallback output")
	assertUnknownWithoutPendingResource(t, err)
	assertNoProcessCompletionReport(t, reported)
	killProcessFromFile(t, pidPath)
	assertProcessFromFileStopped(t, pidPath)
}

func TestPreparedScopedGrepForcedPipeCloseDoesNotAttestInlineFallback(t *testing.T) {
	workdir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(workdir, "match.txt"), []byte("needle\n"), 0o600))
	helperDir := t.TempDir()
	pidPath := filepath.Join(t.TempDir(), "prepared-rg-escaped.pid")
	writeFailingHelperWithEscapedPipeWriter(t, helperDir, "rg", pidPath)
	t.Setenv("PATH", helperDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SEARCH_HELPER_ESCAPED_PIPE_PID", pidPath)

	ctx := context.Background()
	base := NewDefaultToolRegistry().(*DefaultToolRegistry) //nolint:errcheck
	tool := NewGrepTool(WithGrepWorkdir(workdir), WithGrepPathWhitelist([]string{workdir}))
	require.NoError(t, base.registerTrustedScopedWorkspaceRead(ctx, tool))
	registry := NewMiddlewareToolRegistry(base)
	configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{})
	invocation, err := registry.PrepareToolCall(ctx, ToolCall{Name: "grep", Args: map[string]any{"pattern": "needle", "path": workdir}})
	require.NoError(t, err)
	result, dispatchErr := invocation.Dispatch(ctx)
	pid := waitForChildPIDFile(t, pidPath)
	require.Error(t, dispatchErr)
	assert.Empty(t, result.Output, "an uncontained helper failure must not return fallback output")
	assertUnknownWithoutPendingResource(t, dispatchErr)

	signal := invocation.CompletionSignal()
	require.NotNil(t, signal.Done(), "the local invocation return remains observable")
	select {
	case <-signal.Done():
	default:
		t.Fatal("the failed local reader has returned")
	}
	assert.Empty(t, signal.Proof(), "a started but uncontained helper cannot be relabeled as a pure-Go completion")
	assert.NoError(t, syscall.Kill(pid, 0), "the deliberately escaped writer remains alive for the assertion")
	killProcessFromFile(t, pidPath)
	assertProcessFromFileStopped(t, pidPath)
}

func TestGrepTimeoutWithEscapedPipeWriterPreservesCancellationCause(t *testing.T) {
	workdir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(workdir, "match.txt"), []byte("needle\n"), 0o600))
	helperDir := t.TempDir()
	pidPath := filepath.Join(t.TempDir(), "rg-timeout-escaped.pid")
	testBinary, err := os.Executable()
	require.NoError(t, err)
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' '{"type":"match","data":{"path":{"text":"match.txt"},"lines":{"text":"needle\n"},"line_number":1}}'
%s -test.run=%s &
child=$!
while [ ! -s %s ]; do sleep 0.01; done
wait "$child"
`,
		shellQuoteTestArgument(testBinary),
		shellQuoteTestArgument("^TestSearchHelperEscapedPipeWriter$"),
		shellQuoteTestArgument(pidPath))
	require.NoError(t, os.WriteFile(filepath.Join(helperDir, "rg"), []byte(script), 0o700))
	t.Setenv("PATH", helperDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SEARCH_HELPER_ESCAPED_PIPE_PID", pidPath)
	deadline := newControlledDeadlineContext()
	t.Cleanup(func() { deadline.expire() })
	t.Cleanup(func() { killProcessFromFile(t, pidPath) })

	reported := make(chan searchCompletionReport, 1)
	ctx := withPreparedCompletionReporter(deadline, func(proof PreparedCompletionProof, done <-chan struct{}) {
		reported <- searchCompletionReport{proof: proof, done: done}
	})
	type response struct {
		result *ToolResult
		err    error
	}
	done := make(chan response, 1)
	go func() {
		result, err := NewGrepTool(WithGrepWorkdir(workdir)).Execute(ctx, ToolCall{Args: map[string]any{"pattern": "needle"}})
		done <- response{result: result, err: err}
	}()
	pid := waitForChildPIDFile(t, pidPath)
	require.NoError(t, syscall.Kill(pid, 0), "escaped writer remains alive when Grep returns")
	deadline.expire()
	var got response
	select {
	case got = <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Grep did not return after the controlled deadline while its helper writer was escaped")
	}
	require.Error(t, got.err)
	require.Empty(t, got.result.Output, "unknown helper effects must not fall back to Go search")
	assertUnknownWithoutPendingResource(t, got.err)
	assertNoProcessCompletionReport(t, reported)
	killProcessFromFile(t, pidPath)
	assertProcessFromFileStopped(t, pidPath)
	require.ErrorIs(t, got.err, context.DeadlineExceeded, "unknown outcome must retain the caller's cancellation cause")
}

type controlledDeadlineContext struct {
	context.Context
	done chan struct{}
}

func newControlledDeadlineContext() *controlledDeadlineContext {
	return &controlledDeadlineContext{Context: context.Background(), done: make(chan struct{})}
}

func (ctx *controlledDeadlineContext) Deadline() (time.Time, bool) {
	return time.Time{}, false
}

func (ctx *controlledDeadlineContext) Done() <-chan struct{} {
	return ctx.done
}

func (ctx *controlledDeadlineContext) Err() error {
	select {
	case <-ctx.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

func (ctx *controlledDeadlineContext) expire() {
	select {
	case <-ctx.done:
	default:
		close(ctx.done)
	}
}

func TestPreparedScopedGrepTimeoutWithEscapedWriterKeepsCompletionUnproven(t *testing.T) {
	workdir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(workdir, "match.txt"), []byte("needle\n"), 0o600))
	helperDir := t.TempDir()
	pidPath := filepath.Join(t.TempDir(), "prepared-rg-timeout-escaped.pid")
	testBinary, err := os.Executable()
	require.NoError(t, err)
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' '{"type":"match","data":{"path":{"text":"match.txt"},"lines":{"text":"needle\\n"},"line_number":1}}'
%s -test.run=%s &
child=$!
while [ ! -s %s ]; do sleep 0.01; done
wait "$child"
`,
		shellQuoteTestArgument(testBinary),
		shellQuoteTestArgument("^TestSearchHelperEscapedPipeWriter$"),
		shellQuoteTestArgument(pidPath))
	require.NoError(t, os.WriteFile(filepath.Join(helperDir, "rg"), []byte(script), 0o700))
	t.Setenv("PATH", helperDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SEARCH_HELPER_ESCAPED_PIPE_PID", pidPath)
	t.Cleanup(func() { killProcessFromFile(t, pidPath) })

	deadline := newControlledDeadlineContext()
	t.Cleanup(func() { deadline.expire() })
	base := NewDefaultToolRegistry().(*DefaultToolRegistry) //nolint:errcheck
	tool := NewGrepTool(WithGrepWorkdir(workdir), WithGrepPathWhitelist([]string{workdir}))
	require.NoError(t, base.registerTrustedScopedWorkspaceRead(context.Background(), tool))
	registry := NewMiddlewareToolRegistry(base)
	configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{})
	invocation, err := registry.PrepareToolCall(deadline, ToolCall{Name: "grep", Args: map[string]any{"pattern": "needle", "path": workdir}})
	require.NoError(t, err)
	dispatchResult := make(chan struct {
		result *ToolResult
		err    error
	}, 1)
	go func() {
		result, dispatchErr := invocation.Dispatch(deadline)
		dispatchResult <- struct {
			result *ToolResult
			err    error
		}{result: result, err: dispatchErr}
	}()
	pid := waitForChildPIDFile(t, pidPath)
	deadline.expire()
	var got struct {
		result *ToolResult
		err    error
	}
	select {
	case got = <-dispatchResult:
	case <-time.After(3 * time.Second):
		t.Fatal("prepared Grep did not return after the controlled deadline")
	}
	require.ErrorIs(t, got.err, context.DeadlineExceeded)
	assert.Empty(t, got.result.Output, "an unknown helper must not return fallback search output")
	assertUnknownWithoutPendingResource(t, got.err)

	signal := invocation.CompletionSignal()
	require.NotNil(t, signal.Done())
	select {
	case <-signal.Done():
	default:
		t.Fatal("the prepared executor returned after the helper's forced-close path")
	}
	assert.Empty(t, signal.Proof(), "a helper timeout cannot be relabeled as synchronous no-process completion")
	assert.NoError(t, syscall.Kill(pid, 0), "the deliberately escaped writer remains alive for the assertion")
	killProcessFromFile(t, pidPath)
	assertProcessFromFileStopped(t, pidPath)
}

func TestFindForcedPipeCloseStopsFallbackAndWithholdsProof(t *testing.T) {
	workdir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(workdir, "target.go"), []byte("package main\n"), 0o600))
	helperDir := t.TempDir()
	pidPath := filepath.Join(t.TempDir(), "fd-escaped.pid")
	writeFailingHelperWithEscapedPipeWriter(t, helperDir, "fd", pidPath)
	t.Setenv("PATH", helperDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SEARCH_HELPER_ESCAPED_PIPE_PID", pidPath)

	reported := make(chan searchCompletionReport, 2)
	ctx := withPreparedCompletionReporter(context.Background(), func(proof PreparedCompletionProof, done <-chan struct{}) {
		reported <- searchCompletionReport{proof: proof, done: done}
	})
	tool := NewFindTool(WithFindWorkdir(workdir))
	result, err := tool.Execute(ctx, ToolCall{Args: map[string]any{"pattern": "target.go", "type": "f"}})
	pid := waitForChildPIDFile(t, pidPath)
	assert.NoError(t, syscall.Kill(pid, 0), "escaped helper must still be observable when Find returns")
	require.Error(t, err)
	assert.Empty(t, result.Output, "an uncontained fd failure must not continue to system find or pure-Go fallback")
	assertUnknownWithoutPendingResource(t, err)
	assertNoProcessCompletionReport(t, reported)
	killProcessFromFile(t, pidPath)
	assertProcessFromFileStopped(t, pidPath)
}

func TestFindCaptureLimitJoinsContainmentFailure(t *testing.T) {
	workdir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(workdir, "target.go"), []byte("package main\n"), 0o600))
	helperDir := t.TempDir()
	markerDir := t.TempDir()
	pidPath := filepath.Join(t.TempDir(), "fd-capture-escaped.pid")
	fallbackMarker := filepath.Join(markerDir, "find-fallback")
	testBinary, err := os.Executable()
	require.NoError(t, err)
	fdScript := fmt.Sprintf(`#!/bin/sh
printf '%%04097d\000' 0
%s -test.run=%s &
while [ ! -s %s ]; do sleep 0.01; done
exit 2
`,
		shellQuoteTestArgument(testBinary),
		shellQuoteTestArgument("^TestSearchHelperEscapedPipeWriter$"),
		shellQuoteTestArgument(pidPath))
	findScript := "#!/bin/sh\nprintf called > " + shellQuoteTestArgument(fallbackMarker) + "\nprintf 'fallback.txt\\000'\n"
	require.NoError(t, os.WriteFile(filepath.Join(helperDir, "fd"), []byte(fdScript), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(helperDir, "find"), []byte(findScript), 0o700))
	originalPath := os.Getenv("PATH")
	t.Setenv("PATH", helperDir+string(os.PathListSeparator)+originalPath)
	t.Setenv("SEARCH_HELPER_ESCAPED_PIPE_PID", pidPath)
	t.Cleanup(func() { killProcessFromFile(t, pidPath) })

	result, err := NewFindTool(WithFindWorkdir(workdir)).Execute(context.Background(), ToolCall{Args: map[string]any{"pattern": "target.go", "type": "f"}})
	require.ErrorIs(t, err, errDirectoryCaptureLimit)
	require.ErrorIs(t, err, errProcessContainmentUnconfirmed)
	require.NotNil(t, result)
	assert.Empty(t, result.Output, "an uncontained helper failure must not return captured or fallback output")
	assert.NoFileExists(t, fallbackMarker, "capture saturation joined with unknown cleanup must stop fallback")
	assertUnknownWithoutPendingResource(t, err)
	pid := waitForChildPIDFile(t, pidPath)
	assert.NoError(t, syscall.Kill(pid, 0), "escaped helper must still be observable when Find returns")
	killProcessFromFile(t, pidPath)
	assertProcessFromFileStopped(t, pidPath)
}

func writeFailingHelperWithEscapedPipeWriter(t *testing.T, dir, name, pidPath string) {
	t.Helper()
	testBinary, err := os.Executable()
	require.NoError(t, err)
	prelude := ""
	if name == "rg" {
		prelude = `printf '%s\n' '{"type":"match","data":{"path":{"text":"match.txt"},"lines":{"text":"needle\n"},"line_number":1}}'` + "\n"
	}
	script := "#!/bin/sh\n" + prelude + fmt.Sprintf("%s -test.run=%s &\nchild=$!\nwhile [ ! -s %s ]; do sleep 0.01; done\nexit 2\n",
		shellQuoteTestArgument(testBinary),
		shellQuoteTestArgument("^TestSearchHelperEscapedPipeWriter$"),
		shellQuoteTestArgument(pidPath))
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(script), 0o700))
	t.Cleanup(func() {
		killProcessFromFile(t, pidPath)
	})
}

func assertUnknownWithoutPendingResource(t *testing.T, err error) {
	t.Helper()
	var unknown interface{ OutcomeUnknown() bool }
	require.ErrorAs(t, err, &unknown)
	assert.True(t, unknown.OutcomeUnknown())
	var pending PendingExecutionError
	assert.False(t, errors.As(err, &pending), "the local reader joined; no truthful handle can track an escaped writer")
}

func assertNoProcessCompletionReport(t *testing.T, reported <-chan searchCompletionReport) {
	t.Helper()
	select {
	case got := <-reported:
		t.Fatalf("uncontained helper emitted positive completion proof %q", got.proof)
	default:
	}
}

func TestGrepJoinsFailedHelperDescendantBeforeFallback(t *testing.T) {
	workdir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(workdir, "match.txt"), []byte("needle here\n"), 0o600))

	helperDir := t.TempDir()
	pidPath := filepath.Join(t.TempDir(), "rg-descendant.pid")
	writeFailingHelperWithDescendant(t, helperDir, "rg")
	t.Setenv("PATH", helperDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SEARCH_HELPER_DESCENDANT_PID", pidPath)
	defer killProcessFromFile(t, pidPath)

	tool := NewGrepTool(WithGrepWorkdir(workdir))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reported := make(chan searchCompletionReport, 2)
	ctx = withPreparedCompletionReporter(ctx, func(proof PreparedCompletionProof, done <-chan struct{}) {
		reported <- searchCompletionReport{proof: proof, done: done}
	})
	type response struct {
		result *ToolResult
		err    error
	}
	done := make(chan response, 1)
	go func() {
		result, err := tool.Execute(ctx, ToolCall{Args: map[string]any{"pattern": "needle"}})
		done <- response{result: result, err: err}
	}()

	waitForChildPIDFile(t, pidPath)
	select {
	case got := <-done:
		require.NoError(t, got.err)
		require.NotNil(t, got.result)
		require.Contains(t, got.result.Output, "match.txt:1:needle here")
	case <-time.After(3 * time.Second):
		cancel()
		killProcessFromFile(t, pidPath)
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("Grep did not return even after the test released its helper descendant")
		}
		t.Fatal("Grep waited for a helper descendant instead of joining its process group before fallback")
	}
	assertProcessFromFileStopped(t, pidPath)
	assertOneCompletedProcessGroupReport(t, reported)
}

func TestFindTreatsFDExitOneAsErrorAndJoinsDescendantBeforeFallback(t *testing.T) {
	workdir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(workdir, "target.go"), []byte("package main\n"), 0o600))

	helperDir := t.TempDir()
	pidPath := filepath.Join(t.TempDir(), "fd-descendant.pid")
	fdScript := "#!/bin/sh\nsleep 10 &\nprintf '%s' \"$!\" > \"$SEARCH_HELPER_DESCENDANT_PID\"\nexit 1\n"
	require.NoError(t, os.WriteFile(filepath.Join(helperDir, "fd"), []byte(fdScript), 0o700))
	t.Setenv("PATH", helperDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SEARCH_HELPER_DESCENDANT_PID", pidPath)
	defer killProcessFromFile(t, pidPath)

	tool := NewFindTool(WithFindWorkdir(workdir))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reported := make(chan searchCompletionReport, 2)
	ctx = withPreparedCompletionReporter(ctx, func(proof PreparedCompletionProof, done <-chan struct{}) {
		reported <- searchCompletionReport{proof: proof, done: done}
	})
	type response struct {
		result *ToolResult
		err    error
	}
	done := make(chan response, 1)
	go func() {
		result, err := tool.Execute(ctx, ToolCall{Args: map[string]any{"pattern": "target.go", "type": "f"}})
		done <- response{result: result, err: err}
	}()

	waitForChildPIDFile(t, pidPath)
	select {
	case got := <-done:
		require.NoError(t, got.err)
		require.NotNil(t, got.result)
		require.Contains(t, got.result.Output, "target.go")
	case <-time.After(3 * time.Second):
		cancel()
		killProcessFromFile(t, pidPath)
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("Find did not return even after the test released its helper descendant")
		}
		t.Fatal("Find waited for an fd descendant instead of joining its process group before trying system find")
	}
	assertProcessFromFileStopped(t, pidPath)
	assertOneCompletedProcessGroupReport(t, reported)
}

func assertOneCompletedProcessGroupReport(t *testing.T, reported <-chan searchCompletionReport) {
	t.Helper()
	select {
	case got := <-reported:
		require.Equal(t, CompletionOwnedProcessGroupJoined, got.proof)
		select {
		case <-got.done:
		default:
			t.Fatal("completed helper route reported a channel before it closed")
		}
	case <-time.After(time.Second):
		t.Fatal("managed helper route did not report its joined process group")
	}
	select {
	case <-reported:
		t.Fatal("fallback route reported more than once for one action")
	default:
	}
}

func writeFailingHelperWithDescendant(t *testing.T, dir, name string) {
	t.Helper()
	path := filepath.Join(dir, name)
	script := "#!/bin/sh\n" +
		"sleep 10 &\n" +
		"printf '%s' \"$!\" > \"$SEARCH_HELPER_DESCENDANT_PID\"\n" +
		"exit 2\n"
	require.NoError(t, os.WriteFile(path, []byte(script), 0o700))
}

func waitForChildPIDFile(t *testing.T, path string) int {
	t.Helper()
	var pid int
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(data)))
		return err == nil && pid > 0
	}, time.Second, 10*time.Millisecond, "helper descendant should publish its PID before exiting")
	return pid
}

func assertProcessFromFileStopped(t *testing.T, path string) {
	t.Helper()
	pid := waitForChildPIDFile(t, path)
	require.Eventually(t, func() bool {
		err := syscall.Kill(pid, 0)
		return errors.Is(err, syscall.ESRCH)
	}, time.Second, 10*time.Millisecond, "managed helper descendant should be stopped before fallback returns")
}

func killProcessFromFile(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
}
