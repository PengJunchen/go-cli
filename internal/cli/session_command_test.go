package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSessionLegacyInspectAndImportPublicCommand(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "old.jsonl")
	contents := []byte("{\"id\":\"u\",\"type\":\"user\",\"content\":\"old question\"}\n{\"id\":\"a\",\"parent_id\":\"u\",\"type\":\"assistant\",\"content\":\"old answer\"}\n")
	require.NoError(t, os.WriteFile(source, contents, 0600))
	var out bytes.Buffer
	require.NoError(t, Run(t.Context(), NewDefaultConfig(false), []string{"session", "inspect-legacy", source}, &out))
	require.Contains(t, out.String(), "SHA256:")
	destination := filepath.Join(root, "destination")
	require.NoError(t, Run(t.Context(), NewDefaultConfig(false), []string{"session", "import-legacy", "--source", source, "--store", destination, "--id", "history-import"}, &out))
	require.Contains(t, out.String(), "history-import")
	require.NoError(t, Run(t.Context(), NewDefaultConfig(false), []string{"session", "list", "--store", destination}, &out))
	require.Contains(t, out.String(), "old question")
	after, err := os.ReadFile(source)
	require.NoError(t, err)
	require.Equal(t, contents, after)
}

func TestSessionCatalogOutputFailureIsReturned(t *testing.T) {
	require.ErrorContains(t, newSessionCmd(&failingWriter{}).Run(t.Context(), nil, []string{"list", "--store", filepath.Join(t.TempDir(), "absent")}), "write failed")
}

func TestSessionHistoryReadOnlyOutputFailureIsReturned(t *testing.T) {
	configureDurableCLISessionTest(t)
	durable, err := newDurableCLISession(t.Context(), filepath.Join(t.TempDir(), "history"), "output test", "", "")
	require.NoError(t, err)
	defer func() { require.NoError(t, durable.requestClose(t.Context())) }()
	repl := &REPLSession{out: &failingWriter{}, assembly: &AgentAssembly{durableSession: durable}}
	commands := &durableHistoryCommands{repl: repl}
	for _, name := range []string{"save", "load", "session"} {
		var args []string
		if name == "session" {
			args = []string{"resume"}
		}
		_, handled, err := commands.handle(t.Context(), name, args)
		require.True(t, handled)
		require.ErrorContains(t, err, "write failed")
	}
}

func TestSessionRetryOutputFailureDoesNotSubmitNewWork(t *testing.T) {
	configureDurableCLISessionTest(t)
	server, script := newDurablePromptServer(t, `{"choices":[{"message":{"role":"assistant","content":"original answer"}}]}`, `{"choices":[{"message":{"role":"assistant","content":"must not request"}}]}`)
	defer server.Close()
	cfg := durablePromptConfig(t, server.URL, filepath.Join(t.TempDir(), "history"))
	out := &durablePromptFailingWriter{needle: "Retrying last user message", err: errors.New("retry notification failed")}
	cmd := newInteractiveCmd(nil, out)
	cmd.lineEditor = &scriptedLineEditor{results: []lineReadResult{{line: "question"}, {line: "/retry"}, {line: "exit"}}}
	require.NoError(t, cmd.Run(t.Context(), cfg, []string{"--no-onboarding"}))
	require.Len(t, script.requestBodies(), 1, "failed retry acknowledgement must not submit additional model work")
	require.Contains(t, out.String(), "retry notification failed")
}
