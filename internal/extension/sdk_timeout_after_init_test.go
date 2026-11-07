package extension

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	api "github.com/pengjunchen/go-cli/pkg/extensionapi/v1"
)

// TestRPCDescriptorDeadlineMarkerWorker records that the real RPC handler was
// entered before its request deadline, then withholds the response.
func TestRPCDescriptorDeadlineMarkerWorker(t *testing.T) {
	if os.Getenv("GO_CLI_RPC_DEADLINE_WORKER") != "1" {
		return
	}
	manifest := api.Manifest{ID: "deadline.worker", Version: "1", HostAPI: api.Version, Capabilities: []string{"tools"}, Tools: []api.Tool{{Name: "sdk_echo", Description: "deadline probe", Parameters: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)}}}
	handler := api.HandlerFunc(func(ctx context.Context, _ api.Request) (json.RawMessage, error) {
		deadline, ok := ctx.Deadline()
		if !ok || ctx.Err() != nil || !time.Now().Before(deadline) {
			return nil, context.DeadlineExceeded
		}
		if err := os.WriteFile(os.Getenv("GO_CLI_RPC_DEADLINE_MARKER"), []byte("entered\n"), 0o600); err != nil {
			return nil, err
		}
		select {}
	})
	if err := api.Serve(context.Background(), os.Stdin, os.Stdout, manifest, handler); err != nil {
		os.Exit(3)
	}
	os.Exit(0)
}

// TestRPCDescriptorDeadlineAfterInitialize isolates the descriptor's per-RPC
// deadline from startup and from the caller's still-live context.
func TestRPCDescriptorDeadlineAfterInitialize(t *testing.T) {
	outer, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	marker := filepath.Join(t.TempDir(), "handler-entered")
	cfg := sdkProcessConfig(t, "hang")
	cfg.Command[1] = "-test.run=^TestRPCDescriptorDeadlineMarkerWorker$"
	cfg.Env["GO_CLI_RPC_DEADLINE_WORKER"] = "1"
	cfg.Env["GO_CLI_RPC_DEADLINE_MARKER"] = marker
	process, _, err := startRPCProcess(outer, cfg)
	require.NoError(t, err)
	artifact := process.command.Path
	_, err = os.Stat(artifact)
	require.NoError(t, err)
	defer func() {
		cleanup, cancelCleanup := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelCleanup()
		require.NoError(t, process.close(cleanup))
	}()

	// Initialization has already joined; no concurrent call can read timeout.
	process.timeout = 2 * time.Second
	err = process.call(outer, "tool.call", api.ToolCall{Name: "sdk_echo"}, nil)
	var unknown *RPCOutcomeUnknownError
	require.ErrorAs(t, err, &unknown)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NoError(t, outer.Err(), "the caller context did not expire")
	entry, err := os.ReadFile(marker)
	require.NoError(t, err, "the live handler must start before its RPC deadline")
	require.Equal(t, "entered\n", string(entry))
	require.Equal(t, (<-chan struct{})(process.done), unknown.ExecutionDone())
	select {
	case <-unknown.ExecutionDone():
	case <-outer.Done():
		t.Fatal("exact worker owner did not join")
	}
	_, err = os.Stat(artifact)
	require.ErrorIs(t, err, os.ErrNotExist)
	for _, joined := range []<-chan struct{}{process.waitDone, process.readDone} {
		select {
		case <-joined:
		default:
			t.Fatal("owner completed before worker wait or reader joined")
		}
	}
}
