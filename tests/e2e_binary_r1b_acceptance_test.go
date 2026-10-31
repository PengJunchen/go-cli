//go:build e2e && (darwin || linux)

package tests

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/session"
)

// TestE2EBinaryR1bAcceptance is an engineering fixture, not a model benchmark:
// scripted decisions exercise the real CLI, HTTP/SSE, tools and durable store.
func TestE2EBinaryR1bAcceptance(t *testing.T) {
	buildStarted := time.Now()
	binary := buildLongTaskRaceBinary(t)
	buildWall := time.Since(buildStarted).Seconds()
	expected := r1bExpectation{TaskID: r1bNonce(t), Canary: r1bNonce(t)}
	ephemeral := r1bNonce(t)
	plan := &r1bScript{steps: map[int]int{}}
	peer := newLongTaskHTTPPeer(plan.reply)
	fixture := newLongTaskFixture(t, peer, true)
	defer fixture.server.Close()
	gitInit := exec.Command("git", "init", "-q", fixture.workDir)
	gitOutput, err := gitInit.CombinedOutput()
	require.NoError(t, err, "initialize isolated repository: %s", gitOutput)
	store := filepath.Join(t.TempDir(), "durable")
	home := r1bEnvironmentValue(fixture.childEnv, "HOME")
	configPath := filepath.Join(home, ".go-cli", "config.yaml")
	config, err := os.ReadFile(configPath)
	require.NoError(t, err)
	// Separate pressure profile: explicit durable store and a low truncating
	// ceiling, with personal memory off. No production defaults are changed.
	config = bytes.Replace(config, []byte("mode: ask"), []byte("mode: auto"), 1)
	config = bytes.Replace(config, []byte("    - bash\n"), []byte("    - bash\n    - read\n    - write\n"), 1)
	config = append(config, []byte(fmt.Sprintf("session:\n  store_path: %q\n", store))...)
	require.NoError(t, os.WriteFile(configPath, config, 0o600))
	var acceptedReport []byte
	// Register before either child runs. Cleanup executes before the earlier
	// TempDir cleanups, preserving evidence even when a later assertion fails.
	if destination := os.Getenv("GO_CLI_R1B_ARTIFACT_DIR"); destination != "" {
		require.NoError(t, os.Mkdir(destination, 0o700), "artifact destination must be new")
		t.Cleanup(func() {
			r1bExportArtifacts(t, destination, peer, fixture, store, acceptedReport)
		})
	}
	var httpMu sync.Mutex
	var httpWall []float64
	fixture.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		peer.ServeHTTP(w, r)
		httpMu.Lock()
		httpWall = append(httpWall, time.Since(started).Seconds())
		httpMu.Unlock()
	})
	initial := r1bTaskState{TaskID: expected.TaskID, Canary: expected.Canary, Version: 1}
	payload, err := json.Marshal(initial)
	require.NoError(t, err)
	firstInput := "R1B_TURN=001 persist only R1B_STATE; EPHEMERAL_ONLY=" + ephemeral + "\nR1B_STATE=" + string(payload)
	processA, stdoutA, stderrA := r1bRunProcess(t, binary, fixture, "", 1, firstInput)
	require.NoError(t, plan.failure())
	files, err := filepath.Glob(filepath.Join(store, "sessions", "*.jsonl"))
	require.NoError(t, err)
	require.Len(t, files, 1, "A must persist one actual physical session")
	physical := strings.TrimSuffix(filepath.Base(files[0]), ".jsonl")
	processA.SessionID = physical
	aSnapshot, err := session.InspectPhysicalSession(t.Context(), store, physical)
	require.NoError(t, err)
	pendingA, err := session.InspectPendingActions(aSnapshot)
	require.NoError(t, err)
	require.Empty(t, pendingA, "A must finish its own actions before B can resume")
	boundaryA := map[string]bool{}
	for _, row := range aSnapshot.DurableEntries {
		if row.Type == session.EntryTypeCompaction {
			boundaryA[row.ID] = true
		}
	}
	requestsAfterA := len(peer.requestBodies())
	processB, stdoutB, stderrB := r1bRunProcess(t, binary, fixture, physical, 61, "")
	require.NoError(t, plan.failure())
	bFiles, err := filepath.Glob(filepath.Join(store, "sessions", "*.jsonl"))
	require.NoError(t, err)
	require.Len(t, bFiles, 1, "public resume must not silently create a second physical session")
	actualBPhysical := strings.TrimSuffix(filepath.Base(bFiles[0]), ".jsonl")
	bSnapshot, err := session.InspectPhysicalSession(t.Context(), store, actualBPhysical)
	require.NoError(t, err)
	processB.SessionID = bSnapshot.SessionID
	pending, err := session.InspectPendingActions(bSnapshot)
	require.NoError(t, err)
	evidence := r1bEvidence{Processes: []r1bProcess{processA, processB}, Pending: len(pending), PendingBeforeResume: len(pendingA),
		ToolReceipts: map[string]string{}, ToolProofs: map[string]string{}, ToolResolved: map[string]bool{}}
	runTurns := map[string]int{}
	for _, row := range bSnapshot.DurableEntries {
		if row.Type == session.EntryTypeUser {
			if turn, ok := r1bTurn(row.Content); ok {
				runTurns[row.RunID] = turn
			}
		}
	}
	actionCalls := map[string]string{}
	actionStarts := map[string]time.Time{}
	actionWall := map[string]float64{}
	for _, row := range bSnapshot.DurableEntries {
		if row.Type == session.EntryTypeCompaction {
			phase := "B"
			if boundaryA[row.ID] {
				phase = "A"
			}
			evidence.Compactions = append(evidence.Compactions, r1bCompaction{
				ID: row.ID, Boundary: row.CompactedThroughEntryID, Process: phase, Turn: runTurns[row.RunID]})
		}
		if row.ActionID != "" && row.ToolCallID != "" {
			actionCalls[row.ActionID] = row.ToolCallID
		}
		if row.ActionState == session.ActionStateIntent {
			actionStarts[row.ActionID] = row.Timestamp
		}
		if row.Type == session.EntryTypeTool && row.ActionState == "" {
			evidence.ToolReceipts[row.ToolCallID] = row.ReceiptOutcome
		}
		if row.CompletionProof != "" {
			evidence.ToolProofs[row.ToolCallID] = string(row.CompletionProof)
		}
	}
	for _, row := range bSnapshot.DurableEntries {
		if row.ExecutionResolution != nil {
			id := row.ExecutionResolution.ActionID
			evidence.ToolResolved[actionCalls[id]] = true
			if started, ok := actionStarts[id]; ok {
				actionWall[actionCalls[id]] = row.Timestamp.Sub(started).Seconds()
			}
		}
	}
	bodies := peer.requestBodies()
	observations := make([]map[string]any, 0, len(bodies))
	seenTurns := map[int]bool{}
	for index, body := range bodies {
		request, decodeErr := r1bDecodeRequest(body)
		require.NoError(t, decodeErr)
		turn, ok := r1bCurrentTurn(request)
		require.True(t, ok, "each actual request must retain its current user input")
		seenTurns[turn] = true
		require.True(t, request.Stream)
		phase := "A"
		if index >= requestsAfterA {
			phase = "B"
		}
		observations = append(observations, map[string]any{"sequence": index + 1, "process": phase,
			"user_turn": turn, "request_sha256": r1bHash(body),
			"ephemeral_canary_visible": bytes.Contains(body, []byte(ephemeral))})
	}
	require.Len(t, seenTurns, 120, "60+60 user submissions, not 120 autonomous planning steps")
	graderStarted := time.Now()
	gradeErr := gradeR1b(fixture.workDir, expected, evidence)
	graderWall := time.Since(graderStarted).Seconds()
	require.NoError(t, gradeErr)
	require.NotContains(t, stderrA+stderrB, "DATA RACE")
	require.Contains(t, stdoutA, "Session ended.")
	require.Contains(t, stdoutB, "R1B_REPLY_120")
	httpMu.Lock()
	httpTiming := append([]float64(nil), httpWall...)
	httpMu.Unlock()
	binaryBytes, err := os.ReadFile(binary)
	require.NoError(t, err)
	sourceRoot := binaryE2EModuleRoot(t)
	commit, err := exec.Command("git", "-C", sourceRoot, "rev-parse", "HEAD").Output()
	require.NoError(t, err)
	testSources := map[string]string{}
	for _, name := range []string{"r1b_acceptance_fixture_test.go", "r1b_acceptance_grader_test.go", "e2e_binary_r1b_acceptance_test.go"} {
		data, readErr := os.ReadFile(filepath.Join(sourceRoot, "tests", name))
		require.NoError(t, readErr)
		testSources[name] = r1bHash(data)
	}
	artifacts := map[string]string{}
	for _, name := range []string{"task-state.json", "effects.jsonl", "final.json"} {
		data, readErr := os.ReadFile(filepath.Join(fixture.workDir, name))
		require.NoError(t, readErr)
		artifacts[name] = r1bHash(data)
	}
	report := map[string]any{"engineering_acceptance": "passed", "profile": "r1b-truncating-4096-memory-off-local-peer",
		"evidence": evidence, "requests": observations, "build_wall_seconds": buildWall,
		"grader_wall_seconds": graderWall, "server_http_wall_seconds": httpTiming,
		"binary_sha256": r1bHash(binaryBytes), "config_sha256": r1bHash(config),
		"source_commit": strings.TrimSpace(string(commit)), "test_sources_sha256": testSources,
		"artifact_sha256": artifacts, "action_intent_to_resolution_wall_seconds": actionWall,
		"stdout_sha256": []string{r1bHash([]byte(stdoutA)), r1bHash([]byte(stdoutB))},
		"stderr_sha256": []string{r1bHash([]byte(stderrA)), r1bHash([]byte(stderrB))},
		"usage_source":  "local_mock_reports_no_usage", "unknown_usage_requests": len(bodies),
		"real_model_calls": 0, "model_task_score": nil,
		"negative_controls": "separate grader contract sandboxes, not CLI fault injection runs"}
	reportBytes, err := json.Marshal(report)
	require.NoError(t, err)
	acceptedReport = reportBytes
	t.Logf("R1B_EVIDENCE %s", reportBytes)
}

func r1bExportArtifacts(t *testing.T, destination string, peer *longTaskHTTPPeer, fixture *longTaskFixture, store string, accepted []byte) {
	t.Helper()
	write := func(name string, data []byte) {
		if err := os.WriteFile(filepath.Join(destination, name), data, 0o600); err != nil {
			t.Errorf("export local R1b %s: %v", name, err)
		}
	}
	var raw bytes.Buffer
	for _, body := range peer.requestBodies() {
		raw.Write(body)
		raw.WriteByte('\n')
	}
	write("requests.jsonl", raw.Bytes())
	status := map[string]any{"failed": t.Failed(), "local_synthetic_peer_only": true, "real_model_calls": 0}
	exported, missing, exportErr := r1bExportBusinessArtifacts(fixture.workDir, destination)
	status["business_artifact_sha256"] = exported
	status["missing_business_artifacts"] = missing
	if exportErr != nil {
		t.Errorf("export original R1b business artifacts: %v", exportErr)
	}
	if len(accepted) != 0 {
		write("evidence.json", accepted)
		var report struct {
			Hashes map[string]string `json:"artifact_sha256"`
		}
		if err := json.Unmarshal(accepted, &report); err != nil {
			t.Errorf("decode accepted R1b artifact hashes: %v", err)
		} else {
			for name, expected := range report.Hashes {
				if exported[name] != expected {
					t.Errorf("original R1b artifact hash mismatch for %s", name)
				}
			}
		}
	}
	files, err := filepath.Glob(filepath.Join(store, "sessions", "*.jsonl"))
	if err != nil {
		t.Errorf("find local R1b journals: %v", err)
	}
	status["physical_session_files"] = len(files)
	for _, file := range files {
		data, readErr := os.ReadFile(file)
		if readErr != nil {
			t.Errorf("read local R1b journal: %v", readErr)
			continue
		}
		write("session-"+filepath.Base(file), data)
	}
	if data, readErr := os.ReadFile(filepath.Join(fixture.workDir, ".go-cli", "logs", "go-cli.log")); readErr == nil {
		write("cli.log", data)
	}
	status["failed"] = t.Failed()
	data, err := json.Marshal(status)
	if err != nil {
		t.Errorf("encode R1b export status: %v", err)
		return
	}
	write("status.json", data)
}

// r1bExportBusinessArtifacts retains actual files before TempDir cleanup.
// A failed run may not have created every file; missing originals stay missing.
func r1bExportBusinessArtifacts(source, destination string) (map[string]string, []string, error) {
	hashes := make(map[string]string)
	missing := []string{}
	for _, name := range []string{"task-state.json", "effects.jsonl", "final.json"} {
		data, err := os.ReadFile(filepath.Join(source, name))
		if errors.Is(err, os.ErrNotExist) {
			missing = append(missing, name)
			continue
		}
		if err != nil {
			return hashes, missing, fmt.Errorf("read original %s: %w", name, err)
		}
		file, err := os.OpenFile(filepath.Join(destination, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return hashes, missing, fmt.Errorf("create original evidence %s: %w", name, err)
		}
		_, writeErr := file.Write(data)
		if err := errors.Join(writeErr, file.Close()); err != nil {
			return hashes, missing, fmt.Errorf("write original evidence %s: %w", name, err)
		}
		hashes[name] = r1bHash(data)
	}
	return hashes, missing, nil
}

func r1bRunProcess(t *testing.T, binary string, fixture *longTaskFixture, physical string, firstTurn int, firstInput string) (r1bProcess, string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	args := []string{"interactive", "--provider=openai", "--model=" + longTaskModel,
		"--max-tokens=4096", "--thinking=none", "--no-onboarding", "--no-sandbox"}
	if physical != "" {
		args = append(args, "--resume-session", physical)
	}
	var input strings.Builder
	for turn := firstTurn; turn < firstTurn+60; turn++ {
		line := fmt.Sprintf("R1B_TURN=%03d continue the engineering task", turn)
		if turn == firstTurn && firstInput != "" {
			line = firstInput
		}
		// Each user submission must remain one line at the actual REPL boundary.
		input.WriteString(strings.ReplaceAll(line, "\n", " "))
		input.WriteByte('\n')
	}
	input.WriteString("exit\n")
	command := exec.CommandContext(ctx, binary, args...)
	command.Dir, command.Env, command.Stdin = fixture.workDir, fixture.childEnv, strings.NewReader(input.String())
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	started := time.Now()
	require.NoError(t, command.Start())
	pid := command.Process.Pid
	err := command.Wait()
	exited := time.Now()
	result := r1bProcess{PID: pid, SessionID: physical, Turns: 60, Resumed: physical != "", ExitCode: command.ProcessState.ExitCode(), WallSeconds: exited.Sub(started).Seconds(), StartedUTC: started.UTC().Format(time.RFC3339Nano), ExitedUTC: exited.UTC().Format(time.RFC3339Nano)}
	require.NoError(t, err, "real process firstTurn=%d stdout=%s stderr=%s", firstTurn, stdout.String(), stderr.String())
	return result, stdout.String(), stderr.String()
}

type r1bRequest struct {
	Stream   bool `json:"stream"`
	Messages []struct {
		Role       string          `json:"role"`
		Content    json.RawMessage `json:"content"`
		ToolCallID string          `json:"tool_call_id"`
	} `json:"messages"`
}

func r1bDecodeRequest(body []byte) (r1bRequest, error) {
	var request r1bRequest
	err := json.Unmarshal(body, &request)
	return request, err
}

var r1bTurnPattern = regexp.MustCompile(`^R1B_TURN=([0-9]{3})`)

func r1bTurn(content string) (int, bool) {
	match := r1bTurnPattern.FindStringSubmatch(content)
	if len(match) != 2 {
		return 0, false
	}
	turn, err := strconv.Atoi(match[1])
	return turn, err == nil && turn >= 1 && turn <= 120
}

func r1bText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	for _, block := range blocks {
		text += block.Text
	}
	return text
}

func r1bCurrentTurn(request r1bRequest) (int, bool) {
	for index := len(request.Messages) - 1; index >= 0; index-- {
		if request.Messages[index].Role == "user" {
			if turn, ok := r1bTurn(r1bText(request.Messages[index].Content)); ok {
				return turn, true
			}
		}
	}
	return 0, false
}

func r1bToolText(request r1bRequest, id string) string {
	for index := len(request.Messages) - 1; index >= 0; index-- {
		message := request.Messages[index]
		if message.Role == "tool" && message.ToolCallID == id {
			return r1bText(message.Content)
		}
	}
	return ""
}

func r1bReadState(content string) (r1bTaskState, error) {
	for index, character := range content {
		if character != '{' {
			continue
		}
		var state r1bTaskState
		if json.NewDecoder(strings.NewReader(content[index:])).Decode(&state) == nil && state.TaskID != "" && state.Canary != "" {
			return state, nil
		}
	}
	return r1bTaskState{}, fmt.Errorf("actual user/tool result did not contain task state")
}

type r1bScript struct {
	mu    sync.Mutex
	steps map[int]int
	err   error
}

func (script *r1bScript) failure() error {
	script.mu.Lock()
	defer script.mu.Unlock()
	return script.err
}

func (script *r1bScript) reply(_ int, body []byte) longTaskReply {
	script.mu.Lock()
	defer script.mu.Unlock()
	fail := func(err error) longTaskReply {
		if script.err == nil {
			script.err = err
		}
		return longTaskReply{content: "R1B_PROTOCOL_FAILURE"}
	}
	request, err := r1bDecodeRequest(body)
	if err != nil {
		return fail(err)
	}
	turn, ok := r1bCurrentTurn(request)
	if !ok {
		return fail(fmt.Errorf("current user input missing"))
	}
	step := script.steps[turn]
	script.steps[turn]++
	tool := func(id, name string, arguments map[string]string) longTaskReply {
		return longTaskReply{toolCallID: id, toolName: name, toolArgs: arguments}
	}
	if turn == 1 {
		switch step {
		case 0:
			var content string
			for _, message := range request.Messages {
				if message.Role == "user" && strings.Contains(r1bText(message.Content), "R1B_STATE=") {
					content = strings.SplitN(r1bText(message.Content), "R1B_STATE=", 2)[1]
				}
			}
			state, stateErr := r1bReadState(content)
			if stateErr != nil || state.Version != 1 || state.Stage != 0 || state.EffectDone {
				return fail(fmt.Errorf("initial authorized state missing or invalid"))
			}
			data, _ := json.Marshal(state)
			return tool("r1b-write-state", "write", map[string]string{"path": "task-state.json", "content": string(data)})
		case 1:
			if r1bToolText(request, "r1b-write-state") == "" {
				return fail(fmt.Errorf("write result missing from next actual request"))
			}
			return tool("r1b-effect-once", "bash", map[string]string{"command": r1bEffectCommand})
		case 2:
			state, stateErr := r1bReadState(r1bToolText(request, "r1b-effect-once"))
			if stateErr != nil || state.Stage != 1 || !state.EffectDone {
				return fail(fmt.Errorf("actual effect checkpoint result invalid"))
			}
		}
	}
	if turn == 61 || turn == 120 {
		id := "r1b-read-state"
		if turn == 120 {
			id = "r1b-read-final-state"
		}
		if step == 0 {
			return tool(id, "read", map[string]string{"path": "task-state.json"})
		}
		if step == 1 {
			state, stateErr := r1bReadState(r1bToolText(request, id))
			if stateErr != nil || state.Stage != 1 || state.Version != 1 || !state.EffectDone {
				return fail(fmt.Errorf("resume/final real read checkpoint invalid"))
			}
			if turn == 120 {
				// The command's data comes only from this actual tool result,
				// never from the external grader's expected canary.
				data, _ := json.Marshal(state)
				encoded := base64.StdEncoding.EncodeToString(data)
				return tool("r1b-finalize", "bash", map[string]string{"command": r1bFinalCommand(encoded)})
			}
		}
		if turn == 120 && step == 2 {
			state, stateErr := r1bReadState(r1bToolText(request, "r1b-finalize"))
			if stateErr != nil || state.Stage != 2 || state.Version != 2 {
				return fail(fmt.Errorf("final real Bash output invalid"))
			}
		}
	}
	return longTaskReply{content: fmt.Sprintf("R1B_REPLY_%03d %s", turn, strings.Repeat("history continuity padding ", 96))}
}

const r1bEffectCommand = `python3 - <<'PY'
import json
from pathlib import Path
p=Path('task-state.json');s=json.loads(p.read_text())
assert s['stage']==0 and not s['effect_done']
with Path('effects.jsonl').open('a') as f:
    f.write(json.dumps({'task_id':s['task_id'],'event':'once'})+'\n')
s.update(stage=1,effect_done=True)
p.write_text(json.dumps(s))
print(json.dumps(s))
PY`

func r1bFinalCommand(observed string) string {
	return "python3 - '" + observed + "' <<'PY'\n" + `import base64,json,sys
from pathlib import Path
s=json.loads(Path('task-state.json').read_text())
observed=json.loads(base64.b64decode(sys.argv[1]))
assert s==observed and s['stage']==1 and s['effect_done']
s.update(stage=2,version=2)
Path('task-state.json').write_text(json.dumps(s))
Path('final.json').write_text(json.dumps(s))
print(json.dumps(s))
PY`
}

func r1bNonce(t *testing.T) string {
	t.Helper()
	var data [16]byte
	_, err := rand.Read(data[:])
	require.NoError(t, err)
	return hex.EncodeToString(data[:])
}

func r1bHash(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func r1bEnvironmentValue(environment []string, name string) string {
	for _, value := range environment {
		if strings.HasPrefix(value, name+"=") {
			return strings.TrimPrefix(value, name+"=")
		}
	}
	return ""
}

func TestR1bPeerUsesActualCheckpointResult(t *testing.T) {
	observed := r1bTaskState{TaskID: "from-real-tool", Canary: "not-a-grader-answer", Version: 1, Stage: 1, EffectDone: true}
	data, err := json.Marshal(observed)
	require.NoError(t, err)
	request := func(id, content string) []byte {
		messages := []map[string]any{{"role": "user", "content": "R1B_TURN=120 finish the task"}}
		if id != "" {
			messages = append(messages, map[string]any{"role": "tool", "tool_call_id": id, "content": content})
		}
		body, encodeErr := json.Marshal(map[string]any{"stream": true, "messages": messages})
		require.NoError(t, encodeErr)
		return body
	}
	script := &r1bScript{steps: map[int]int{}}
	read := script.reply(0, request("", ""))
	require.Equal(t, "r1b-read-final-state", read.toolCallID)
	final := script.reply(1, request(read.toolCallID, "untrusted file output:\n"+string(data)))
	require.NoError(t, script.failure())
	require.Equal(t, "r1b-finalize", final.toolCallID)
	require.Equal(t, r1bFinalCommand(base64.StdEncoding.EncodeToString(data)), final.toolArgs["command"])
	missing := &r1bScript{steps: map[int]int{}}
	missing.reply(0, request("", ""))
	reply := missing.reply(1, request("wrong-tool-id", string(data)))
	require.Equal(t, "R1B_PROTOCOL_FAILURE", reply.content)
	require.Error(t, missing.failure(), "the peer cannot manufacture a checkpoint without the actual matching tool result")
}

func TestR1bActionScriptContract(t *testing.T) {
	root := t.TempDir()
	state := r1bTaskState{TaskID: "script-contract", Canary: "observed-file-state", Version: 1}
	data, err := json.Marshal(state)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, "task-state.json"), data, 0o600))
	run := func(command string) ([]byte, error) {
		cmd := exec.Command("bash", "-c", command)
		cmd.Dir = root
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "LANG=C"}
		return cmd.CombinedOutput()
	}
	result, err := run(r1bEffectCommand)
	require.NoError(t, err, "%s", result)
	checkpoint, err := r1bReadState(string(result))
	require.NoError(t, err)
	require.Equal(t, 1, checkpoint.Stage)
	_, err = run(r1bEffectCommand)
	require.Error(t, err, "an already completed effect must not be appended again")
	checkpointJSON, err := json.Marshal(checkpoint)
	require.NoError(t, err)
	result, err = run(r1bFinalCommand(base64.StdEncoding.EncodeToString(checkpointJSON)))
	require.NoError(t, err, "%s", result)
	final, err := r1bReadState(string(result))
	require.NoError(t, err)
	require.Equal(t, 2, final.Stage)
	require.Equal(t, 2, final.Version)
	require.Equal(t, state.Canary, final.Canary)
	ledger, err := os.ReadFile(filepath.Join(root, "effects.jsonl"))
	require.NoError(t, err)
	require.Equal(t, 1, bytes.Count(ledger, []byte{'\n'}))
}

func TestR1bBusinessArtifactExportContract(t *testing.T) {
	for _, missingFinal := range []bool{false, true} {
		t.Run(fmt.Sprintf("missing_final_%t", missingFinal), func(t *testing.T) {
			source, destination := t.TempDir(), t.TempDir()
			want := map[string]string{"task-state.json": "checkpoint\n", "effects.jsonl": "one-effect\n", "final.json": "final-state\n"}
			for name, data := range want {
				if missingFinal && name == "final.json" {
					continue
				}
				require.NoError(t, os.WriteFile(filepath.Join(source, name), []byte(data), 0o600))
			}
			hashes, missing, err := r1bExportBusinessArtifacts(source, destination)
			require.NoError(t, err)
			if missingFinal {
				require.Equal(t, []string{"final.json"}, missing)
				_, err := os.Stat(filepath.Join(destination, "final.json"))
				require.ErrorIs(t, err, os.ErrNotExist, "missing original must not become an empty fabricated file")
			} else {
				require.Empty(t, missing)
			}
			for name, data := range want {
				if missingFinal && name == "final.json" {
					continue
				}
				copied, err := os.ReadFile(filepath.Join(destination, name))
				require.NoError(t, err)
				require.Equal(t, data, string(copied))
				require.Equal(t, r1bHash(copied), hashes[name])
			}
			_, _, err = r1bExportBusinessArtifacts(source, destination)
			require.ErrorIs(t, err, os.ErrExist, "existing evidence must never be overwritten")
			checkpoint, err := os.ReadFile(filepath.Join(destination, "task-state.json"))
			require.NoError(t, err)
			require.Equal(t, want["task-state.json"], string(checkpoint))
		})
	}
}
