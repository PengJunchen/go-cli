package extension

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	api "github.com/pengjunchen/go-cli/pkg/extensionapi/v1"
)

func TestSDKRPCWorker(t *testing.T) {
	if os.Getenv("GO_CLI_SDK_TEST_WORKER") != "1" {
		return
	}
	manifest := api.Manifest{ID: "test.worker", Version: "1", HostAPI: api.Version, Capabilities: []string{"tools"}, Tools: []api.Tool{{Name: "sdk_echo", Description: "echo", Parameters: json.RawMessage(`{"type":"object","properties":{"fail":{"type":"boolean"}},"additionalProperties":false}`)}}}
	if os.Getenv("GO_CLI_SDK_TEST_MODE") == "bad-version" {
		manifest.HostAPI = "99"
	}
	handler := api.HandlerFunc(func(ctx context.Context, request api.Request) (json.RawMessage, error) {
		switch os.Getenv("GO_CLI_SDK_TEST_MODE") {
		case "hang":
			select {}
		case "panic":
			panic("must not expose panic detail")
		case "bad-shape":
			return json.RawMessage(`[1,2,3]`), nil
		case "null-result":
			return json.RawMessage(`null`), nil
		case "missing-output":
			return json.RawMessage(`{}`), nil
		case "wrong-output":
			return json.RawMessage(`{"output": 7}`), nil
		case "duplicate-output":
			return json.RawMessage(`{"output":"first","output":"second"}`), nil
		case "error-result":
			return json.Marshal(api.ToolResult{Output: "known worker failure", IsError: true})
		case "oversize":
			return json.Marshal(strings.Repeat("x", api.MaxFrameBytes))
		case "disconnect":
			os.Exit(0)
		}
		var call api.ToolCall
		_ = json.Unmarshal(request.Params, &call)
		if fail, _ := call.Args["fail"].(bool); fail {
			return json.Marshal(api.ToolResult{Output: "known worker failure", IsError: true})
		}
		_, leaked := os.LookupEnv("GO_CLI_SDK_HOST_SECRET")
		return json.Marshal(api.ToolResult{Output: "worker-result", Metadata: map[string]any{"host_secret_leaked": leaked}})
	})
	err := api.Serve(context.Background(), os.Stdin, os.Stdout, manifest, handler)
	if err != nil {
		os.Exit(3)
	}
	os.Exit(0)
}

func sdkProcessConfig(t *testing.T, mode string) RPCProcessConfig {
	t.Helper()
	executable, err := os.Executable()
	require.NoError(t, err)
	data, err := os.ReadFile(executable)
	require.NoError(t, err)
	digest := sha256.Sum256(data)
	return RPCProcessConfig{Command: []string{executable, "-test.run=^TestSDKRPCWorker$"}, Cwd: t.TempDir(), Env: map[string]string{"GO_CLI_SDK_TEST_WORKER": "1", "GO_CLI_SDK_TEST_MODE": mode}, ArtifactSHA256: hex.EncodeToString(digest[:]), TimeoutMillis: 10000, MaxInflight: 2}
}

func TestExtensionProcessDoesNotInheritHostSecrets(t *testing.T) {
	t.Setenv("GO_CLI_SDK_HOST_SECRET", "must-stay-in-host")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	process, manifest, err := startRPCProcess(ctx, sdkProcessConfig(t, "normal"))
	require.NoError(t, err)
	defer func() { require.NoError(t, process.close(ctx)) }()
	require.Equal(t, "test.worker", manifest.ID)
	var result api.ToolResult
	require.NoError(t, process.call(ctx, "tool.call", api.ToolCall{Name: "sdk_echo"}, &result))
	require.Equal(t, false, result.Metadata["host_secret_leaked"])
}

func TestRPCTimeoutFrameAndDisconnectCleanProcess(t *testing.T) {
	for _, mode := range []string{"hang", "oversize", "disconnect", "bad-shape", "panic"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			process, _, err := startRPCProcess(ctx, sdkProcessConfig(t, mode))
			require.NoError(t, err)
			callCtx, cancelCall := context.WithTimeout(ctx, 100*time.Millisecond)
			defer cancelCall()
			var result api.ToolResult
			err = process.call(callCtx, "tool.call", api.ToolCall{Name: "sdk_echo"}, &result)
			require.Error(t, err)
			var unknown *RPCOutcomeUnknownError
			require.True(t, errors.As(err, &unknown), "%T: %v", err, err)
			require.NoError(t, process.close(ctx))
			select {
			case <-process.done:
			default:
				t.Fatal("reported closed before worker exit")
			}
		})
	}
}

func TestExtensionHandshakeAndChecksumRejectFailedCandidate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	config := sdkProcessConfig(t, "normal")
	config.ArtifactSHA256 = strings.Repeat("0", 64)
	_, _, err := startRPCProcess(ctx, config)
	require.ErrorContains(t, err, "digest mismatch")
	_, _, err = startRPCProcess(ctx, sdkProcessConfig(t, "bad-version"))
	require.Error(t, err)
}

func TestRPCToolResultRequiresTypedOutput(t *testing.T) {
	for _, mode := range []string{"null-result", "missing-output", "wrong-output", "duplicate-output"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			process, _, err := startRPCProcess(ctx, sdkProcessConfig(t, mode))
			require.NoError(t, err)
			defer func() { require.NoError(t, process.close(ctx)) }()
			var result api.ToolResult
			err = process.call(ctx, "tool.call", api.ToolCall{Name: "sdk_echo"}, &result)
			var unknown *RPCOutcomeUnknownError
			require.ErrorAs(t, err, &unknown)
		})
	}
}
