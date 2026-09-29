package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	stdioHelperModeEnv = "GO_CLI_MCP_STDIO_HELPER_MODE"
	stdioHelperPIDEnv  = "GO_CLI_MCP_STDIO_HELPER_PID_FILE"
	stdioHelperWireEnv = "GO_CLI_MCP_STDIO_HELPER_WIRE_FILE"
)

// TestMCPStdioOwnedChildHelper is started as a real child process by the
// owned-stdio lifecycle tests below. It keeps a silent tools/call open until
// the client closes its owned pipes or kills it.
func TestMCPStdioOwnedChildHelper(t *testing.T) {
	mode := os.Getenv(stdioHelperModeEnv)
	if mode == "" {
		return
	}

	if pidFile := os.Getenv(stdioHelperPIDEnv); pidFile != "" {
		if err := os.WriteFile(pidFile, []byte(fmt.Sprint(os.Getpid())), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			continue
		}
		if wireFile := os.Getenv(stdioHelperWireEnv); wireFile != "" {
			file, err := os.OpenFile(wireFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := fmt.Fprintln(file, request.Method); err != nil {
				_ = file.Close()
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
		}

		if len(request.ID) > 0 && request.Method == "tools/call" {
			switch mode {
			case "oversized-line":
				writeHelperChildOutput(t, []byte(strings.Repeat("x", maxHTTPResponseSize+1)+"\n"))
				continue
			case "oversized-fragment":
				writeHelperChildOutput(t, []byte(strings.Repeat("x", maxHTTPResponseSize+1)))
				continue
			case "ignored-lines-over-budget":
				padding := strings.Repeat("p", 64*1024)
				for index := 0; index < 161; index++ {
					frame := map[string]any{"jsonrpc": "2.0"}
					if index%2 == 0 {
						frame["method"] = "notifications/progress"
						frame["params"] = map[string]any{"padding": padding}
					} else {
						frame["id"] = 999999
						frame["result"] = map[string]any{"padding": padding}
					}
					response, err := json.Marshal(frame)
					if err != nil {
						t.Fatal(err)
					}
					response = append(response, '\n')
					writeHelperChildOutput(t, response)
				}
				continue
			case "boundary-line":
				prefix := []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"content":"`, request.ID))
				suffix := []byte(`"}}` + "\n")
				paddingSize := maxHTTPResponseSize - len(prefix) - len(suffix)
				if paddingSize < 0 {
					t.Fatal("response prefix exceeds the stdio boundary")
				}
				response := make([]byte, 0, maxHTTPResponseSize)
				response = append(response, prefix...)
				response = append(response, strings.Repeat("a", paddingSize)...)
				response = append(response, suffix...)
				if len(response) != maxHTTPResponseSize {
					t.Fatalf("constructed response length %d, want %d", len(response), maxHTTPResponseSize)
				}
				writeHelperChildOutput(t, response)
				continue
			}
		}

		if len(request.ID) == 0 || request.Method != "initialize" {
			continue
		}
		if mode == "initialize-silent" {
			continue
		}

		protocolVersion := LatestProtocolVersion
		if mode == "initialize-failure" {
			protocolVersion = "unsupported-test-version"
		}
		response, err := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      json.RawMessage(request.ID),
			"result": map[string]any{
				"protocolVersion": protocolVersion,
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]any{"name": "stdio-helper", "version": "1"},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fmt.Fprintln(os.Stdout, string(response)); err != nil {
			t.Fatal(err)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
}

func writeHelperChildOutput(t *testing.T, payload []byte) {
	t.Helper()
	n, err := os.Stdout.Write(payload)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(payload) {
		t.Fatalf("child wrote %d bytes, want %d", n, len(payload))
	}
}

func newOwnedStdioHelperAdapter(t *testing.T, mode string) (*OfficialSDKAdapter, string, string) {
	t.Helper()

	tempDir := t.TempDir()
	pidFile := filepath.Join(tempDir, "child.pid")
	wireFile := filepath.Join(tempDir, "wire.log")
	executable, err := os.Executable()
	require.NoError(t, err)
	adapter := NewOfficialSDKAdapter(MCPServerConfig{
		Name:      "stdio-helper",
		Transport: MCPTransportStdio,
		Command:   executable,
		Args:      []string{"-test.run=^TestMCPStdioOwnedChildHelper$"},
		Env: []string{
			stdioHelperModeEnv + "=" + mode,
			stdioHelperPIDEnv + "=" + pidFile,
			stdioHelperWireEnv + "=" + wireFile,
		},
	})
	t.Cleanup(func() {
		_ = adapter.Disconnect(context.Background())
		if data, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				if process, err := os.FindProcess(pid); err == nil {
					_, _ = process.Wait()
				}
			}
		}
	})
	return adapter, pidFile, wireFile
}

func waitForHelperFile(t *testing.T, filename string) {
	t.Helper()
	require.Eventually(t, func() bool {
		_, err := os.Stat(filename)
		return err == nil
	}, 2*time.Second, 5*time.Millisecond, "stdio child should create %s", filepath.Base(filename))
}

func TestOwnedStdioFailedInitializeJoinsChild(t *testing.T) {
	adapter, pidFile, _ := newOwnedStdioHelperAdapter(t, "initialize-failure")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := adapter.Connect(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported protocol version")
	waitForHelperFile(t, pidFile)

	// The child command's ProcessState is populated only after cmd.Wait has
	// positively joined it. A failed handshake must retain enough ownership to
	// finish that join before Connect reports the failure.
	require.NotNil(t, adapter.adapterCore.proc, "failed Connect must not forget its started child")
	require.NotNil(t, adapter.adapterCore.proc.ProcessState, "failed Connect returned before Wait joined its child")
	require.NoError(t, adapter.Disconnect(context.Background()), "Disconnect after a failed Connect joins the same completed owner")
	require.NoError(t, adapter.Disconnect(context.Background()), "repeated Disconnect is idempotent")
}

func TestOwnedStdioCanceledInitializeKeepsJoinableOwner(t *testing.T) {
	adapter, pidFile, wireFile := newOwnedStdioHelperAdapter(t, "initialize-silent")
	ctx, cancel := context.WithCancel(context.Background())
	connectDone := make(chan error, 1)
	go func() { connectDone <- adapter.Connect(ctx) }()

	waitForHelperFile(t, pidFile)
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(wireFile)
		return err == nil && strings.Contains(string(data), "initialize")
	}, 2*time.Second, 5*time.Millisecond, "child should receive initialize before cancellation")
	cancel()

	var connectErr error
	select {
	case connectErr = <-connectDone:
	case <-time.After(2 * time.Second):
		t.Fatal("canceled initialize remained blocked on the owned child's silent stdout")
	}
	require.Error(t, connectErr)
	assert.ErrorIs(t, connectErr, context.Canceled)

	adapter.adapterCore.mu.Lock()
	owner := adapter.adapterCore.procOwner
	proc := adapter.adapterCore.proc
	adapter.adapterCore.mu.Unlock()
	require.NotNil(t, owner, "canceled Connect must retain its child owner through join")
	require.NotNil(t, proc)
	select {
	case <-owner.done:
	case <-time.After(2 * time.Second):
		t.Fatal("canceled initialize did not join the owned child")
	}
	require.NotNil(t, proc.ProcessState, "canceled Connect returned before cmd.Wait completed")
	require.NoError(t, adapter.Disconnect(context.Background()), "Disconnect must join/release a failed Connect owner")
	require.NoError(t, adapter.Disconnect(context.Background()), "repeated Disconnect after failed Connect must be idempotent")
}

func TestOwnedStdioDisconnectFencesSilentInitialize(t *testing.T) {
	adapter, pidFile, wireFile := newOwnedStdioHelperAdapter(t, "initialize-silent")
	connectDone := make(chan error, 1)
	go func() { connectDone <- adapter.Connect(context.Background()) }()

	waitForHelperFile(t, pidFile)
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(wireFile)
		return err == nil && strings.Contains(string(data), "initialize")
	}, 2*time.Second, 5*time.Millisecond, "child should receive initialize before Disconnect")
	adapter.adapterCore.mu.Lock()
	proc := adapter.adapterCore.proc
	adapter.adapterCore.mu.Unlock()
	require.NotNil(t, proc)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, adapter.Disconnect(ctx), "Disconnect must cancel and join an in-flight initialize outside the client lock")
	select {
	case err := <-connectDone:
		require.Error(t, err, "a fenced Connect must not report success")
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("Disconnect returned before the in-flight Connect operation unwound")
	}
	require.NotNil(t, proc.ProcessState, "Disconnect returned before cmd.Wait joined the child")
}

func TestOwnedStdioCancellationInterruptsSilentRequestAndJoinsChild(t *testing.T) {
	adapter, pidFile, wireFile := newOwnedStdioHelperAdapter(t, "silent-call")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, adapter.Connect(ctx))
	waitForHelperFile(t, pidFile)
	proc := adapter.adapterCore.proc
	require.NotNil(t, proc)

	callCtx, cancelCall := context.WithCancel(context.Background())
	callDone := make(chan error, 1)
	go func() {
		_, err := adapter.CallTool(callCtx, "silent", map[string]any{"value": "one-shot"})
		callDone <- err
	}()

	require.Eventually(t, func() bool {
		data, err := os.ReadFile(wireFile)
		return err == nil && strings.Contains(string(data), "tools/call")
	}, 2*time.Second, 5*time.Millisecond, "child should receive the one tool call")
	cancelCall()

	var callErr error
	select {
	case callErr = <-callDone:
	case <-time.After(2 * time.Second):
		t.Fatal("canceled CallTool remained blocked on the owned child's silent stdout")
	}
	require.Error(t, callErr)
	var unknown interface{ OutcomeUnknown() bool }
	require.ErrorAs(t, callErr, &unknown)
	assert.True(t, unknown.OutcomeUnknown(), "a sent call without a trusted response remains outcome-unknown")
	var pending interface {
		Done() <-chan struct{}
		ExecutionDone() <-chan struct{}
	}
	require.ErrorAs(t, callErr, &pending, "the process join must be available to the run owner")
	require.NotNil(t, pending.ExecutionDone())
	assert.True(t, pending.Done() == pending.ExecutionDone(), "both receipts must name the same process owner")
	assert.ErrorIs(t, callErr, context.Canceled)

	select {
	case <-pending.ExecutionDone():
	case <-time.After(2 * time.Second):
		t.Fatal("the owned child and request reader were not positively joined")
	}
	require.NotNil(t, proc.ProcessState, "ExecutionDone closed before cmd.Wait completed")

	wire, err := os.ReadFile(wireFile)
	require.NoError(t, err)
	callCount := 0
	for _, method := range strings.Fields(string(wire)) {
		if method == "tools/call" {
			callCount++
		}
	}
	assert.Equal(t, 1, callCount, "an unknown tools/call must not be automatically replayed")
	require.NoError(t, adapter.Disconnect(context.Background()))
	assert.NoError(t, adapter.Disconnect(context.Background()), "repeated Disconnect must not start a second Wait")
}

func TestOwnedStdioDisconnectJoinsActiveSilentCall(t *testing.T) {
	adapter, pidFile, wireFile := newOwnedStdioHelperAdapter(t, "silent-call")
	require.NoError(t, adapter.Connect(context.Background()))
	waitForHelperFile(t, pidFile)
	proc := adapter.adapterCore.proc
	require.NotNil(t, proc)

	callDone := make(chan error, 1)
	go func() {
		_, err := adapter.CallTool(context.Background(), "silent", nil)
		callDone <- err
	}()
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(wireFile)
		return err == nil && strings.Contains(string(data), "tools/call")
	}, 2*time.Second, 5*time.Millisecond, "child should receive the one tool call")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, adapter.Disconnect(ctx))
	select {
	case err := <-callDone:
		require.Error(t, err, "killing an in-flight call cannot produce a successful result")
		var unknown interface{ OutcomeUnknown() bool }
		require.ErrorAs(t, err, &unknown)
		assert.True(t, unknown.OutcomeUnknown(), "Disconnect cannot establish the remote tool outcome")
		var pending interface{ ExecutionDone() <-chan struct{} }
		require.ErrorAs(t, err, &pending)
		select {
		case <-pending.ExecutionDone():
		case <-time.After(time.Second):
			t.Fatal("CallTool returned before the shared child and reader completion")
		}
	case <-time.After(time.Second):
		t.Fatal("Disconnect returned before the request reader unwound")
	}
	require.NotNil(t, proc.ProcessState, "Disconnect returned before cmd.Wait joined the child")
	assert.NoError(t, adapter.Disconnect(context.Background()), "repeated Disconnect is idempotent")

	wire, err := os.ReadFile(wireFile)
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(string(wire), "tools/call"), "Disconnect must not replay a dispatched call")
}

func TestOwnedStdioCancelAndDisconnectShareOneJoin(t *testing.T) {
	adapter, pidFile, wireFile := newOwnedStdioHelperAdapter(t, "silent-call")
	require.NoError(t, adapter.Connect(context.Background()))
	waitForHelperFile(t, pidFile)
	proc := adapter.adapterCore.proc
	owner := adapter.adapterCore.procOwner
	require.NotNil(t, proc)
	require.NotNil(t, owner)

	callCtx, cancelCall := context.WithCancel(context.Background())
	defer cancelCall()
	callDone := make(chan error, 1)
	go func() {
		_, err := adapter.CallTool(callCtx, "silent", nil)
		callDone <- err
	}()
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(wireFile)
		return err == nil && strings.Contains(string(data), "tools/call")
	}, 2*time.Second, 5*time.Millisecond, "child should receive the one tool call")
	owner.mu.Lock()
	activeCall := owner.activeIO > 0
	owner.mu.Unlock()
	require.True(t, activeCall, "the call should be inside owned transport I/O before teardown races start")

	start := make(chan struct{})
	disconnectDone := make(chan error, 1)
	go func() {
		<-start
		cancelCall()
	}()
	go func() {
		<-start
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		disconnectDone <- adapter.Disconnect(ctx)
	}()
	close(start)

	var callErr error
	select {
	case callErr = <-callDone:
	case <-time.After(2 * time.Second):
		t.Fatal("canceled and disconnected CallTool remained blocked on the silent child")
	}
	require.Error(t, callErr, "teardown cannot turn a dispatched call into success")
	var unknown interface{ OutcomeUnknown() bool }
	require.ErrorAs(t, callErr, &unknown)
	assert.True(t, unknown.OutcomeUnknown(), "no response leaves the remote tool outcome unknown")
	var pending interface{ ExecutionDone() <-chan struct{} }
	require.ErrorAs(t, callErr, &pending)
	select {
	case <-pending.ExecutionDone():
	case <-time.After(2 * time.Second):
		t.Fatal("request completion did not join the same owned child")
	}
	select {
	case err := <-disconnectDone:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Disconnect did not join the concurrent request teardown")
	}
	require.NotNil(t, proc.ProcessState, "the shared completion receipt closed before cmd.Wait")
	assert.True(t, pending.ExecutionDone() == owner.done, "request and Disconnect must observe the same owner completion")
	require.NoError(t, adapter.Disconnect(context.Background()), "repeated Disconnect must not start a second Wait")

	wire, err := os.ReadFile(wireFile)
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(string(wire), "tools/call"), "racing teardown must not replay a dispatched call")
}

func TestOwnedStdioResponseBudgetRejectsOversizedFramesAndJoinsChild(t *testing.T) {
	for _, mode := range []string{"oversized-line", "oversized-fragment", "ignored-lines-over-budget"} {
		t.Run(mode, func(t *testing.T) {
			adapter, pidFile, wireFile := newOwnedStdioHelperAdapter(t, mode)
			require.NoError(t, adapter.Connect(context.Background()))
			waitForHelperFile(t, pidFile)
			proc := adapter.adapterCore.proc
			owner := adapter.adapterCore.procOwner
			require.NotNil(t, proc)
			require.NotNil(t, owner)

			callCtx, cancelCall := context.WithTimeout(context.Background(), time.Second)
			defer cancelCall()
			_, callErr := adapter.CallTool(callCtx, "oversized", nil)
			require.Error(t, callErr)
			var protocolErr interface{ isStdioProtocolError() }
			require.ErrorAs(t, callErr, &protocolErr, "oversize must be reported as a typed stdio protocol failure")
			var unknown interface{ OutcomeUnknown() bool }
			require.ErrorAs(t, callErr, &unknown)
			assert.True(t, unknown.OutcomeUnknown(), "an oversized reply cannot establish the dispatched call outcome")
			var pending interface{ ExecutionDone() <-chan struct{} }
			require.ErrorAs(t, callErr, &pending)
			assert.True(t, pending.ExecutionDone() == owner.done, "the request must expose its exact owned-process receipt")
			select {
			case <-pending.ExecutionDone():
			case <-time.After(3 * time.Second):
				t.Fatal("oversized response did not close the owned transport and join its child")
			}
			require.NotNil(t, proc.ProcessState, "response completion closed before cmd.Wait")

			wire, err := os.ReadFile(wireFile)
			require.NoError(t, err)
			assert.Equal(t, 1, strings.Count(string(wire), "tools/call"), "a protocol error must not replay the dispatched call")
			require.NoError(t, adapter.Disconnect(context.Background()))
		})
	}
}

func TestOwnedStdioResponseBudgetAcceptsExactBoundary(t *testing.T) {
	adapter, pidFile, _ := newOwnedStdioHelperAdapter(t, "boundary-line")
	require.NoError(t, adapter.Connect(context.Background()))
	waitForHelperFile(t, pidFile)

	callCtx, cancelCall := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelCall()
	result, err := adapter.CallTool(callCtx, "boundary", nil)
	require.NoError(t, err, "a complete response at the byte limit should be accepted")
	content, ok := result.Content.(string)
	require.True(t, ok, "helper should return its boundary payload as a string")
	assert.Len(t, content, maxHTTPResponseSize-len(`{"jsonrpc":"2.0","id":1,"result":{"content":""}}`)-1)
	require.NoError(t, adapter.Disconnect(context.Background()))
}

func TestOwnedStdioConnectRecoversAfterCanceledCallWithoutReplay(t *testing.T) {
	adapter, pidFile, wireFile := newOwnedStdioHelperAdapter(t, "silent-call")
	require.NoError(t, adapter.Connect(context.Background()))
	waitForHelperFile(t, pidFile)
	oldOwner := adapter.adapterCore.procOwner
	require.NotNil(t, oldOwner)

	callCtx, cancelCall := context.WithCancel(context.Background())
	callDone := make(chan error, 1)
	go func() {
		_, err := adapter.CallTool(callCtx, "silent", nil)
		callDone <- err
	}()
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(wireFile)
		return err == nil && strings.Contains(string(data), "tools/call")
	}, 2*time.Second, 5*time.Millisecond, "child should receive the one tool call")
	cancelCall()
	callErr := <-callDone
	require.Error(t, callErr)
	var pending interface{ ExecutionDone() <-chan struct{} }
	require.ErrorAs(t, callErr, &pending)
	select {
	case <-pending.ExecutionDone():
	case <-time.After(2 * time.Second):
		t.Fatal("canceled call did not positively join its child")
	}

	require.NoError(t, adapter.Connect(context.Background()), "an explicit Connect should replace an already-stopped stdio generation")
	adapter.adapterCore.mu.Lock()
	newOwner := adapter.adapterCore.procOwner
	connected := adapter.adapterCore.connected
	adapter.adapterCore.mu.Unlock()
	require.NotNil(t, newOwner)
	if newOwner == oldOwner {
		t.Error("Connect must not report a healthy session backed by a completed owner")
	}
	assert.True(t, connected, "successful recovery should publish the new session")
	wire, err := os.ReadFile(wireFile)
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(string(wire), "tools/call"), "recovery must not replay the outcome-unknown call")
	require.NoError(t, adapter.Disconnect(context.Background()))
}

type cancelCauseWriter struct {
	writer        io.Writer
	cancel        context.CancelCauseFunc
	cause         error
	writeReady    chan struct{}
	continueWrite <-chan struct{}
}

func (w cancelCauseWriter) Write(data []byte) (int, error) {
	n, err := w.writer.Write(data)
	if err == nil {
		close(w.writeReady)
		<-w.continueWrite
		w.cancel(w.cause)
	}
	return n, err
}

func TestOwnedStdioRequestPreservesPreWriteCancelCause(t *testing.T) {
	adapter, _, wireFile := newOwnedStdioHelperAdapter(t, "silent-call")
	require.NoError(t, adapter.Connect(context.Background()))

	cause := errors.New("request canceled before dispatch")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(cause)
	_, err := adapter.adapterCore.conn.Request(ctx, "tools/call", nil)
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, err, cause, "pre-write cancellation should preserve its caller-supplied cause")
	assert.False(t, requestMayHaveBeenDispatched(err), "pre-write cancellation must not claim dispatch")
	wire, readErr := os.ReadFile(wireFile)
	require.NoError(t, readErr)
	assert.NotContains(t, string(wire), "tools/call")
	require.NoError(t, adapter.Disconnect(context.Background()))
}

func TestOwnedStdioRequestPreservesCancelCauseAfterDispatch(t *testing.T) {
	adapter, _, wireFile := newOwnedStdioHelperAdapter(t, "silent-call")
	require.NoError(t, adapter.Connect(context.Background()))
	owner := adapter.adapterCore.procOwner
	conn := adapter.adapterCore.conn
	require.NotNil(t, owner)
	require.NotNil(t, conn)

	cause := errors.New("request canceled after dispatch")
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	writeReady := make(chan struct{})
	continueWrite := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(continueWrite)
		}
	}()
	conn.out = cancelCauseWriter{
		writer:        conn.out,
		cancel:        cancel,
		cause:         cause,
		writeReady:    writeReady,
		continueWrite: continueWrite,
	}
	requestDone := make(chan error, 1)
	go func() {
		_, requestErr := conn.Request(ctx, "tools/call", nil)
		requestDone <- requestErr
	}()
	select {
	case <-writeReady:
	case <-time.After(2 * time.Second):
		t.Fatal("request writer did not reach the post-write barrier")
	}
	require.Eventually(t, func() bool {
		data, readErr := os.ReadFile(wireFile)
		return readErr == nil && strings.Contains(string(data), "tools/call")
	}, 2*time.Second, 5*time.Millisecond, "child should receive the write before cancellation")
	close(continueWrite)
	released = true
	var err error
	select {
	case err = <-requestDone:
	case <-time.After(2 * time.Second):
		t.Fatal("canceled request remained blocked after the write completed")
	}
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, err, cause, "post-write cancellation should preserve its caller-supplied cause")
	assert.True(t, requestMayHaveBeenDispatched(err), "the successful write leaves the tool outcome unknown")
	var unknown interface{ OutcomeUnknown() bool }
	require.ErrorAs(t, err, &unknown)
	assert.True(t, unknown.OutcomeUnknown())
	var pending interface{ ExecutionDone() <-chan struct{} }
	require.ErrorAs(t, err, &pending)
	assert.True(t, pending.ExecutionDone() == owner.done)
	select {
	case <-pending.ExecutionDone():
	case <-time.After(2 * time.Second):
		t.Fatal("post-dispatch cancellation did not join the owned child")
	}
	wire, readErr := os.ReadFile(wireFile)
	require.NoError(t, readErr)
	assert.Equal(t, 1, strings.Count(string(wire), "tools/call"))
	assert.NotNil(t, adapter.adapterCore.proc.ProcessState)
	assert.NoError(t, adapter.Disconnect(context.Background()))
}

func TestOwnedStdioTransportCloseJoinsActiveSilentCall(t *testing.T) {
	adapter, pidFile, wireFile := newOwnedStdioHelperAdapter(t, "silent-call")
	require.NoError(t, adapter.Connect(context.Background()))
	waitForHelperFile(t, pidFile)
	proc := adapter.adapterCore.proc
	conn := adapter.adapterCore.conn
	require.NotNil(t, proc)
	require.NotNil(t, conn)

	callDone := make(chan error, 1)
	go func() {
		_, err := adapter.CallTool(context.Background(), "silent", nil)
		callDone <- err
	}()
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(wireFile)
		return err == nil && strings.Contains(string(data), "tools/call")
	}, 2*time.Second, 5*time.Millisecond, "child should receive the one tool call")

	closeDone := make(chan error, 1)
	go func() { closeDone <- conn.Close() }()
	select {
	case err := <-closeDone:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("transport Close remained blocked after closing its owned pipes")
	}
	select {
	case err := <-callDone:
		require.Error(t, err, "Close cannot turn an in-flight call into success")
	case <-time.After(time.Second):
		t.Fatal("transport Close returned before the request reader unwound")
	}
	require.NotNil(t, proc.ProcessState, "transport Close returned before cmd.Wait joined the child")
	require.NoError(t, adapter.Disconnect(context.Background()))
}

func TestOwnedStdioDisconnectDeadlineRetainsSameOwnerUntilDone(t *testing.T) {
	adapter, pidFile, _ := newOwnedStdioHelperAdapter(t, "silent-call")
	require.NoError(t, adapter.Connect(context.Background()))
	waitForHelperFile(t, pidFile)
	owner := adapter.adapterCore.procOwner
	require.NotNil(t, owner)
	proc := adapter.adapterCore.proc
	require.NotNil(t, proc)

	// Reserve the owner's tracked-I/O count as a deterministic join barrier.
	// The other lifecycle tests exercise the real blocking pipe read.
	require.True(t, owner.beginIO())
	released := false
	defer func() {
		if !released {
			owner.endIO()
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := adapter.Disconnect(ctx)
	require.ErrorIs(t, err, context.Canceled)
	var pending interface{ Done() <-chan struct{} }
	require.ErrorAs(t, err, &pending)
	require.True(t, pending.Done() == owner.done, "pending Disconnect must expose the exact retained owner")
	assert.Same(t, owner, adapter.adapterCore.procOwner, "the process owner must remain attached while Wait is pending")

	errAgain := adapter.Disconnect(ctx)
	require.ErrorIs(t, errAgain, context.Canceled)
	var pendingAgain interface{ Done() <-chan struct{} }
	require.ErrorAs(t, errAgain, &pendingAgain)
	require.True(t, pendingAgain.Done() == pending.Done(), "repeated Disconnect must join the same cleanup operation")
	select {
	case <-pending.Done():
		t.Fatal("cleanup Done closed while an owned transport operation was still active")
	default:
	}

	owner.endIO()
	released = true
	select {
	case <-pending.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("shared Disconnect completion did not close after the join barrier released")
	}
	require.NoError(t, adapter.Disconnect(context.Background()))
	require.NotNil(t, proc.ProcessState, "shared Done closed before cmd.Wait completed")
}
