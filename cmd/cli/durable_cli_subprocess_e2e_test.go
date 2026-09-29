package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/session"
)

const durableCLIFixtureAPIKey = "durable-cli-fixture-key"

func TestDurableCLIBinaryPersistsWriteReceiptAndDoesNotReplayAfterRestart(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	binary := filepath.Join(t.TempDir(), "go-cli")
	build := exec.Command("go", "build", "-o", binary, "./cmd/cli")
	build.Dir = root
	buildOutput, err := build.CombinedOutput()
	require.NoError(t, err, "build real CLI: %s", buildOutput)
	t.Logf("build command: go build -o %s ./cmd/cli\nbuild exit: 0\nbuild output: %s", binary, buildOutput)

	t.Run("normal process exit and exact physical resume", func(t *testing.T) {
		fixture := newDurableCLIBinaryFixture(t, binary, false)
		defer fixture.server.Close()

		first := fixture.run(t, "prompt", "-provider=openai", "-output=text", "-approve=auto", "create durable marker")
		requireCLIChildExit(t, first, 0)
		physicalID := durableCLIPhysicalSessionIDFromOutput(t, first.stdout)
		require.Contains(t, first.stdout, "provider completed call")
		firstRequests := fixture.assertPeerRequests(t, 2)
		markerPath := filepath.Join(fixture.workDir, "durable-effect.txt")
		marker, err := os.ReadFile(markerPath)
		require.NoError(t, err)
		require.Equal(t, "effect-once", string(marker))

		firstRows := fixture.readRows(t, physicalID)
		firstAction := requireDurableWriteLifecycle(t, firstRows, physicalID)
		requireDurableTranscriptSequence(t, firstRows, []string{"create durable marker"}, true)
		firstTerminals := requireRunTerminals(t, firstRows, physicalID, 1)
		logDurableFileEvidence(t, "first completed child", physicalID, firstRows, firstAction, firstTerminals)
		firstIndex := fixture.readWorkspaceIndex(t, firstAction.workspaceID)
		requireWorkspaceActionLifecycle(t, firstIndex, physicalID, firstAction.actionID, 1, 1)
		require.Contains(t, string(firstRequests[1]), `"role":"tool"`)
		require.Contains(t, string(firstRequests[1]), `"tool_call_id":"call-durable-write-once"`)

		second := fixture.run(t, "prompt", "-provider=openai", "-output=text", "-approve=auto", "-resume-session", physicalID, "confirm durable marker after restart")
		requireCLIChildExit(t, second, 0)
		require.Equal(t, physicalID, durableCLIPhysicalSessionIDFromOutput(t, second.stdout), "resume must reopen the exact physical JSONL session")
		require.Contains(t, second.stdout, "provider completed call")
		marker, err = os.ReadFile(markerPath)
		require.NoError(t, err)
		require.Equal(t, "effect-once", string(marker), "resuming a completed CLI process must not replay the completed write")
		require.Equal(t, 1, strings.Count(fixture.logContents(t), "msg=write.done"), "raw production logs contain one successful managed write across both processes")

		rows := fixture.readRows(t, physicalID)
		action := requireDurableWriteLifecycle(t, rows, physicalID)
		requireDurableTranscriptSequence(t, rows, []string{"create durable marker", "confirm durable marker after restart"}, true)
		require.Equal(t, firstAction.actionID, action.actionID)
		require.Equal(t, firstAction.toolCallID, action.toolCallID)
		require.Equal(t, firstAction.workspaceID, action.workspaceID)
		terminals := requireRunTerminals(t, rows, physicalID, 2)
		logDurableFileEvidence(t, "normal exact-resume pair", physicalID, rows, action, terminals)
		require.NotEqual(t, firstTerminals[0].RunID, terminals[1].RunID, "each process invocation owns one distinct successful run")
		allRequests := fixture.assertPeerRequests(t, 3)
		restartedRequest := string(allRequests[2])
		require.Contains(t, restartedRequest, `"content":"create durable marker"`)
		require.Contains(t, restartedRequest, `"content":"confirm durable marker after restart"`)
		require.Contains(t, restartedRequest, `"tool_call_id":"call-durable-write-once"`, "the physical history contains the original call and receipt")
		requireWorkspaceActionLifecycle(t, fixture.readWorkspaceIndex(t, action.workspaceID), physicalID, action.actionID, 1, 1)
	})

	t.Run("kill after synced receipt before provider completion", func(t *testing.T) {
		fixture := newDurableCLIBinaryFixture(t, binary, true)
		defer func() {
			fixture.releaseBarrier()
			fixture.server.Close()
		}()

		command, stdout, stderr, wait := fixture.start(t, "prompt", "-provider=openai", "-output=text", "-approve=auto", "create durable marker")
		physicalID := waitForDurableCLIOutput(t, stdout, "Session ID: ")
		waitForDurableCLIBarrier(t, fixture.peer.barrierReached)
		fixture.assertPeerRequests(t, 2)
		sessionPath := filepath.Join(fixture.storeDir, "sessions", physicalID+".jsonl")
		rows := waitForDurableReceipt(t, sessionPath, physicalID)
		action := requireDurableWriteLifecycle(t, rows, physicalID)
		requireDurableTranscriptSequence(t, rows, []string{"create durable marker"}, false)
		requireWorkspaceActionLifecycle(t, fixture.readWorkspaceIndex(t, action.workspaceID), physicalID, action.actionID, 1, 1)
		require.Empty(t, requireRunTerminals(t, rows, physicalID, 0), "the provider completion barrier must precede any terminal checkpoint")
		logDurableFileEvidence(t, "killed child after synced receipt", physicalID, rows, action, nil)
		markerPath := filepath.Join(fixture.workDir, "durable-effect.txt")
		marker, err := os.ReadFile(markerPath)
		require.NoError(t, err, "the actual managed write must have happened before the blocked second provider request")
		require.Equal(t, "effect-once", string(marker))

		require.NoError(t, command.Process.Kill())
		firstErr := <-wait
		first := durableCLIChildResult{stdout: stdout.String(), stderr: stderr.String(), err: firstErr, exitCode: command.ProcessState.ExitCode(), state: command.ProcessState.String()}
		requireCLIChildExit(t, first, -1)
		t.Logf("blocked child command: %s prompt -provider=openai -output=text -approve=auto create durable marker\nblocked child exit code: %d state: %s error: %v\nblocked child stdout:\n%s\nblocked child stderr:\n%s\nraw CLI log records:\n%s", fixture.binary, first.exitCode, first.state, first.err, first.stdout, first.stderr, relevantDurableCLILogRecords(fixture.logContents(t)))

		fixture.releaseBarrier()
		second := fixture.run(t, "prompt", "-provider=openai", "-output=text", "-approve=auto", "-resume-session", physicalID, "continue after interrupted process")
		requireCLIChildExit(t, second, 0)
		require.Equal(t, physicalID, durableCLIPhysicalSessionIDFromOutput(t, second.stdout))
		marker, err = os.ReadFile(markerPath)
		require.NoError(t, err)
		require.Equal(t, "effect-once", string(marker), "recovery must not dispatch the receipted write again")
		require.Equal(t, 1, strings.Count(fixture.logContents(t), "msg=write.done"), "raw production logs contain one successful managed write across the kill/restart boundary")

		rows = fixture.readRows(t, physicalID)
		actionAfterRestart := requireDurableWriteLifecycle(t, rows, physicalID)
		requireDurableTranscriptSequence(t, rows, []string{"create durable marker", "continue after interrupted process"}, false)
		require.Equal(t, action.actionID, actionAfterRestart.actionID)
		require.Equal(t, action.toolCallID, actionAfterRestart.toolCallID)
		terminals := requireRunTerminals(t, rows, physicalID, 1)
		logDurableFileEvidence(t, "killed child exact-resume", physicalID, rows, actionAfterRestart, terminals)
		allRequests := fixture.assertPeerRequests(t, 3)
		resumedRequest := string(allRequests[2])
		require.Contains(t, resumedRequest, `"tool_call_id":"call-durable-write-once"`)
		require.Contains(t, resumedRequest, `"content":"continue after interrupted process"`)
		requireWorkspaceActionLifecycle(t, fixture.readWorkspaceIndex(t, action.workspaceID), physicalID, action.actionID, 1, 1)
	})
}

type durableCLIBinaryFixture struct {
	binary      string
	home        string
	workDir     string
	storeDir    string
	configDir   string
	cacheDir    string
	server      *httptest.Server
	peer        *durableCLIPeer
	childEnv    []string
	releaseOnce sync.Once
}

func newDurableCLIBinaryFixture(t *testing.T, binary string, blockAfterToolReceipt bool) *durableCLIBinaryFixture {
	t.Helper()
	peer := newDurableCLIPeer(blockAfterToolReceipt)
	server := httptest.NewServer(peer)
	fixture := &durableCLIBinaryFixture{
		binary: binary, server: server, peer: peer,
		home:      filepath.Join(t.TempDir(), "home"),
		workDir:   filepath.Join(t.TempDir(), "work"),
		storeDir:  filepath.Join(t.TempDir(), "durable-store"),
		configDir: filepath.Join(t.TempDir(), "config"),
		cacheDir:  filepath.Join(t.TempDir(), "cache"),
	}
	for _, directory := range []string{fixture.home, fixture.workDir, fixture.storeDir, fixture.configDir, fixture.cacheDir} {
		require.NoError(t, os.MkdirAll(directory, 0o700))
	}
	tmpDir := filepath.Join(t.TempDir(), "tmp")
	require.NoError(t, os.MkdirAll(tmpDir, 0o700))
	configPath := filepath.Join(fixture.home, ".go-cli", "config.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(configPath), 0o700))
	configYAML := fmt.Sprintf("provider:\n  name: openai\n  base_url: %q\n  model: durable-cli-fixture\nmodel:\n  name: durable-cli-fixture\nsession:\n  store_path: %q\ntools:\n  builtin:\n    - write\ntracing:\n  enabled: false\n", server.URL+"/v1", fixture.storeDir)
	require.NoError(t, os.WriteFile(configPath, []byte(configYAML), 0o600))
	fixture.childEnv = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + fixture.home,
		"XDG_CONFIG_HOME=" + fixture.configDir,
		"XDG_CACHE_HOME=" + fixture.cacheDir,
		"TMPDIR=" + tmpDir,
		"GO_CLI_PROVIDER_NAME=openai",
		"GO_CLI_PROVIDER_API_KEY=" + durableCLIFixtureAPIKey,
		"GO_CLI_PROVIDER_BASE_URL=" + server.URL + "/v1",
		"GO_CLI_PROVIDER_MODEL=durable-cli-fixture",
		"GO_CLI_NO_ONBOARDING=1",
		"NO_PROXY=127.0.0.1,localhost",
		"no_proxy=127.0.0.1,localhost",
		"HTTP_PROXY=", "HTTPS_PROXY=", "ALL_PROXY=", "http_proxy=", "https_proxy=", "all_proxy=",
		"OPENAI_API_KEY=", "ANTHROPIC_API_KEY=", "GOOGLE_API_KEY=", "GEMINI_API_KEY=", "MISTRAL_API_KEY=",
		"NO_COLOR=1", "TERM=dumb", "TZ=UTC",
	}
	if runtime.GOOS == "windows" {
		fixture.childEnv = append(fixture.childEnv, "SYSTEMROOT="+os.Getenv("SYSTEMROOT"))
	}
	return fixture
}

func (fixture *durableCLIBinaryFixture) releaseBarrier() {
	fixture.releaseOnce.Do(func() { close(fixture.peer.release) })
}

func (fixture *durableCLIBinaryFixture) run(t *testing.T, args ...string) durableCLIChildResult {
	t.Helper()
	previousLog := fixture.logContents(t)
	command, stdout, stderr, wait := fixture.start(t, args...)
	var err error
	select {
	case err = <-wait:
	case <-time.After(45 * time.Second):
		_ = command.Process.Kill()
		err = <-wait
		t.Fatalf("actual CLI process exceeded 45 seconds: %s %s", fixture.binary, strings.Join(args, " "))
	}
	result := durableCLIChildResult{stdout: stdout.String(), stderr: stderr.String(), err: err, exitCode: command.ProcessState.ExitCode(), state: command.ProcessState.String()}
	currentLog := fixture.logContents(t)
	if strings.HasPrefix(currentLog, previousLog) {
		currentLog = strings.TrimPrefix(currentLog, previousLog)
	}
	t.Logf("child command: %s %s\nchild exit code: %d state: %s error: %v\nchild stdout:\n%s\nchild stderr:\n%s\nraw CLI log records:\n%s", fixture.binary, strings.Join(args, " "), result.exitCode, result.state, result.err, result.stdout, result.stderr, relevantDurableCLILogRecords(currentLog))
	return result
}

func (fixture *durableCLIBinaryFixture) start(t *testing.T, args ...string) (*exec.Cmd, *authPersistenceOutput, *authPersistenceOutput, <-chan error) {
	t.Helper()
	command := exec.Command(fixture.binary, args...)
	command.Dir = fixture.workDir
	command.Env = append([]string(nil), fixture.childEnv...)
	stdout := newAuthPersistenceOutput()
	stderr := newAuthPersistenceOutput()
	command.Stdout = stdout
	command.Stderr = stderr
	require.NoError(t, command.Start())
	wait := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		wait <- command.Wait()
		close(finished)
	}()
	t.Cleanup(func() {
		select {
		case <-finished:
			return
		default:
			_ = command.Process.Kill()
			<-finished
		}
	})
	return command, stdout, stderr, wait
}

func (fixture *durableCLIBinaryFixture) logContents(t *testing.T) string {
	t.Helper()
	logPath := filepath.Join(fixture.workDir, ".go-cli", "logs", "go-cli.log")
	data, err := os.ReadFile(logPath)
	if err != nil && !os.IsNotExist(err) {
		t.Logf("read CLI log: %v", err)
	}
	return string(data)
}

func relevantDurableCLILogRecords(raw string) string {
	var selected []string
	for _, line := range strings.Split(raw, "\n") {
		if line == "" {
			continue
		}
		if strings.Contains(line, "config_load_trusted") ||
			strings.Contains(line, "cli_prompt_durable_session") ||
			strings.Contains(line, "llm_stream_start") ||
			strings.Contains(line, "write.done") ||
			strings.Contains(line, "core.run_slot.released") ||
			strings.Contains(line, "core.loop.finish") ||
			strings.Contains(line, "cli_prompt_complete") ||
			strings.Contains(line, "command_end") {
			selected = append(selected, line)
		}
	}
	return strings.Join(selected, "\n")
}

func (fixture *durableCLIBinaryFixture) readRows(t *testing.T, physicalID string) []session.SessionEntry {
	t.Helper()
	path := filepath.Join(fixture.storeDir, "sessions", physicalID+".jsonl")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	rows := make([]session.SessionEntry, 0)
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), 4*1024*1024)
	for scanner.Scan() {
		var row session.SessionEntry
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &row), "decode physical JSONL row %d", len(rows))
		rows = append(rows, row)
	}
	require.NoError(t, scanner.Err())
	require.NotEmpty(t, rows, "the physical session file must contain real synced records")
	return rows
}

func (fixture *durableCLIBinaryFixture) readWorkspaceIndex(t *testing.T, workspaceID string) []durableCLIWorkspaceIndexRow {
	t.Helper()
	root := fixture.configDir
	if runtime.GOOS == "darwin" {
		root = filepath.Join(fixture.home, "Library", "Application Support")
	} else if runtime.GOOS == "windows" {
		root = filepath.Join(fixture.home, "AppData", "Roaming")
	}
	path := filepath.Join(root, "go-cli", "workspace-quarantine", workspaceID+".jsonl")
	data, err := os.ReadFile(path)
	require.NoError(t, err, "read authoritative synced workspace index %s", path)
	rows := make([]durableCLIWorkspaceIndexRow, 0)
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		var row durableCLIWorkspaceIndexRow
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &row))
		rows = append(rows, row)
	}
	require.NoError(t, scanner.Err())
	return rows
}

type durableCLIChildResult struct {
	stdout   string
	stderr   string
	err      error
	exitCode int
	state    string
}

func requireCLIChildExit(t *testing.T, result durableCLIChildResult, want int) {
	t.Helper()
	if want == -1 {
		var exitErr *exec.ExitError
		require.ErrorAs(t, result.err, &exitErr)
		require.Equal(t, -1, result.exitCode, "a killed process has no ordinary exit code")
		return
	}
	require.NoError(t, result.err, "child exit=%d state=%s stdout=%s stderr=%s", result.exitCode, result.state, result.stdout, result.stderr)
	require.Equal(t, want, result.exitCode)
}

func waitForDurableCLIOutput(t *testing.T, output *authPersistenceOutput, fragment string) string {
	t.Helper()
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	for {
		output.mu.Lock()
		text := output.buffer.String()
		if strings.Contains(text, fragment) {
			output.mu.Unlock()
			return durableCLIPhysicalSessionIDFromOutput(t, text)
		}
		changed := output.changed
		output.mu.Unlock()
		select {
		case <-changed:
		case <-timer.C:
			t.Fatalf("actual CLI child did not print %q; stdout/stderr=%q", fragment, output.String())
		}
	}
}

func durableCLIPhysicalSessionIDFromOutput(t *testing.T, output string) string {
	t.Helper()
	const prefix = "Session ID: "
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, prefix) {
			id := strings.TrimSpace(strings.TrimPrefix(line, prefix))
			require.NotEmpty(t, id)
			return id
		}
	}
	t.Fatalf("actual CLI child did not print its physical session ID: %q", output)
	return ""
}

func waitForDurableCLIBarrier(t *testing.T, barrier <-chan struct{}) {
	t.Helper()
	select {
	case <-barrier:
	case <-time.After(20 * time.Second):
		t.Fatal("actual CLI child did not reach the deterministic provider response barrier")
	}
}

func waitForDurableReceipt(t *testing.T, path, physicalID string) []session.SessionEntry {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			var rows []session.SessionEntry
			valid := true
			scanner := bufio.NewScanner(bytes.NewReader(data))
			scanner.Buffer(make([]byte, 4096), 4*1024*1024)
			for scanner.Scan() {
				var row session.SessionEntry
				if json.Unmarshal(scanner.Bytes(), &row) != nil {
					valid = false
					break
				}
				rows = append(rows, row)
			}
			if valid && scanner.Err() == nil {
				for _, row := range rows {
					if row.SessionID == physicalID && row.Type == session.EntryTypeTool && row.ReceiptOutcome == "success" && row.ToolName == "write" {
						return rows
					}
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("actual CLI child did not sync a successful write receipt to %s", path)
	return nil
}

type durableCLIAction struct {
	actionID    string
	toolCallID  string
	workspaceID string
}

func requireDurableWriteLifecycle(t *testing.T, rows []session.SessionEntry, physicalID string) durableCLIAction {
	t.Helper()
	seenIDs := make(map[string]struct{}, len(rows))
	var intents, receipts, executions, toolCalls []session.SessionEntry
	var actionIDs = make(map[string]struct{})
	for _, row := range rows {
		require.NotEmpty(t, row.ID, "every physical JSONL row has a source ID")
		if _, duplicate := seenIDs[row.ID]; duplicate {
			t.Fatalf("physical JSONL repeats source ID %q", row.ID)
		}
		seenIDs[row.ID] = struct{}{}
		require.Equal(t, uint16(1), row.RecordVersion, "all durable production rows retain explicit schema metadata: %+v", row)
		require.Equal(t, physicalID, row.SessionID, "each source row is scoped to the exact physical session")
		if len(row.ToolCalls) > 0 {
			toolCalls = append(toolCalls, row)
		}
		if row.ActionState == session.ActionStateIntent {
			intents = append(intents, row)
		}
		if row.ActionState == session.ActionStateExecution {
			executions = append(executions, row)
		}
		if row.Type == session.EntryTypeTool && row.ReceiptOutcome != "" {
			receipts = append(receipts, row)
		}
		if row.ActionID != "" {
			require.NotEmpty(t, row.ActionBatchID)
			require.NotEmpty(t, row.ToolCallID)
			require.NotEmpty(t, row.RunID)
			require.NotEmpty(t, row.WorkspaceID)
			actionIDs[row.ActionID] = struct{}{}
		}
	}
	require.Len(t, toolCalls, 1, "exactly one real provider tool call produced a canonical source row")
	require.Len(t, toolCalls[0].ToolCalls, 1)
	require.Equal(t, "write", toolCalls[0].ToolCalls[0].Name)
	require.Len(t, intents, 1, "one synced intent precedes the managed write dispatch")
	require.NotEmpty(t, executions, "the physical journal records executor lifecycle evidence")
	require.Len(t, receipts, 1, "one successful receipt records the single managed write effect")
	require.Equal(t, "success", receipts[0].ReceiptOutcome)
	require.Equal(t, "dispatched", string(receipts[0].DispatchState))
	require.Equal(t, "write", receipts[0].ToolName)
	require.Len(t, actionIDs, 1, "a restart must not mint a second action for the original tool call")
	actionID := intents[0].ActionID
	require.NotEmpty(t, actionID)
	require.Equal(t, actionID, receipts[0].ActionID)
	require.Equal(t, toolCalls[0].ToolCalls[0].ID, receipts[0].ToolCallID)
	require.Equal(t, toolCalls[0].RunID, intents[0].RunID)
	require.Equal(t, toolCalls[0].WorkspaceID, intents[0].WorkspaceID)
	return durableCLIAction{actionID: actionID, toolCallID: receipts[0].ToolCallID, workspaceID: intents[0].WorkspaceID}
}

func requireRunTerminals(t *testing.T, rows []session.SessionEntry, physicalID string, want int) []session.SessionEntry {
	t.Helper()
	terminals := make([]session.SessionEntry, 0)
	seenRuns := make(map[string]struct{})
	seenIDs := make(map[string]struct{})
	for _, row := range rows {
		if row.Type != session.EntryTypeCheckpoint || row.CheckpointKind != session.CheckpointRunTerminal {
			continue
		}
		require.Equal(t, physicalID, row.SessionID)
		require.NotEmpty(t, row.ID)
		require.NotEmpty(t, row.RunID)
		require.NotEmpty(t, row.WorkspaceID)
		require.Equal(t, "success", string(row.RunOutcome))
		if _, duplicate := seenRuns[row.RunID]; duplicate {
			t.Fatalf("physical JSONL has more than one run terminal for run %q", row.RunID)
		}
		if _, duplicate := seenIDs[row.ID]; duplicate {
			t.Fatalf("physical JSONL repeats terminal source ID %q", row.ID)
		}
		seenRuns[row.RunID] = struct{}{}
		seenIDs[row.ID] = struct{}{}
		var lastTranscriptSeq uint64
		for _, message := range rows {
			if message.RunID == row.RunID && message.TranscriptIndex != nil && message.Seq > lastTranscriptSeq {
				lastTranscriptSeq = message.Seq
			}
		}
		require.Greater(t, row.Seq, lastTranscriptSeq, "terminal checkpoint follows the completed transcript for its run")
		terminals = append(terminals, row)
	}
	require.Len(t, terminals, want, "assert the actual on-disk terminal checkpoints for completed child runs")
	return terminals
}

func requireDurableTranscriptSequence(t *testing.T, rows []session.SessionEntry, prompts []string, firstRunCompleted bool) {
	t.Helper()
	transcript := make([]session.SessionEntry, 0)
	indicesByRun := make(map[string][]uint64)
	for _, row := range rows {
		if row.TranscriptIndex == nil {
			continue
		}
		transcript = append(transcript, row)
		indicesByRun[row.RunID] = append(indicesByRun[row.RunID], *row.TranscriptIndex)
	}
	sort.Slice(transcript, func(i, j int) bool { return transcript[i].Seq < transcript[j].Seq })
	for runID, indices := range indicesByRun {
		require.NotEmpty(t, runID)
		sort.Slice(indices, func(i, j int) bool { return indices[i] < indices[j] })
		for index, transcriptIndex := range indices {
			require.Equal(t, uint64(index), transcriptIndex, "run %q transcript source indices are contiguous", runID)
		}
	}
	wantCount := 3 + len(prompts) - 1
	if firstRunCompleted {
		wantCount++
	}
	if len(prompts) > 1 {
		wantCount++
	}
	require.Len(t, transcript, wantCount, "the actual JSONL transcript has only accepted inputs, one write call/receipt, and completed assistant messages")
	index := 0
	require.Equal(t, session.EntryTypeUser, transcript[index].Type)
	require.Equal(t, prompts[0], transcript[index].Content)
	index++
	require.Equal(t, session.EntryTypeAssistant, transcript[index].Type)
	require.Len(t, transcript[index].ToolCalls, 1)
	require.Equal(t, "write", transcript[index].ToolCalls[0].Name)
	toolCallID := transcript[index].ToolCalls[0].ID
	index++
	require.Equal(t, session.EntryTypeTool, transcript[index].Type)
	require.Equal(t, toolCallID, transcript[index].ToolCallID)
	require.Equal(t, "success", transcript[index].ReceiptOutcome)
	index++
	if firstRunCompleted {
		require.Equal(t, session.EntryTypeAssistant, transcript[index].Type)
		require.Equal(t, "provider completed call", transcript[index].Content)
		index++
	}
	if len(prompts) > 1 {
		require.Equal(t, session.EntryTypeUser, transcript[index].Type)
		require.Equal(t, prompts[1], transcript[index].Content)
		index++
		require.Equal(t, session.EntryTypeAssistant, transcript[index].Type)
		require.Equal(t, "provider completed call", transcript[index].Content)
	}
}

type durableCLIWorkspaceIndexRow struct {
	Version   uint16 `json:"version"`
	Seq       uint64 `json:"seq"`
	RecordID  string `json:"record_id"`
	Kind      string `json:"kind"`
	SessionID string `json:"session_id"`
	RunID     string `json:"run_id"`
	Actions   []struct {
		Action struct {
			ActionID   string `json:"action_id"`
			ToolCallID string `json:"tool_call_id"`
			SessionID  string `json:"session_id"`
		} `json:"action"`
	} `json:"actions"`
	Action *struct {
		ActionID   string `json:"action_id"`
		ToolCallID string `json:"tool_call_id"`
		SessionID  string `json:"session_id"`
	} `json:"action,omitempty"`
}

func requireWorkspaceActionLifecycle(t *testing.T, rows []durableCLIWorkspaceIndexRow, physicalID, actionID string, wantDispatch, wantResolution int) {
	t.Helper()
	dispatches, resolutions := 0, 0
	seenRecordIDs := make(map[string]struct{}, len(rows))
	seenSeq := make(map[uint64]struct{}, len(rows))
	for _, row := range rows {
		require.Equal(t, uint16(1), row.Version)
		require.NotEmpty(t, row.RecordID)
		require.Greater(t, row.Seq, uint64(0))
		if _, duplicate := seenRecordIDs[row.RecordID]; duplicate {
			t.Fatalf("workspace index repeats source record ID %q", row.RecordID)
		}
		if _, duplicate := seenSeq[row.Seq]; duplicate {
			t.Fatalf("workspace index repeats sequence %d", row.Seq)
		}
		seenRecordIDs[row.RecordID] = struct{}{}
		seenSeq[row.Seq] = struct{}{}
		switch row.Kind {
		case "dispatch_ready":
			for _, action := range row.Actions {
				require.Equal(t, physicalID, action.Action.SessionID)
				if action.Action.ActionID == actionID {
					dispatches++
					require.Equal(t, "call-durable-write-once", action.Action.ToolCallID)
				}
			}
		case "execution_resolution":
			if row.Action != nil && row.Action.ActionID == actionID {
				resolutions++
				require.Equal(t, physicalID, row.Action.SessionID)
				require.Equal(t, "call-durable-write-once", row.Action.ToolCallID)
			}
		}
	}
	require.Equal(t, wantDispatch, dispatches, "the authoritative workspace JSONL has one synced dispatch marker")
	require.Equal(t, wantResolution, resolutions, "the authoritative workspace JSONL records the matching effect-resolution fact")
}

func logDurableFileEvidence(t *testing.T, phase, physicalID string, rows []session.SessionEntry, action durableCLIAction, terminals []session.SessionEntry) {
	t.Helper()
	sourceIDs := make([]string, 0, len(rows))
	for _, row := range rows {
		sourceIDs = append(sourceIDs, row.ID)
	}
	terminalEvidence := make([]string, 0, len(terminals))
	for _, terminal := range terminals {
		terminalEvidence = append(terminalEvidence, fmt.Sprintf("id=%s run=%s outcome=%s seq=%d", terminal.ID, terminal.RunID, terminal.RunOutcome, terminal.Seq))
	}
	t.Logf("%s physical_id=%s jsonl_rows=%d source_ids=%v action_id=%s tool_call_id=%s workspace_id=%s terminals=%v", phase, physicalID, len(rows), sourceIDs, action.actionID, action.toolCallID, action.workspaceID, terminalEvidence)
}

type durableCLIPeer struct {
	mu             sync.Mutex
	bodies         [][]byte
	authorizations []string
	blockAfterTool bool
	barrierReached chan struct{}
	release        chan struct{}
	barrierOnce    sync.Once
}

func newDurableCLIPeer(blockAfterTool bool) *durableCLIPeer {
	return &durableCLIPeer{
		blockAfterTool: blockAfterTool,
		barrierReached: make(chan struct{}),
		release:        make(chan struct{}),
	}
}

func (peer *durableCLIPeer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/chat/completions" {
		http.Error(w, "unexpected path", http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "cannot read request", http.StatusBadRequest)
		return
	}
	peer.mu.Lock()
	index := len(peer.bodies)
	peer.bodies = append(peer.bodies, append([]byte(nil), body...))
	peer.authorizations = append(peer.authorizations, r.Header.Get("Authorization"))
	peer.mu.Unlock()

	if peer.blockAfterTool && index == 1 {
		peer.barrierOnce.Do(func() { close(peer.barrierReached) })
		select {
		case <-peer.release:
		case <-r.Context().Done():
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	if index == 0 {
		_, _ = fmt.Fprint(w, `{"id":"chatcmpl-durable-write","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call-durable-write-once","type":"function","function":{"name":"write","arguments":"{\"path\":\"durable-effect.txt\",\"content\":\"effect-once\"}"}}]},"finish_reason":"tool_calls"}]}`)
		return
	}
	_, _ = fmt.Fprintf(w, `{"id":"chatcmpl-durable-final-%d","choices":[{"index":0,"message":{"role":"assistant","content":"provider completed call"},"finish_reason":"stop"}]}`, index)
}

func (peer *durableCLIPeer) requestBodies() [][]byte {
	peer.mu.Lock()
	defer peer.mu.Unlock()
	bodies := make([][]byte, len(peer.bodies))
	for i, body := range peer.bodies {
		bodies[i] = append([]byte(nil), body...)
	}
	return bodies
}

func waitForPeerRequests(t *testing.T, peer *durableCLIPeer, count int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		peer.mu.Lock()
		if len(peer.bodies) >= count {
			authorizations := append([]string(nil), peer.authorizations...)
			peer.mu.Unlock()
			for _, authorization := range authorizations {
				require.Equal(t, "Bearer "+durableCLIFixtureAPIKey, authorization)
			}
			return
		}
		peer.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("loopback OpenAI peer received fewer than %d actual CLI requests", count)
}

func (fixture *durableCLIBinaryFixture) assertPeerRequests(t *testing.T, count int) [][]byte {
	t.Helper()
	waitForPeerRequests(t, fixture.peer, count)
	bodies := fixture.peer.requestBodies()
	require.Len(t, bodies, count)
	return bodies
}
