package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/verify"
)

func TestDurableREPLProviderLocalTimeoutRefreshesAndContinues(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	configureDurableCLISessionTest(t)

	const firstPrompt = "trigger a provider-local timeout"
	const secondPrompt = "continue after the local timeout"
	const secondReply = "the next turn completed"
	var timeoutRequests atomic.Int32
	var followUpRequests atomic.Int32
	localTimeoutObserved := make(chan struct{}, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read provider request: %v", err)
			http.Error(w, "unable to read request", http.StatusBadRequest)
			return
		}
		var request struct {
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			t.Errorf("decode provider request: %v", err)
			http.Error(w, "unable to decode request", http.StatusBadRequest)
			return
		}
		lastUserMessage := ""
		for _, message := range request.Messages {
			if message.Role != "user" {
				continue
			}
			var content string
			if json.Unmarshal(message.Content, &content) == nil {
				lastUserMessage = content
			}
		}

		switch lastUserMessage {
		case firstPrompt:
			timeoutRequests.Add(1)
			select {
			case <-r.Context().Done():
				select {
				case localTimeoutObserved <- struct{}{}:
				default:
				}
				return
			case <-time.After(5 * time.Second):
				http.Error(w, "request was not canceled by the configured local timeout", http.StatusGatewayTimeout)
				return
			}
		case secondPrompt:
			followUpRequests.Add(1)
			w.Header().Set("Content-Type", "application/json")
			if _, err := io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"`+secondReply+`"},"finish_reason":"stop"}]}`); err != nil {
				t.Errorf("write follow-up response: %v", err)
			}
		default:
			t.Errorf("unexpected last user message in provider request: %q", lastUserMessage)
			http.Error(w, "unexpected request", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	storeDirectory := filepath.Join(t.TempDir(), "sessions")
	cfg := durablePromptConfig(server.URL, storeDirectory)
	cfg.Production.ModelTimeout.Total = "250ms"
	cfg.Production.ModelTimeout.StreamIdle = "150ms"

	lineEditor := &scriptedLineEditor{results: []lineReadResult{
		{line: firstPrompt},
		{line: secondPrompt},
		{line: exitCommand},
	}}
	var output bytes.Buffer
	cmd := newInteractiveCmd(nil, &output)
	cmd.lineEditor = lineEditor
	repl := &REPLSession{cmd: cmd, out: &output, lineEditor: lineEditor}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	require.NoError(t, repl.Run(ctx, cfg, []string{"--no-onboarding"}))
	assert.NoError(t, ctx.Err(), "the provider-local timeout must not cancel the parent REPL context")
	select {
	case <-localTimeoutObserved:
	default:
		t.Fatal("the provider request was not canceled by its local model timeout")
	}
	assert.EqualValues(t, 1, timeoutRequests.Load(), "the local timeout should be surfaced without replaying the provider request")
	assert.EqualValues(t, 1, followUpRequests.Load(), "the REPL must admit the next user turn after the durable error refresh ACK")
	assert.Empty(t, lineEditor.results, "the REPL should consume the follow-up turn and exit command")
	assert.NoError(t, repl.turnErr, "the final user turn should complete successfully")
	assert.Contains(t, strings.ToLower(output.String()), "error:", "the local provider failure must be reported to the user")
	assert.Contains(t, output.String(), "AI: "+secondReply, "a follow-up answer should be displayed after the error")

	physicalID := physicalSessionIDFromAssembly(t, repl.assembly)
	require.NotEmpty(t, physicalID)
	readback, err := newDurableCLISession(t.Context(), storeDirectory, "timeout-review", physicalID, "")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, readback.manager.requestClose(context.Background())) })
	snapshot, err := readback.snapshotSelected()
	require.NoError(t, err)
	requirePublishedMessages(t, snapshot, firstPrompt, secondPrompt, secondReply)

	entriesPath := filepath.Join(storeDirectory, "sessions", physicalID+".jsonl")
	contents, err := os.ReadFile(entriesPath)
	require.NoError(t, err)
	var terminalOutcomes []core.RunOutcome
	for _, line := range strings.Split(strings.TrimSpace(string(contents)), "\n") {
		var entry struct {
			Type           string          `json:"type"`
			CheckpointKind string          `json:"checkpoint_kind"`
			RunOutcome     core.RunOutcome `json:"run_outcome"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("decode durable session row: %v", err)
		}
		if entry.Type == "checkpoint" && entry.CheckpointKind == "run_terminal" {
			terminalOutcomes = append(terminalOutcomes, entry.RunOutcome)
		}
	}
	require.Equal(t, []core.RunOutcome{core.RunOutcomeError, core.RunOutcomeSuccess}, terminalOutcomes,
		"the first run's local timeout must be durably an error while the acknowledged follow-up remains successful")
}
