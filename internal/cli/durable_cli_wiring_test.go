package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/session"
)

type durablePromptScript struct {
	mu        sync.Mutex
	responses []string
	bodies    []string
	turns     int
}

func newDurablePromptServer(t *testing.T, responses ...string) (*httptest.Server, *durablePromptScript) {
	t.Helper()
	script := &durablePromptScript{responses: append([]string(nil), responses...)}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read provider request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		script.mu.Lock()
		script.bodies = append(script.bodies, string(body))
		response := `{"choices":[{"message":{"role":"assistant","content":"[]"}}]}`
		if !strings.Contains(string(body), "Analyze the following conversation") {
			responseIndex := script.turns
			script.turns++
			if responseIndex >= len(script.responses) {
				responseIndex = len(script.responses) - 1
			}
			response = script.responses[responseIndex]
		}
		script.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if _, err := io.WriteString(w, response); err != nil {
			t.Errorf("write provider response: %v", err)
		}
	}))
	return server, script
}

func (script *durablePromptScript) requestBodies() []string {
	script.mu.Lock()
	defer script.mu.Unlock()
	return append([]string(nil), script.bodies...)
}

func durablePromptConfig(baseURL, storeDirectory string) *config.Config {
	cfg := newPromptConfig(baseURL)
	cfg.Session.StorePath = storeDirectory
	return cfg
}

func physicalSessionIDFromOutput(t *testing.T, output string) string {
	t.Helper()
	const prefix = "Session ID: "
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, prefix) {
			id := strings.TrimSpace(strings.TrimPrefix(line, prefix))
			require.NotEmpty(t, id)
			return id
		}
	}
	t.Fatalf("prompt output does not expose a resumable physical session ID: %q", output)
	return ""
}

func physicalSessionIDFromAssembly(t *testing.T, assembly *AgentAssembly) string {
	t.Helper()
	if assembly == nil {
		return ""
	}
	field := reflect.ValueOf(assembly).Elem().FieldByName("PhysicalSessionID")
	if !field.IsValid() || field.Kind() != reflect.String {
		return ""
	}
	return field.String()
}

func requirePublishedMessages(t *testing.T, snapshot session.PreparedSessionSnapshot, expected ...string) {
	t.Helper()
	require.Len(t, snapshot.PublishedView.Messages, len(expected))
	for index, content := range expected {
		require.Equal(t, content, snapshot.PublishedView.Messages[index].Content, "message %d", index)
	}
}

func TestPromptDurableTwoInvocationsResumeExactPhysicalSession(t *testing.T) {
	configureDurableCLISessionTest(t)
	server, script := newDurablePromptServer(t,
		`{"choices":[{"message":{"role":"assistant","content":"first reply"}}]}`,
		`{"choices":[{"message":{"role":"assistant","content":"second reply"}}]}`,
	)
	defer server.Close()

	storeDirectory := filepath.Join(t.TempDir(), "sessions")
	firstOut := &bytes.Buffer{}
	firstCmd := newPromptCmd(firstOut)
	require.NoError(t, firstCmd.Run(t.Context(), durablePromptConfig(server.URL, storeDirectory), []string{"first request"}))
	physicalID := physicalSessionIDFromOutput(t, firstOut.String())
	_, err := os.Stat(filepath.Join(storeDirectory, "sessions", physicalID+".jsonl"))
	require.NoError(t, err, "a configured prompt store creates a real durable physical session")

	secondOut := &bytes.Buffer{}
	secondCmd := newPromptCmd(secondOut)
	require.NoError(t, secondCmd.Run(t.Context(), durablePromptConfig(server.URL, storeDirectory), []string{"--resume-session", physicalID, "second request"}))
	require.Equal(t, physicalID, physicalSessionIDFromOutput(t, secondOut.String()))

	bodies := script.requestBodies()
	require.Len(t, bodies, 2)
	require.Contains(t, bodies[0], `"content":"first request"`)
	require.Contains(t, bodies[1], `"content":"first request"`, "exact resume must restore the prior user message")
	require.Contains(t, bodies[1], `"content":"first reply"`, "exact resume must restore the prior assistant message")
	require.Contains(t, bodies[1], `"content":"second request"`)

	resumed, err := newDurableCLISession(t.Context(), storeDirectory, "readback", physicalID, "")
	require.NoError(t, err, "a closed command must release its physical file lock for restart")
	snapshot, err := resumed.snapshotSelected()
	require.NoError(t, err)
	requirePublishedMessages(t, snapshot, "first request", "first reply", "second request", "second reply")
	require.NoError(t, resumed.requestClose(t.Context()))
}

func TestPromptDurableForkRejectsBeforeInitializingWorkspace(t *testing.T) {
	configureDurableCLISessionTest(t)
	server, script := newDurablePromptServer(t, `{"choices":[{"message":{"content":"unused"}}]}`)
	defer server.Close()
	storeDirectory := filepath.Join(t.TempDir(), "sessions")
	configRoot := os.Getenv("XDG_CONFIG_HOME")

	cmd := newPromptCmd(io.Discard)
	err := cmd.Run(t.Context(), durablePromptConfig(server.URL, storeDirectory), []string{"--fork", "entry-1", "hello"})
	require.Error(t, err)
	var unsupported *UnsupportedDurableCLIError
	require.ErrorAs(t, err, &unsupported)
	require.ErrorIs(t, err, ErrDurableCLIUnsupported)
	require.Contains(t, err.Error(), "durable prompt fork is unsupported")
	_, statErr := os.Stat(storeDirectory)
	require.True(t, os.IsNotExist(statErr), "unsupported fork must not create a durable session")
	_, statErr = os.Stat(filepath.Join(configRoot, "go-cli", "workspace-quarantine"))
	require.True(t, os.IsNotExist(statErr), "unsupported fork must be rejected before workspace authority initialization")
	require.Empty(t, script.requestBodies(), "unsupported fork must be rejected before provider work")
}

func TestPromptDurableRejectsLegacyJSONLPathWithoutRewrite(t *testing.T) {
	configureDurableCLISessionTest(t)
	server, script := newDurablePromptServer(t, `{"choices":[{"message":{"content":"unused"}}]}`)
	defer server.Close()
	legacyPath := filepath.Join(t.TempDir(), "legacy.jsonl")
	legacyContents := []byte(`{"type":"legacy"}` + "\n")
	require.NoError(t, os.WriteFile(legacyPath, legacyContents, 0o600))

	err := newPromptCmd(io.Discard).Run(t.Context(), durablePromptConfig(server.URL, legacyPath), []string{"hello"})
	require.Error(t, err)
	require.ErrorIs(t, err, errDurableSessionPathNotDirectory)
	require.Contains(t, err.Error(), "durable session path must be a directory")
	got, readErr := os.ReadFile(legacyPath)
	require.NoError(t, readErr)
	require.Equal(t, legacyContents, got)
	require.Empty(t, script.requestBodies(), "incompatible legacy storage must fail before provider work")
}

type durablePromptFailingWriter struct {
	mu     sync.Mutex
	buffer bytes.Buffer
	needle string
	err    error
	failed bool
}

func (writer *durablePromptFailingWriter) Write(data []byte) (int, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if !writer.failed && strings.Contains(string(data), writer.needle) {
		writer.failed = true
		return 0, writer.err
	}
	return writer.buffer.Write(data)
}

func (writer *durablePromptFailingWriter) String() string {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return writer.buffer.String()
}

func TestPromptDurableOutputFailureDrainsRunAndReturnsLease(t *testing.T) {
	configureDurableCLISessionTest(t)
	server, script := newDurablePromptServer(t, `{"choices":[{"message":{"role":"assistant","content":"answer after writer failure"}}]}`)
	defer server.Close()
	writeErr := errors.New("prompt output failed")
	writer := &durablePromptFailingWriter{needle: "answer after writer failure", err: writeErr}
	storeDirectory := filepath.Join(t.TempDir(), "sessions")

	err := newPromptCmd(writer).Run(t.Context(), durablePromptConfig(server.URL, storeDirectory), []string{"persist despite output failure"})
	require.ErrorIs(t, err, writeErr)
	physicalID := physicalSessionIDFromOutput(t, writer.String())
	require.Len(t, script.requestBodies(), 1)

	resumed, resumeErr := newDurableCLISession(t.Context(), storeDirectory, "readback", physicalID, "")
	require.NoError(t, resumeErr, "writer failure must still drain and join the accepted run before Assembly close")
	snapshot, snapshotErr := resumed.snapshotSelected()
	require.NoError(t, snapshotErr)
	require.NotEmpty(t, snapshot.PublishedView.Messages)
	require.Equal(t, "persist despite output failure", snapshot.PublishedView.Messages[0].Content)
	require.LessOrEqual(t, len(snapshot.PublishedView.Messages), 2, "a canceled run may or may not finish publishing the assistant response")
	if len(snapshot.PublishedView.Messages) == 2 {
		require.Equal(t, "answer after writer failure", snapshot.PublishedView.Messages[1].Content)
	}
	require.NoError(t, resumed.requestClose(t.Context()))
}

func TestInteractiveDurableSessionKeepsOnePhysicalSessionAcrossTurns(t *testing.T) {
	configureDurableCLISessionTest(t)
	server, script := newDurablePromptServer(t,
		`{"choices":[{"message":{"role":"assistant","content":"interactive first"}}]}`,
		`{"choices":[{"message":{"role":"assistant","content":"interactive second"}}]}`,
	)
	defer server.Close()
	storeDirectory := filepath.Join(t.TempDir(), "sessions")
	cfg := durablePromptConfig(server.URL, storeDirectory)
	var out bytes.Buffer
	cmd := newInteractiveCmd(nil, &out)
	cmd.lineEditor = &scriptedLineEditor{results: []lineReadResult{
		{line: "first turn"},
		{line: "/help"},
		{line: "/session tree"},
		{line: "/session branches"},
		{line: "/session fork alternate"},
		{line: "/session resume some-entry"},
		{line: "/session clone main copied"},
		{line: "/session switch main"},
		{line: "second turn"},
		{line: "exit"},
	}}
	repl := &REPLSession{cmd: cmd, out: &out, lineEditor: cmd.lineEditor}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	require.NoError(t, repl.Run(ctx, cfg, []string{"--no-onboarding"}))
	require.NotNil(t, repl.assembly)
	physicalID := physicalSessionIDFromAssembly(t, repl.assembly)
	require.NotEmpty(t, physicalID)
	require.Nil(t, repl.pendingWrites, "durable mode must not start a second transcript writer")
	require.Nil(t, repl.flushTicker, "durable mode must not start the legacy flush ticker")
	require.Contains(t, out.String(), "Session tree", "read-only slash view must use the selected canonical snapshot")
	require.Contains(t, out.String(), "No branches in session tree.", "branch listing remains available in durable mode")
	require.Contains(t, out.String(), "durable session command is unsupported", "branch and resume mutations must fail before changing state")
	require.Contains(t, out.String(), "Durable session commands: /session tree and /session branches; resume at startup with --resume-session <physical-id>.")
	sessionHandler, ok := repl.slashReg.Lookup("session")
	require.True(t, ok)
	sessionCompletions, ok := sessionHandler.(SubcommandProvider)
	require.True(t, ok)
	require.Equal(t, []Subcommand{
		{Name: "tree", Description: "Show the durable session tree"},
		{Name: "branches", Description: "List durable session branches"},
	}, sessionCompletions.Subcommands(), "durable completion must not offer slash mutations")
	require.Empty(t, repl.sessionTree.ListBranches(), "rejected slash mutations must not change the selected tree")
	_, slashErr := repl.sessionHandler.Handle(context.Background(), session.SlashCommand{Name: "fork", Args: []string{"rejected"}})
	var unsupportedSlash *session.UnsupportedDurableSessionCommandError
	require.ErrorAs(t, slashErr, &unsupportedSlash)
	require.ErrorIs(t, slashErr, session.ErrDurableSessionCommandUnsupported)

	var turnBodies []string
	for _, body := range script.requestBodies() {
		if strings.Contains(body, `"content":"first turn"`) || strings.Contains(body, `"content":"second turn"`) {
			turnBodies = append(turnBodies, body)
		}
	}
	require.Len(t, turnBodies, 2)
	require.Contains(t, turnBodies[1], `"content":"first turn"`)
	require.Contains(t, turnBodies[1], `"content":"interactive first"`, "the second turn must use the first turn's acknowledged canonical history")
	require.Contains(t, turnBodies[1], `"content":"second turn"`)

	resumed, err := newDurableCLISession(t.Context(), storeDirectory, "readback", physicalID, "")
	require.NoError(t, err)
	snapshot, err := resumed.snapshotSelected()
	require.NoError(t, err)
	requirePublishedMessages(t, snapshot, "first turn", "interactive first", "second turn", "interactive second")
	require.Equal(t, "main", snapshot.ActiveBranchID)
	require.NoError(t, resumed.requestClose(t.Context()))
}

func TestInteractiveWithoutSessionStoreReportsSessionCommandsUnsupported(t *testing.T) {
	configureDurableCLISessionTest(t)
	server, script := newDurablePromptServer(t, `{"choices":[{"message":{"content":"unused"}}]}`)
	defer server.Close()
	cfg := durablePromptConfig(server.URL, "")
	var out bytes.Buffer
	cmd := newInteractiveCmd(nil, &out)
	cmd.lineEditor = &scriptedLineEditor{results: []lineReadResult{
		{line: "/help"},
		{line: "/session"},
		{line: "/session fork draft"},
		{line: "exit"},
	}}
	repl := &REPLSession{cmd: cmd, out: &out, lineEditor: cmd.lineEditor}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	require.NoError(t, repl.Run(ctx, cfg, []string{"--no-onboarding"}))
	require.Nil(t, repl.sessionHandler, "the default no-store CLI must not imply a session handler exists")
	require.Contains(t, out.String(), "Session commands are unsupported without session.store_path.")
	require.Contains(t, out.String(), "Unsupported: /session requires session.store_path.")
	require.NotContains(t, out.String(), "Session tree not configured")
	require.Empty(t, script.requestBodies(), "slash capability checks must not call the model")
	handler, ok := repl.slashReg.Lookup("session")
	require.True(t, ok)
	provider, ok := handler.(SubcommandProvider)
	require.True(t, ok)
	require.Empty(t, provider.Subcommands(), "no-store completion must not offer unavailable session commands")
}

func TestInteractiveConfiguredStoreRejectsBooleanResume(t *testing.T) {
	configureDurableCLISessionTest(t)
	server, script := newDurablePromptServer(t, `{"choices":[{"message":{"content":"unused"}}]}`)
	defer server.Close()
	storeDirectory := filepath.Join(t.TempDir(), "sessions")
	cfg := durablePromptConfig(server.URL, storeDirectory)
	cmd := newInteractiveCmd(nil, io.Discard)
	cmd.lineEditor = &scriptedLineEditor{results: []lineReadResult{{line: "exit"}}}
	err := cmd.Run(t.Context(), cfg, []string{"--resume", "--no-onboarding"})
	require.Error(t, err)
	var usageErr *UsageError
	require.ErrorAs(t, err, &usageErr)
	require.Contains(t, err.Error(), "--resume-session <physical-id>")
	require.Empty(t, script.requestBodies())
}

func TestInteractiveDurableResumeUsesExactPhysicalSessionAfterRestart(t *testing.T) {
	configureDurableCLISessionTest(t)
	server, script := newDurablePromptServer(t,
		`{"choices":[{"message":{"role":"assistant","content":"restart first"}}]}`,
		`{"choices":[{"message":{"role":"assistant","content":"restart second"}}]}`,
	)
	defer server.Close()
	storeDirectory := filepath.Join(t.TempDir(), "sessions")
	cfg := durablePromptConfig(server.URL, storeDirectory)

	firstCmd := newInteractiveCmd(nil, io.Discard)
	firstCmd.lineEditor = &scriptedLineEditor{results: []lineReadResult{
		{line: "first turn"},
		{line: "exit"},
	}}
	firstREPL := &REPLSession{cmd: firstCmd, out: io.Discard, lineEditor: firstCmd.lineEditor}
	require.NoError(t, firstREPL.Run(t.Context(), cfg, []string{"--no-onboarding"}))
	require.NotNil(t, firstREPL.assembly)
	physicalID := firstREPL.assembly.PhysicalSessionID
	require.NotEmpty(t, physicalID)

	secondCmd := newInteractiveCmd(nil, io.Discard)
	secondCmd.lineEditor = &scriptedLineEditor{results: []lineReadResult{
		{line: "second turn"},
		{line: "exit"},
	}}
	secondREPL := &REPLSession{cmd: secondCmd, out: io.Discard, lineEditor: secondCmd.lineEditor}
	require.NoError(t, secondREPL.Run(t.Context(), cfg, []string{"--resume-session", physicalID, "--no-onboarding"}))
	require.Equal(t, physicalID, secondREPL.assembly.PhysicalSessionID)

	var secondTurnBodies []string
	for _, body := range script.requestBodies() {
		if strings.Contains(body, `"content":"second turn"`) {
			secondTurnBodies = append(secondTurnBodies, body)
		}
	}
	require.Len(t, secondTurnBodies, 1)
	require.Contains(t, secondTurnBodies[0], `"content":"first turn"`)
	require.Contains(t, secondTurnBodies[0], `"content":"restart first"`)

	resumed, err := newDurableCLISession(t.Context(), storeDirectory, "readback", physicalID, "")
	require.NoError(t, err)
	snapshot, err := resumed.snapshotSelected()
	require.NoError(t, err)
	requirePublishedMessages(t, snapshot, "first turn", "restart first", "second turn", "restart second")
	require.NoError(t, resumed.requestClose(t.Context()))
}

func TestInteractiveDurableHistoryMutatorsAreUnsupportedAndReadOnlySlashStillWorks(t *testing.T) {
	for _, command := range []string{"compact", "clear", "retry"} {
		err := durableHistoryMutationUnsupported(command)
		var unsupported *UnsupportedDurableCLIError
		require.ErrorAs(t, err, &unsupported)
		require.ErrorIs(t, err, ErrDurableCLIUnsupported)
	}
	require.NoError(t, durableHistoryMutationUnsupported("cost"))

	configureDurableCLISessionTest(t)
	server, script := newDurablePromptServer(t,
		`{"choices":[{"message":{"role":"assistant","content":"canonical first reply"}}]}`,
		`{"choices":[{"message":{"role":"assistant","content":"canonical second reply"}}]}`,
		`{"choices":[{"message":{"role":"assistant","content":"unexpected retry reply"}}]}`,
	)
	defer server.Close()

	storeDirectory := filepath.Join(t.TempDir(), "sessions")
	cfg := durablePromptConfig(server.URL, storeDirectory)
	var out bytes.Buffer
	cmd := newInteractiveCmd(nil, &out)
	cmd.lineEditor = &scriptedLineEditor{results: []lineReadResult{
		{line: "first turn"},
		{line: "/compact"},
		{line: "/cost"},
		{line: "/retry"},
		{line: "/clear"},
		{line: "/session tree"},
		{line: "second turn"},
		{line: "exit"},
	}}
	repl := &REPLSession{cmd: cmd, out: &out, lineEditor: cmd.lineEditor}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	require.NoError(t, repl.Run(ctx, cfg, []string{"--no-onboarding"}))

	var turnBodies []string
	for _, body := range script.requestBodies() {
		if strings.Contains(body, "Analyze the following conversation") {
			continue
		}
		turnBodies = append(turnBodies, body)
	}
	require.Len(t, turnBodies, 2, "durable history mutators must not replay a provider turn")
	require.Contains(t, turnBodies[1], `"content":"first turn"`, "local history must remain aligned with the published transcript")
	require.Contains(t, turnBodies[1], `"content":"canonical first reply"`)
	require.Contains(t, turnBodies[1], `"content":"second turn"`)
	require.NotContains(t, out.String(), "unexpected retry reply")
	require.Equal(t, 3, strings.Count(out.String(), "durable /"), "compact, retry, and clear each explain the durable boundary")
	require.Contains(t, out.String(), "Known cost subtotal:", "read-only commands remain available")
	require.Contains(t, out.String(), "Provider attempts:", "the read-only report distinguishes provider attempts")
	require.Contains(t, out.String(), "Session tree", "durable published history remains readable")
	physicalID := repl.assembly.PhysicalSessionID
	require.NotEmpty(t, physicalID)
	readback, err := newDurableCLISession(t.Context(), storeDirectory, "readback", physicalID, "")
	require.NoError(t, err)
	snapshot, err := readback.snapshotSelected()
	require.NoError(t, err)
	requirePublishedMessages(t, snapshot, "first turn", "canonical first reply", "second turn", "canonical second reply")
	require.NoError(t, readback.requestClose(t.Context()))
}

func TestServeRejectsConfiguredDurableSessionBeforeListenerStart(t *testing.T) {
	configureDurableCLISessionTest(t)
	server, script := newDurablePromptServer(t, `{"choices":[{"message":{"content":"unused"}}]}`)
	defer server.Close()
	cfg := durablePromptConfig(server.URL, filepath.Join(t.TempDir(), "sessions"))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := newServeCmd(io.Discard).Run(ctx, cfg, []string{"--addr", "127.0.0.1:0", "--no-auth", "--approve", "deny"})
	require.Error(t, err)
	var unsupported *UnsupportedDurableCLIError
	require.ErrorAs(t, err, &unsupported)
	require.ErrorIs(t, err, ErrDurableCLIUnsupported)
	require.Contains(t, err.Error(), "durable ACP sessions are unsupported")
	require.Empty(t, script.requestBodies())
}
