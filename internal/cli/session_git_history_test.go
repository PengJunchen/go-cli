package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/session"
)

func historyGitFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Chdir(root)
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Session Test", "GIT_AUTHOR_EMAIL=session@example.invalid", "GIT_COMMITTER_NAME=Session Test", "GIT_COMMITTER_EMAIL=session@example.invalid")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
	}
	run("init", "-b", "main")
	run("commit", "--allow-empty", "-m", "initial")
	return root
}

func TestDurableGitAwareSessionCommandsUseHostAction(t *testing.T) {
	configureDurableCLISessionTest(t)
	root := historyGitFixture(t)
	server, script := newDurablePromptServer(t, `{"choices":[{"message":{"role":"assistant","content":"common answer"}}]}`)
	defer server.Close()
	cfg := durablePromptConfig(t, server.URL, filepath.Join(root, "history"))
	cfg.Approval.Mode = "auto"
	var out bytes.Buffer
	cmd := newInteractiveCmd(nil, &out)
	cmd.lineEditor = &scriptedLineEditor{results: []lineReadResult{{line: "question"}, {line: "/session fork feature --git topic"}, {line: "/session switch main"}, {line: "/session switch feature --git"}, {line: "exit"}}}
	require.NoError(t, cmd.Run(t.Context(), cfg, []string{"--no-onboarding"}))
	head, err := exec.Command("git", "branch", "--show-current").Output()
	require.NoError(t, err)
	require.Equal(t, "topic\n", string(head))
	require.Len(t, script.requestBodies(), 1, "host administrative actions must not invoke a model")
	require.Contains(t, out.String(), "Selected physical session")
}

func TestDurableGitAwareSessionPlanCannotStartGitMutation(t *testing.T) {
	configureDurableCLISessionTest(t)
	root := historyGitFixture(t)
	server, _ := newDurablePromptServer(t, `{"choices":[{"message":{"role":"assistant","content":"answer"}}]}`)
	defer server.Close()
	cfg := durablePromptConfig(t, server.URL, filepath.Join(root, "history"))
	cfg.Approval.Mode = "auto"
	var out bytes.Buffer
	cmd := newInteractiveCmd(nil, &out)
	cmd.lineEditor = &scriptedLineEditor{results: []lineReadResult{{line: "question"}, {line: "/plan enter"}, {line: "/session fork feature --git forbidden-topic"}, {line: "exit"}}}
	require.NoError(t, cmd.Run(t.Context(), cfg, []string{"--no-onboarding"}))
	branches, err := exec.Command("git", "branch", "--list", "forbidden-topic").Output()
	require.NoError(t, err)
	require.Empty(t, string(branches))
	require.Contains(t, out.String(), "Error:")
}

type historyGitAdmissionEditor struct {
	scriptedLineEditor
	before func(int)
	reads  int
}

func (e *historyGitAdmissionEditor) ReadLine(ctx context.Context, prompt string) (string, error) {
	e.reads++
	if e.before != nil {
		e.before(e.reads)
	}
	return e.scriptedLineEditor.ReadLine(ctx, prompt)
}

type historyGitCloseFailure struct{ session.PreparedSession }

func (p historyGitCloseFailure) Close() error {
	return errors.Join(p.PreparedSession.Close(), errors.New("injected previous session cleanup failure"))
}

func TestDurableGitAwareSessionRejectsAppliedSelectionCleanupFenceBeforeGit(t *testing.T) {
	configureDurableCLISessionTest(t)
	root := historyGitFixture(t)
	server, script := newDurablePromptServer(t, `{"choices":[{"message":{"role":"assistant","content":"answer"}}]}`)
	defer server.Close()
	cfg := durablePromptConfig(t, server.URL, filepath.Join(root, "history"))
	cfg.Approval.Mode = "auto"
	disabledMemory := false
	cfg.Memory.Enabled = &disabledMemory
	var out bytes.Buffer
	editor := &historyGitAdmissionEditor{}
	editor.results = []lineReadResult{{line: "question"}, {line: "/clear"}, {line: "/session fork after-cleanup --git must-not-execute"}, {line: "exit"}}
	cmd := newInteractiveCmd(nil, &out)
	cmd.lineEditor = editor
	repl := &REPLSession{cmd: cmd, out: &out, lineEditor: editor}
	editor.before = func(read int) {
		if read == 2 {
			manager := repl.assembly.durableSession.manager
			manager.mu.Lock()
			manager.current.prepared = historyGitCloseFailure{manager.current.prepared}
			manager.mu.Unlock()
		}
	}
	require.ErrorContains(t, repl.Run(t.Context(), cfg, []string{"--no-onboarding"}), "injected previous session cleanup failure")
	require.Contains(t, out.String(), "prior-session cleanup needs attention")
	branches, err := exec.Command("git", "branch", "--list", "must-not-execute").Output()
	require.NoError(t, err)
	require.Empty(t, string(branches), "cleanup failure fence must reject before creating any Git branch")
	require.Len(t, script.requestBodies(), 1, "administrative denial never requests a model response")
	snapshot, err := session.InspectPhysicalSession(t.Context(), cfg.Session.StorePath, repl.assembly.PhysicalSessionID)
	require.NoError(t, err)
	for _, row := range snapshot.DurableEntries {
		require.NotEqual(t, "git_create_branch", row.ToolName, "rejected work must not acquire a Git execution journal")
	}
}
