//go:build e2e && (darwin || linux)

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
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	longTaskTurns       = 120
	longTaskAPIKey      = "e2e-longtask-fixture-key"
	longTaskModel       = "e2e-longtask-fixture"
	longTaskStartMarker = "LONGTASK_STREAM_MARKER"
	// readLineTTY creates its input owner and sets raw mode before enabling
	// bracketed paste and printing this prompt. Editor redraws and the TUI's
	// follow-up prompt are not acknowledgments of the next REPL input owner.
	longTaskREPLPromptMarker = "\x1b[?2004h> "
)

func TestLongTaskREPLPromptMarkerExcludesRenderedPrompts(t *testing.T) {
	firstPrompt := "\x1b[?2004h> "
	renderedTurn := firstPrompt + "\r\x1b[J> p\r\x1b[J> please run\r\n\x1b[?2004l" +
		"\x1b[?25l\x1b[?2004h\r\x1b[K\r\n[Assistant response is being generated and checked]" +
		"\n> followup> queued follow-up\nAI: LONGTASK_FOLLOWUP_COMPLETE\n" +
		"\x1b[?2004l\x1b[?25h\x1b[?1002l\x1b[?1003l\x1b[?1006l"
	for _, testCase := range []struct {
		name   string
		output string
		want   int
	}{
		{name: "first reader ready", output: firstPrompt, want: 1},
		{name: "redraw follow-up and completed answer do not acknowledge handoff", output: renderedTurn, want: 1},
		{name: "partial next prompt is not ready", output: renderedTurn + "\x1b[?2004h>", want: 1},
		{name: "next reader ready", output: renderedTurn + firstPrompt, want: 2},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			require.Equal(t, testCase.want, strings.Count(testCase.output, longTaskREPLPromptMarker))
		})
	}
}

func TestLongTaskREPLPromptWaitJoinsSplitMarker(t *testing.T) {
	reader, writer := io.Pipe()
	capture := newLongTaskPTYCapture(reader)
	t.Cleanup(func() {
		_ = writer.Close()
		_ = reader.Close()
		<-capture.done
	})
	written := make(chan error, 1)
	go func() {
		for _, chunk := range []string{"\x1b[?20", "04h> "} {
			if _, err := io.WriteString(writer, chunk); err != nil {
				written <- err
				return
			}
		}
		written <- nil
	}()
	capture.waitREPLPrompt(t, 1, time.Second)
	require.NoError(t, <-written)
}

// TestE2EBinaryLongTask runs the real, race-instrumented CLI binary against a
// local OpenAI-compatible HTTP peer. The first subtest covers a 120-turn
// interactive process and real compaction; the PTY subtests drive the actual
// terminal UI while Bash is running, including follow-up and cancellation.
func TestE2EBinaryLongTask(t *testing.T) {
	binary := buildLongTaskRaceBinary(t)
	t.Run("120 real binary turns compact streamed history", func(t *testing.T) {
		testLongTaskBinary120Turns(t, binary)
	})
	t.Run("PTY follow-up preserves Bash side effect", func(t *testing.T) {
		testLongTaskPTYFollowUp(t, binary)
	})
	t.Run("PTY Escape cancels long Bash before delayed effect", func(t *testing.T) {
		testLongTaskPTYCancel(t, binary)
	})
}

func buildLongTaskRaceBinary(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("..")
	require.NoError(t, err)
	binary := filepath.Join(t.TempDir(), "go-cli-race")
	command := exec.Command("go", "build", "-race", "-o", binary, "./cmd/cli")
	command.Dir = root
	output, err := command.CombinedOutput()
	require.NoError(t, err, "build race-instrumented CLI: %s", output)
	t.Logf("built real CLI with `go build -race -o %s ./cmd/cli`", binary)
	return binary
}

func testLongTaskBinary120Turns(t *testing.T, binary string) {
	t.Helper()
	peer := newLongTaskHTTPPeer(func(index int, _ []byte) longTaskReply {
		return longTaskReply{content: fmt.Sprintf("reply-%03d %s", index+1, strings.Repeat(fmt.Sprintf("history-%03d ", index+1), 48))}
	})
	fixture := newLongTaskFixture(t, peer, true)
	defer fixture.server.Close()
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		lines := strings.Split(fixture.readCLIlog(t), "\n")
		if len(lines) > 80 {
			lines = lines[len(lines)-80:]
		}
		t.Logf("failed 120-turn CLI log tail:\n%s", strings.Join(lines, "\n"))
	})

	var input strings.Builder
	for turn := 1; turn <= longTaskTurns; turn++ {
		fmt.Fprintf(&input, "turn-%03d-input carry this work forward\n", turn)
	}
	input.WriteString("exit\n")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, binary,
		"interactive", "--provider=openai", "--model="+longTaskModel,
		"--max-tokens=4096", "--thinking=none", "--no-onboarding")
	command.Dir = fixture.workDir
	command.Env = fixture.childEnv
	command.Stdin = strings.NewReader(input.String())
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	require.NoError(t, err, "real interactive binary: stdout=%q stderr=%q", stdout.String(), stderr.String())
	require.NoError(t, ctx.Err(), "the 120-turn process must finish before its timeout")
	require.NotContains(t, stderr.String(), "DATA RACE", "the nested race-enabled CLI must not report a race")
	require.Contains(t, stdout.String(), "Session ended.")
	require.Contains(t, stdout.String(), fmt.Sprintf("reply-%03d", longTaskTurns), "CLI stderr: %s", stderr.String())

	bodies := peer.requestBodies()
	require.Len(t, bodies, longTaskTurns, "each user submission should make one actual provider request")
	for index, body := range bodies {
		request, err := decodeLongTaskRequest(body)
		require.NoError(t, err, "decode provider request %d", index+1)
		require.True(t, request.Stream, "request %d must exercise HTTP streaming", index+1)
		currentPrompt := fmt.Sprintf("turn-%03d-input", index+1)
		require.True(t, requestHasRoleContent(request, "user", currentPrompt), "provider request %d must contain its current user submission", index+1)
	}

	lastRequest, err := decodeLongTaskRequest(bodies[len(bodies)-1])
	require.NoError(t, err)
	require.True(t, requestHasRoleContent(lastRequest, "user", fmt.Sprintf("turn-%03d-input", longTaskTurns)))
	require.False(t, requestHasRoleContent(lastRequest, "user", "turn-001-input"), "the real provider context should no longer carry the oldest turn after compaction")
	require.Contains(t, fixture.readCLIlog(t), "compaction.truncating.done", "the production compactor must emit runtime evidence")

	t.Logf("evidence: real CLI PID completed %d user turns; actual HTTP requests=%d; last request message_count=%d; first turn omitted=%t; compaction log present=true; binary race-enabled=true",
		longTaskTurns, len(bodies), len(lastRequest.Messages), !requestHasRoleContent(lastRequest, "user", "turn-001-input"))
}

func testLongTaskPTYFollowUp(t *testing.T, binary string) {
	t.Helper()
	const followUp = "FOLLOW-UP-CANARY include the shell result in your completion"
	const startedFile = "followup-started.txt"
	const releaseFile = "followup-release.txt"
	const effectFile = "followup-proof.txt"
	const shellPIDFile = "followup-shell.pid"
	shellToken := fmt.Sprintf("GO_CLI_LONGTASK_%x", time.Now().UnixNano())
	commandText := ": '" + shellToken + "'; printf '%s\\n' \"$$\" > " + shellPIDFile + "; printf '" + longTaskStartMarker + "\\n'; printf started > " + startedFile + "; while [ ! -f " + releaseFile + " ]; do sleep 0.02; done; printf 'FOLLOWUP_SIDE_EFFECT' > " + effectFile + "; cat " + effectFile
	peer := newLongTaskHTTPPeer(func(index int, _ []byte) longTaskReply {
		if index == 0 {
			return longTaskReply{toolCallID: "call-longtask-followup", toolName: "bash", toolArgs: map[string]string{"command": commandText}}
		}
		return longTaskReply{content: "LONGTASK_FOLLOWUP_COMPLETE"}
	})
	fixture := newLongTaskFixture(t, peer, true)
	defer fixture.server.Close()

	pty := startLongTaskPTYChild(t, binary, fixture, "--max-tokens=4096")
	t.Cleanup(func() { cleanupLongTaskBashGroup(t, fixture.workDir, shellPIDFile, shellToken) })
	pty.capture.waitREPLPrompt(t, 1, 15*time.Second)
	pty.write(t, "please run the long task and report the result\r")
	pty.capture.waitContains(t, "[approval]", 20*time.Second)
	pty.write(t, "y")
	require.Eventually(t, func() bool {
		_, pidErr := os.Stat(filepath.Join(fixture.workDir, shellPIDFile))
		_, startedErr := os.Stat(filepath.Join(fixture.workDir, startedFile))
		return pidErr == nil && startedErr == nil
	}, 20*time.Second, 10*time.Millisecond, "the approved Bash command must create its PID and start markers")
	// The command text is visible in the approval view. The PID and start files
	// prove Bash actually began, without treating a rendered command preview as
	// streamed tool output.
	require.NoFileExists(t, filepath.Join(fixture.workDir, releaseFile), "Bash must remain blocked until the test releases it")
	require.NoFileExists(t, filepath.Join(fixture.workDir, effectFile), "Bash must not finish before the follow-up is queued")
	// Submit follow-up through Bubble Tea while the approved Bash subprocess is
	// blocked on the test-controlled release file. The next provider request must
	// contain this actual user message along with the completed Bash result.
	pty.write(t, "f")
	pty.capture.waitContains(t, "> followup>", 10*time.Second)
	pty.write(t, followUp)
	pty.capture.waitContains(t, followUp, 10*time.Second)
	pty.write(t, "\r")
	require.NoFileExists(t, filepath.Join(fixture.workDir, effectFile), "the shell must still be blocked after the follow-up is entered")
	// View visibility only proves that the text reached the editor. Wait until
	// the production callback logs after FollowUp accepted it into the running
	// turn's queue before allowing Bash to complete.
	require.Eventually(t, func() bool {
		log, err := os.ReadFile(filepath.Join(fixture.workDir, ".go-cli", "logs", "go-cli.log"))
		return err == nil && strings.Contains(string(log), "core.turn.runner.followup.sent")
	}, 10*time.Second, 10*time.Millisecond, "the production follow-up callback must confirm queue acceptance")
	require.NoError(t, os.WriteFile(filepath.Join(fixture.workDir, releaseFile), []byte("release"), 0o600))
	requests := peer.waitForRequestCount(t, 2, 30*time.Second)
	require.NoError(t, pty.capture.readError())
	second, err := decodeLongTaskRequest(requests[1])
	require.NoError(t, err)
	require.True(t, requestHasRoleContent(second, "user", followUp), "the actual second HTTP request must contain the PTY follow-up")
	require.True(t, requestHasRoleContent(second, "tool", "FOLLOWUP_SIDE_EFFECT"), "the actual provider context must contain the tool's stdout result")

	proof, err := os.ReadFile(filepath.Join(fixture.workDir, effectFile))
	require.NoError(t, err, "Bash must create its real side-effect file")
	require.Equal(t, "FOLLOWUP_SIDE_EFFECT", string(proof))
	pty.capture.waitContains(t, "LONGTASK_FOLLOWUP_COMPLETE", 20*time.Second)
	pty.capture.waitREPLPrompt(t, 2, 10*time.Second)
	pty.write(t, "exit\r")
	pty.waitSuccess(t, 20*time.Second)
	require.NotContains(t, pty.capture.output(), "DATA RACE")

	t.Logf("evidence: PTY drove follow-up while test-owned Bash PID/start markers proved execution; production queue acknowledgment preceded release; actual HTTP request 2 contained user follow-up=%q and role=tool stdout=%q; side_effect=%s:%q; final streamed answer visible; race-enabled child clean",
		followUp, "FOLLOWUP_SIDE_EFFECT", effectFile, string(proof))
}

func testLongTaskPTYCancel(t *testing.T, binary string) {
	t.Helper()
	const startedFile = "cancel-started.txt"
	const effectFile = "cancelled-proof.txt"
	const shellPIDFile = "cancel-shell.pid"
	shellToken := fmt.Sprintf("GO_CLI_LONGTASK_%x", time.Now().UnixNano())
	commandText := ": '" + shellToken + "'; printf '%s\\n' \"$$\" > " + shellPIDFile + "; printf 'CANCEL_LONGTASK_STARTED\\n'; printf started > " + startedFile + "; sleep 5; printf 'SHOULD_NOT_RUN' > " + effectFile
	peer := newLongTaskHTTPPeer(func(index int, _ []byte) longTaskReply {
		if index == 0 {
			return longTaskReply{toolCallID: "call-longtask-cancel", toolName: "bash", toolArgs: map[string]string{"command": commandText}}
		}
		return longTaskReply{content: "CANCEL_FLOW_CONTINUED"}
	})
	fixture := newLongTaskFixture(t, peer, true)
	defer fixture.server.Close()

	pty := startLongTaskPTYChild(t, binary, fixture, "--max-tokens=4096")
	t.Cleanup(func() { cleanupLongTaskBashGroup(t, fixture.workDir, shellPIDFile, shellToken) })
	pty.capture.waitREPLPrompt(t, 1, 15*time.Second)
	pty.write(t, "run the long task and wait for it\r")
	pty.capture.waitContains(t, "[approval]", 20*time.Second)
	pty.write(t, "y")
	require.Eventually(t, func() bool {
		_, pidErr := os.Stat(filepath.Join(fixture.workDir, shellPIDFile))
		_, startedErr := os.Stat(filepath.Join(fixture.workDir, startedFile))
		return pidErr == nil && startedErr == nil
	}, 20*time.Second, 10*time.Millisecond, "the approved Bash command must create its PID and start markers")
	cancelAt := time.Now()
	pty.writeBytes(t, []byte{0x1b})
	pty.capture.waitContains(t, "[interrupted]", 3*time.Second)
	pty.capture.waitREPLPrompt(t, 2, 10*time.Second)
	cancelLatency := time.Since(cancelAt)
	require.Less(t, cancelLatency, 4*time.Second, "Esc cancellation should return before the delayed shell effect")

	// Wait past the shell's full delay to catch a detached child that outlives
	// the visible cancellation result.
	time.Sleep(5 * time.Second)
	_, statErr := os.Stat(filepath.Join(fixture.workDir, effectFile))
	require.ErrorIs(t, statErr, os.ErrNotExist, "cancel must prevent the delayed Bash side effect")
	require.Len(t, peer.requestBodies(), 1, "cancellation must not ask the model to continue the cancelled task")
	pty.write(t, "start a fresh turn after cancelling\r")
	pty.capture.waitContains(t, "CANCEL_FLOW_CONTINUED", 20*time.Second)
	pty.capture.waitREPLPrompt(t, 3, 10*time.Second)
	bodies := peer.requestBodies()
	require.Len(t, bodies, 2, "the prompt after cancellation should start a new provider turn")
	continuedRequest, err := decodeLongTaskRequest(bodies[1])
	require.NoError(t, err)
	require.True(t, requestHasRoleContent(continuedRequest, "user", "start a fresh turn after cancelling"), "the explicit next prompt must be sent as a new user turn")
	pty.write(t, "exit\r")
	pty.waitSuccess(t, 20*time.Second)
	require.NotContains(t, pty.capture.output(), "DATA RACE")

	t.Logf("evidence: Escape returned [interrupted] in %s; delayed side-effect file remained absent after 5s; no automatic follow-up request was sent; a later explicit prompt completed as a fresh turn; race-enabled child clean",
		cancelLatency.Round(time.Millisecond))
}

func TestCleanupLongTaskBashGroupVerifiesFixtureIdentity(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		wrongToken bool
	}{
		{name: "matching token kills test-owned group"},
		{name: "wrong token leaves group running", wrongToken: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			workDir := t.TempDir()
			const pidFile = "owned-bash.pid"
			token := fmt.Sprintf("GO_CLI_LONGTASK_CLEANUP_%x", time.Now().UnixNano())
			commandText := ": '" + token + "'; printf '%s\\n' \"$$\" > " + pidFile + "; while :; do sleep 1; done"
			command := exec.Command("bash", "-lc", commandText)
			command.Dir = workDir
			command.Stdout = io.Discard
			command.Stderr = io.Discard
			command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			require.NoError(t, command.Start())
			pid := command.Process.Pid
			waitDone := make(chan struct{})
			go func() {
				_ = command.Wait()
				close(waitDone)
			}()
			t.Cleanup(func() {
				select {
				case <-waitDone:
					return
				default:
				}
				if groupID, err := syscall.Getpgid(pid); err == nil && groupID == pid {
					_ = syscall.Kill(-pid, syscall.SIGKILL)
				}
				select {
				case <-waitDone:
				case <-time.After(2 * time.Second):
					t.Errorf("test Bash child %d did not exit during cleanup", pid)
				}
			})
			require.Eventually(t, func() bool {
				data, err := os.ReadFile(filepath.Join(workDir, pidFile))
				if err != nil {
					return false
				}
				markedPID, err := strconv.Atoi(strings.TrimSpace(string(data)))
				return err == nil && markedPID == pid
			}, 3*time.Second, 10*time.Millisecond, "fixture Bash must publish its own PID")
			require.Equal(t, pid, func() int {
				groupID, err := syscall.Getpgid(pid)
				require.NoError(t, err)
				return groupID
			}(), "fixture Bash must remain its own process-group leader")

			cleanupToken := token
			if testCase.wrongToken {
				cleanupToken += "_mismatch"
			}
			cleanupLongTaskBashGroup(t, workDir, pidFile, cleanupToken)
			if testCase.wrongToken {
				select {
				case <-waitDone:
					t.Fatal("Bash fixture exited after a mismatched cleanup token")
				case <-time.After(100 * time.Millisecond):
				}
				groupID, err := syscall.Getpgid(pid)
				require.NoError(t, err, "wrong fixture token must not signal the process group")
				require.Equal(t, pid, groupID)
				cleanupLongTaskBashGroup(t, workDir, pidFile, token)
			}
			require.Eventually(t, func() bool {
				return syscall.Kill(-pid, 0) != nil
			}, 2*time.Second, 10*time.Millisecond, "the matching fixture token must stop its owned process group")
			select {
			case <-waitDone:
			case <-time.After(2 * time.Second):
				t.Fatalf("test Bash child %d did not exit after process-group cleanup", pid)
			}
		})
	}
}

type longTaskFixture struct {
	server   *httptest.Server
	workDir  string
	childEnv []string
}

func newLongTaskFixture(t *testing.T, peer *longTaskHTTPPeer, exposeBash bool) *longTaskFixture {
	t.Helper()
	server := httptest.NewServer(peer)
	root := t.TempDir()
	fixture := &longTaskFixture{
		server:  server,
		workDir: filepath.Join(root, "work"),
	}
	home := filepath.Join(root, "home")
	configDir := filepath.Join(root, "xdg-config")
	cacheDir := filepath.Join(root, "xdg-cache")
	tmpDir := filepath.Join(root, "tmp")
	for _, dir := range []string{fixture.workDir, home, configDir, cacheDir, tmpDir} {
		require.NoError(t, os.MkdirAll(dir, 0o700))
	}
	toolsYAML := "  builtin:\n    - bash\n"
	if !exposeBash {
		toolsYAML = "  builtin:\n    - bash\n"
	}
	configYAML := fmt.Sprintf(
		"provider:\n  name: openai\n  base_url: %q\n  api_key: %q\n  model: %q\nmodel:\n  name: %q\ntools:\n%sapproval:\n  mode: ask\ncompaction:\n  strategy: truncating\n  max_tokens: 4096\ntracing:\n  enabled: false\nmemory:\n  enabled: false\n",
		server.URL+"/v1", longTaskAPIKey, longTaskModel, longTaskModel, toolsYAML,
	)
	configPath := filepath.Join(home, ".go-cli", "config.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(configPath), 0o700))
	require.NoError(t, os.WriteFile(configPath, []byte(configYAML), 0o600))
	fixture.childEnv = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + configDir,
		"XDG_CACHE_HOME=" + cacheDir,
		"TMPDIR=" + tmpDir,
		"GO_CLI_NO_ONBOARDING=1",
		"NO_COLOR=1",
		"TERM=xterm-256color",
		"TZ=UTC",
		"NO_PROXY=127.0.0.1,localhost",
		"no_proxy=127.0.0.1,localhost",
		"HTTP_PROXY=",
		"HTTPS_PROXY=",
		"ALL_PROXY=",
		"http_proxy=",
		"https_proxy=",
		"all_proxy=",
		"OPENAI_API_KEY=",
		"ANTHROPIC_API_KEY=",
		"GOOGLE_API_KEY=",
		"GEMINI_API_KEY=",
	}
	if runtime.GOOS == "windows" {
		fixture.childEnv = append(fixture.childEnv, "SYSTEMROOT="+os.Getenv("SYSTEMROOT"))
	}
	return fixture
}

func (fixture *longTaskFixture) readCLIlog(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixture.workDir, ".go-cli", "logs", "go-cli.log"))
	require.NoError(t, err, "read isolated child CLI log")
	return string(data)
}

type longTaskReply struct {
	content    string
	toolCallID string
	toolName   string
	toolArgs   map[string]string
}

type longTaskHTTPPeer struct {
	mu       sync.Mutex
	requests [][]byte
	plan     func(index int, request []byte) longTaskReply
	notify   chan struct{}
}

func newLongTaskHTTPPeer(plan func(index int, request []byte) longTaskReply) *longTaskHTTPPeer {
	return &longTaskHTTPPeer{plan: plan, notify: make(chan struct{}, 1)}
}

func (peer *longTaskHTTPPeer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/chat/completions" {
		http.Error(w, "unexpected HTTP path", http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "cannot read request body", http.StatusBadRequest)
		return
	}
	_ = r.Body.Close()
	if _, err := decodeLongTaskRequest(body); err != nil {
		http.Error(w, "invalid chat completion request", http.StatusBadRequest)
		return
	}
	peer.mu.Lock()
	index := len(peer.requests)
	requestCopy := append([]byte(nil), body...)
	peer.requests = append(peer.requests, requestCopy)
	peer.mu.Unlock()
	select {
	case peer.notify <- struct{}{}:
	default:
	}
	peer.writeReply(w, index, requestCopy, peer.plan(index, requestCopy))
}

func (peer *longTaskHTTPPeer) writeReply(w http.ResponseWriter, index int, body []byte, reply longTaskReply) {
	request, err := decodeLongTaskRequest(body)
	if err != nil {
		http.Error(w, "invalid recorded request", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if request.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		var delta map[string]any
		finishReason := "stop"
		if reply.toolCallID != "" {
			args, marshalErr := json.Marshal(reply.toolArgs)
			if marshalErr != nil {
				http.Error(w, "cannot encode tool arguments", http.StatusInternalServerError)
				return
			}
			delta = map[string]any{"tool_calls": []any{map[string]any{
				"index":    0,
				"id":       reply.toolCallID,
				"type":     "function",
				"function": map[string]any{"name": reply.toolName, "arguments": string(args)},
			}}}
			finishReason = "tool_calls"
		} else {
			delta = map[string]any{"content": reply.content}
		}
		chunk, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{
			"index": 0, "delta": delta, "finish_reason": finishReason,
		}}})
		_, _ = fmt.Fprintf(w, "data: %s\n\n", chunk)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		return
	}

	message := map[string]any{"role": "assistant", "content": reply.content}
	finishReason := "stop"
	if reply.toolCallID != "" {
		args, _ := json.Marshal(reply.toolArgs)
		message["content"] = nil
		message["tool_calls"] = []any{map[string]any{
			"id":       reply.toolCallID,
			"type":     "function",
			"function": map[string]any{"name": reply.toolName, "arguments": string(args)},
		}}
		finishReason = "tool_calls"
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{
		"index": 0, "message": message, "finish_reason": finishReason,
	}}})
	_ = index
}

func (peer *longTaskHTTPPeer) requestBodies() [][]byte {
	peer.mu.Lock()
	defer peer.mu.Unlock()
	bodies := make([][]byte, len(peer.requests))
	for index := range peer.requests {
		bodies[index] = append([]byte(nil), peer.requests[index]...)
	}
	return bodies
}

func (peer *longTaskHTTPPeer) waitForRequestCount(t *testing.T, want int, timeout time.Duration) [][]byte {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		bodies := peer.requestBodies()
		if len(bodies) >= want {
			return bodies
		}
		select {
		case <-peer.notify:
		case <-timer.C:
			t.Fatalf("timed out waiting for %d actual provider requests; got %d", want, len(peer.requestBodies()))
		}
	}
}

type longTaskRequest struct {
	Stream   bool
	Messages []longTaskRequestMessage
}

type longTaskRequestMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

func decodeLongTaskRequest(body []byte) (longTaskRequest, error) {
	var raw struct {
		Stream   bool                     `json:"stream"`
		Messages []longTaskRequestMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return longTaskRequest{}, err
	}
	if len(raw.Messages) == 0 {
		return longTaskRequest{}, fmt.Errorf("chat request contains no messages")
	}
	return longTaskRequest{Stream: raw.Stream, Messages: raw.Messages}, nil
}

func requestHasRoleContent(request longTaskRequest, role, needle string) bool {
	for _, message := range request.Messages {
		if message.Role != role {
			continue
		}
		var content string
		if err := json.Unmarshal(message.Content, &content); err == nil && strings.Contains(content, needle) {
			return true
		}
		if bytes.Contains(message.Content, []byte(needle)) {
			return true
		}
	}
	return false
}

type longTaskPTYChild struct {
	command      *exec.Cmd
	master       io.Writer
	closer       io.Closer
	capture      *longTaskPTYCapture
	waitDone     chan struct{}
	waitErr      error
	processGroup int
}

func startLongTaskPTYChild(t *testing.T, binary string, fixture *longTaskFixture, maxTokens string) *longTaskPTYChild {
	t.Helper()
	master, slave, cleanupPTY := openLongTaskPTY(t)
	command := exec.Command(binary, "interactive", "--provider=openai", "--model="+longTaskModel,
		maxTokens, "--thinking=none", "--no-onboarding", "--no-sandbox")
	command.Dir = fixture.workDir
	command.Env = fixture.childEnv
	command.Stdin = slave
	command.Stdout = slave
	command.Stderr = slave
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	require.NoError(t, command.Start())
	require.NoError(t, slave.Close())
	capture := newLongTaskPTYCapture(master)
	child := &longTaskPTYChild{
		command:      command,
		master:       master,
		closer:       master,
		capture:      capture,
		waitDone:     make(chan struct{}),
		processGroup: command.Process.Pid,
	}
	go func() {
		child.waitErr = command.Wait()
		close(child.waitDone)
	}()
	t.Cleanup(func() {
		select {
		case <-child.waitDone:
		default:
			_ = syscall.Kill(-child.processGroup, syscall.SIGKILL)
			_ = command.Process.Kill()
			<-child.waitDone
		}
		_ = child.closer.Close()
		cleanupPTY()
	})
	return child
}

func (child *longTaskPTYChild) write(t *testing.T, text string) {
	t.Helper()
	child.writeBytes(t, []byte(text))
}

func (child *longTaskPTYChild) writeBytes(t *testing.T, data []byte) {
	t.Helper()
	_, err := child.master.Write(data)
	require.NoError(t, err, "write terminal input")
}

func (child *longTaskPTYChild) waitSuccess(t *testing.T, timeout time.Duration) {
	t.Helper()
	select {
	case <-child.waitDone:
		require.NoError(t, child.waitErr, "real CLI process output=%q", child.capture.output())
	case <-time.After(timeout):
		_ = syscall.Kill(-child.processGroup, syscall.SIGKILL)
		_ = child.command.Process.Kill()
		t.Fatalf("real CLI did not exit within %s; output=%q", timeout, child.capture.output())
	}
}

func cleanupLongTaskBashGroup(t *testing.T, workDir, pidFile, token string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(workDir, pidFile))
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 1 {
		return
	}
	groupID, err := syscall.Getpgid(pid)
	if err != nil || groupID != pid {
		return
	}
	command, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil || !strings.Contains(string(command), token) {
		return
	}
	// Recheck both identity and group ownership immediately before signaling so
	// a stale fixture PID cannot target a reused process group.
	groupID, err = syscall.Getpgid(pid)
	if err != nil || groupID != pid {
		return
	}
	command, err = exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil || !strings.Contains(string(command), token) {
		return
	}
	t.Logf("cleaning up still-running test-owned Bash process group %d", pid)
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}

type longTaskPTYCapture struct {
	mu         sync.Mutex
	outputText strings.Builder
	notify     chan struct{}
	done       chan struct{}
	err        error
}

func newLongTaskPTYCapture(reader io.Reader) *longTaskPTYCapture {
	capture := &longTaskPTYCapture{notify: make(chan struct{}, 1), done: make(chan struct{})}
	go func() {
		defer close(capture.done)
		buffer := make([]byte, 4096)
		for {
			n, err := reader.Read(buffer)
			if n > 0 {
				capture.mu.Lock()
				_, _ = capture.outputText.Write(buffer[:n])
				capture.mu.Unlock()
				select {
				case capture.notify <- struct{}{}:
				default:
				}
			}
			if err != nil {
				capture.mu.Lock()
				capture.err = err
				capture.mu.Unlock()
				return
			}
		}
	}()
	return capture
}

func (capture *longTaskPTYCapture) output() string {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	return capture.outputText.String()
}

func (capture *longTaskPTYCapture) readError() error {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	return capture.err
}

func (capture *longTaskPTYCapture) waitContains(t *testing.T, needle string, timeout time.Duration) string {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		output := capture.output()
		if strings.Contains(output, needle) {
			return output
		}
		select {
		case <-capture.notify:
		case <-capture.done:
			t.Fatalf("PTY closed before %q appeared; read error=%v output=%q", needle, capture.readError(), capture.output())
		case <-timer.C:
			t.Fatalf("timed out waiting for %q in PTY output=%q", needle, capture.output())
		}
	}
}

func (capture *longTaskPTYCapture) waitREPLPrompt(t *testing.T, want int, timeout time.Duration) string {
	t.Helper()
	needle := longTaskREPLPromptMarker
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		output := capture.output()
		if strings.Count(output, needle) >= want {
			return output
		}
		select {
		case <-capture.notify:
		case <-capture.done:
			t.Fatalf("PTY closed before %d occurrences of %q; read error=%v output=%q", want, needle, capture.readError(), capture.output())
		case <-timer.C:
			t.Fatalf("timed out waiting for %d occurrences of %q in PTY output=%q", want, needle, capture.output())
		}
	}
}
