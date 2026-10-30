//go:build darwin || linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/session"
)

func TestInteractiveLocalBudgetSafeRuntimeDiagnostics(t *testing.T) {
	const childEnv = "GO_CLI_TEST_LOCAL_BUDGET_DIAGNOSTIC_CHILD"
	if os.Getenv(childEnv) == "1" {
		os.Exit(run([]string{"interactive", "--provider=openai", "--model=codex/gpt-6.1-sol", "--max-tokens=1", "--thinking=high", "--no-onboarding", "--no-sandbox"}, os.Stdout, os.Stderr, config.Load))
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected provider request", http.StatusInternalServerError)
	}))
	defer server.Close()
	home, work, store, runtimeDir := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	runtimeDir, err := filepath.EvalSymlinks(runtimeDir)
	require.NoError(t, err)
	require.NoError(t, os.Chmod(runtimeDir, 0o700))
	configDir := filepath.Join(home, ".go-cli")
	require.NoError(t, os.Mkdir(configDir, 0o700))
	const canary = "LOCAL-BUDGET-SUBPROCESS-PRIVATE-CANARY"
	cfg := map[string]any{
		"provider":   map[string]any{"name": "openai", "model": "codex/gpt-6.1-sol", "base_url": server.URL + "/v1", "api_key": canary, "max_tokens": 16384},
		"model":      map[string]any{"name": "codex/gpt-6.1-sol", "max_tokens": 16384},
		"compaction": map[string]any{"strategy": "truncating", "max_tokens": 1},
		"session":    map[string]any{"store_path": store},
		"memory":     map[string]any{"enabled": false},
		"tracing":    map[string]any{"enabled": false},
	}
	raw, err := json.Marshal(cfg)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "config.json"), raw, 0o600))
	binary, err := os.Executable()
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, "-test.run=^TestInteractiveLocalBudgetSafeRuntimeDiagnostics$")
	command.Dir = work
	logPath := filepath.Join(runtimeDir, "go-cli.log")
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + home, "USERPROFILE=" + home,
		childEnv + "=1", "GO_CLI_LOG_FILE_PATH=" + logPath,
		"NO_COLOR=1", "TERM=dumb", "NO_PROXY=127.0.0.1,localhost", "no_proxy=127.0.0.1,localhost",
	}
	if systemRoot := os.Getenv("SYSTEMROOT"); systemRoot != "" {
		command.Env = append(command.Env, "SYSTEMROOT="+systemRoot)
	}
	command.Stdin = bytes.NewBufferString("bounded public budget check\nexit\n")
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	require.NoError(t, command.Run(), "stdout=%s stderr=%s", stdout.String(), stderr.String())
	require.Zero(t, command.ProcessState.ExitCode(), "a failed turn must still allow normal REPL exit")
	require.Zero(t, requests.Load(), "no provider POST or catalog GET before local rejection")
	files, err := filepath.Glob(filepath.Join(store, "sessions", "*.jsonl"))
	require.NoError(t, err)
	require.Len(t, files, 1)
	physicalID := filepath.Base(files[0])
	physicalID = physicalID[:len(physicalID)-len(".jsonl")]
	snapshot, err := session.InspectPhysicalSession(t.Context(), store, physicalID)
	require.NoError(t, err)
	var terminals int
	for _, row := range snapshot.DurableEntries {
		if row.CheckpointKind == session.CheckpointRunTerminal {
			terminals++
			require.Equal(t, core.RunOutcomeError, row.RunOutcome)
		}
	}
	require.Equal(t, 1, terminals)
	pending, err := session.InspectPendingActions(snapshot)
	require.NoError(t, err)
	require.Empty(t, pending)
	require.Contains(t, stdout.String(), "Session ended")
	require.Contains(t, stdout.String(), "mandatory")
	require.Contains(t, stdout.String(), "--max-tokens")
	require.NotContains(t, stdout.String()+stderr.String(), canary)
	logData, err := os.ReadFile(logPath)
	require.NoError(t, err)
	require.Contains(t, string(logData), "error_type=mandatory_system_prompt_over_budget")
	require.NotContains(t, string(logData), canary)
}
