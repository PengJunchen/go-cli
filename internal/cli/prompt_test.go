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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// writePromptResponse writes body to an httptest response and fails the test
// if the write errors. It satisfies errcheck in the test handler goroutines.
func writePromptResponse(t *testing.T, w http.ResponseWriter, body string) {
	t.Helper()
	if _, err := w.Write([]byte(body)); err != nil {
		t.Errorf("write response: %v", err)
	}
}

// newPromptConfig builds a *config.Config pointing at the given base URL so the
// prompt command builds a custom provider backed by the httptest endpoint.
func newPromptConfig(baseURL string) *config.Config {
	return &config.Config{
		Provider: config.ProviderConfig{
			Name:    "test",
			BaseURL: baseURL,
			APIKey:  "test",
			Model:   "test-model",
		},
	}
}

// TestPromptCmd_RunCannedCompletion wires a prompt command to an httptest
// OpenAI-compatible endpoint and verifies the agent's response is streamed to
// the output writer.
func TestPromptCmd_RunCannedCompletion(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/chat/completions", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		writePromptResponse(t, w, `{
			"choices":[{"message":{"role":"assistant","content":"ok"}}],
			"usage":{"prompt_tokens":3,"completion_tokens":1}
		}`)
	}))
	defer srv.Close()

	var out bytes.Buffer
	cmd := newPromptCmd(&out)
	cfg := newPromptConfig(srv.URL)

	err := cmd.Run(t.Context(), cfg, []string{"hello"})
	require.NoError(t, err)
	assert.Equal(t, "ok\n", out.String())
}

// TestPromptCmd_RunFlagsOverridesConfig verifies the -model and -provider flags
// override the loaded configuration while still reaching the httptest endpoint.
func TestPromptCmd_RunFlagsOverridesConfig(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, rerr := io.ReadAll(r.Body)
		require.NoError(t, rerr)
		assert.Contains(t, string(body), `"model":"flagged-model"`)
		w.Header().Set("Content-Type", "application/json")
		writePromptResponse(t, w, `{"choices":[{"message":{"content":"hello there"}}]}`)
	}))
	defer srv.Close()

	var out bytes.Buffer
	cmd := newPromptCmd(&out)
	cfg := newPromptConfig(srv.URL)

	err := cmd.Run(t.Context(), cfg, []string{"-model", "flagged-model", "-provider", "test", "-verbose", "hello"})
	require.NoError(t, err)
	assert.Equal(t, "hello there\n", out.String())
}

// TestPromptCmd_RunUnknownFlag verifies an unknown flag produces a UsageError.
func TestPromptCmd_RunUnknownFlag(t *testing.T) {
	var out bytes.Buffer
	cmd := newPromptCmd(&out)

	err := cmd.Run(t.Context(), newPromptConfig("ignored"), []string{"-bogus", "hello"})
	require.Error(t, err)
	var usageErr *UsageError
	assert.True(t, errors.As(err, &usageErr))
	assert.True(t, strings.Contains(err.Error(), "bogus"))
}

// TestPromptCmd_RunMissingMessage verifies a missing prompt argument produces a
// UsageError without attempting any network call.
func TestPromptCmd_RunMissingMessage(t *testing.T) {
	var out bytes.Buffer
	cmd := newPromptCmd(&out)

	err := cmd.Run(t.Context(), newPromptConfig("ignored"), nil)
	require.Error(t, err)
	var usageErr *UsageError
	assert.True(t, errors.As(err, &usageErr))
	assert.Contains(t, err.Error(), "missing message argument")
}

// TestPromptCmd_Metadata verifies the prompt command's registration metadata.
func TestPromptCmd_Metadata(t *testing.T) {
	cmd := newPromptCmd(&bytes.Buffer{})
	assert.Equal(t, "prompt", cmd.Name())
	assert.NotEmpty(t, cmd.Synopsis())
}

// TestForkSession verifies that forkSession builds a tree from the store,
// branches at the requested entry, rebuilds context, and injects history.
func TestForkSession(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	storePath := filepath.Join(dir, "session.jsonl")
	store := session.NewJSONLSessionStore(storePath)
	require.NoError(t, store.Open(context.Background()))

	ctx := context.Background()
	require.NoError(t, store.Append(ctx, &session.SessionEntry{ID: "e1", ParentID: "", Type: session.EntryTypeUser, Content: "hello"}))
	require.NoError(t, store.Append(ctx, &session.SessionEntry{ID: "e2", ParentID: "e1", Type: session.EntryTypeAssistant, Content: "hi there"}))
	require.NoError(t, store.Append(ctx, &session.SessionEntry{ID: "e3", ParentID: "e2", Type: session.EntryTypeUser, Content: "what is 2+2?"}))
	require.NoError(t, store.Append(ctx, &session.SessionEntry{ID: "e4", ParentID: "e3", Type: session.EntryTypeAssistant, Content: "4"}))

	agent := core.NewAgentImpl("test", stubLoop{})
	assembly := &AgentAssembly{SessionManagement: SessionManagement{SessionStore: store}, CoreRuntime: CoreRuntime{Agent: agent}}

	cmd := newPromptCmd(&bytes.Buffer{})
	// Fork from e2 — history should contain e1 (user) and e2 (assistant).
	require.NoError(t, cmd.forkSession(ctx, assembly, "e2"))

	history := agent.Messages()
	require.Len(t, history, 2)
	assert.Equal(t, "user", history[0].Role)
	assert.Equal(t, "hello", history[0].Content)
	assert.Equal(t, "assistant", history[1].Role)
	assert.Equal(t, "hi there", history[1].Content)
}

// TestForkSession_NilStore verifies that forkSession returns a friendly error
// when no session store is available.
func TestForkSession_NilStore(t *testing.T) {
	agent := core.NewAgentImpl("test", stubLoop{})
	assembly := &AgentAssembly{SessionManagement: SessionManagement{SessionStore: nil}, CoreRuntime: CoreRuntime{Agent: agent}}
	cmd := newPromptCmd(&bytes.Buffer{})
	err := cmd.forkSession(context.Background(), assembly, "some-id")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "session store unavailable")
}

// TestForkSession_NotFound verifies that forkSession returns an error when the
// requested session id does not exist in the store.
func TestForkSession_NotFound(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	storePath := filepath.Join(dir, "session.jsonl")
	store := session.NewJSONLSessionStore(storePath)
	require.NoError(t, store.Open(context.Background()))
	ctx := context.Background()
	require.NoError(t, store.Append(ctx, &session.SessionEntry{ID: "e1", ParentID: "", Type: session.EntryTypeUser, Content: "hello"}))

	agent := core.NewAgentImpl("test", stubLoop{})
	assembly := &AgentAssembly{SessionManagement: SessionManagement{SessionStore: store}, CoreRuntime: CoreRuntime{Agent: agent}}
	cmd := newPromptCmd(&bytes.Buffer{})
	err := cmd.forkSession(ctx, assembly, "nonexistent")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fork from session")
}

// TestForkSession_EmptyStore verifies that forkSession returns an error when
// the session store has no entries.
func TestForkSession_EmptyStore(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	storePath := filepath.Join(dir, "session.jsonl")
	store := session.NewJSONLSessionStore(storePath)
	require.NoError(t, store.Open(context.Background()))

	agent := core.NewAgentImpl("test", stubLoop{})
	assembly := &AgentAssembly{SessionManagement: SessionManagement{SessionStore: store}, CoreRuntime: CoreRuntime{Agent: agent}}
	cmd := newPromptCmd(&bytes.Buffer{})
	err := cmd.forkSession(context.Background(), assembly, "any-id")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty")
}

// TestPromptCmd_NoForkNoPersistence verifies AC-5: without --fork, the prompt
// command does not enable session persistence — no session file is created even
// when a store path is configured.
func TestPromptCmd_NoForkNoPersistence(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		writePromptResponse(t, w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()

	dir := t.TempDir()
	storePath := filepath.Join(dir, "session.jsonl")

	var out bytes.Buffer
	cmd := newPromptCmd(&out)
	cfg := newPromptConfig(srv.URL)
	cfg.Session.StorePath = storePath

	err := cmd.Run(t.Context(), cfg, []string{"hello"})
	require.NoError(t, err)
	assert.Equal(t, "ok\n", out.String())

	// Without --fork, session persistence is not enabled, so no session file
	// should have been created.
	_, statErr := os.Stat(storePath)
	assert.True(t, os.IsNotExist(statErr), "session file should not exist without --fork")
}
