package core

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tools"
)

func TestProcessSessionClosureObserverFailureWakesWaiterWithoutSuccessAck(t *testing.T) {
	ownerCtx, cancelOwner := context.WithCancel(context.Background())
	defer cancelOwner()
	closureEntered := make(chan struct{}, 1)
	closureRelease := make(chan struct{})
	var releaseOnce sync.Once
	releaseClosure := func() { releaseOnce.Do(func() { close(closureRelease) }) }
	observerErr := errors.New("durable closure write failed")
	manager, err := tools.NewLocalProcessSessionManager(ownerCtx, tools.ProcessSessionManagerConfig{
		OwnerID: "owner", WorkspaceID: "workspace", Workdir: t.TempDir(), Sandbox: tools.AllowAllSandbox{},
		ScopeResolver: func(context.Context) (tools.ProcessSessionScope, error) {
			return tools.ProcessSessionScope{OwnerID: "owner", WorkspaceID: "workspace", RunID: "run"}, nil
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, manager.Close(context.Background())) })
	defer releaseClosure()

	action := tools.ProcessSessionActionRef{
		SessionID: "physical-session", WorkspaceID: "workspace", RunID: "run", ActionBatchID: "batch",
		ActionID: "start-action", OwnerGeneration: "generation", AssistantTranscriptOrdinal: 0,
		ToolCallID: "start-call", SchemaDigest: [32]byte{1}, ArgumentDigest: [32]byte{2},
	}
	observer := func(_ context.Context, lease tools.ProcessSessionLeaseSnapshot) error {
		if lease.Phase != tools.ProcessSessionLeaseStopped {
			return nil
		}
		closureEntered <- struct{}{}
		<-closureRelease
		return observerErr
	}
	base := tools.NewDefaultToolRegistry().(*tools.DefaultToolRegistry) //nolint:errcheck
	registry := tools.NewMiddlewareToolRegistry(base)
	require.NoError(t, registry.SetPreparedToolStages(tools.PreparedToolStages{}))
	require.NoError(t, registry.SetPreparedToolPolicy(func(_ context.Context, request tools.PreparedPolicyRequest) (tools.PreparedPolicyDecision, error) {
		return tools.PreparedPolicyDecision{Allowed: true, Generations: request.Generations}, nil
	}))
	require.NoError(t, tools.RegisterDefaults(context.Background(), registry, tools.WithRegisteredProcessSessionManager(manager)))
	prepared, err := registry.PrepareToolCall(context.Background(), tools.ToolCall{
		ID: "start-call", Name: "process_session", Args: map[string]any{"operation": "start", "command": "exec sleep 30"},
	})
	require.NoError(t, err)
	invocation, ok := tools.ProcessSessionInvocationFor(prepared)
	require.True(t, ok)
	startCtx := tools.WithProcessSessionAction(context.Background(), action, observer)
	started, err := manager.Start(startCtx, "exec sleep 30")
	require.NoError(t, err)
	require.NoError(t, invocation.WithStartLeasePublication(started.SessionID, func(snapshot tools.ProcessSessionLeaseSnapshot) error {
		require.Equal(t, tools.ProcessSessionLeaseActive, snapshot.Phase)
		return nil
	}))

	consumer := &runCommitConsumer{ctx: context.Background(), processSession: &invocation}
	waitResult := make(chan error, 1)
	go func() {
		_, waitErr := consumer.waitForProcessSessionClosure(started.SessionID)
		waitResult <- waitErr
	}()
	select {
	case waitErr := <-waitResult:
		t.Fatalf("closure wait returned before cleanup began: %v", waitErr)
	case <-time.After(20 * time.Millisecond):
	}

	stopDone := make(chan error, 1)
	go func() {
		_, stopErr := manager.Stop(context.Background(), started.SessionID)
		stopDone <- stopErr
	}()
	select {
	case <-closureEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("process-session closure observer did not start")
	}
	releaseClosure()
	select {
	case waitErr := <-waitResult:
		require.ErrorIs(t, waitErr, ErrRunCommitProcessSessionClosureUnconfirmed)
	case <-time.After(2 * time.Second):
		t.Fatal("closure observer failure did not wake the completion waiter")
	}
	select {
	case <-invocation.SessionClosed(started.SessionID):
		t.Fatal("failed closure persistence must not signal successful durable closure")
	default:
	}
	select {
	case stopErr := <-stopDone:
		require.NoError(t, stopErr)
	case <-time.After(5 * time.Second):
		t.Fatal("process-session stop did not finish")
	}
}

func TestValidateActiveProcessSessionRefsAndRoster(t *testing.T) {
	refs := []ActiveProcessSessionRef{
		{OwnerID: "owner", WorkspaceID: "workspace", RunID: "run", SessionID: "sess_A", OwnerGeneration: "generation", StartActionID: "action_A"},
		{OwnerID: "owner", WorkspaceID: "workspace", RunID: "run", SessionID: "sess_B", OwnerGeneration: "generation", StartActionID: "action_B"},
	}
	require.NoError(t, ValidateActiveProcessSessionRefs(refs, "run", "workspace"))
	roster, err := ActiveProcessSessionRoster(refs)
	require.NoError(t, err)
	require.Equal(t, "Active process sessions (use these exact IDs with process_session read, status, or stop):\n- \"sess_A\"\n- \"sess_B\"", roster)
	require.NotEmpty(t, strings.TrimSpace(roster))
}

func TestValidateActiveProcessSessionRefsRejectsInvalidBindings(t *testing.T) {
	valid := ActiveProcessSessionRef{OwnerID: "owner", WorkspaceID: "workspace", RunID: "run", SessionID: "sess_A", OwnerGeneration: "generation", StartActionID: "action_A"}
	tests := map[string][]ActiveProcessSessionRef{
		"missing identity":  {func() ActiveProcessSessionRef { ref := valid; ref.OwnerGeneration = ""; return ref }()},
		"wrong run":         {func() ActiveProcessSessionRef { ref := valid; ref.RunID = "other"; return ref }()},
		"unsafe session ID": {func() ActiveProcessSessionRef { ref := valid; ref.SessionID = "session\ninjected"; return ref }()},
		"duplicate session": {valid, valid},
		"not sorted": {
			func() ActiveProcessSessionRef {
				ref := valid
				ref.SessionID = "sess_B"
				ref.StartActionID = "action_B"
				return ref
			}(),
			valid,
		},
	}
	for name, refs := range tests {
		t.Run(name, func(t *testing.T) {
			require.Error(t, ValidateActiveProcessSessionRefs(refs, "run", "workspace"))
		})
	}
	tooMany := make([]ActiveProcessSessionRef, maxActiveProcessSessionRefs+1)
	for index := range tooMany {
		tooMany[index] = ActiveProcessSessionRef{
			OwnerID: "owner", WorkspaceID: "workspace", RunID: "run", SessionID: "sess_" + string(rune('A'+index)),
			OwnerGeneration: "generation", StartActionID: "action_" + string(rune('A'+index)),
		}
	}
	require.Error(t, ValidateActiveProcessSessionRefs(tooMany, "run", "workspace"))
}
