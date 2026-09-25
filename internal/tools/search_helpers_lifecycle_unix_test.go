//go:build darwin || linux

package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type searchCompletionReport struct {
	proof PreparedCompletionProof
	done  <-chan struct{}
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

func TestFindJoinsFDFailedHelperDescendantBeforeSystemFindFallback(t *testing.T) {
	workdir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(workdir, "target.go"), []byte("package main\n"), 0o600))

	helperDir := t.TempDir()
	pidPath := filepath.Join(t.TempDir(), "fd-descendant.pid")
	writeFailingHelperWithDescendant(t, helperDir, "fd")
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
