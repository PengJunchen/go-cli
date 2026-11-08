package extension

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	api "github.com/pengjunchen/go-cli/pkg/extensionapi/v1"
)

func TestRPCExitAfterRequestWorker(t *testing.T) {
	mode := os.Getenv("GO_CLI_SDK_EXIT_AFTER_REQUEST")
	if mode == "" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		os.Exit(2)
	}
	var request api.Request
	if json.Unmarshal(scanner.Bytes(), &request) != nil {
		os.Exit(3)
	}
	switch mode {
	case "valid", "valid-nonzero":
		frame, err := json.Marshal(api.Response{ID: request.ID, Result: json.RawMessage(`{"output":"worker-result"}`)})
		if err != nil {
			os.Exit(4)
		}
		_, _ = os.Stdout.Write(append(frame, '\n'))
	case "malformed":
		_, _ = io.WriteString(os.Stdout, "{\n")
	case "absent", "drain-timeout":
	default:
		os.Exit(5)
	}
	if mode == "valid-nonzero" {
		os.Exit(7)
	}
	os.Exit(0)
}

type rpcGatedReadCloser struct {
	reader io.ReadCloser
	gate   <-chan struct{}
	closed chan struct{}
	once   sync.Once
}

func (r *rpcGatedReadCloser) Read(buffer []byte) (int, error) {
	select {
	case <-r.gate:
		return r.reader.Read(buffer)
	case <-r.closed:
		return 0, os.ErrClosed
	}
}

func (r *rpcGatedReadCloser) Close() error {
	r.once.Do(func() { close(r.closed) })
	return r.reader.Close()
}

type rpcLeaderExitWriteInput struct {
	writes         int
	firstWritten   chan struct{}
	secondStarted  chan struct{}
	releaseSecond  chan struct{}
	secondWritten  int
	secondWriteErr error
}

func (w *rpcLeaderExitWriteInput) Write(data []byte) (int, error) {
	w.writes++
	switch w.writes {
	case 1:
		close(w.firstWritten)
		return len(data), nil
	case 2:
		close(w.secondStarted)
		<-w.releaseSecond
		return w.secondWritten, w.secondWriteErr
	default:
		return len(data), nil
	}
}

func (*rpcLeaderExitWriteInput) Close() error { return nil }

func TestRPCLeaderExitDrainsValidatedFrameAndKeepsUnknowns(t *testing.T) {
	for _, mode := range []string{"valid", "valid-nonzero", "absent", "malformed", "drain-timeout", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			executable, err := os.Executable()
			require.NoError(t, err)
			command := exec.Command(executable, "-test.run=^TestRPCExitAfterRequestWorker$")
			workerMode := mode
			if workerMode == "drain-timeout" {
				workerMode = "absent"
			} else if workerMode == "cancel" {
				workerMode = "valid"
			}
			command.Env = append(os.Environ(), "GO_CLI_SDK_EXIT_AFTER_REQUEST="+workerMode)
			command.Stderr = io.Discard
			configureRPCProcess(command)
			output, outputWriter, err := os.Pipe()
			require.NoError(t, err)
			command.Stdout = outputWriter
			input, err := command.StdinPipe()
			require.NoError(t, err)
			require.NoError(t, command.Start())
			require.NoError(t, outputWriter.Close())

			gate := make(chan struct{})
			process := &rpcProcess{command: command, input: input, pending: make(map[uint64]*rpcPending), permits: make(chan struct{}, 2), writer: make(chan struct{}, 1), timeout: 5 * time.Second, done: make(chan struct{}), readDone: make(chan struct{}), waitDone: make(chan struct{})}
			process.monitor(&rpcGatedReadCloser{reader: output, gate: gate, closed: make(chan struct{})}, "")

			type callResult struct {
				result api.ToolResult
				err    error
			}
			baseCtx, cancelBase := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancelBase()
			callCtx, cancelCall := context.WithCancel(baseCtx)
			defer cancelCall()
			callDone := make(chan callResult, 1)
			go func() {
				var result api.ToolResult
				err := process.call(callCtx, "tool.call", api.ToolCall{Name: "sdk_echo"}, &result)
				callDone <- callResult{result: result, err: err}
			}()

			select {
			case <-process.waitDone:
			case <-time.After(5 * time.Second):
				t.Fatal("worker leader did not exit after the request")
			}
			_, err = process.Acquire()
			require.Error(t, err, "leader exit must retire the worker before reader drain")
			process.mu.Lock()
			next := process.next
			process.mu.Unlock()
			require.Error(t, process.call(callCtx, "tool.call", api.ToolCall{Name: "sdk_echo"}, nil))
			process.mu.Lock()
			laterNext := process.next
			process.mu.Unlock()
			require.Equal(t, next, laterNext, "retired worker must reject before creating another request")
			if mode == "cancel" {
				cancelCall()
				require.Eventually(t, func() bool {
					process.mu.Lock()
					defer process.mu.Unlock()
					return process.closed
				}, time.Second, time.Millisecond, "cancellation must settle before releasing the response reader")
			}
			if mode != "drain-timeout" {
				close(gate)
			}

			var outcome callResult
			select {
			case outcome = <-callDone:
			case <-time.After(4 * time.Second):
				t.Fatal("call did not settle after stdout drain")
			}
			if mode == "valid" || mode == "valid-nonzero" {
				require.NoError(t, outcome.err)
				require.Equal(t, "worker-result", outcome.result.Output)
			} else {
				var unknown *RPCOutcomeUnknownError
				require.ErrorAs(t, outcome.err, &unknown)
				switch mode {
				case "absent":
					require.ErrorContains(t, outcome.err, "EOF")
				case "malformed":
					require.ErrorContains(t, outcome.err, "invalid RPC object end")
				case "drain-timeout":
					require.ErrorContains(t, outcome.err, "RPC response drain exceeded")
				case "cancel":
					require.ErrorIs(t, outcome.err, context.Canceled)
				}
			}

			select {
			case <-process.done:
			case <-time.After(4 * time.Second):
				t.Fatal("worker lifecycle did not join after response drain")
			}
			if mode == "valid-nonzero" {
				process.mu.Lock()
				failure := process.failure
				process.mu.Unlock()
				require.ErrorContains(t, failure, "exit status 7")
			}
		})
	}
}

func TestRPCLeaderExitDoesNotLetPartialWriteDiscardOtherReply(t *testing.T) {
	for _, mode := range []string{"partial", "zero"} {
		t.Run(mode, func(t *testing.T) {
			secondWritten := 1
			if mode == "zero" {
				secondWritten = 0
			}
			input := &rpcLeaderExitWriteInput{firstWritten: make(chan struct{}), secondStarted: make(chan struct{}), releaseSecond: make(chan struct{}), secondWritten: secondWritten, secondWriteErr: errors.New("simulated RPC frame write error")}
			output, outputWriter, err := os.Pipe()
			require.NoError(t, err)
			gate := make(chan struct{})
			process := &rpcProcess{command: exec.Command("test-only-not-started"), input: input, pending: make(map[uint64]*rpcPending), permits: make(chan struct{}, 3), writer: make(chan struct{}, 1), timeout: 5 * time.Second, done: make(chan struct{}), readDone: make(chan struct{}), waitDone: make(chan struct{})}
			go process.read(&rpcGatedReadCloser{reader: output, gate: gate, closed: make(chan struct{})})
			waitPublish := make(chan struct{})
			var waitPublishOnce sync.Once
			publishWait := func() { waitPublishOnce.Do(func() { close(waitPublish) }) }
			var releaseSecondOnce sync.Once
			releaseSecond := func() { releaseSecondOnce.Do(func() { close(input.releaseSecond) }) }
			var releaseReaderOnce sync.Once
			releaseReader := func() { releaseReaderOnce.Do(func() { close(gate) }) }
			t.Cleanup(func() {
				releaseSecond()
				releaseReader()
				_ = outputWriter.Close()
				publishWait()
			})
			go func() {
				<-waitPublish
				process.retire(io.EOF, false)
				close(process.waitDone)
			}()

			type callOutcome struct {
				result api.ToolResult
				err    error
			}
			firstDone := make(chan callOutcome, 1)
			go func() {
				var result api.ToolResult
				err := process.call(t.Context(), "tool.call", api.ToolCall{Name: "first"}, &result)
				firstDone <- callOutcome{result: result, err: err}
			}()
			select {
			case <-input.firstWritten:
			case <-time.After(time.Second):
				t.Fatal("first request frame was not written")
			}

			secondDone := make(chan callOutcome, 1)
			go func() {
				var result api.ToolResult
				err := process.call(t.Context(), "tool.call", api.ToolCall{Name: "second"}, &result)
				secondDone <- callOutcome{result: result, err: err}
			}()
			select {
			case <-input.secondStarted:
			case <-time.After(time.Second):
				t.Fatal("second request did not reach its controlled write")
			}

			response, err := json.Marshal(api.Response{ID: 1, Result: json.RawMessage(`{"output":"first-result"}`)})
			require.NoError(t, err)
			_, err = outputWriter.Write(append(response, '\n'))
			require.NoError(t, err)
			require.NoError(t, outputWriter.Close())
			select {
			case <-process.waitDone:
				t.Fatal("synthetic write failure must precede Wait publication")
			default:
			}
			releaseSecond()
			require.Eventually(t, func() bool {
				process.mu.Lock()
				defer process.mu.Unlock()
				return process.retired
			}, time.Second, time.Millisecond, "write failure must retire before releasing the writer")
			select {
			case <-process.waitDone:
				t.Fatal("write error must retire before the gated Wait result is published")
			default:
			}
			_, err = process.Acquire()
			require.Error(t, err)
			process.mu.Lock()
			next := process.next
			process.mu.Unlock()
			require.Error(t, process.call(t.Context(), "tool.call", api.ToolCall{Name: "later"}, nil))
			process.mu.Lock()
			laterNext := process.next
			process.mu.Unlock()
			require.Equal(t, next, laterNext, "retired generation must reject later calls before a frame")
			publishWait()
			select {
			case <-process.waitDone:
			case <-time.After(time.Second):
				t.Fatal("gated Wait result was not published")
			}

			if mode == "zero" {
				select {
				case outcome := <-secondDone:
					var unknown *RPCOutcomeUnknownError
					require.Error(t, outcome.err)
					require.False(t, errors.As(outcome.err, &unknown), "zero-byte write is known unavailable before dispatch")
					require.ErrorContains(t, outcome.err, "simulated RPC frame write error")
				case <-time.After(time.Second):
					t.Fatal("zero-byte write did not return a known unavailable error")
				}
			}
			releaseReader()

			select {
			case outcome := <-firstDone:
				require.NoError(t, outcome.err, "a second write failure must not discard the validated first reply")
				require.Equal(t, "first-result", outcome.result.Output)
			case <-time.After(time.Second):
				t.Fatal("first call did not receive its validated response")
			}
			if mode == "partial" {
				select {
				case outcome := <-secondDone:
					var unknown *RPCOutcomeUnknownError
					require.ErrorAs(t, outcome.err, &unknown)
					require.ErrorContains(t, outcome.err, "simulated RPC frame write error")
					require.ErrorContains(t, outcome.err, "EOF")
				case <-time.After(time.Second):
					t.Fatal("partial second request did not settle as UNKNOWN")
				}
			}
			select {
			case <-process.readDone:
			case <-time.After(time.Second):
				t.Fatal("gated reader did not join after EOF")
			}
		})
	}
}

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
