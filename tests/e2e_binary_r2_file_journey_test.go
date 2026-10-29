//go:build darwin || linux

package tests

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/session"
)

// r2FilePeer only consumes actual HTTP tool results. It has no disk accessor
// and no copy of the expected input or final business bytes.
type r2FilePeer struct {
	mu                    sync.Mutex
	root, business, token string
	step                  int
	source, target        string
	calls                 []r2FileCall
	discovered            map[string]bool
	final                 bool
	failure               error
}

type r2FileRunArtifacts struct {
	Identity map[string]any
	Invoked  bool
	Returned bool
	Result   binaryRunResult
}

func (peer *r2FilePeer) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	peer.mu.Lock()
	defer peer.mu.Unlock()
	fail := func(err error) {
		if peer.failure == nil {
			peer.failure = err
		}
		http.Error(w, "R2 fixture contract failed", http.StatusBadRequest)
	}
	if request.Method != http.MethodPost || !strings.HasSuffix(request.URL.Path, "/chat/completions") {
		fail(fmt.Errorf("unexpected fixture route"))
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, 4<<20))
	if err != nil {
		fail(err)
		return
	}
	if len(peer.calls) > 0 {
		previous := &peer.calls[len(peer.calls)-1]
		if len(previous.NextRequest) == 0 {
			previous.NextRequest = append([]byte(nil), body...)
			result, resultErr := r2ToolResult(body, previous.ID)
			if resultErr != nil {
				fail(resultErr)
				return
			}
			switch previous.ID {
			case "r2-grep":
				path, _, _, parseErr := r2GrepMatch(result)
				if parseErr != nil {
					fail(parseErr)
					return
				}
				normalized, pathErr := r2BusinessPath(peer.root, peer.business, path)
				if pathErr != nil {
					fail(pathErr)
					return
				}
				peer.target = filepath.Join(peer.business, normalized)
			case "r2-read-source":
				peer.source = result
			case "r2-edit-bad":
				if !strings.Contains(result, "not found") {
					fail(fmt.Errorf("failed edit did not reach next real model request"))
					return
				}
			case "r2-read-after-bad":
				if result != peer.source {
					fail(fmt.Errorf("failed edit changed the actual readback"))
					return
				}
			case "r2-read-final":
				if result != strings.Replace(peer.source, "status:pending", "status:done", 1) {
					fail(fmt.Errorf("actual final readback did not match transformation"))
					return
				}
			}
		}
	}
	completion := binaryCompletion{Text: "R2_FILE_JOURNEY_COMPLETE"}
	if peer.step < 9 {
		call := peer.nextCall()
		args, encodeErr := json.Marshal(call.Args)
		if encodeErr != nil {
			fail(encodeErr)
			return
		}
		schemaHash, guardErr := r2GuardAdvertisedCall(body, call.Name, args)
		if guardErr != nil && !peer.discovered[call.Name] {
			// Search is an ordinary advertised call, never a made-up load API.
			// If the target is still unavailable in the subsequent request, fail.
			discovery := binaryToolCall{ID: "r2-discover-" + call.Name, Name: "tool_search", Args: map[string]any{"category": "file"}}
			args, _ = json.Marshal(discovery.Args)
			if hash, discoveryErr := r2GuardAdvertisedCall(body, discovery.Name, args); discoveryErr == nil {
				peer.discovered[call.Name] = true
				call, schemaHash, guardErr = discovery, hash, nil
			}
		}
		if guardErr != nil {
			fail(fmt.Errorf("current wire reachability: %w", guardErr))
			return
		}
		peer.calls = append(peer.calls, r2FileCall{ID: call.ID, Tool: call.Name, Args: args,
			Request: append([]byte(nil), body...), RequestSHA256: r2FileHash(body), SchemaSHA256: schemaHash})
		completion.Tool = &call
		if call.Name != "tool_search" {
			peer.step++
		}
	} else {
		peer.final = true
	}
	response, err := binaryCompletionJSON(completion)
	if err != nil {
		fail(err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(response); err != nil {
		peer.failure = err
	}
}

func (peer *r2FilePeer) nextCall() binaryToolCall {
	result := filepath.Join(peer.business, "result.txt")
	steps := []binaryToolCall{
		{ID: "r2-ls", Name: "ls", Args: map[string]any{"path": peer.business}},
		{ID: "r2-find", Name: "find", Args: map[string]any{"path": peer.business, "pattern": "*.txt", "type": "f"}},
		{ID: "r2-grep", Name: "grep", Args: map[string]any{"path": peer.business, "glob": "*.txt", "pattern": peer.token}},
		{ID: "r2-read-source", Name: "read", Args: map[string]any{"path": peer.target}},
		{ID: "r2-write", Name: "write", Args: map[string]any{"path": result, "content": peer.source}},
		{ID: "r2-edit-bad", Name: "edit", Args: map[string]any{"file_path": result, "old_string": "missing-" + peer.token, "new_string": "wrong"}},
		{ID: "r2-read-after-bad", Name: "read", Args: map[string]any{"path": result}},
		{ID: "r2-edit-good", Name: "edit", Args: map[string]any{"file_path": result, "old_string": "status:pending", "new_string": "status:done"}},
		{ID: "r2-read-final", Name: "read", Args: map[string]any{"path": result}},
	}
	return steps[peer.step]
}

func (peer *r2FilePeer) snapshot() (r2FileEvidence, error) {
	peer.mu.Lock()
	defer peer.mu.Unlock()
	return r2FileEvidence{Calls: append([]r2FileCall(nil), peer.calls...), FinalDelivered: peer.final, ExitCode: -1}, peer.failure
}

// TestRealBinaryR2FileJourney exercises the public CLI with scripted decisions,
// not a paid model or a model capability score. Native TopTools stays enabled.
func TestRealBinaryR2FileJourney(t *testing.T) {
	binary := buildCLIForBinaryE2E(t)
	store := filepath.Join(t.TempDir(), "durable")
	entropy := make([]byte, 16)
	if _, err := rand.Read(entropy); err != nil {
		t.Fatal(err)
	}
	peer := &r2FilePeer{business: "data space", token: r2FileHash(entropy)[:24], discovered: map[string]bool{}}
	server := httptest.NewServer(peer)
	t.Cleanup(server.Close)
	fixture := newBinaryFixture(t, server.URL, store)
	canonical, err := filepath.EvalSymlinks(fixture.workdir)
	if err != nil {
		t.Fatal(err)
	}
	peer.root = canonical
	configPath := filepath.Join(fixture.home, ".go-cli", "config.yaml")
	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	skills := t.TempDir()
	config = r2FileConfiguration(config, skills)
	if err := os.WriteFile(configPath, config, 0o600); err != nil {
		t.Fatal(err)
	}
	expected := r2FileExpectation{Business: peer.business, Token: peer.token, Target: "nested/target.txt", Initial: map[string]string{
		"nested/target.txt": "record\nR2_TOKEN=" + peer.token + "\nstatus:pending\n",
		"decoy.txt":         "unrelated text\n",
		"ignored.log":       "R2_TOKEN=" + peer.token + "\n",
	}}
	for name, content := range expected.Initial {
		r2ContractWrite(t, filepath.Join(fixture.workdir, expected.Business, name), content)
	}
	evidence := r2FileEvidence{ExitCode: -1}
	var run r2FileRunArtifacts
	if destination := os.Getenv("GO_CLI_R2_ARTIFACT_DIR"); destination != "" {
		if err := os.Mkdir(destination, 0o700); err != nil {
			t.Fatalf("artifact destination must be new: %v", err)
		}
		t.Cleanup(func() { r2ExportFileEvidence(t, destination, store, fixture, peer, evidence, run) })
		// These records exist before any product CLI invocation. A failed first
		// run therefore retains its actual initial oracle and frozen identities.
		r2WriteFileArtifact(t, destination, "expected.json", map[string]any{"expectation": expected, "rule": "copy actual target read bytes; replace one status:pending with status:done; preserve every input; result.txt is the only new business file"})
		if err := os.WriteFile(filepath.Join(destination, "config.yaml"), config, 0o600); err != nil {
			t.Fatal(err)
		}
		run.Identity = r2FileIdentity(t, binary, config)
		r2WriteFileArtifact(t, destination, "identity.json", run.Identity)
		r2WriteFileArtifact(t, destination, "prestart-status.json", map[string]any{"cli_invoked": false, "cli_returned": false, "stdout_stderr_available": false, "actual_paid_model_calls": 0})
	}
	goal := "Use ls, find, grep, read, write and edit for a file journey in data space. Find txt files and search token " + peer.token + "; read the actual matching source, write result.txt with those bytes, observe a failed edit, read unchanged bytes, correct status:pending to status:done, read back and finish. Discover tools with tool_search if needed."
	started := time.Now()
	run.Invoked = true
	run.Result = runBinaryPrompt(t, binary, fixture, "--approve", "auto", "--no-sandbox", "--output", "text", goal)
	run.Returned = true
	result := run.Result
	wall := time.Since(started).Seconds()
	evidence, peerErr := peer.snapshot()
	if result.err == nil {
		evidence.ExitCode = 0
	} else {
		var exitError *exec.ExitError
		if errors.As(result.err, &exitError) {
			evidence.ExitCode = exitError.ExitCode()
		}
	}
	if peerErr != nil {
		t.Fatalf("real wire contract: %v; CLI err=%v stderr=%s", peerErr, result.err, result.stderr)
	}
	successfulBinaryRun(t, result)
	if !strings.Contains(result.stdout, "R2_FILE_JOURNEY_COMPLETE") {
		t.Fatal("public CLI did not deliver the scripted final text")
	}
	physical := binarySessionID(t, result.stdout)
	snapshot, err := session.InspectPhysicalSession(t.Context(), store, physical)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := session.InspectPendingActions(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	evidence.Pending = len(pending)
	evidence.Actions, err = r2FileActions(snapshot.DurableEntries)
	if err != nil {
		t.Fatal(err)
	}
	if err := gradeR2FileJourney(canonical, expected, evidence); err != nil {
		t.Fatalf("independent file journey acceptance: %v", err)
	}
	if strings.Contains(result.stderr, "DATA RACE") {
		t.Fatal("real binary reported a race")
	}
	binaryBytes, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("R2_FILE_ACCEPTED profile=fresh-home-memory-off-native-toptools binary_sha256=%s config_sha256=%s calls=%d unique_actions=%d pending=%d cli_wall_seconds=%.3f paid_model_calls=0 token_usage=unknown grader_negatives=separate_sandboxes",
		r2FileHash(binaryBytes), r2FileHash(config), len(evidence.Calls), len(evidence.Actions), evidence.Pending, wall)
}

func r2FileConfiguration(base []byte, skills string) []byte {
	// This YAML subset treats flow sequences, including [], as strings.
	// Fresh HOME/workdir and the isolated child environment have no MCP
	// source; retain the default empty server set by omitting that section.
	return append(base, []byte(fmt.Sprintf("memory:\n  enabled: false\nsmall_model:\n  model: \"\"\nextensions:\n  enabled: false\nskill:\n  dir: %q\ngit:\n  enabled: false\nmodel_cycler:\n  enabled: false\n", skills))...)
}

func TestR2FixtureConfigurationContract(t *testing.T) {
	fixture := newBinaryFixture(t, "http://127.0.0.1:1", filepath.Join(t.TempDir(), "sessions"))
	base, err := os.ReadFile(filepath.Join(fixture.home, ".go-cli", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	skills := t.TempDir()
	data := r2FileConfiguration(base, skills)
	var declared config.Config
	if err := config.UnmarshalConfig(data, config.ConfigFormatYAML, &declared); err != nil {
		t.Fatalf("actual fixture config cannot be parsed before CLI: %v", err)
	}
	if declared.Memory.Enabled == nil || *declared.Memory.Enabled || declared.Extensions.Enabled == nil || *declared.Extensions.Enabled ||
		declared.SmallModel.Model != "" || len(declared.MCP.Servers) != 0 || declared.Skill.Dir != skills || len(declared.Tools.Builtin) != 0 {
		t.Fatal("fixture config changed its isolation/native-tool declaration")
	}
}

// InspectPhysicalSession/InspectPendingActions validate durable owner references
// and resolutions first. This projection checks the journey's unique actions;
// it does not replace that production durable validator or mint authority.
func r2FileActions(entries []session.SessionEntry) (map[string]r2FileAction, error) {
	actions, calls, runs := map[string]r2FileAction{}, map[string]string{}, map[string]string{}
	mergeDispatch := func(current, observed string) (string, error) {
		if observed == "" {
			return current, nil
		}
		if current != "" && current != observed {
			return "", fmt.Errorf("conflicting dispatch observations: %s/%s", current, observed)
		}
		return observed, nil
	}
	for _, entry := range entries {
		if entry.ActionState != session.ActionStateIntent {
			continue
		}
		action := actions[entry.ToolCallID]
		if action.ID != "" && action.ID != entry.ActionID {
			return nil, fmt.Errorf("tool call reused across durable actions")
		}
		action.ID, action.Tool, action.IntentCount = entry.ActionID, entry.ToolName, action.IntentCount+1
		actions[entry.ToolCallID], calls[entry.ActionID], runs[entry.ActionID] = action, entry.ToolCallID, entry.RunID
	}
	for _, entry := range entries {
		id := entry.ToolCallID
		if entry.ExecutionResolution != nil {
			resolution := entry.ExecutionResolution
			id = calls[resolution.ActionID]
			if id == "" || resolution.RunID != runs[resolution.ActionID] {
				return nil, fmt.Errorf("resolution not bound to this action/run")
			}
			action := actions[id]
			action.ResolutionCount++
			action.Resolution, action.ResolutionRef = string(resolution.Evidence), resolution.EvidenceRef
			actions[id] = action
			continue
		}
		if id == "" || entry.Type != session.EntryTypeTool {
			continue
		}
		action, exists := actions[id]
		if !exists || entry.ActionID != action.ID || entry.RunID != runs[action.ID] {
			return nil, fmt.Errorf("receipt/execution has no matching intent: %s", id)
		}
		switch entry.ActionState {
		case session.ActionStateExecution:
			dispatch, err := mergeDispatch(action.Dispatch, string(entry.DispatchState))
			if err != nil {
				return nil, err
			}
			action.Dispatch, action.Resource, action.Proof = dispatch, string(entry.ResourceState), string(entry.CompletionProof)
		case "":
			dispatch, err := mergeDispatch(action.Dispatch, string(entry.DispatchState))
			if err != nil {
				return nil, err
			}
			action.Dispatch = dispatch
			action.ReceiptCount++
			action.Outcome = entry.ReceiptOutcome
		}
		actions[id] = action
	}
	return actions, nil
}

func r2FileIdentity(t *testing.T, binary string, config []byte) map[string]any {
	t.Helper()
	module := binaryE2EModuleRoot(t)
	commit, err := exec.Command("git", "-C", module, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("record actual source commit: %v", err)
	}
	data, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	sources := map[string]string{}
	for _, name := range []string{"r2_file_journey_fixture_test.go", "r2_file_journey_grader_test.go", "e2e_binary_r2_file_journey_test.go"} {
		content, err := os.ReadFile(filepath.Join(module, "tests", name))
		if err != nil {
			t.Fatal(err)
		}
		sources[name] = r2FileHash(content)
	}
	return map[string]any{"source_commit": strings.TrimSpace(string(commit)), "test_source_sha256": sources,
		"binary_sha256": r2FileHash(data), "config_sha256": r2FileHash(config),
		"profile": "fresh-home-memory-off-native-toptools", "product_source_modified_by_test": false}
}

func r2WriteFileArtifact(t *testing.T, destination, name string, value any) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err == nil {
		err = os.WriteFile(filepath.Join(destination, name), data, 0o600)
	}
	if err != nil {
		t.Fatalf("write synthetic artifact %s: %v", name, err)
	}
}

func r2ExportFileEvidence(t *testing.T, destination, store string, fixture binaryFixture, peer *r2FilePeer, evidence r2FileEvidence, run r2FileRunArtifacts) {
	t.Helper()
	missing := []string{}
	exportErrors := []string{}
	observed, peerErr := peer.snapshot()
	observed.Actions, observed.Pending, observed.ExitCode = evidence.Actions, evidence.Pending, evidence.ExitCode
	data, err := json.MarshalIndent(map[string]any{"evidence": observed, "peer_error": fmt.Sprint(peerErr), "synthetic_fixture_only": true}, "", "  ")
	if err == nil {
		err = os.WriteFile(filepath.Join(destination, "evidence.json"), data, 0o600)
	}
	if err != nil {
		t.Errorf("export synthetic evidence: %v", err)
		exportErrors = append(exportErrors, "evidence.json: "+err.Error())
	}
	if run.Returned {
		for name, content := range map[string]string{"stdout.txt": run.Result.stdout, "stderr.txt": run.Result.stderr} {
			if err := os.WriteFile(filepath.Join(destination, name), []byte(content), 0o600); err != nil {
				exportErrors = append(exportErrors, name+": "+err.Error())
				t.Errorf("export captured CLI stream: %v", err)
			}
		}
	} else {
		missing = append(missing, "stdout.txt: CLI result unavailable", "stderr.txt: CLI result unavailable")
	}
	// Business bytes, durable sessions and native audit logs stay separate.
	for _, source := range []string{filepath.Join(fixture.workdir, peer.business), filepath.Join(store, "sessions"), filepath.Join(fixture.workdir, ".go-cli", "logs")} {
		err := filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			relative, relErr := filepath.Rel(source, path)
			if relErr != nil {
				return relErr
			}
			target := filepath.Join(destination, filepath.Base(source), relative)
			if entry.IsDir() {
				return os.MkdirAll(target, 0o700)
			}
			if !entry.Type().IsRegular() {
				return fmt.Errorf("unsupported export entry %s", relative)
			}
			content, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			return os.WriteFile(target, content, 0o600)
		})
		if os.IsNotExist(err) {
			missing = append(missing, filepath.Base(source))
		} else if err != nil {
			exportErrors = append(exportErrors, filepath.Base(source)+": "+err.Error())
			t.Errorf("export fixture artifacts: %v", err)
		}
	}
	for _, name := range []string{"expected.json", "identity.json", "config.yaml", "prestart-status.json"} {
		if _, err := os.Stat(filepath.Join(destination, name)); os.IsNotExist(err) {
			missing = append(missing, name)
		} else if err != nil {
			exportErrors = append(exportErrors, name+": "+err.Error())
		}
	}
	var exitCode any
	if run.Returned && evidence.ExitCode >= 0 {
		exitCode = evidence.ExitCode
	}
	status := map[string]any{"cli_invoked": run.Invoked, "cli_returned": run.Returned, "cli_error": fmt.Sprint(run.Result.err),
		"exit_code": exitCode, "missing": missing, "export_errors": exportErrors, "actual_paid_model_calls": 0}
	data, err = json.MarshalIndent(status, "", "  ")
	if err == nil {
		err = os.WriteFile(filepath.Join(destination, "export-status.json"), data, 0o600)
	}
	if err != nil {
		t.Errorf("export synthetic status: %v", err)
	}
}

func TestR2EvidenceExportContract(t *testing.T) {
	for _, returned := range []bool{false, true} {
		t.Run(fmt.Sprintf("CLI-returned-%v", returned), func(t *testing.T) {
			destination := filepath.Join(t.TempDir(), "new-run")
			if err := os.Mkdir(destination, 0o700); err != nil {
				t.Fatal(err)
			}
			fixture := binaryFixture{workdir: t.TempDir()}
			peer := &r2FilePeer{business: "data space"}
			r2ContractWrite(t, filepath.Join(fixture.workdir, peer.business, "input.txt"), "synthetic original\n")
			r2ContractWrite(t, filepath.Join(fixture.workdir, ".go-cli/logs/native.log"), "native audit retained\n")
			for _, name := range []string{"expected.json", "identity.json", "prestart-status.json"} {
				r2WriteFileArtifact(t, destination, name, map[string]any{"synthetic_export_contract": true})
			}
			r2ContractWrite(t, filepath.Join(destination, "config.yaml"), "synthetic config\n")
			before, err := os.ReadFile(filepath.Join(destination, "prestart-status.json"))
			if err != nil {
				t.Fatal(err)
			}
			run := r2FileRunArtifacts{Invoked: returned, Returned: returned}
			evidence := r2FileEvidence{ExitCode: -1}
			if returned {
				run.Result = binaryRunResult{stdout: "actual synthetic stdout", stderr: "actual synthetic stderr", err: fmt.Errorf("synthetic CLI error")}
				evidence.ExitCode = 23
			}
			r2ExportFileEvidence(t, destination, t.TempDir(), fixture, peer, evidence, run)
			statusBytes, err := os.ReadFile(filepath.Join(destination, "export-status.json"))
			if err != nil {
				t.Fatal(err)
			}
			var status map[string]any
			if err := json.Unmarshal(statusBytes, &status); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(statusBytes), "sessions") || status["cli_returned"] != returned {
				t.Fatalf("missing physical session or availability was hidden: %s", statusBytes)
			}
			if !returned && status["exit_code"] != nil {
				t.Fatal("unstarted CLI acquired a fabricated successful exit")
			}
			for name, expected := range map[string]string{"data space/input.txt": "synthetic original\n", "logs/native.log": "native audit retained\n"} {
				actual, err := os.ReadFile(filepath.Join(destination, name))
				if err != nil || string(actual) != expected {
					t.Fatalf("exported original %s mismatch: %v", name, err)
				}
			}
			if returned {
				for name, expected := range map[string]string{"stdout.txt": run.Result.stdout, "stderr.txt": run.Result.stderr} {
					actual, err := os.ReadFile(filepath.Join(destination, name))
					if err != nil || string(actual) != expected {
						t.Fatalf("captured stream %s mismatch: %v", name, err)
					}
				}
			}
			after, err := os.ReadFile(filepath.Join(destination, "prestart-status.json"))
			if err != nil || string(before) != string(after) {
				t.Fatal("cleanup overwrote the pre-CLI evidence")
			}
		})
	}
}

func TestR2FileActionCollectorContract(t *testing.T) {
	for _, scenario := range []struct {
		name, receiptDispatch, executionDispatch string
		receiptFirst, wantError                  bool
	}{
		{name: "never-dispatched-without-execution", receiptDispatch: "not_dispatched"},
		{name: "dispatched-without-execution-does-not-mint-proof", receiptDispatch: "dispatched"},
		{name: "dispatched-consistent", receiptDispatch: "dispatched", executionDispatch: "dispatched"},
		{name: "conflict-execution-first", receiptDispatch: "not_dispatched", executionDispatch: "dispatched", wantError: true},
		{name: "conflict-receipt-first", receiptDispatch: "not_dispatched", executionDispatch: "dispatched", receiptFirst: true, wantError: true},
		{name: "missing-receipt-dispatch-without-execution-stays-unknown"},
		{name: "missing-receipt-dispatch-retains-actual-execution", executionDispatch: "dispatched"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			intent := session.SessionEntry{Type: session.EntryTypeTool, ActionState: session.ActionStateIntent,
				ActionID: "action-contract", ToolCallID: "r2-edit-bad", ToolName: "edit", RunID: "run-contract"}
			receipt := intent
			receipt.ActionState, receipt.DispatchState, receipt.ReceiptOutcome = "", session.DispatchState(scenario.receiptDispatch), "error"
			execution := intent
			execution.ActionState, execution.DispatchState = session.ActionStateExecution, session.DispatchState(scenario.executionDispatch)
			execution.ResourceState, execution.CompletionProof = "quiescent", "inline_pipeline_joined"
			rows := []session.SessionEntry{intent}
			if scenario.executionDispatch != "" && !scenario.receiptFirst {
				rows = append(rows, execution)
			}
			rows = append(rows, receipt)
			if scenario.executionDispatch != "" && scenario.receiptFirst {
				rows = append(rows, execution)
			}
			resolution := core.ResolutionLocalResourcesStopped
			if scenario.receiptDispatch == "not_dispatched" {
				resolution = core.ResolutionNeverDispatched
			}
			rows = append(rows, session.SessionEntry{ExecutionResolution: &core.ExecutionResolution{
				ActionID: intent.ActionID, RunID: intent.RunID, Evidence: resolution, EvidenceRef: "synthetic-ref"}})
			actions, err := r2FileActions(rows)
			if scenario.wantError {
				if err == nil || !strings.Contains(err.Error(), "conflicting dispatch") {
					t.Fatalf("collector did not reject conflicting actual dispatch: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			action := actions[intent.ToolCallID]
			wantedDispatch := scenario.receiptDispatch
			if wantedDispatch == "" {
				wantedDispatch = scenario.executionDispatch
			}
			if action.Dispatch != wantedDispatch || action.Outcome != "error" {
				t.Fatalf("lost actual error receipt state: %+v", action)
			}
			if scenario.executionDispatch == "" && (action.Proof != "" || action.Resource != "") {
				t.Fatal("absence of execution was turned into fabricated resource proof")
			}
		})
	}
}
