package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tools"
)

func newProcessSessionHostForTest(parent context.Context, t *testing.T) (*ProcessSessionInvocationHost, *tools.LocalProcessSessionManager) {
	t.Helper()
	host, err := NewProcessSessionInvocationHost(parent, "workspace-test")
	require.NoError(t, err)
	manager, err := tools.NewLocalProcessSessionManager(host.Context(), tools.ProcessSessionManagerConfig{
		OwnerID:     host.OwnerID(),
		WorkspaceID: host.WorkspaceID(),
		Workdir:     t.TempDir(),
		Sandbox:     tools.AllowAllSandbox{},
		ScopeResolver: func(context.Context) (tools.ProcessSessionScope, error) {
			return tools.ProcessSessionScope{OwnerID: host.OwnerID(), RunID: "run-test", WorkspaceID: host.WorkspaceID()}, nil
		},
	})
	require.NoError(t, err)
	require.NoError(t, host.RegisterManager(manager))
	t.Cleanup(func() { require.NoError(t, host.Close(context.Background())) })
	return host, manager
}

func newWatchedProcessSessionHostForTest(parent context.Context) (*ProcessSessionInvocationHost, chan os.Signal) {
	ctx, cancel := context.WithCancelCause(parent)
	signals := make(chan os.Signal, 1)
	ownerID := "test-process-session-host"
	host := &ProcessSessionInvocationHost{
		ctx: ctx, cancel: cancel, ownerID: ownerID, releaseCause: &processSessionHostReleaseCause{ownerID: ownerID}, releaseSignal: signals,
		releasedCh: make(chan struct{}), watchDone: make(chan struct{}), stop: func() {},
	}
	go host.watchRelease(parent)
	return host, signals
}

func TestProcessSessionHostReleaseAndSignalShareOneTransition(t *testing.T) {
	for iteration := 0; iteration < 25; iteration++ {
		parent, cancelParent := context.WithCancelCause(context.Background())
		parentCause := errors.New("parent stopped")
		host, signals := newWatchedProcessSessionHostForTest(parent)
		host.mu.Lock()
		host.accepted = true
		host.mu.Unlock()

		start := make(chan struct{})
		var callers sync.WaitGroup
		callers.Add(3)
		go func() {
			defer callers.Done()
			<-start
			host.Release()
		}()
		go func() {
			defer callers.Done()
			<-start
			signals <- syscall.SIGUSR1
		}()
		go func() {
			defer callers.Done()
			<-start
			cancelParent(parentCause)
		}()
		close(start)
		callers.Wait()
		select {
		case <-host.watchDone:
		case <-time.After(time.Second):
			t.Fatal("release watcher did not join")
		}
		waitErr := host.WaitForRelease(context.Background())
		if waitErr == nil {
			require.Same(t, host.releaseCause, context.Cause(host.Context()))
		} else {
			require.ErrorIs(t, waitErr, parentCause)
			require.NotErrorIs(t, waitErr, ErrProcessSessionHostReleased)
		}
		require.NoError(t, host.Close(context.Background()))
	}
}

func TestProcessSessionHostWaitReturnsParentCauseWhenAlreadyReleased(t *testing.T) {
	parent, cancelParent := context.WithCancelCause(context.Background())
	parentCause := errors.New("parent teardown requested")
	ctx, cancel := context.WithCancelCause(parent)
	watchDone := make(chan struct{})
	close(watchDone)
	host := &ProcessSessionInvocationHost{
		ctx: ctx, cancel: cancel, releaseCause: &processSessionHostReleaseCause{ownerID: "test-wait-host"}, releasedCh: make(chan struct{}), watchDone: watchDone,
		stop: func() {}, accepted: true,
	}
	cancelParent(parentCause)
	host.transitionRelease(false, context.Cause(parent))

	require.ErrorIs(t, host.WaitForRelease(context.Background()), parentCause)
	require.NotErrorIs(t, host.WaitForRelease(context.Background()), ErrProcessSessionHostReleased)
	require.NoError(t, host.Close(context.Background()))
}

func TestProcessSessionChildDoesNotTreatParentHostReleaseAsOwnRelease(t *testing.T) {
	parent, _ := newWatchedProcessSessionHostForTest(context.Background())
	childCtx, childCancel := context.WithCancelCause(parent.Context())
	childOwnerID := "test-child-process-session-host"
	child := &ProcessSessionInvocationHost{
		ctx: childCtx, cancel: childCancel, ownerID: childOwnerID,
		releaseCause: &processSessionHostReleaseCause{ownerID: childOwnerID},
		releasedCh:   make(chan struct{}), watchDone: make(chan struct{}), stop: func() {},
	}
	child.mu.Lock()
	child.accepted = true
	child.mu.Unlock()

	parent.Release()
	require.Same(t, parent.releaseCause, context.Cause(child.Context()))
	child.Release()
	child.mu.Lock()
	require.False(t, child.releasedByHost, "a parent release cause cannot be mistaken for this child's release")
	child.mu.Unlock()
	go child.watchRelease(parent.Context())
	select {
	case <-child.watchDone:
	case <-time.After(time.Second):
		t.Fatal("child release watcher did not join")
	}
	err := child.WaitForRelease(context.Background())
	require.Same(t, parent.releaseCause, context.Cause(child.Context()))
	require.NotErrorIs(t, err, ErrProcessSessionHostReleased)
	require.NoError(t, child.Close(context.Background()))
	require.NoError(t, parent.Close(context.Background()))
}

func TestProcessSessionInvocationHostAcceptsLiveRunAndJoinsOnSIGUSR1(t *testing.T) {
	parent := context.Background()
	host, manager := newProcessSessionHostForTest(parent, t)
	marker := filepath.Join(t.TempDir(), "leader.pid")
	command := "printf '%s' \"$$\" > " + shellSingleQuote(marker) + "; exec sleep 60"
	started, err := tools.NewProcessSessionTool(manager).Execute(context.Background(), tools.ToolCall{
		Name: "process_session", Args: map[string]any{"operation": "start", "command": command},
	})
	require.NoError(t, err)
	sessionID, ok := started.Metadata["session_id"].(string)
	require.True(t, ok)
	require.Eventually(t, func() bool {
		_, err := os.Stat(marker)
		return err == nil
	}, time.Second, 5*time.Millisecond)
	var leaderPID int
	pidBytes, err := os.ReadFile(marker)
	require.NoError(t, err)
	_, err = fmt.Sscanf(string(pidBytes), "%d", &leaderPID)
	require.NoError(t, err)
	require.Positive(t, leaderPID)

	handoff, accepted, err := host.AcceptHandoff(manager, "run-test")
	require.NoError(t, err)
	require.True(t, accepted)
	require.Equal(t, tools.ProcessSessionHandoff{
		Kind: "process_session_handoff", Version: 1, Status: "delivered-active",
		OwnerID: host.OwnerID(), RunID: "run-test", WorkspaceID: host.WorkspaceID(), SessionIDs: []string{sessionID},
	}, handoff)
	encoded, err := json.Marshal(handoff)
	require.NoError(t, err)
	assert.Contains(t, string(encoded), `"kind":"process_session_handoff"`)

	waited := make(chan error, 1)
	go func() { waited <- host.WaitForRelease(context.Background()) }()
	require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGUSR1))
	select {
	case err := <-waited:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("process-session host did not observe SIGUSR1")
	}

	require.NoError(t, host.Close(context.Background()))
	status, err := manager.Status(context.Background(), sessionID)
	require.NoError(t, err)
	require.Equal(t, tools.ProcessSessionStopped, status.Status)
	err = syscall.Kill(-leaderPID, 0)
	assert.ErrorIs(t, err, syscall.ESRCH, "owned process group must be absent after host close")
}

func TestProcessSessionInvocationHostReleaseBeforeHandoffRejectsDelivery(t *testing.T) {
	host, manager := newProcessSessionHostForTest(context.Background(), t)
	host.Release()
	_, accepted, err := host.AcceptHandoff(manager, "run-test")
	require.NoError(t, err)
	assert.False(t, accepted)
	_, err = manager.Start(context.Background(), "exec sleep 60")
	require.Error(t, err, "release-before-handoff must cancel the manager before any child starts")
	require.NoError(t, host.Close(context.Background()))
}

func TestProcessSessionInvocationHostParentCancellationJoinsAcceptedSession(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	host, manager := newProcessSessionHostForTest(parent, t)
	started, err := manager.Start(context.Background(), "exec sleep 60")
	require.NoError(t, err)
	_, accepted, err := host.AcceptHandoff(manager, "run-test")
	require.NoError(t, err)
	require.True(t, accepted)
	cancelParent()
	require.ErrorIs(t, host.WaitForRelease(context.Background()), context.Canceled)
	require.NoError(t, host.Close(context.Background()))
	status, err := manager.Status(context.Background(), started.SessionID)
	require.NoError(t, err)
	require.Equal(t, tools.ProcessSessionStopped, status.Status)
}

func TestProcessSessionInvocationHostReleaseHandoffRaceCannotOrphanGroup(t *testing.T) {
	host, manager := newProcessSessionHostForTest(context.Background(), t)
	started, err := manager.Start(context.Background(), "exec sleep 60")
	require.NoError(t, err)
	type handoffResult struct {
		accepted bool
		err      error
	}
	handoffDone := make(chan handoffResult, 1)
	go func() {
		_, accepted, acceptErr := host.AcceptHandoff(manager, "run-test")
		handoffDone <- handoffResult{accepted: accepted, err: acceptErr}
	}()
	go host.Release()
	result := <-handoffDone
	if result.err == nil && result.accepted {
		require.NoError(t, host.WaitForRelease(context.Background()))
	}
	require.NoError(t, host.Close(context.Background()))
	status, err := manager.Status(context.Background(), started.SessionID)
	require.NoError(t, err)
	require.Equal(t, tools.ProcessSessionStopped, status.Status)
}

func TestProcessSessionInvocationHostDoesNotHandoffNaturalExit(t *testing.T) {
	host, manager := newProcessSessionHostForTest(context.Background(), t)
	started, err := manager.Start(context.Background(), "exit 0")
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		status, statusErr := manager.Status(context.Background(), started.SessionID)
		return statusErr == nil && status.Status == tools.ProcessSessionExited
	}, time.Second, 5*time.Millisecond)
	_, accepted, err := host.AcceptHandoff(manager, "run-test")
	require.NoError(t, err)
	assert.False(t, accepted)
}

func TestProcessSessionHandoffFormattingDependsOnOutputMode(t *testing.T) {
	handoff := tools.ProcessSessionHandoff{
		Kind: "process_session_handoff", Version: 1, Status: "delivered-active",
		OwnerID: "opaque-owner", RunID: "run-test", WorkspaceID: "opaque-workspace", SessionIDs: []string{"opaque-session"},
	}
	t.Run("json emits typed event", func(t *testing.T) {
		var out bytes.Buffer
		require.NoError(t, writeProcessSessionHandoff(&out, OutputJSON, handoff))
		var got tools.ProcessSessionHandoff
		require.NoError(t, json.Unmarshal(out.Bytes(), &got))
		require.Equal(t, handoff, got)
	})
	t.Run("text explains attached lifetime without opaque IDs", func(t *testing.T) {
		var out bytes.Buffer
		require.NoError(t, writeProcessSessionHandoff(&out, OutputText, handoff))
		require.Contains(t, out.String(), "Background process sessions are still running")
		require.Contains(t, out.String(), "Ctrl+C")
		require.NotContains(t, out.String(), handoff.OwnerID)
		require.NotContains(t, out.String(), handoff.SessionIDs[0])
		require.NotContains(t, out.String(), `"kind"`)
	})
	t.Run("raw stream stays unmodified", func(t *testing.T) {
		var out bytes.Buffer
		require.NoError(t, writeProcessSessionHandoff(&out, OutputStream, handoff))
		require.Empty(t, strings.TrimSpace(out.String()))
	})
}
