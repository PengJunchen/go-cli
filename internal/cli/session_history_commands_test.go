package cli

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/session"
)

func runSessionHistoryJourney(t *testing.T, lines []string, responses ...string) (*REPLSession, *durablePromptScript, string) {
	t.Helper()
	configureDurableCLISessionTest(t)
	server, script := newDurablePromptServer(t, responses...)
	t.Cleanup(server.Close)
	cfg := durablePromptConfig(t, server.URL, filepath.Join(t.TempDir(), "sessions"))
	var out bytes.Buffer
	editor := &scriptedLineEditor{}
	for _, line := range lines {
		editor.results = append(editor.results, lineReadResult{line: line})
	}
	cmd := newInteractiveCmd(nil, &out)
	cmd.lineEditor = editor
	repl := &REPLSession{cmd: cmd, out: &out, lineEditor: editor}
	require.NoError(t, repl.Run(context.Background(), cfg, []string{"--no-onboarding"}))
	return repl, script, out.String()
}

func TestDurableSessionSaveLoadCatalogThroughREPL(t *testing.T) {
	_, script, out := runSessionHistoryJourney(t, []string{"question", "/save", "/load", "/session resume", "exit"}, `{"choices":[{"message":{"role":"assistant","content":"canonical answer"}}]}`)
	require.Len(t, script.requestBodies(), 1)
	require.Contains(t, out, "Durable records synced")
	require.Contains(t, out, "Persisted session")
	require.Contains(t, out, "canonical answer")
	require.Contains(t, out, "Physical sessions")
	require.NotContains(t, out, "Session store not configured")
}

func TestInteractiveDurableClearAndRetry(t *testing.T) {
	_, script, out := runSessionHistoryJourney(t, []string{"first question", "/retry", "/clear", "fresh question", "exit"},
		`{"choices":[{"message":{"role":"assistant","content":"first answer"}}]}`,
		`{"choices":[{"message":{"role":"assistant","content":"replacement answer"}}]}`,
		`{"choices":[{"message":{"role":"assistant","content":"fresh answer"}}]}`)
	bodies := script.requestBodies()
	require.Len(t, bodies, 3)
	require.Contains(t, bodies[1], "first question")
	require.NotContains(t, bodies[1], "first answer")
	require.NotContains(t, bodies[2], "first question")
	require.NotContains(t, bodies[2], "replacement answer")
	require.Contains(t, out, "Previous session retained")
}

func TestInteractiveDurableBranchesThroughREPL(t *testing.T) {
	_, script, out := runSessionHistoryJourney(t, []string{"common question", "/session fork feature", "feature question", "/session switch main", "main question", "/session clone feature copied", "copied question", "exit"},
		`{"choices":[{"message":{"role":"assistant","content":"common answer"}}]}`,
		`{"choices":[{"message":{"role":"assistant","content":"feature answer"}}]}`,
		`{"choices":[{"message":{"role":"assistant","content":"main answer"}}]}`,
		`{"choices":[{"message":{"role":"assistant","content":"copied answer"}}]}`)
	bodies := script.requestBodies()
	require.Len(t, bodies, 4)
	require.Contains(t, bodies[2], "common answer")
	require.NotContains(t, bodies[2], "feature answer")
	require.Contains(t, bodies[3], "feature answer")
	require.NotContains(t, bodies[3], "main answer")
	require.NotContains(t, out, "unsupported")
}

func TestPromptDurableForkAndBooleanResumeCompatibility(t *testing.T) {
	configureDurableCLISessionTest(t)
	server, script := newDurablePromptServer(t, `{"choices":[{"message":{"role":"assistant","content":"old answer"}}]}`, `{"choices":[{"message":{"role":"assistant","content":"fork answer"}}]}`)
	defer server.Close()
	store := filepath.Join(t.TempDir(), "sessions")
	cfg := durablePromptConfig(t, server.URL, store)
	var out bytes.Buffer
	require.NoError(t, newPromptCmd(&out).Run(t.Context(), cfg, []string{"old question"}))
	physical := physicalSessionIDFromOutput(t, out.String())
	snapshot, err := session.InspectPhysicalSession(t.Context(), store, physical)
	require.NoError(t, err)
	leaf := snapshot.PublishedView.SourceEntryIDs[len(snapshot.PublishedView.SourceEntryIDs)-1]
	require.NoError(t, newPromptCmd(&out).Run(t.Context(), cfg, []string{"--fork", leaf, "fork question"}))
	require.Contains(t, script.requestBodies()[1], "old answer")
	cmd := newInteractiveCmd(nil, &out)
	cmd.lineEditor = &scriptedLineEditor{results: []lineReadResult{{line: "/history"}, {line: "exit"}}}
	require.NoError(t, cmd.Run(t.Context(), cfg, []string{"--resume", "--no-onboarding"}))
	require.Contains(t, out.String(), "fork answer")
	require.Len(t, script.requestBodies(), 2)
}

func TestInteractiveDurableManualCompactionUsesConfiguredSummaryAndRestarts(t *testing.T) {
	configureDurableCLISessionTest(t)
	answer := strings.Repeat("large historical answer ", 100)
	server, script := newDurablePromptServer(t, fmt.Sprintf(`{"choices":[{"message":{"role":"assistant","content":%q}}]}`, answer), `{"choices":[{"message":{"role":"assistant","content":"concise source summary"}}]}`, `{"choices":[{"message":{"role":"assistant","content":"next answer"}}]}`)
	defer server.Close()
	store := filepath.Join(t.TempDir(), "sessions")
	cfg := durablePromptConfig(t, server.URL, store)
	cfg.Compaction.Strategy = "summary"
	cfg.SmallModel.Model = "summary-model"
	var out bytes.Buffer
	require.NoError(t, newPromptCmd(&out).Run(t.Context(), cfg, []string{strings.Repeat("old question ", 100)}))
	physical := physicalSessionIDFromOutput(t, out.String())
	cmd := newInteractiveCmd(nil, &out)
	cmd.lineEditor = &scriptedLineEditor{results: []lineReadResult{{line: "/compact"}, {line: "exit"}}}
	require.NoError(t, cmd.Run(t.Context(), cfg, []string{"--resume-session", physical, "--max-tokens", "100", "--no-onboarding"}))
	require.Contains(t, out.String(), "Compacted history:")
	snapshot, err := session.InspectPhysicalSession(t.Context(), store, physical)
	require.NoError(t, err)
	require.Contains(t, snapshot.PublishedView.Messages[0].Content, "concise source summary")
	require.NotContains(t, snapshot.PublishedView.Messages[0].Content, answer)
	require.Len(t, script.requestBodies(), 2, "manual compact uses one auxiliary model request and no new agent turn")
	require.Contains(t, script.requestBodies()[1], `"model":"summary-model"`)
}

func TestHistorySelectionAppliedCleanupErrorHydratesActualSelection(t *testing.T) {
	old := newManagerTestPrepared("physical-old", "main")
	old.closeErr = errManagerTestClose
	fresh := newManagerTestPrepared("physical-new", "main")
	fresh.snapshot.PublishedView = session.SessionView{SessionID: "physical-new", BranchID: "main"}
	preparer := newManagerTestPreparer(old)
	manager := newManagerTestManager(t, preparer)
	require.NoError(t, manager.prepareNew(t.Context(), "old"))
	preparer.newSession = fresh
	agent := core.NewAgentImpl("history-test", stubLoop{})
	agent.SetHistory([]core.AgentMessage{{Role: "user", Content: "old context"}})
	repl := &REPLSession{spanCtx: t.Context(), assembly: &AgentAssembly{CoreRuntime: CoreRuntime{Agent: agent}, SessionManagement: SessionManagement{PhysicalSessionID: "physical-old"}, durableSession: &durableCLISession{manager: manager}}}
	err := repl.selectHistorySession(t.Context(), "", true)
	var applied *SelectionAppliedCleanupError
	require.ErrorAs(t, err, &applied)
	require.Equal(t, "physical-new", repl.assembly.PhysicalSessionID)
	require.Equal(t, "physical-new", repl.slashCtx.sessionID)
	require.Empty(t, agent.Messages())
	require.Error(t, repl.historyAdmissionError)
	_ = manager.requestClose(t.Context())
}
