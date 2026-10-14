package tests

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
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type binaryToolCall struct {
	ID   string
	Name string
	Args map[string]any
}

type binaryCompletion struct {
	Tool   *binaryToolCall
	Text   string
	Status int
}

type binaryProviderRequest struct {
	Body      map[string]any
	Raw       string
	Auxiliary bool
}

type binaryMockProvider struct {
	mu       sync.Mutex
	steps    []binaryCompletion
	nextStep int
	requests []binaryProviderRequest
}

type binaryFixture struct {
	home        string
	workdir     string
	sessionPath string
}

type binaryRunResult struct {
	stdout string
	stderr string
	err    error
}

func buildCLIForBinaryE2E(t *testing.T) string {
	t.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve binary E2E source path")
	}
	repoRoot := filepath.Dir(filepath.Dir(sourceFile))
	binaryPath := filepath.Join(t.TempDir(), "go-cli")
	cmd := exec.Command("go", "build", "-race", "-o", binaryPath, "./cmd/cli")
	cmd.Dir = repoRoot
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build CLI binary: %v\n%s", err, output)
	}
	return binaryPath
}

func newBinaryMockProvider(t *testing.T, steps ...binaryCompletion) (*httptest.Server, *binaryMockProvider) {
	t.Helper()
	provider := &binaryMockProvider{steps: append([]binaryCompletion(nil), steps...)}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.Error(w, "unexpected mock provider route", http.StatusNotFound)
			return
		}
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read CLI provider request: %v", err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		var body map[string]any
		if err := json.Unmarshal(bodyBytes, &body); err != nil {
			t.Errorf("decode CLI provider request: %v", err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		raw := string(bodyBytes)
		auxiliary := strings.Contains(raw, "Analyze the following conversation")

		provider.mu.Lock()
		provider.requests = append(provider.requests, binaryProviderRequest{Body: body, Raw: raw, Auxiliary: auxiliary})
		completion := binaryCompletion{Text: "[]"}
		if !auxiliary {
			if provider.nextStep >= len(provider.steps) {
				provider.mu.Unlock()
				http.Error(w, "mock provider script exhausted", http.StatusInternalServerError)
				return
			}
			completion = provider.steps[provider.nextStep]
			provider.nextStep++
		}
		provider.mu.Unlock()

		if completion.Status != 0 {
			http.Error(w, "scripted provider failure", completion.Status)
			return
		}
		response, err := binaryCompletionJSON(completion)
		if err != nil {
			t.Errorf("encode mock provider response: %v", err)
			http.Error(w, "invalid scripted response", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write(response); err != nil {
			t.Errorf("write mock provider response: %v", err)
		}
	}))
	t.Cleanup(server.Close)
	return server, provider
}

func binaryCompletionJSON(completion binaryCompletion) ([]byte, error) {
	message := map[string]any{"role": "assistant"}
	finishReason := "stop"
	if completion.Tool != nil {
		arguments, err := json.Marshal(completion.Tool.Args)
		if err != nil {
			return nil, fmt.Errorf("encode tool arguments: %w", err)
		}
		message["content"] = nil
		message["tool_calls"] = []any{map[string]any{
			"id":   completion.Tool.ID,
			"type": "function",
			"function": map[string]any{
				"name":      completion.Tool.Name,
				"arguments": string(arguments),
			},
		}}
		finishReason = "tool_calls"
	} else {
		message["content"] = completion.Text
	}
	return json.Marshal(map[string]any{
		"id":     "binary-e2e-completion",
		"object": "chat.completion",
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
		}},
	})
}

func (provider *binaryMockProvider) userRequests() []binaryProviderRequest {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	requests := make([]binaryProviderRequest, 0, len(provider.requests))
	for _, request := range provider.requests {
		if !request.Auxiliary {
			requests = append(requests, request)
		}
	}
	return requests
}

func newBinaryFixture(t *testing.T, baseURL, sessionPath string) binaryFixture {
	t.Helper()
	home := t.TempDir()
	workdir := t.TempDir()
	configDir := filepath.Join(home, ".go-cli")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatalf("create isolated CLI home: %v", err)
	}
	configText := fmt.Sprintf("provider:\n  name: openai\n  api_key: binary-e2e-dummy\n  base_url: %s\n  model: binary-e2e-model\nmodel:\n  name: binary-e2e-model\ntracing:\n  enabled: false\n  exporter: none\nsandbox:\n  allowed_paths:\n    - %s\n",
		strconv.Quote(baseURL), strconv.Quote(workdir))
	if sessionPath != "" {
		configText += "session:\n  store_path: " + strconv.Quote(sessionPath) + "\n"
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(configText), 0o600); err != nil {
		t.Fatalf("write isolated CLI config: %v", err)
	}
	return binaryFixture{home: home, workdir: workdir, sessionPath: sessionPath}
}

func runBinaryPrompt(t *testing.T, binaryPath string, fixture binaryFixture, args ...string) binaryRunResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	argv := append([]string{"prompt"}, args...)
	cmd := exec.CommandContext(ctx, binaryPath, argv...)
	cmd.Dir = fixture.workdir
	cmd.Env = binaryE2EEnvironment(fixture.home)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return binaryRunResult{stdout: stdout.String(), stderr: stderr.String(), err: err}
}

func binaryE2EEnvironment(home string) []string {
	path := os.Getenv("PATH")
	if path == "" {
		path = "/usr/bin:/bin"
	}
	env := []string{"HOME=" + home, "PATH=" + path, "TMPDIR=" + os.TempDir(), "LANG=C"}
	if systemRoot := os.Getenv("SystemRoot"); systemRoot != "" {
		env = append(env, "SystemRoot="+systemRoot)
	}
	return env
}

func successfulBinaryRun(t *testing.T, result binaryRunResult) {
	t.Helper()
	if result.err != nil {
		t.Fatalf("CLI failed: %v\nstdout:\n%s\nstderr:\n%s", result.err, result.stdout, result.stderr)
	}
	if strings.Contains(result.stderr, "DATA RACE") {
		t.Fatalf("race-instrumented CLI reported a data race:\nstderr:\n%s", result.stderr)
	}
}

func requireBinaryToolResult(t *testing.T, requests []binaryProviderRequest, callID string) string {
	t.Helper()
	if len(requests) < 2 {
		t.Fatalf("mock provider received %d user requests, want tool call followed by response", len(requests))
	}
	messages, ok := requests[1].Body["messages"].([]any)
	if !ok {
		t.Fatalf("second provider request has no messages array: %#v", requests[1].Body["messages"])
	}
	for _, value := range messages {
		message, ok := value.(map[string]any)
		if !ok || message["role"] != "tool" || message["tool_call_id"] != callID {
			continue
		}
		content, ok := message["content"].(string)
		if !ok {
			t.Fatalf("tool result content for %q is not text: %#v", callID, message["content"])
		}
		if content == "" {
			t.Fatalf("tool result content for %q is empty", callID)
		}
		return content
	}
	t.Fatalf("second provider request omitted tool result for %q: %s", callID, requests[1].Raw)
	return ""
}

func binaryToolArgs(action, target string) map[string]any {
	switch action {
	case "read":
		return map[string]any{"path": filepath.Base(target)}
	case "write":
		return map[string]any{"path": filepath.Base(target), "content": "written-by-cli"}
	case "edit":
		return map[string]any{"file_path": filepath.Base(target), "old_string": "before", "new_string": "after"}
	case "bash":
		return map[string]any{"command": "printf 'BASH_OUTPUT' > " + shellQuote(filepath.Base(target)) + "; printf 'BASH_OUTPUT'"}
	default:
		panic("unknown binary E2E action: " + action)
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func prepareBinaryToolAction(t *testing.T, fixture binaryFixture, action string) (string, string) {
	t.Helper()
	name := action + "-target.txt"
	path := filepath.Join(fixture.workdir, name)
	switch action {
	case "read":
		if err := os.WriteFile(path, []byte("READ_CONTENT"), 0o600); err != nil {
			t.Fatalf("write read fixture: %v", err)
		}
		return path, "READ_CONTENT"
	case "write":
		return path, "written-by-cli"
	case "edit":
		if err := os.WriteFile(path, []byte("before"), 0o600); err != nil {
			t.Fatalf("write edit fixture: %v", err)
		}
		return path, "after"
	case "bash":
		return path, "BASH_OUTPUT"
	default:
		panic("unknown binary E2E action: " + action)
	}
}

func assertBinaryToolSideEffect(t *testing.T, path, expected string, action string, allowed bool) {
	t.Helper()
	contents, err := os.ReadFile(path)
	if allowed {
		if err != nil {
			t.Fatalf("read %s side effect: %v", action, err)
		}
		if string(contents) != expected {
			t.Fatalf("%s side effect = %q, want %q", action, contents, expected)
		}
		return
	}
	if action == "edit" {
		if err != nil || string(contents) != "before" {
			t.Fatalf("denied edit changed fixture: content=%q err=%v", contents, err)
		}
		return
	}
	if action == "read" {
		if err != nil || string(contents) != "READ_CONTENT" {
			t.Fatalf("denied read changed fixture: content=%q err=%v", contents, err)
		}
		return
	}
	if !os.IsNotExist(err) {
		t.Fatalf("denied %s unexpectedly left a side effect: content=%q err=%v", action, contents, err)
	}
}

func TestRealBinaryPromptToolApprovalMatrixAndErrorRecovery(t *testing.T) {
	binaryPath := buildCLIForBinaryE2E(t)
	actions := []string{"read", "write", "edit"}
	if runtime.GOOS != "windows" {
		actions = append(actions, "bash")
	}
	for _, action := range actions {
		for _, mode := range []struct {
			name    string
			flag    string
			allowed bool
		}{
			{name: "auto", flag: "auto", allowed: true},
			{name: "deny", flag: "deny"},
		} {
			t.Run(action+"/"+mode.name, func(t *testing.T) {
				callID := "call-" + action + "-" + mode.name
				server, provider := newBinaryMockProvider(t,
					binaryCompletion{Tool: &binaryToolCall{ID: callID, Name: action, Args: map[string]any{}}},
					binaryCompletion{Text: "tool turn complete"},
				)
				fixture := newBinaryFixture(t, server.URL, "")
				path, expected := prepareBinaryToolAction(t, fixture, action)
				provider.mu.Lock()
				provider.steps[0].Tool.Args = binaryToolArgs(action, path)
				provider.mu.Unlock()

				args := []string{"--approve", mode.flag, "--output", "json"}
				if action == "bash" {
					args = append(args, "--no-sandbox")
				}
				args = append(args, "Use the "+action+" tool for this request")
				result := runBinaryPrompt(t, binaryPath, fixture, args...)
				successfulBinaryRun(t, result)

				requests := provider.userRequests()
				toolResult := requireBinaryToolResult(t, requests, callID)
				if action == "read" && mode.allowed && !strings.Contains(toolResult, "READ_CONTENT") {
					t.Fatalf("read tool result omitted file contents: %q", toolResult)
				}
				if action == "bash" && mode.allowed && !strings.Contains(toolResult, "BASH_OUTPUT") {
					t.Fatalf("bash tool result omitted command output: %q", toolResult)
				}
				assertBinaryToolSideEffect(t, path, expected, action, mode.allowed)
			})
		}
	}

	t.Run("default ask keeps read available and denies a non-TTY write", func(t *testing.T) {
		callID := "call-default-read"
		server, provider := newBinaryMockProvider(t,
			binaryCompletion{Tool: &binaryToolCall{ID: callID, Name: "read", Args: map[string]any{"path": "read-target.txt"}}},
			binaryCompletion{Text: "read complete"},
		)
		fixture := newBinaryFixture(t, server.URL, "")
		readPath, _ := prepareBinaryToolAction(t, fixture, "read")
		readResult := runBinaryPrompt(t, binaryPath, fixture, "--output", "json", "read the fixture")
		successfulBinaryRun(t, readResult)
		if result := requireBinaryToolResult(t, provider.userRequests(), callID); !strings.Contains(result, "READ_CONTENT") {
			logData, _ := os.ReadFile(filepath.Join(fixture.workdir, ".go-cli", "logs", "go-cli.log"))
			var approvalLog []string
			for _, line := range strings.Split(string(logData), "\n") {
				if strings.Contains(line, "approval.") {
					approvalLog = append(approvalLog, line)
				}
			}
			t.Fatalf("default read result omitted fixture contents: %q\napproval log:\n%s", result, strings.Join(approvalLog, "\n"))
		}
		assertBinaryToolSideEffect(t, readPath, "READ_CONTENT", "read", true)

		writeID := "call-default-write"
		writeServer, writeProvider := newBinaryMockProvider(t,
			binaryCompletion{Tool: &binaryToolCall{ID: writeID, Name: "write", Args: map[string]any{"path": "write-target.txt", "content": "must-not-write"}}},
			binaryCompletion{Text: "write was not approved"},
		)
		writeFixture := newBinaryFixture(t, writeServer.URL, "")
		writePath := filepath.Join(writeFixture.workdir, "write-target.txt")
		writeResult := runBinaryPrompt(t, binaryPath, writeFixture, "--output", "json", "write the fixture")
		successfulBinaryRun(t, writeResult)
		_ = requireBinaryToolResult(t, writeProvider.userRequests(), writeID)
		assertBinaryToolSideEffect(t, writePath, "must-not-write", "write", false)
	})

	t.Run("failed edit is returned to the model and a corrected edit runs", func(t *testing.T) {
		server, provider := newBinaryMockProvider(t,
			binaryCompletion{Tool: &binaryToolCall{ID: "edit-bad", Name: "edit", Args: map[string]any{
				"file_path": "recover.txt", "old_string": "missing", "new_string": "wrong",
			}}},
			binaryCompletion{Tool: &binaryToolCall{ID: "edit-good", Name: "edit", Args: map[string]any{
				"file_path": "recover.txt", "old_string": "before", "new_string": "recovered",
			}}},
			binaryCompletion{Text: "recovered after tool error"},
		)
		fixture := newBinaryFixture(t, server.URL, "")
		path := filepath.Join(fixture.workdir, "recover.txt")
		if err := os.WriteFile(path, []byte("before"), 0o600); err != nil {
			t.Fatalf("write recovery fixture: %v", err)
		}
		result := runBinaryPrompt(t, binaryPath, fixture, "--approve", "auto", "--output", "json", "repair the fixture after a failed edit")
		successfulBinaryRun(t, result)
		requests := provider.userRequests()
		if len(requests) != 3 {
			t.Fatalf("mock provider received %d user requests, want failed edit, corrected edit, and final response", len(requests))
		}
		failed := requireBinaryToolResult(t, requests[:2], "edit-bad")
		if !strings.Contains(failed, "not found") {
			t.Fatalf("failed edit result did not report its recoverable mismatch: %q", failed)
		}
		if !strings.Contains(requests[2].Raw, "edit-bad") || !strings.Contains(requests[2].Raw, "not found") {
			t.Fatalf("model's correction request omitted the failed edit result: %s", requests[2].Raw)
		}
		finalEditResult := findBinaryToolResult(t, requests[2], "edit-good")
		if finalEditResult == "" {
			t.Fatal("model did not receive a result for the corrected edit")
		}
		contents, err := os.ReadFile(path)
		if err != nil || string(contents) != "recovered" {
			t.Fatalf("recovered edit left %q (err=%v), want %q", contents, err, "recovered")
		}
	})
}

func findBinaryToolResult(t *testing.T, request binaryProviderRequest, callID string) string {
	t.Helper()
	messages, ok := request.Body["messages"].([]any)
	if !ok {
		t.Fatalf("provider request has no messages array: %#v", request.Body["messages"])
	}
	for _, value := range messages {
		message, ok := value.(map[string]any)
		if !ok || message["role"] != "tool" || message["tool_call_id"] != callID {
			continue
		}
		content, _ := message["content"].(string)
		return content
	}
	return ""
}

func TestRealBinaryPromptResumesDurableSession(t *testing.T) {
	binaryPath := buildCLIForBinaryE2E(t)
	server, provider := newBinaryMockProvider(t,
		binaryCompletion{Text: "first binary reply"},
		binaryCompletion{Text: "second binary reply"},
	)
	sessionPath := filepath.Join(t.TempDir(), "session-store")
	fixture := newBinaryFixture(t, server.URL, sessionPath)

	first := runBinaryPrompt(t, binaryPath, fixture, "--output", "text", "first binary request")
	successfulBinaryRun(t, first)
	physicalID := binarySessionID(t, first.stdout)
	if _, err := os.Stat(filepath.Join(sessionPath, "sessions", physicalID+".jsonl")); err != nil {
		t.Fatalf("first invocation did not persist the durable session: %v", err)
	}

	second := runBinaryPrompt(t, binaryPath, fixture, "--output", "text", "--resume-session", physicalID, "second binary request")
	successfulBinaryRun(t, second)
	if got := binarySessionID(t, second.stdout); got != physicalID {
		t.Fatalf("resumed invocation session ID = %q, want %q", got, physicalID)
	}
	requests := provider.userRequests()
	if len(requests) != 2 {
		t.Fatalf("mock provider received %d user requests, want 2", len(requests))
	}
	for _, expected := range []string{"first binary request", "first binary reply", "second binary request"} {
		if !strings.Contains(requests[1].Raw, expected) {
			t.Fatalf("resumed request omitted %q: %s", expected, requests[1].Raw)
		}
	}
}

func binarySessionID(t *testing.T, output string) string {
	t.Helper()
	const prefix = "Session ID: "
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, prefix) {
			id := strings.TrimSpace(strings.TrimPrefix(line, prefix))
			if id != "" {
				return id
			}
		}
	}
	t.Fatalf("CLI output does not contain a durable session ID: %q", output)
	return ""
}
