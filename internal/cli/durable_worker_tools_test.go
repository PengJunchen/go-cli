package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/approval"
	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/extension"
	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/tools"
	api "github.com/pengjunchen/go-cli/pkg/extensionapi/v1"
)

func TestDurableWorkerProcess(t *testing.T) {
	if os.Getenv("GO_CLI_DURABLE_WORKER") != "1" {
		return
	}
	appendMarker := func(path, value string) {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			os.Exit(4)
		}
		if _, err := file.WriteString(value); err != nil {
			os.Exit(4)
		}
		if err := file.Close(); err != nil {
			os.Exit(4)
		}
	}
	appendMarker(os.Getenv("GO_CLI_WORKER_STARTS"), "start\n")
	manifest := api.Manifest{ID: "durable.worker", Version: "1", HostAPI: api.Version, Capabilities: []string{"tools"}, Tools: []api.Tool{{Name: "worker_echo", Description: "owned worker", Parameters: json.RawMessage(`{"type":"object","properties":{"fail":{"type":"boolean"}},"additionalProperties":false}`)}}}
	handler := api.HandlerFunc(func(ctx context.Context, request api.Request) (json.RawMessage, error) {
		appendMarker(os.Getenv("GO_CLI_WORKER_CALLS"), "call\n")
		switch os.Getenv("GO_CLI_WORKER_MODE") {
		case "null":
			return json.RawMessage(`null`), nil
		case "missing-output":
			return json.RawMessage(`{}`), nil
		case "duplicate-output":
			return json.RawMessage(`{"output":"first","output":"second"}`), nil
		case "disconnect":
			os.Exit(0)
		case "timeout":
			<-ctx.Done()
			// Withhold the response until the host terminates this worker. Returning
			// ctx.Err() lets Serve emit a known callback_failed rejection which can
			// beat the host's deadline callback and legitimately carry a join proof.
			select {}
		case "deadline-rejection":
			return nil, context.DeadlineExceeded
		case "wrong-id":
			frame, _ := json.Marshal(api.Response{ID: request.ID + 100, Result: json.RawMessage(`{"output":"wrong occurrence"}`)})
			_, _ = os.Stdout.Write(append(frame, '\n'))
			<-ctx.Done()
			return nil, ctx.Err()
		}
		var call api.ToolCall
		if err := json.Unmarshal(request.Params, &call); err != nil {
			return nil, err
		}
		fail, _ := call.Args["fail"].(bool)
		return json.Marshal(api.ToolResult{Output: "actual owned worker result", IsError: fail})
	})
	if err := api.Serve(context.Background(), os.Stdin, os.Stdout, manifest, handler); err != nil {
		os.Exit(3)
	}
	os.Exit(0)
}

func durableWorkerConfigWithTimeout(t *testing.T, mode string, timeoutMillis int) (*config.Config, string, string) {
	t.Helper()
	configureDurableCLISessionTest(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	t.Chdir(root)
	executable, err := os.Executable()
	require.NoError(t, err)
	artifact, err := os.ReadFile(executable)
	require.NoError(t, err)
	digest := sha256.Sum256(artifact)
	calls, starts := filepath.Join(root, "calls"), filepath.Join(root, "starts")
	descriptor := extension.RPCProcessConfig{Command: []string{executable, "-test.run=^TestDurableWorkerProcess$"}, Cwd: root, Env: map[string]string{"GO_CLI_DURABLE_WORKER": "1", "GO_CLI_WORKER_MODE": mode, "GO_CLI_WORKER_CALLS": calls, "GO_CLI_WORKER_STARTS": starts}, ArtifactSHA256: hex.EncodeToString(digest[:]), TimeoutMillis: timeoutMillis}
	encoded, err := json.Marshal(descriptor)
	require.NoError(t, err)
	path := filepath.Join(root, "worker.rpc.json")
	require.NoError(t, os.WriteFile(path, encoded, 0o600))
	cfg := newAssembleTestConfig(t)
	cfg.Git.WorkDir = root
	cfg.Session.StorePath = filepath.Join(t.TempDir(), "sessions")
	enabled, disabled := true, false
	cfg.Extensions.Enabled, cfg.Extensions.PluginPaths = &enabled, []string{path}
	cfg.Memory.Enabled = &disabled
	return cfg, calls, starts
}

func TestPromptDurableOwnedWorkerResultsAndProofSurviveResume(t *testing.T) {
	cfg, calls, starts := durableWorkerConfigWithTimeout(t, "normal", 0)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	server, _ := newDurablePromptServer(t,
		durableApprovalToolResponse(t, "worker-first", "worker_echo", map[string]any{}),
		durableApprovalToolResponse(t, "worker-error", "worker_echo", map[string]any{"fail": true}),
		`{"choices":[{"message":{"role":"assistant","content":"worker results observed"}}]}`,
		`{"choices":[{"message":{"role":"assistant","content":"resumed without replay"}}]}`)
	defer server.Close()
	cfg.Provider.BaseURL = server.URL
	var output bytes.Buffer
	require.NoError(t, newPromptCmd(&output).Run(ctx, cfg, []string{"--approve", "auto", "call worker twice"}))
	id := physicalSessionIDFromOutput(t, output.String())
	startBytes, err := os.ReadFile(starts)
	require.NoError(t, err)
	require.Equal(t, "start\n", string(startBytes), "two requests reuse one initialized worker")
	resumed, err := newDurableCLISession(ctx, cfg.Session.StorePath, "inspect", id, "")
	require.NoError(t, err)
	snapshot, err := resumed.snapshotSelected()
	require.NoError(t, err)
	known := map[string]string{}
	proofs := 0
	for _, row := range snapshot.DurableEntries {
		if row.ToolName != "worker_echo" {
			continue
		}
		if row.ActionState == session.ActionStateIntent {
			require.Equal(t, []tools.TrustedToolEffect{tools.EffectUnknown}, row.EffectSet)
		}
		if row.Type == session.EntryTypeTool && row.ActionState == "" {
			known[row.ToolCallID] = row.ReceiptOutcome
		}
		if row.CompletionProof != "" {
			require.Equal(t, tools.CompletionWorkerRequestJoined, row.CompletionProof)
			proofs++
		}
	}
	require.Equal(t, core.ReceiptOutcomeSuccess, known["worker-first"])
	require.Equal(t, core.ReceiptOutcomeError, known["worker-error"])
	require.GreaterOrEqual(t, proofs, 2)
	pending, err := session.InspectPendingActions(snapshot)
	require.NoError(t, err)
	require.Empty(t, pending)
	require.NoError(t, resumed.requestClose(ctx))
	runFreshWorkerResumeHost(ctx, t, cfg, id, false)
	callBytes, err := os.ReadFile(calls)
	require.NoError(t, err)
	require.Equal(t, "call\ncall\n", string(callBytes), "physical-session resume never replays worker calls")
}

func TestPromptDurableOwnedWorkerUnknownRetainsPendingWithoutReplay(t *testing.T) {
	for _, tc := range []struct {
		mode  string
		cause string
	}{
		{mode: "null", cause: "RPC value must be an object"},
		{mode: "missing-output", cause: "RPC tool result requires string output"},
		{mode: "duplicate-output", cause: "duplicate RPC object field"},
		{mode: "wrong-id", cause: "unknown or duplicate RPC response id"},
		{mode: "disconnect", cause: "EOF"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			cfg, calls, _ := durableWorkerConfigWithTimeout(t, tc.mode, 0)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			server, script := newDurablePromptServer(t, durableApprovalToolResponse(t, "unknown-worker", "worker_echo", map[string]any{}))
			defer server.Close()
			cfg.Provider.BaseURL = server.URL
			var output bytes.Buffer
			runErr := newPromptCmd(&output).Run(ctx, cfg, []string{"--approve", "auto", "one effect"})
			require.Error(t, runErr)
			require.NoError(t, ctx.Err(), "worker initialization and the protocol fault must precede the caller deadline")
			var unknown *extension.RPCOutcomeUnknownError
			require.ErrorAs(t, runErr, &unknown)
			require.ErrorIs(t, runErr, core.ErrRunCommitQuiescenceUnproven)
			require.NotErrorIs(t, runErr, context.DeadlineExceeded)
			require.ErrorContains(t, runErr, tc.cause)
			require.Len(t, script.requestBodies(), 1)
			id := physicalSessionIDFromOutput(t, output.String())
			assertUnknownWorkerPending(ctx, t, cfg, id, calls, tc.mode == "null")
			require.Len(t, script.requestBodies(), 1)
		})
	}
}

func assertUnknownWorkerPending(ctx context.Context, t *testing.T, cfg *config.Config, id, calls string, checkFreshHost bool) {
	t.Helper()
	resumed, err := newDurableCLISession(ctx, cfg.Session.StorePath, "unknown inspect", id, "")
	require.NoError(t, err)
	closed := false
	defer func() {
		if !closed {
			cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			require.NoError(t, resumed.requestClose(cleanup))
		}
	}()
	snapshot, err := resumed.snapshotSelected()
	require.NoError(t, err)
	pending, err := session.InspectPendingActions(snapshot)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	intents, receipts := 0, 0
	for _, row := range snapshot.DurableEntries {
		if row.ToolCallID != "unknown-worker" {
			continue
		}
		require.Empty(t, row.CompletionProof)
		require.NotEqual(t, session.CheckpointExecutionResolution, row.CheckpointKind)
		switch {
		case row.ActionState == session.ActionStateIntent:
			intents++
		case row.Type == session.EntryTypeTool && row.ActionState == "":
			receipts++
			require.Equal(t, core.ReceiptOutcomeUnknown, row.ReceiptOutcome)
			require.Equal(t, core.DispatchStateDispatched, row.DispatchState)
		}
	}
	require.Equal(t, 1, intents)
	require.Equal(t, 1, receipts)
	before, err := os.ReadFile(calls)
	require.NoError(t, err)
	require.Equal(t, "call\n", string(before))
	if !checkFreshHost {
		return
	}
	require.NoError(t, resumed.requestClose(ctx))
	closed = true
	runFreshWorkerResumeHost(ctx, t, cfg, id, true)
	after, err := os.ReadFile(calls)
	require.NoError(t, err)
	require.Equal(t, before, after, "a fresh host must never replay an unknown worker request")
}

// The caller deadline starts after the real worker has initialized, so this
// test cannot mistake a slow startup for a dispatched request timing out.
func TestDurableWorkerCallerDeadlineAfterInitialize(t *testing.T) {
	cfg, calls, starts := durableWorkerConfigWithTimeout(t, "timeout", 0)
	outer, cancelOuter := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancelOuter()
	server, script := newDurablePromptServer(t, durableApprovalToolResponse(t, "unknown-worker", "worker_echo", map[string]any{}))
	defer server.Close()
	cfg.Provider.BaseURL = server.URL
	durable, err := newDurableCLISession(outer, cfg.Session.StorePath, "prompt", "", "")
	require.NoError(t, err)
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		require.NoError(t, durable.requestClose(cleanup))
	}()
	assembly, err := AssembleAgent(outer, cfg, "openai", "test-model", io.Discard, WithApproveMode(ApproveAuto), withDurableCLISession(durable))
	require.NoError(t, err)
	closed := false
	defer func() {
		if !closed {
			cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			require.NoError(t, closeAssemblyForCommand(cleanup, assembly, "prompt"))
		}
	}()
	id := assembly.PhysicalSessionID
	require.NotEmpty(t, id)
	started, err := os.ReadFile(starts)
	require.NoError(t, err)
	require.Equal(t, "start\n", string(started))

	caller, cancelCaller := context.WithTimeout(outer, 5*time.Second)
	defer cancelCaller()
	runCtx, sink, err := durable.bindRun(caller)
	require.NoError(t, err)
	defer sink.CloseRunCommit()
	require.NoError(t, hydrateDurableRunHistory(assembly.Agent, sink))
	stream, err := assembly.Harness.Submit(runCtx, "one effect")
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() {
		for range stream.Events() {
		}
		_, runErr := stream.Result()
		done <- runErr
	}()
	joined := false
	defer func() {
		cancelCaller()
		if !joined {
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Error("harness did not join after caller cancellation")
			}
		}
	}()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		data, readErr := os.ReadFile(calls)
		if readErr == nil && string(data) == "call\n" {
			require.NoError(t, caller.Err(), "the call must be dispatched before its deadline")
			break
		}
		select {
		case earlyErr := <-done:
			joined = true
			t.Fatalf("harness returned before the worker call: %v", earlyErr)
		case <-caller.Done():
			t.Fatal("caller deadline expired before the worker call was observed")
		case <-ticker.C:
		}
	}
	var runErr error
	select {
	case runErr = <-done:
		joined = true
	case <-outer.Done():
		t.Fatal("outer context expired before the harness joined")
	}
	require.ErrorIs(t, caller.Err(), context.DeadlineExceeded)
	require.NoError(t, outer.Err())
	var unknown *extension.RPCOutcomeUnknownError
	require.ErrorAs(t, runErr, &unknown)
	require.ErrorIs(t, runErr, context.DeadlineExceeded)
	require.ErrorIs(t, runErr, core.ErrRunCommitQuiescenceUnproven)
	require.Len(t, script.requestBodies(), 1)
	sink.CloseRunCommit()
	select {
	case <-unknown.ExecutionDone():
	case <-outer.Done():
		t.Fatal("the exact worker owner did not join")
	}
	cleanup, cancelCleanup := context.WithTimeout(context.Background(), 10*time.Second)
	require.NoError(t, closeAssemblyForCommand(cleanup, assembly, "prompt"))
	cancelCleanup()
	closed = true
	inspect, cancelInspect := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancelInspect()
	assertUnknownWorkerPending(inspect, t, cfg, id, calls, true)
	require.Len(t, script.requestBodies(), 1)
}

func TestDurableOwnedWorkerKnownDeadlineRejectionHasJoinedProof(t *testing.T) {
	cfg, calls, starts := durableWorkerConfigWithTimeout(t, "deadline-rejection", 0)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	assembly, err := AssembleAgent(ctx, cfg, "openai", "test-model", io.Discard, WithApproveMode(ApproveAuto))
	require.NoError(t, err)
	defer assembly.Cleanup()
	preparer, ok := assembly.ToolRegistry.(tools.ToolCallPreparer)
	require.True(t, ok)
	for _, id := range []string{"first-rejection", "second-rejection"} {
		invocation, err := preparer.PrepareToolCall(ctx, tools.ToolCall{ID: id, Name: "worker_echo", Args: map[string]any{}})
		require.NoError(t, err)
		_, err = invocation.Dispatch(ctx)
		require.ErrorContains(t, err, "callback_failed")
		require.NoError(t, ctx.Err(), "the remote rejection precedes the host deadline")
		select {
		case <-invocation.CompletionSignal().Done():
		default:
			t.Fatal("known rejection returned before its exact request joined")
		}
		require.Equal(t, tools.CompletionWorkerRequestJoined, invocation.CompletionSignal().Proof())
	}
	data, err := os.ReadFile(calls)
	require.NoError(t, err)
	require.Equal(t, "call\ncall\n", string(data))
	data, err = os.ReadFile(starts)
	require.NoError(t, err)
	require.Equal(t, "start\n", string(data), "known rejections keep the initialized worker reusable")
}

func TestDurableOwnedWorkerDeniedSendsNoToolFrame(t *testing.T) {
	for _, mode := range []string{"deny", "plan", "replacement"} {
		t.Run(mode, func(t *testing.T) {
			cfg, calls, _ := durableWorkerConfigWithTimeout(t, "normal", 0)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			server, _ := newDurablePromptServer(t, durableApprovalToolResponse(t, "denied-worker", "worker_echo", map[string]any{}), `{"choices":[{"message":{"role":"assistant","content":"denial observed"}}]}`)
			defer server.Close()
			cfg.Provider.BaseURL = server.URL
			approvalMode := ApproveAuto
			if mode == "deny" {
				approvalMode = ApproveDeny
			}
			durable, err := newDurableCLISession(ctx, cfg.Session.StorePath, "denied worker", "", "")
			require.NoError(t, err)
			defer func() { require.NoError(t, durable.requestClose(context.Background())) }()
			assembly, err := AssembleAgent(ctx, cfg, "openai", "test-model", io.Discard, WithApproveMode(approvalMode), withDurableCLISession(durable))
			require.NoError(t, err)
			defer assembly.Cleanup()
			assembly.LoopAgent.WithToolSearchThreshold(0)
			if mode == "plan" {
				require.NoError(t, assembly.PlanCtrl.Enter(ctx, "read only"))
			}
			if mode == "replacement" {
				require.NoError(t, assembly.ToolRegistry.Register(ctx, tools.NewCustomCommandTool("worker_echo", "", []string{"/bin/sh", "-c", "printf forbidden > '" + calls + "'"}, nil, nil, 0, "")))
			}
			runCtx, sink, err := durable.bindRun(ctx)
			require.NoError(t, err)
			require.NoError(t, hydrateDurableRunHistory(assembly.Agent, sink))
			stream, err := assembly.Harness.Submit(runCtx, "observe denial")
			require.NoError(t, err)
			for range stream.Events() {
			}
			_, err = stream.Result()
			require.NoError(t, err)
			sink.CloseRunCommit()
			_, err = os.Stat(calls)
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestProjectOwnedWorkerCapturedDescriptorAndTrustFence(t *testing.T) {
	base, calls, starts := durableWorkerConfigWithTimeout(t, "normal", 0)
	root := base.Git.WorkDir
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	require.NoError(t, os.WriteFile(filepath.Join(root, ".go-cli.json"), []byte(`{"extensions":{"enabled":true,"plugin_paths":["worker.rpc.json"]}}`), 0o600))
	manager := approval.NewDefaultTrustManager(approval.NewInMemoryTrustStore())
	pending, err := config.LoadTrustedWithManager(ctx, manager)
	require.NoError(t, err)
	grant := manager.(approval.FrozenProjectSourceTrust)
	require.NoError(t, grant.TrustProjectSourceSnapshot(ctx, pending.CapturedProjectSourcesForGrant()))
	cfg, err := config.LoadTrustedWithManager(ctx, manager)
	require.NoError(t, err)
	require.Equal(t, config.ProjectSourceTrustVerified, cfg.ProjectSourceTrustStatus)
	cfg.Provider, cfg.Git, cfg.Session = base.Provider, base.Git, base.Session
	cfg.Memory, cfg.History, cfg.Production = base.Memory, base.History, base.Production
	// The verified descriptor remains authoritative even when its live path is
	// replaced before the actual worker is initialized.
	require.NoError(t, os.WriteFile(filepath.Join(root, "worker.rpc.json"), []byte(`{"command":["/must-not-start-replaced-worker"],"artifact_sha256":"bad"}`), 0o600))
	assembly, err := AssembleAgent(ctx, cfg, "openai", "test-model", io.Discard, WithApproveMode(ApproveAuto))
	require.NoError(t, err)
	defer assembly.Cleanup()
	preparer, ok := assembly.ToolRegistry.(tools.ToolCallPreparer)
	require.True(t, ok)
	prepared, err := preparer.PrepareToolCall(ctx, tools.ToolCall{ID: "captured-worker", Name: "worker_echo", Args: map[string]any{}})
	require.NoError(t, err)
	require.Equal(t, tools.RegistrationSourceProjectSourced, prepared.ToolIdentity().RegistrationSource())
	supported, err := tools.PreparedCompletionSupported(ctx, prepared, nil)
	require.NoError(t, err)
	require.True(t, supported)
	result, err := prepared.Dispatch(ctx)
	require.NoError(t, err)
	require.Equal(t, "actual owned worker result", result.Output)
	require.Equal(t, tools.CompletionWorkerRequestJoined, prepared.CompletionSignal().Proof())
	stale, err := preparer.PrepareToolCall(ctx, tools.ToolCall{ID: "revoked-worker", Name: "worker_echo", Args: map[string]any{}})
	require.NoError(t, err)
	require.NoError(t, manager.RevokeTrust(ctx, root))
	_, err = stale.Dispatch(ctx)
	require.Error(t, err)
	require.False(t, stale.DispatchCommitted())
	data, err := os.ReadFile(calls)
	require.NoError(t, err)
	require.Equal(t, "call\n", string(data), "revocation must send zero additional tool frames")
	data, err = os.ReadFile(starts)
	require.NoError(t, err)
	require.Equal(t, "start\n", string(data), "captured descriptor starts exactly its original worker")
}

// TestDurableWorkerResumeHostProcess runs in a fresh host process. Its worker
// subprocess is separate again, exercising persisted proof validation without
// inheriting any prepared invocation, registry, owner or in-memory store.
func TestDurableWorkerResumeHostProcess(t *testing.T) {
	path := os.Getenv("GO_CLI_WORKER_RESUME_CONFIG")
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var cfg config.Config
	require.NoError(t, json.Unmarshal(data, &cfg))
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	err = newPromptCmd(io.Discard).Run(ctx, &cfg, []string{"--approve", "auto", "--resume-session", os.Getenv("GO_CLI_WORKER_RESUME_SESSION"), "continue"})
	if os.Getenv("GO_CLI_WORKER_RESUME_UNKNOWN") == "1" {
		require.Error(t, err)
		require.Contains(t, err.Error(), "pending")
	} else {
		require.NoError(t, err)
	}
}

func runFreshWorkerResumeHost(ctx context.Context, t *testing.T, cfg *config.Config, id string, unknown bool) {
	t.Helper()
	encoded, err := json.Marshal(cfg)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "resume-config.json")
	require.NoError(t, os.WriteFile(path, encoded, 0o600))
	executable, err := os.Executable()
	require.NoError(t, err)
	command := exec.CommandContext(ctx, executable, "-test.run=^TestDurableWorkerResumeHostProcess$")
	command.Env = append(os.Environ(), "GO_CLI_WORKER_RESUME_CONFIG="+path, "GO_CLI_WORKER_RESUME_SESSION="+id)
	if unknown {
		command.Env = append(command.Env, "GO_CLI_WORKER_RESUME_UNKNOWN=1")
	}
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)
}
