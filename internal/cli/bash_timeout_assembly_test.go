package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAssembleBashTimeoutRejectsBeforeModelOrExtensions(t *testing.T) {
	for _, value := range []string{"", "0s", "-1s", "bad"} {
		cfg := newAssembleTestConfig(t)
		cfg.Tools.Bash.Timeout = &value
		stateCreated := false
		// If invalid config reaches model selection, this provider would fail
		// with a different error. Preflight must happen before creating state.
		assembly, err := AssembleAgent(context.Background(), cfg, "invalid-provider-before-timeout", "invalid", io.Discard,
			func(*assembleConfig) { stateCreated = true })
		require.Nil(t, assembly)
		require.ErrorContains(t, err, "tools.bash.timeout")
		require.False(t, stateCreated, "invalid host input must be rejected even before assembly options run")
	}
}

func TestPromptInvalidBashWindowMakesNoModelOrToolCall(t *testing.T) {
	configureDurableCLISessionTest(t)
	root := t.TempDir()
	t.Chdir(root)
	first := `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"forbidden","type":"function","function":{"name":"bash","arguments":"{\"command\":\"printf forbidden > must-not-execute\"}"}}]}}]}`
	server, script := newDurablePromptServer(t, first, `{"choices":[{"message":{"role":"assistant","content":"unexpected"},"finish_reason":"stop"}]}`)
	t.Cleanup(server.Close)
	cfg := durablePromptConfig(t, server.URL, filepath.Join(t.TempDir(), "sessions"))
	invalid := ""
	cfg.Tools.Bash.Timeout = &invalid
	err := newPromptCmd(&bytes.Buffer{}).Run(t.Context(), cfg, []string{"--approve", "auto", "--no-sandbox", "synthetic invalid-host-config fixture"})
	require.ErrorContains(t, err, "tools.bash.timeout")
	require.Empty(t, script.requestBodies())
	_, err = os.Stat(filepath.Join(root, "must-not-execute"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestAssembleBashTimeoutUsesRegisteredNativeTool(t *testing.T) {
	for _, noSandbox := range []bool{false, true} {
		cfg := newAssembleTestConfig(t)
		value := "300s"
		cfg.Tools.Bash.Timeout = &value
		options := []AssembleOption{WithApproveMode(ApproveAuto)}
		if noSandbox {
			options = append(options, WithNoSandbox())
		}
		assembly, err := AssembleAgent(context.Background(), cfg, "openai", "test-model", io.Discard, options...)
		require.NoError(t, err)
		t.Cleanup(assembly.Cleanup)
		definition, err := assembly.ToolRegistry.Get(context.Background(), "bash")
		require.NoError(t, err)
		require.Contains(t, definition.Description(), (5 * time.Minute).String(), "the actual assembled tool advertises its host window")
	}
}
