package cli

import (
	"bytes"
	"context"
	"encoding/json"
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

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/memory"
)

func TestAutomaticMemoryDefaultOnThroughInteractiveCommand(t *testing.T) {
	server := newMemoryTestServer(t)
	defer server.Close()
	cfg, _ := newMemoryTestConfig(t, server.URL)
	cfg.Memory.Enabled = nil
	var output bytes.Buffer
	require.NoError(t, newInteractiveCmd(strings.NewReader("Please remember focused tests\nexit\n"), &output).Run(t.Context(), cfg, nil))
	require.Equal(t, int32(1), server.extractionRequests.Load())
}

func TestAutomaticMemoryHelpDescribesDefaultAndDisable(t *testing.T) {
	server := newMemoryTestServer(t)
	defer server.Close()
	cfg, _ := newMemoryTestConfig(t, server.URL)
	var output bytes.Buffer
	require.NoError(t, newInteractiveCmd(strings.NewReader("/help\nexit\n"), &output).Run(t.Context(), cfg, nil))
	require.Contains(t, output.String(), "enabled by default")
	require.Contains(t, output.String(), "memory.enabled: false")
	require.NotContains(t, output.String(), "no automatic extraction or prompt injection")
	require.Empty(t, server.requestBodies())
}

func TestAutomaticMemoryCommandSubprocess(t *testing.T) {
	path := os.Getenv("GO_CLI_MEMORY_COMMAND_CONFIG")
	if path == "" {
		t.Skip("subprocess entry")
	}
	cfg, err := config.NewLoader().WithEnvPrefix("MEMORY_COMMAND_FIXTURE").WithFile(path).Load(t.Context())
	require.NoError(t, err)
	require.NoError(t, newPromptCmd(io.Discard).Run(t.Context(), cfg, []string{"follow up from new process"}))
}

func runMemoryPromptProcess(t *testing.T, cfg *config.Config, workspace string) {
	t.Helper()
	data, err := json.Marshal(cfg)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "config.json")
	require.NoError(t, os.WriteFile(path, data, 0o600))
	executable, err := os.Executable()
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "-test.run=^TestAutomaticMemoryCommandSubprocess$")
	command.Env = append(os.Environ(), "GO_CLI_MEMORY_COMMAND_CONFIG="+path)
	command.Dir = workspace
	output, err := command.CombinedOutput()
	require.NoError(t, err, string(output))
}

func mainMemoryRequestBodies(server *memoryTestServer) [][]byte {
	var result [][]byte
	for _, body := range server.requestBodies() {
		if !bytes.Contains(body, []byte("Analyze the following conversation")) {
			result = append(result, body)
		}
	}
	return result
}

func TestAutomaticMemoryPersistsInjectsAndHonorsDisabledAndWorkspaceScopeAcrossProcesses(t *testing.T) {
	workspace, err := os.Getwd()
	require.NoError(t, err)
	server := newMemoryTestServer(t)
	defer server.Close()
	cfg, home := newMemoryTestConfig(t, server.URL)
	cfg.Memory.Enabled = nil
	var output bytes.Buffer
	require.NoError(t, newInteractiveCmd(strings.NewReader("remember testing preference\nexit\n"), &output).Run(t.Context(), cfg, nil))
	require.Equal(t, int32(1), server.extractionRequests.Load())
	runMemoryPromptProcess(t, cfg, workspace)
	require.Equal(t, int32(2), server.extractionRequests.Load())
	main := mainMemoryRequestBodies(server)
	require.Len(t, main, 2)
	require.Contains(t, string(main[1]), "automatically extracted canary")

	_, workspaceID, err := resolveWorkspaceIdentity(os.Getwd)
	require.NoError(t, err)
	disabled := false
	cfg.Memory.Enabled = &disabled
	cfg.Memory.StorePath = filepath.Join(home, ".go-cli", "memories", workspaceID+".jsonl")
	runMemoryPromptProcess(t, cfg, workspace)
	require.Equal(t, int32(2), server.extractionRequests.Load(), "disabled mode makes only its main request")
	main = mainMemoryRequestBodies(server)
	require.Len(t, main, 3)
	require.NotContains(t, string(main[2]), "automatically extracted canary")

	cfg.Memory.Enabled = nil // Explicitly sharing a file still cannot share workspace facts.
	runMemoryPromptProcess(t, cfg, t.TempDir())
	main = mainMemoryRequestBodies(server)
	require.Len(t, main, 4)
	require.NotContains(t, string(main[3]), "automatically extracted canary")
}

func TestAutomaticMemoryDefaultOnThroughEphemeralInteractive(t *testing.T) {
	server := newMemoryTestServer(t)
	defer server.Close()
	cfg, _ := newMemoryTestConfig(t, server.URL)
	cfg.Memory.Enabled = nil
	cfg.Session.StorePath = ""
	var output bytes.Buffer
	require.NoError(t, newInteractiveCmd(strings.NewReader("remember ephemeral success\nexit\n"), &output).Run(t.Context(), cfg, nil))
	require.Equal(t, int32(1), server.extractionRequests.Load())
}

func TestAutomaticMemoryManagedChildrenDoNotExtractOrInjectForEitherContextMode(t *testing.T) {
	for _, mode := range []string{"independent", "inherit"} {
		t.Run(mode, func(t *testing.T) {
			configureDurableCLISessionTest(t)
			t.Chdir(t.TempDir())
			server, script := newDurablePromptServer(t,
				`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"memory-child","type":"function","function":{"name":"dispatch_subagent","arguments":"{\"prompt\":\"child task\",\"context_mode\":\"`+mode+`\",\"max_turns\":2}"}}]}}]}`,
				`{"choices":[{"message":{"role":"assistant","content":"child real answer"}}]}`,
				`{"choices":[{"message":{"role":"assistant","content":"parent real answer"}}]}`,
				memoryExtractionResponse,
			)
			defer server.Close()
			cfg := durablePromptConfig(t, server.URL, filepath.Join(t.TempDir(), "sessions"))
			cfg.Memory.Enabled = nil
			cfg.Memory.StorePath = filepath.Join(t.TempDir(), "memory.jsonl")
			_, workspaceID, err := resolveWorkspaceIdentity(os.Getwd)
			require.NoError(t, err)
			addStoredMemory(t, cfg.Memory.StorePath, memory.Memory{Content: "workspace memory must not enter a child", Category: "fact", WorkspaceID: workspaceID})
			require.NoError(t, newPromptCmd(io.Discard).Run(t.Context(), cfg, []string{"-approve=auto", "parent current reference"}))
			requests := script.requestBodies()
			require.Len(t, requests, 4, "only the parent adds one auxiliary extraction request")
			require.Contains(t, requests[0], "workspace memory must not enter a child")
			require.NotContains(t, requests[1], "workspace memory must not enter a child")
			if mode == "inherit" {
				require.Contains(t, requests[1], "parent current reference")
			} else {
				require.NotContains(t, requests[1], "parent current reference")
			}
			require.Contains(t, requests[3], "Analyze the following conversation")
			require.NotContains(t, requests[3], "child real answer", "tool output is not an automatic-memory source")
		})
	}
}

func TestAutomaticMemorySmallModelHasIndependentTrackedOperation(t *testing.T) {
	server := newMemoryTestServer(t)
	defer server.Close()
	cfg, _ := newMemoryTestConfig(t, server.URL)
	cfg.Memory.Enabled = nil
	cfg.Session.StorePath = ""
	cfg.SmallModel.Model = "memory-small"
	cfg.SmallModel.SetMaxTokens(128)
	assembly, err := AssembleAgent(t.Context(), cfg, "test", "test-model", io.Discard)
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)
	assembly.RefreshMemoryContext(t.Context())
	result, err := assembly.TurnRunner.RunTurn(t.Context(), core.Submission{Content: "remember focused tests"})
	require.NoError(t, err)
	assembly.RecordCompletedMemory(t.Context(), result, nil, "remember focused tests")
	assembly.DrainMemory(t.Context())
	require.Equal(t, int32(1), server.extractionRequests.Load())
	var extractionModel string
	for _, body := range server.requestBodies() {
		if bytes.Contains(body, []byte("Analyze the following conversation")) {
			var request struct {
				Model     string `json:"model"`
				MaxTokens int    `json:"max_tokens"`
			}
			require.NoError(t, json.Unmarshal(body, &request))
			extractionModel = request.Model
			require.Equal(t, 128, request.MaxTokens, "automatic memory preserves a lower configured output limit")
		}
	}
	require.Equal(t, "memory-small", extractionModel)
	attempts, _ := assembly.CostTracker.AttemptCostSnapshot()
	require.Len(t, attempts, 2)
	require.NotEqual(t, attempts[0].Result.OperationID, attempts[1].Result.OperationID)
	require.NotEqual(t, attempts[0].Result.AttemptID, attempts[1].Result.AttemptID)
}

func TestAutomaticMemoryPlanEpochAndDisableRevokeHeldHTTPExtraction(t *testing.T) {
	for _, mode := range []string{"disable", "plan"} {
		t.Run(mode, func(t *testing.T) {
			started := make(chan struct{})
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if bytes.Contains(body, []byte("Analyze the following conversation")) {
					close(started)
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
					_, _ = io.WriteString(w, memoryExtractionResponse)
					return
				}
				_, _ = io.WriteString(w, conversationResponse)
			}))
			defer server.Close()
			cfg, _ := newMemoryTestConfig(t, server.URL)
			cfg.Memory.Enabled = nil
			cfg.Session.StorePath = ""
			assembly, err := AssembleAgent(t.Context(), cfg, "test", "test-model", io.Discard)
			require.NoError(t, err)
			t.Cleanup(assembly.Cleanup)
			assembly.RefreshMemoryContext(t.Context())
			result, err := assembly.TurnRunner.RunTurn(t.Context(), core.Submission{Content: "remember this held turn"})
			require.NoError(t, err)
			assembly.RecordCompletedMemory(t.Context(), result, nil, "remember this held turn")
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("extraction did not start")
			}
			if mode == "disable" {
				assembly.SetAutomaticMemoryEnabled(false)
			} else {
				require.NoError(t, assembly.PlanCtrl.Enter(t.Context(), "revoke"))
				require.NoError(t, assembly.PlanCtrl.Exit(t.Context(), "remain revoked"))
			}
			close(release)
			assembly.DrainMemory(t.Context())
			items, err := assembly.MemoryStore.List(t.Context())
			require.NoError(t, err)
			require.Empty(t, items)
		})
	}
}

func TestAutomaticLegacyMemoryDoesNotBlockCurrentWorkspaceRelearning(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	server := newMemoryTestServer(t)
	defer server.Close()
	cfg, home := newMemoryTestConfig(t, server.URL)
	cfg.Memory.Enabled = nil
	cfg.Memory.StorePath = filepath.Join(home, "shared-legacy.jsonl")
	addStoredMemory(t, cfg.Memory.StorePath, memory.Memory{ID: "legacy-original", Content: "automatically extracted canary", Category: "preference", Source: "manual"})
	var out bytes.Buffer
	require.NoError(t, newPromptCmd(&out).Run(t.Context(), cfg, []string{"Remember my current workspace preference."}))
	require.NoError(t, newPromptCmd(&out).Run(t.Context(), cfg, []string{"What preference should apply here?"}))
	items := readStoredMemories(t, cfg.Memory.StorePath)
	legacy := false
	scoped := false
	for _, item := range items {
		if item.ID == "legacy-original" && item.WorkspaceID == "" {
			legacy = true
		}
		if item.WorkspaceID != "" && item.Content == "automatically extracted canary" {
			scoped = true
		}
	}
	require.True(t, legacy, "legacy record stays manually visible without reassignment")
	require.True(t, scoped, "current positive conversation evidence should create a new scoped fact despite same-text unscoped legacy")
	bodies := mainMemoryRequestBodies(server)
	require.Len(t, bodies, 2)
	require.Contains(t, string(bodies[1]), "automatically extracted canary")
}

func TestAutomaticMemoryDeltaExcludesNewCompactionSummary(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	var mu sync.Mutex
	var extractionBodies []string
	turns := 0
	summaries := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		text := string(body)
		w.Header().Set("Content-Type", "application/json")
		response := ""
		mu.Lock()
		if strings.Contains(text, "Analyze the following conversation") {
			extractionBodies = append(extractionBodies, text)
			response = `{"choices":[{"message":{"role":"assistant","content":"[]"}}]}`
		} else if strings.Contains(text, "Summarize the following conversation") {
			summaries++
			response = `{"choices":[{"message":{"role":"assistant","content":"summary-from-earlier-only"}}]}`
		} else {
			turns++
			answer := "fresh answer"
			if turns == 1 {
				answer = strings.Repeat("historical-only-token ", 4000)
			}
			response = fmt.Sprintf(`{"choices":[{"message":{"role":"assistant","content":%q}}]}`, answer)
		}
		mu.Unlock()
		_, _ = io.WriteString(w, response)
	}))
	defer server.Close()
	cfg, _ := newMemoryTestConfig(t, server.URL)
	cfg.Memory.Enabled = nil
	cfg.Compaction.Strategy = "summary"
	cfg.SmallModel.Model = "small-model"
	var out bytes.Buffer
	cmd := newInteractiveCmd(strings.NewReader("historical question\nfresh question\nexit\n"), &out)
	require.NoError(t, cmd.Run(t.Context(), cfg, []string{"--max-tokens", "6000", "--no-onboarding"}))
	mu.Lock()
	defer mu.Unlock()
	require.Positive(t, summaries)
	second := false
	for _, body := range extractionBodies {
		if strings.Contains(body, "fresh question") {
			second = true
			require.NotContains(t, body, "summary-from-earlier-only", "new compaction rows summarize prior history and are not committed current-turn user/assistant delta")
		}
	}
	require.True(t, second, "new successful turn should still have its current text eligible for extraction")
}

func TestAutomaticMemoryGracefulPromptWaitsForSlowHTTPExtraction(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte("Analyze the following conversation")) {
			select {
			case <-time.After(2300 * time.Millisecond):
			case <-r.Context().Done():
				return
			}
			_, _ = io.WriteString(w, memoryExtractionResponse)
			return
		}
		_, _ = io.WriteString(w, conversationResponse)
	}))
	defer server.Close()
	cfg, home := newMemoryTestConfig(t, server.URL)
	cfg.Memory.Enabled = nil
	cfg.Memory.StorePath = filepath.Join(home, "slow.jsonl")
	require.NoError(t, newPromptCmd(io.Discard).Run(t.Context(), cfg, []string{"remember slow success"}))
	require.Len(t, readStoredMemories(t, cfg.Memory.StorePath), 1, "normal exit must permit extraction slower than two seconds")
}

func TestAutomaticMemoryCanceledGracefulDrainStopsHTTPExtraction(t *testing.T) {
	started := make(chan struct{})
	requestDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte("Analyze the following conversation")) {
			close(started)
			<-r.Context().Done()
			close(requestDone)
			return
		}
		_, _ = io.WriteString(w, conversationResponse)
	}))
	defer server.Close()
	cfg, home := newMemoryTestConfig(t, server.URL)
	cfg.Memory.Enabled = nil
	cfg.Memory.StorePath = filepath.Join(home, "canceled.jsonl")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = newPromptCmd(io.Discard).Run(ctx, cfg, []string{"remember canceled success"})
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("extraction did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancel did not promptly join prompt")
	}
	select {
	case <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("HTTP extraction was not canceled")
	}
	require.Empty(t, readStoredMemories(t, cfg.Memory.StorePath))
}

// TestAutomaticMemoryDiagnosticProcess is a real command subprocess so stdout
// and stderr remain independent, as they are for shell pipelines.
func TestAutomaticMemoryDiagnosticProcess(t *testing.T) {
	path := os.Getenv("GO_CLI_MEMORY_DIAGNOSTIC_CONFIG")
	if path == "" {
		t.Skip("subprocess entry")
	}
	cfg, err := config.NewLoader().WithEnvPrefix("MEMORY_DIAGNOSTIC_FIXTURE").WithFile(path).Load(t.Context())
	if err == nil {
		mode := os.Getenv("GO_CLI_MEMORY_DIAGNOSTIC_MODE")
		if mode == "interactive" {
			err = newInteractiveCmd(strings.NewReader("question\nexit\n"), os.Stdout).Run(t.Context(), cfg, nil)
		} else {
			err = newPromptCmd(os.Stdout).Run(t.Context(), cfg, []string{"--output", mode, "question"})
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}
