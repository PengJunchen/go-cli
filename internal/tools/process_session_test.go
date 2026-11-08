package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

func newTestProcessSessionManager(ownerCtx context.Context, t *testing.T, maxOutput int) *LocalProcessSessionManager {
	t.Helper()
	manager, err := NewLocalProcessSessionManager(ownerCtx, ProcessSessionManagerConfig{
		OwnerID:     "owner-test",
		WorkspaceID: "workspace-test",
		Workdir:     t.TempDir(),
		Sandbox:     AllowAllSandbox{},
		MaxOutput:   maxOutput,
		ScopeResolver: func(context.Context) (ProcessSessionScope, error) {
			return ProcessSessionScope{OwnerID: "owner-test", WorkspaceID: "workspace-test", RunID: "agent-run-test"}, nil
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, manager.Close(context.Background())) })
	return manager
}

func TestProcessSessionToolStartReadStatusStopOwnsProcessAfterToolContextReturns(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ownerCtx, cancelOwner := context.WithCancel(context.Background())
	defer cancelOwner()
	manager := newTestProcessSessionManager(ownerCtx, t, 4096)
	tool := NewProcessSessionTool(manager)

	callCtx, cancelCall := context.WithCancel(context.Background())
	started, err := tool.Execute(callCtx, ToolCall{Name: "process_session", Args: map[string]any{
		"operation": "start", "command": "printf 'ready\\n'; exec sleep 60",
	}})
	require.NoError(t, err)
	cancelCall()
	sessionID, ok := started.Metadata["session_id"].(string)
	require.True(t, ok)
	require.NotEmpty(t, sessionID)
	assert.NotContains(t, sessionID, "/")

	var output *ToolResult
	require.Eventually(t, func() bool {
		var readErr error
		output, readErr = tool.Execute(context.Background(), ToolCall{Name: "process_session", Args: map[string]any{
			"operation": "read", "session_id": sessionID, "offset": 0, "max_bytes": 128,
		}})
		return readErr == nil && strings.Contains(output.Output, "ready")
	}, time.Second, 5*time.Millisecond)
	assert.Equal(t, ProcessSessionRunning, started.Metadata["status"])
	assert.EqualValues(t, 0, output.Metadata["offset"])
	assert.EqualValues(t, 6, output.Metadata["next_offset"])

	status, err := tool.Execute(context.Background(), ToolCall{Name: "process_session", Args: map[string]any{
		"operation": "status", "session_id": sessionID,
	}})
	require.NoError(t, err)
	assert.Equal(t, ProcessSessionRunning, status.Metadata["status"])

	stopped, err := tool.Execute(context.Background(), ToolCall{Name: "process_session", Args: map[string]any{
		"operation": "stop", "session_id": sessionID,
	}})
	require.NoError(t, err)
	assert.Equal(t, ProcessSessionStopped, stopped.Metadata["status"])
	finalStatus, err := tool.Execute(context.Background(), ToolCall{Name: "process_session", Args: map[string]any{
		"operation": "status", "session_id": sessionID,
	}})
	require.NoError(t, err)
	assert.Equal(t, ProcessSessionStopped, finalStatus.Metadata["status"])
}

func TestProcessSessionAdmissionAllowsHealthyJoinUntilAckAndRevokesOnFailure(t *testing.T) {
	for _, test := range []struct {
		name        string
		observerErr error
	}{
		{name: "healthy closure acknowledgement"},
		{name: "closure persistence failure revokes admission", observerErr: errors.New("closure persistence failed")},
	} {
		t.Run(test.name, func(t *testing.T) {
			ownerCtx, cancelOwner := context.WithCancel(context.Background())
			defer cancelOwner()
			closureEntered := make(chan ProcessSessionLeaseSnapshot, 1)
			closureRelease := make(chan struct{})
			observerReturned := make(chan error, 1)
			action := ProcessSessionActionRef{
				SessionID: "physical-session", WorkspaceID: "workspace-test", RunID: "agent-run-test",
				ActionBatchID: "batch-start", ActionID: "action-start", OwnerGeneration: "owner-generation",
				AssistantTranscriptOrdinal: 0, ToolCallID: "call-start",
				SchemaDigest: [32]byte{1}, ArgumentDigest: [32]byte{2},
			}
			manager, err := NewLocalProcessSessionManager(ownerCtx, ProcessSessionManagerConfig{
				OwnerID: "owner-test", WorkspaceID: "workspace-test", Workdir: t.TempDir(), Sandbox: AllowAllSandbox{},
				ScopeResolver: func(context.Context) (ProcessSessionScope, error) {
					return ProcessSessionScope{OwnerID: "owner-test", WorkspaceID: "workspace-test", RunID: "agent-run-test"}, nil
				},
			})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, manager.Close(context.Background())) })
			observer := func(_ context.Context, lease ProcessSessionLeaseSnapshot) error {
				if lease.Phase != ProcessSessionLeaseStopped {
					return nil
				}
				closureEntered <- lease
				<-closureRelease
				observerReturned <- test.observerErr
				return test.observerErr
			}
			startCtx := WithProcessSessionAction(context.Background(), action, observer)
			started, err := manager.Start(startCtx, "exec sleep 30")
			require.NoError(t, err)
			invocation := ProcessSessionInvocation{manager: manager, operation: "start"}
			var published ProcessSessionLeaseSnapshot
			require.NoError(t, invocation.WithStartLeasePublication(started.SessionID, func(snapshot ProcessSessionLeaseSnapshot) error {
				published = snapshot
				return nil
			}))
			require.Equal(t, ProcessSessionLeaseActive, published.Phase)
			capability, ok := manager.ManagerAdmissionCapability(action.RunID, action.OwnerGeneration)
			require.True(t, ok)

			stopDone := make(chan error, 1)
			go func() {
				_, stopErr := manager.Stop(context.Background(), started.SessionID)
				stopDone <- stopErr
			}()
			var stopped ProcessSessionLeaseSnapshot
			select {
			case stopped = <-closureEntered:
			case <-time.After(5 * time.Second):
				t.Fatal("process-session closure observer did not start")
			}
			assert.Equal(t, ProcessSessionLeaseStopped, stopped.Phase)
			assert.False(t, capability.AllowsLease(published), "physical-liveness checks must reject a joined process")
			assert.True(t, capability.AllowsLeaseForAdmission(published), "the exact durable Active lease remains admitted while healthy closure persistence is owned")
			select {
			case <-manager.SessionClosed(started.SessionID):
				t.Fatal("session closure was acknowledged before its durable observer returned")
			default:
			}

			close(closureRelease)
			select {
			case observerErr := <-observerReturned:
				if test.observerErr == nil {
					require.NoError(t, observerErr)
				} else {
					require.ErrorIs(t, observerErr, test.observerErr)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("process-session closure observer did not return")
			}
			select {
			case stopErr := <-stopDone:
				require.NoError(t, stopErr)
			case <-time.After(5 * time.Second):
				t.Fatal("process-session stop did not finish")
			}
			if test.observerErr == nil {
				select {
				case <-manager.SessionClosed(started.SessionID):
				case <-time.After(5 * time.Second):
					t.Fatal("durable closure acknowledgement did not arrive")
				}
				assert.False(t, capability.AllowsLeaseForAdmission(published), "the acknowledgement revokes the former Active lease")
			} else {
				assert.Eventually(t, func() bool { return !capability.AllowsLeaseForAdmission(published) }, 5*time.Second, time.Millisecond,
					"a failed closure observer must revoke admission")
				select {
				case <-manager.SessionClosed(started.SessionID):
					t.Fatal("failed closure persistence must not acknowledge the lease")
				default:
				}
			}
		})
	}
}

func TestProcessSessionOutputIsBoundedAndCursorReportsDroppedPrefix(t *testing.T) {
	manager := newTestProcessSessionManager(context.Background(), t, 8)
	tool := NewProcessSessionTool(manager)

	started, err := tool.Execute(context.Background(), ToolCall{Name: "process_session", Args: map[string]any{
		"operation": "start", "command": "printf '0123456789'; exit 0",
	}})
	require.NoError(t, err)
	sessionID := started.Metadata["session_id"].(string)

	var output *ToolResult
	require.Eventually(t, func() bool {
		var readErr error
		output, readErr = tool.Execute(context.Background(), ToolCall{Name: "process_session", Args: map[string]any{
			"operation": "read", "session_id": sessionID, "offset": 0, "max_bytes": 8,
		}})
		return readErr == nil && output.Metadata["status"] == ProcessSessionExited
	}, time.Second, 5*time.Millisecond)
	var readResult struct {
		Output string `json:"output"`
	}
	require.NoError(t, json.Unmarshal([]byte(output.Output), &readResult))
	assert.Equal(t, "23456789", readResult.Output)
	assert.True(t, output.Metadata["truncated"].(bool))
	assert.EqualValues(t, 2, output.Metadata["output_start_offset"])
	assert.EqualValues(t, 10, output.Metadata["total_bytes"])
	assert.EqualValues(t, 10, output.Metadata["next_offset"])
}

func TestProcessSessionToolRejectsInvalidAndUnknownOperations(t *testing.T) {
	tool := NewProcessSessionTool(newTestProcessSessionManager(context.Background(), t, 1024))

	_, err := tool.Execute(context.Background(), ToolCall{Name: "process_session", Args: map[string]any{"operation": "detach"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "operation")

	_, err = tool.Execute(context.Background(), ToolCall{Name: "process_session", Args: map[string]any{
		"operation": "status", "session_id": "unknown",
	}})
	require.ErrorIs(t, err, ErrProcessSessionNotFound)

	_, err = tool.Execute(context.Background(), ToolCall{Name: "process_session", Args: map[string]any{
		"operation": "start", "command": "printf no", "session_id": "not-allowed",
	}})
	require.Error(t, err)
}

func TestProcessSessionStartAppliesBashSandboxBeforeStartingAnything(t *testing.T) {
	ownerCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sandbox := processSessionRejectingSandbox{err: errors.New("sandbox denied")}
	manager, err := NewLocalProcessSessionManager(ownerCtx, ProcessSessionManagerConfig{
		OwnerID: "owner", WorkspaceID: "workspace", Workdir: t.TempDir(), Sandbox: sandbox,
		ScopeResolver: func(context.Context) (ProcessSessionScope, error) {
			return ProcessSessionScope{OwnerID: "owner", RunID: "agent-run", WorkspaceID: "workspace"}, nil
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, manager.Close(context.Background())) })
	tool := NewProcessSessionTool(manager)

	_, err = tool.Execute(context.Background(), ToolCall{Name: "process_session", Args: map[string]any{
		"operation": "start", "command": "printf forbidden",
	}})
	require.Error(t, err)
	assert.ErrorIs(t, err, sandbox.err)
	assert.Empty(t, manager.sessions)
}

func TestProcessSessionAdmissionRequiresExactManagerOwnedLease(t *testing.T) {
	if !ownedCommandPlatformSupported() {
		t.Skip("local process-group ownership is unsupported")
	}
	ownerCtx, cancelOwner := context.WithCancel(context.Background())
	defer cancelOwner()
	manager := newTestProcessSessionManager(ownerCtx, t, 1024)
	ref := ProcessSessionActionRef{
		SessionID: "durable-session", WorkspaceID: "workspace-test", RunID: "agent-run-test",
		ActionBatchID: "batch", ActionID: "action", OwnerGeneration: "generation",
		AssistantTranscriptOrdinal: 1, ToolCallID: "call", SchemaDigest: [32]byte{1}, ArgumentDigest: [32]byte{2},
	}
	tool := NewProcessSessionTool(manager)
	started, err := tool.Execute(WithProcessSessionAction(context.Background(), ref, nil), ToolCall{
		Name: "process_session", Args: map[string]any{"operation": "start", "command": "exec sleep 30"},
	})
	require.NoError(t, err)
	require.NotNil(t, started.ProcessSessionLease)
	capability, ok := manager.ManagerAdmissionCapability(ref.RunID, ref.OwnerGeneration)
	require.True(t, ok)

	valid := CloneProcessSessionLeaseSnapshot(*started.ProcessSessionLease)
	assert.True(t, capability.AllowsLease(valid))

	for name, mutate := range map[string]func(*ProcessSessionLeaseSnapshot){
		"other owner":            func(lease *ProcessSessionLeaseSnapshot) { lease.OwnerID = "forged-owner" },
		"other session":          func(lease *ProcessSessionLeaseSnapshot) { lease.SessionID = "forged-session" },
		"other action":           func(lease *ProcessSessionLeaseSnapshot) { lease.Action.ActionID = "forged-action" },
		"other arguments":        func(lease *ProcessSessionLeaseSnapshot) { lease.Action.ArgumentDigest[0] ^= 1 },
		"other process identity": func(lease *ProcessSessionLeaseSnapshot) { lease.Identity.LeaderPID++ },
		"inactive phase":         func(lease *ProcessSessionLeaseSnapshot) { lease.Phase = ProcessSessionLeaseStopped },
	} {
		t.Run(name, func(t *testing.T) {
			forged := CloneProcessSessionLeaseSnapshot(valid)
			mutate(&forged)
			assert.False(t, capability.AllowsLease(forged))
		})
	}
}

func TestProcessSessionCompletedHistoryIsBoundedAndDoesNotConsumeActiveCapacity(t *testing.T) {
	if !ownedCommandPlatformSupported() {
		t.Skip("local process-group ownership is unsupported")
	}
	manager := newTestProcessSessionManager(context.Background(), t, 1024)
	const runs = processSessionHistoryCount + 8
	ids := make([]string, 0, runs)
	for index := 0; index < runs; index++ {
		started, err := manager.Start(context.Background(), "exit 0")
		require.NoError(t, err, "completed sessions must not consume active process capacity")
		ids = append(ids, started.SessionID)
		require.Eventually(t, func() bool {
			status, statusErr := manager.Status(context.Background(), started.SessionID)
			return statusErr == nil && status.Status == ProcessSessionExited
		}, time.Second, 5*time.Millisecond)
	}
	manager.mu.Lock()
	retained := len(manager.sessions)
	active := manager.activeSessionCountLocked()
	manager.mu.Unlock()
	assert.LessOrEqual(t, retained, processSessionHistoryCount)
	assert.Zero(t, active)
	_, err := manager.Status(context.Background(), ids[0])
	assert.ErrorIs(t, err, ErrProcessSessionNotFound, "old completed output is explicitly unavailable after bounded eviction")
	for _, id := range ids[len(ids)-processSessionHistoryCount:] {
		_, err := manager.Status(context.Background(), id)
		assert.NoError(t, err, "recent completion history remains readable")
	}
}

type processSessionRejectingSandbox struct{ err error }

func (s processSessionRejectingSandbox) Validate(context.Context, string, string) error { return s.err }
