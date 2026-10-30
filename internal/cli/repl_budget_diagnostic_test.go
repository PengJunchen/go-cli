package cli

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/tracing"
)

func TestAssembledREPLLocalBudgetDiagnosticPreservesFailedDurableTurn(t *testing.T) {
	configureDurableCLISessionTest(t)
	t.Setenv("USERPROFILE", os.Getenv("HOME"))
	t.Chdir(t.TempDir())
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected provider dispatch", http.StatusInternalServerError)
	}))
	defer server.Close()
	cfg := durablePromptConfig(t, server.URL, filepath.Join(t.TempDir(), "store"))
	cfg.Provider.Name = "openai"
	cfg.Provider.Model = "codex/gpt-6.1-sol"
	cfg.Provider.MaxTokens = 16384
	cfg.Compaction.Strategy = "truncating"
	cfg.Compaction.MaxTokens = 1
	require.Empty(t, cfg.Agent.SystemPrompt, "use the stock mandatory system instructions")
	logPath := filepath.Join(t.TempDir(), "runtime.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	previous := slog.Default()
	slog.SetDefault(slog.New(tracing.NewRedactingSlogHandler(slog.NewTextHandler(logFile, nil))))
	t.Cleanup(func() {
		slog.SetDefault(previous)
		require.NoError(t, logFile.Close())
	})
	const canary = "REPL-LOCAL-BUDGET-INPUT-CANARY"
	var output bytes.Buffer
	editor := &scriptedLineEditor{results: []lineReadResult{{line: canary}, {line: "exit"}}}
	cmd := newInteractiveCmd(nil, &output)
	cmd.lineEditor = editor
	repl := &REPLSession{cmd: cmd, out: &output, lineEditor: editor}
	require.NoError(t, repl.Run(t.Context(), cfg, []string{"--max-tokens=1", "--thinking=high", "--no-onboarding", "--no-sandbox"}))
	// This assertion observes the real Assembly/Composite/compactor error;
	// classification must follow its type, not assume every C=1 failure alike.
	require.ErrorIs(t, repl.turnErr, compaction.ErrMandatorySystemPromptOverBudget)
	require.IsType(t, compaction.NewCompositeTokenEstimator(0), repl.assembly.Estimator)
	require.Zero(t, requests.Load(), "no model or model-catalog request before rejection")
	physical := physicalSessionIDFromAssembly(t, repl.assembly)
	snapshot, err := session.InspectPhysicalSession(t.Context(), cfg.Session.StorePath, physical)
	require.NoError(t, err)
	require.Equal(t, core.RunOutcomeError, durableTerminalOutcome(snapshot, selectedRunID(snapshot)))
	pending, err := session.InspectPendingActions(snapshot)
	require.NoError(t, err)
	require.Empty(t, pending)
	require.Contains(t, output.String(), "Session ended")
	require.Contains(t, output.String(), "mandatory")
	require.Contains(t, output.String(), "--max-tokens")
	require.NotContains(t, output.String(), canary)
	logData, err := os.ReadFile(logPath)
	require.NoError(t, err)
	require.Contains(t, string(logData), "cli.turn.failed")
	require.Contains(t, string(logData), "error_type=mandatory_system_prompt_over_budget")
	require.NotContains(t, string(logData), canary)
}
