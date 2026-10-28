package extension

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tools"
	api "github.com/pengjunchen/go-cli/pkg/extensionapi/v1"
)

type genericSDKCaller struct{ calls int }

func (c *genericSDKCaller) call(_ context.Context, _ string, _ any, result any) error {
	c.calls++
	if value, ok := result.(*api.ToolResult); ok {
		value.Output = "generic"
	}
	return nil
}
func (*genericSDKCaller) close(context.Context) error            { return nil }
func (*genericSDKCaller) WorkerRequestCompletionSupported() bool { return true }

type embeddedOwnedSDKTool struct{ tools.ToolDefinition }

func (*embeddedOwnedSDKTool) Execute(context.Context, tools.ToolCall) (*tools.ToolResult, error) {
	return &tools.ToolResult{Output: "spoof"}, nil
}

func sdkPreparedRegistry(t *testing.T, definition tools.ToolDefinition) *tools.MiddlewareToolRegistry {
	t.Helper()
	registry := tools.NewMiddlewareToolRegistry(tools.NewDeferredToolRegistryAdapter(tools.NewDefaultToolRegistry()))
	require.NoError(t, registry.Register(context.Background(), definition))
	require.NoError(t, registry.SetPreparedToolPolicy(func(_ context.Context, request tools.PreparedPolicyRequest) (tools.PreparedPolicyDecision, error) {
		return tools.PreparedPolicyDecision{Allowed: true, Generations: request.Generations}, nil
	}))
	require.NoError(t, registry.SetPreparedToolStages(tools.PreparedToolStages{}))
	return registry
}

func TestOwnedSDKToolExactReceiverKnownErrorAndRetirement(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	process, manifest, err := startRPCProcess(ctx, sdkProcessConfig(t, "normal"))
	require.NoError(t, err)
	defer func() { require.NoError(t, process.close(ctx)) }()
	extension := &sdkExtension{manifest: manifest, caller: process}
	catalog := NewExtensionRegistry().(*DefaultExtensionRegistry)
	require.NoError(t, extension.Init(ctx, catalog))
	definition := catalog.tool("sdk_echo")
	require.NotNil(t, definition)
	generic := &genericSDKCaller{}
	extension.caller = generic // frozen executor must keep the actual owned process
	registry := sdkPreparedRegistry(t, definition)
	for _, fail := range []bool{false, true} {
		invocation, err := registry.PrepareToolCall(ctx, tools.ToolCall{Name: "sdk_echo", Args: map[string]any{"fail": fail}})
		require.NoError(t, err)
		supported, err := tools.PreparedCompletionSupported(ctx, invocation, nil)
		require.NoError(t, err)
		require.True(t, supported)
		result, err := invocation.Dispatch(ctx)
		require.NoError(t, err)
		require.Equal(t, fail, result.IsError)
		require.Equal(t, tools.CompletionWorkerRequestJoined, invocation.CompletionSignal().Proof())
	}
	require.Zero(t, generic.calls)
	prepared, err := registry.PrepareToolCall(ctx, tools.ToolCall{Name: "sdk_echo", Args: map[string]any{}})
	require.NoError(t, err)
	require.NoError(t, process.close(ctx))
	_, err = prepared.Dispatch(ctx)
	require.ErrorIs(t, err, tools.ErrPreparedDispatchLeaseUnavailable)
	require.Empty(t, prepared.CompletionSignal().Proof())
	require.Zero(t, process.leases)
}

func TestGenericSDKAndEmbeddingCannotDeclareCompletion(t *testing.T) {
	ctx := context.Background()
	caller := &genericSDKCaller{}
	declaration := api.Tool{Name: "sdk_echo", Parameters: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)}
	generic := &sdkTool{declaration: declaration, caller: caller}
	registry := sdkPreparedRegistry(t, generic)
	invocation, err := registry.PrepareToolCall(ctx, tools.ToolCall{Name: "sdk_echo", Args: map[string]any{}})
	require.NoError(t, err)
	supported, err := tools.PreparedCompletionSupported(ctx, invocation, nil)
	require.NoError(t, err)
	require.False(t, supported)
	// Embedding a host-owned handle must not preserve its sealed concrete type.
	owner, err := tools.NewOwnedWorkerRequestTool(generic, sdkExecutorForTest{}, sdkLeaseForTest{})
	require.NoError(t, err)
	require.NoError(t, registry.Register(ctx, &embeddedOwnedSDKTool{ToolDefinition: owner}))
	invocation, err = registry.PrepareToolCall(ctx, tools.ToolCall{Name: "sdk_echo", Args: map[string]any{}})
	require.NoError(t, err)
	supported, err = tools.PreparedCompletionSupported(ctx, invocation, nil)
	require.NoError(t, err)
	require.False(t, supported)
	require.Zero(t, caller.calls)
}

type sdkExecutorForTest struct{}

func (sdkExecutorForTest) ExecuteWorkerTool(context.Context, tools.ToolCall) (*tools.ToolResult, error) {
	return nil, errors.New("test only")
}

type sdkLeaseForTest struct{}

func (sdkLeaseForTest) Acquire() (func(), error) { return func() {}, nil }

type cancelDuringRPCDecode struct {
	cancel context.CancelFunc
	result api.ToolResult
}

func (r *cancelDuringRPCDecode) UnmarshalJSON(data []byte) error {
	r.cancel()
	// Allow an already-started cancellation callback to contend with the
	// validated response. call must join it before returning known success.
	timer := time.NewTimer(20 * time.Millisecond)
	defer timer.Stop()
	<-timer.C
	return json.Unmarshal(data, &r.result)
}

func TestRPCCompletedResponseCannotCancelNextRequest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	process, _, err := startRPCProcess(ctx, sdkProcessConfig(t, "normal"))
	require.NoError(t, err)
	defer func() { require.NoError(t, process.close(ctx)) }()
	callCtx, stopCall := context.WithCancel(ctx)
	result := &cancelDuringRPCDecode{cancel: stopCall}
	require.NoError(t, process.call(callCtx, "tool.call", api.ToolCall{Name: "sdk_echo"}, result))
	require.Equal(t, "worker-result", result.result.Output)
	var next api.ToolResult
	require.NoError(t, process.call(ctx, "tool.call", api.ToolCall{Name: "sdk_echo"}, &next))
	require.Equal(t, "worker-result", next.Output)
}

func TestRPCResponseEnvelopeRejectsAmbiguity(t *testing.T) {
	for _, raw := range []string{
		`{"id":1,"result":{"output":"x"},"error":{"code":"callback_failed"}}`,
		`{"id":1}`, `{"id":1,"result":null}`, `{"id":0,"result":{"output":"x"}}`,
		`{"id":1,"id":2,"result":{"output":"x"}}`,
		`{"id":1,"result":{"output":"x"}} {}`, `{"id":1,"error":null}`,
		`{"id":1,"error":{"code":"arbitrary_callback_claim"}}`,
	} {
		_, err := decodeRPCResponse([]byte(raw))
		require.Error(t, err, raw)
	}
	response, err := decodeRPCResponse([]byte(`{"id":1,"result":{"output":"","is_error":true}}`))
	require.NoError(t, err)
	require.NoError(t, validateRPCToolResult(response.Result))
}

type rpcTestInput struct {
	writeReady chan struct{}
	closeReady chan struct{}
	closeGate  <-chan struct{}
	short      bool
	writes     int
}

func (w *rpcTestInput) Write(data []byte) (int, error) {
	w.writes++
	if w.writeReady != nil {
		close(w.writeReady)
	}
	if w.short {
		return len(data) - 1, nil
	}
	return len(data), nil
}
func (w *rpcTestInput) Close() error {
	if w.closeReady != nil {
		close(w.closeReady)
	}
	if w.closeGate != nil {
		<-w.closeGate
	}
	return nil
}

func isolatedRPCProcess(input *rpcTestInput) *rpcProcess {
	return &rpcProcess{command: exec.Command("test-only-not-started"), input: input,
		pending: make(map[uint64]*rpcPending), permits: make(chan struct{}, 1), writer: make(chan struct{}, 1),
		timeout: time.Second, done: make(chan struct{})}
}

func TestRPCCancellationJoinsStartedCallbackBeforeReturningUnknown(t *testing.T) {
	gate := make(chan struct{})
	input := &rpcTestInput{writeReady: make(chan struct{}), closeReady: make(chan struct{}), closeGate: gate}
	process := isolatedRPCProcess(input)
	ctx, cancel := context.WithCancel(t.Context())
	returned := make(chan error, 1)
	go func() { returned <- process.call(ctx, "tool.call", api.ToolCall{Name: "echo"}, nil) }()
	<-input.writeReady
	cancel()
	<-input.closeReady
	select {
	case err := <-returned:
		t.Fatalf("returned before cancellation callback joined: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(gate)
	var unknown *RPCOutcomeUnknownError
	require.ErrorAs(t, <-returned, &unknown)
	require.Equal(t, (<-chan struct{})(process.done), unknown.ExecutionDone())
	select {
	case <-unknown.ExecutionDone():
		t.Fatal("callback return cannot self-certify worker owner completion")
	default:
	}
}

func TestRPCPartialWriteIsUnknownAndPrewriteRejectionWritesNothing(t *testing.T) {
	input := &rpcTestInput{short: true}
	process := isolatedRPCProcess(input)
	err := process.call(t.Context(), "tool.call", api.ToolCall{Name: "echo"}, nil)
	var unknown *RPCOutcomeUnknownError
	require.ErrorAs(t, err, &unknown)
	require.Equal(t, 1, input.writes)
	require.Equal(t, (<-chan struct{})(process.done), unknown.ExecutionDone())

	for _, mode := range []string{"canceled", "frame-budget", "overloaded", "writer-wait"} {
		t.Run(mode, func(t *testing.T) {
			input := &rpcTestInput{}
			process := isolatedRPCProcess(input)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var params any = api.ToolCall{Name: "echo"}
			switch mode {
			case "canceled":
				cancel()
			case "frame-budget":
				params = strings.Repeat("x", api.MaxFrameBytes)
			case "overloaded":
				process.permits <- struct{}{}
			case "writer-wait":
				process.writer <- struct{}{}
				process.timeout = 10 * time.Millisecond
			}
			err := process.call(ctx, "tool.call", params, nil)
			require.Error(t, err)
			var unknown *RPCOutcomeUnknownError
			require.False(t, errors.As(err, &unknown), "no frame was dispatched")
			require.Zero(t, input.writes)
			require.Empty(t, process.pending)
			require.False(t, process.closed)
		})
	}
}

func TestRPCUnknownJoinsExactWorkerReadersWaitAndArtifact(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	process, _, err := startRPCProcess(ctx, sdkProcessConfig(t, "hang"))
	require.NoError(t, err)
	artifact := process.command.Path
	_, err = os.Stat(artifact)
	require.NoError(t, err)
	select {
	case <-process.done:
		t.Fatal("live worker already reported owner completion")
	default:
	}
	callCtx, cancelCall := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancelCall()
	err = process.call(callCtx, "tool.call", api.ToolCall{Name: "sdk_echo"}, nil)
	var unknown *RPCOutcomeUnknownError
	require.ErrorAs(t, err, &unknown)
	require.Equal(t, (<-chan struct{})(process.done), unknown.ExecutionDone())
	select {
	case <-unknown.ExecutionDone():
	case <-ctx.Done():
		t.Fatal("exact worker owner did not join")
	}
	for _, joined := range []<-chan struct{}{process.waitDone, process.readDone} {
		select {
		case <-joined:
		default:
			t.Fatal("owner completed before reader or process wait")
		}
	}
	_, err = os.Stat(artifact)
	require.ErrorIs(t, err, os.ErrNotExist)
	require.True(t, unknown.OutcomeUnknown(), "later owner join cannot upgrade the original request outcome")
	require.NoError(t, process.close(ctx))
}

func TestOwnedSDKWaitingWriterRechecksPreparedFenceBeforeFrame(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	process, manifest, err := startRPCProcess(ctx, sdkProcessConfig(t, "normal"))
	require.NoError(t, err)
	defer func() { require.NoError(t, process.close(ctx)) }()
	extension := &sdkExtension{manifest: manifest, caller: process}
	catalog := NewExtensionRegistry().(*DefaultExtensionRegistry)
	require.NoError(t, extension.Init(ctx, catalog))
	registry := sdkPreparedRegistry(t, catalog.tool("sdk_echo"))
	invocation, err := registry.PrepareToolCall(ctx, tools.ToolCall{Name: "sdk_echo", Args: map[string]any{}})
	require.NoError(t, err)
	process.writer <- struct{}{}
	returned := make(chan error, 1)
	go func() { _, err := invocation.Dispatch(ctx); returned <- err }()
	require.Eventually(t, func() bool {
		process.mu.Lock()
		defer process.mu.Unlock()
		return len(process.pending) == 1
	}, time.Second, time.Millisecond)
	_, err = registry.DispatchFence().Publish(tools.DispatchFencePlan, nil, func() {})
	require.NoError(t, err)
	<-process.writer
	require.ErrorIs(t, <-returned, tools.ErrDispatchFenceChanged)
	process.mu.Lock()
	require.False(t, process.closed, "prewrite denial must preserve the worker generation")
	process.mu.Unlock()
	// A separately authorized ordinary request still works; denial sent no
	// protocol frame that could leave a stray response for this next request.
	var result api.ToolResult
	require.NoError(t, process.call(ctx, "tool.call", api.ToolCall{Name: "sdk_echo"}, &result))
}
