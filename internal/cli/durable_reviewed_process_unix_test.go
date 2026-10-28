//go:build darwin || linux

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/session"
)

func TestReviewedEscapedPipeWriter(t *testing.T) {
	pidFile := os.Getenv("GO_CLI_REVIEWED_ESCAPED_PID")
	if pidFile == "" {
		return
	}
	if _, err := syscall.Setsid(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Hour)
}

func TestPromptDurableGitAggregateWaitsForLaterProcessAndRetainsUnknownReader(t *testing.T) {
	configureDurableCLISessionTest(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	t.Chdir(root)
	bin := t.TempDir()
	pidFile, started, release, trace := filepath.Join(root, "escaped.pid"), filepath.Join(root, "later-started"), filepath.Join(root, "release-leader"), filepath.Join(root, "commands")
	binary, err := os.Executable()
	require.NoError(t, err)
	t.Setenv("GO_CLI_REVIEWED_ESCAPED_PID", pidFile)
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }
	body := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %s
case "$*" in
  *--is-inside-work-tree*) exit 0 ;;
  'add -A') exit 0 ;;
  commit*)
    %s -test.run='^TestReviewedEscapedPipeWriter$' &
    while [ ! -s %s ]; do /bin/sleep .01; done
    printf started > %s
    while [ ! -f %s ]; do /bin/sleep .01; done
    exit 0 ;;
  *) printf unexpected-followup; exit 0 ;;
esac
`, quote(trace), quote(binary), quote(pidFile), quote(started), quote(release))
	require.NoError(t, os.WriteFile(filepath.Join(bin, "git"), []byte(body), 0o700))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Cleanup(func() {
		if data, readErr := os.ReadFile(pidFile); readErr == nil {
			if pid, parseErr := strconv.Atoi(string(data)); parseErr == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	server, script := newDurablePromptServer(t, durableApprovalToolResponse(t, "aggregate-git", "git_commit", map[string]any{"message": "three commands", "add_all": true}))
	defer server.Close()
	store := filepath.Join(t.TempDir(), "sessions")
	cfg := durablePromptConfig(t, server.URL, store)
	disabledMemory := false
	cfg.Memory.Enabled = &disabledMemory
	cfg.Git.WorkDir = root
	var output bytes.Buffer
	finished := make(chan error, 1)
	go func() {
		finished <- newPromptCmd(&output).Run(ctx, cfg, []string{"--approve", "auto", "run the explicit commit"})
	}()
	require.Eventually(t, func() bool { _, statErr := os.Stat(started); return statErr == nil }, 3*time.Second, 5*time.Millisecond)
	select {
	case runErr := <-finished:
		t.Fatalf("action returned while its third subprocess was blocked: %v", runErr)
	default:
	}
	files, err := filepath.Glob(filepath.Join(store, "sessions", "*.jsonl"))
	require.NoError(t, err)
	require.Len(t, files, 1)
	journal, err := os.ReadFile(files[0])
	require.NoError(t, err)
	for _, line := range bytes.Split(journal, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var row session.SessionEntry
		require.NoError(t, json.Unmarshal(line, &row))
		if row.ToolCallID == "aggregate-git" {
			require.Empty(t, row.ReceiptOutcome)
			require.Empty(t, row.CompletionProof)
		}
	}
	require.NoError(t, os.WriteFile(release, nil, 0o600))
	require.Error(t, <-finished, "an exited leader with an escaped output writer cannot establish a known command result")
	id := physicalSessionIDFromOutput(t, output.String())
	resumed, err := newDurableCLISession(ctx, store, "aggregate inspect", id, "")
	require.NoError(t, err)
	defer func() { require.NoError(t, resumed.requestClose(context.Background())) }()
	snapshot, err := resumed.snapshotSelected()
	require.NoError(t, err)
	pending, err := session.InspectPendingActions(snapshot)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	for _, row := range snapshot.DurableEntries {
		if row.ToolCallID == "aggregate-git" {
			require.Empty(t, row.CompletionProof)
		}
	}
	commands, err := os.ReadFile(trace)
	require.NoError(t, err)
	require.Contains(t, string(commands), "add -A")
	require.Contains(t, string(commands), "commit -m")
	require.NotContains(t, string(commands), "rev-parse HEAD", "unknown third command must stop the fourth hash probe")
	require.Len(t, script.requestBodies(), 1, "unknown effect never solicits a model replay")
	pidData, err := os.ReadFile(pidFile)
	require.NoError(t, err)
	pid, err := strconv.Atoi(string(pidData))
	require.NoError(t, err)
	require.NoError(t, syscall.Kill(pid, 0), "the independent escaped reader is still live when unknown is recorded")
}
