package cli

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/session"
)

func TestDurableREPLStopsAfterRunCommitBindingFailure(t *testing.T) {
	ctx := context.Background()
	configRoot := t.TempDir()
	t.Setenv("HOME", configRoot)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(configRoot, ".config"))
	workspaceID, err := resolveWorkspaceID(os.Getwd)
	require.NoError(t, err)
	index, err := session.NewWorkspaceQuarantineIndex()
	require.NoError(t, err)
	require.NoError(t, index.EnsureWorkspace(ctx, workspaceID))

	const physicalID = "session-binding-failure"
	const branchID = "branch-main"
	prepared := newManagerTestPrepared(physicalID, branchID)
	prepared.snapshot.PublishedView = session.SessionView{
		SessionID: physicalID,
		BranchID:  branchID,
	}
	manager, err := newDurableSessionManager(newManagerTestPreparer(prepared), workspaceID)
	require.NoError(t, err)
	require.NoError(t, manager.prepareNew(ctx, "binding failure"))
	t.Cleanup(func() { require.NoError(t, manager.requestClose(ctx)) })
	durableSession := &durableCLISession{
		manager: manager, index: index, workspaceID: workspaceID,
	}
	_, err = durableSession.snapshotSelected()
	require.NoError(t, err, "the selected durable snapshot remains healthy independently of sink construction")

	loop := &durableBindingFailureLoop{}
	agent := core.NewAgentImpl("binding-test", loop)
	lines := &durableBindingFailureLineEditor{lines: []string{"first input", "must not be read", exitCommand}}
	repl := &REPLSession{
		assembly: &AgentAssembly{
			CoreRuntime: CoreRuntime{
				Agent:      agent,
				TurnRunner: core.NewEinoTurnRunner(loop),
			},
			SessionManagement: SessionManagement{SessionID: physicalID},
			durableSession:    durableSession,
		},
		lineEditor: lines,
		spanCtx:    ctx,
		out:        io.Discard,
		logger:     slog.Default(),
	}

	err = repl.readInput()
	assert.ErrorContains(t, err, "session: prepared run-commit session has no durable writer")
	var bindingErr *durableRunBindingError
	assert.ErrorAs(t, err, &bindingErr)
	assert.Equal(t, 1, lines.reads, "durable run binding failure must end input admission before the next user prompt")
	assert.Zero(t, loop.runs.Load(), "a failed run-commit sink construction must not reach the agent/provider path")
	require.NoError(t, manager.requestClose(ctx), "the failed bind returns its lease so close is not left pending")
	require.Equal(t, 1, prepared.closeCount(), "the shared prepared handle closes once after the failed run lease is returned")
}

type durableBindingFailureLoop struct {
	runs atomic.Int32
}

func (loop *durableBindingFailureLoop) Run(context.Context, core.Submission, ...core.EventStream) ([]core.AgentEvent, error) {
	loop.runs.Add(1)
	return nil, nil
}

type durableBindingFailureLineEditor struct {
	lines []string
	reads int
}

func (editor *durableBindingFailureLineEditor) ReadLine(context.Context, string) (string, error) {
	editor.reads++
	if len(editor.lines) == 0 {
		return "", io.EOF
	}
	line := editor.lines[0]
	editor.lines = editor.lines[1:]
	return line, nil
}

func (*durableBindingFailureLineEditor) SetHistory([]string)    {}
func (*durableBindingFailureLineEditor) History() []string      { return nil }
func (*durableBindingFailureLineEditor) SetCompleter(Completer) {}
