//go:build linux || darwin

package core_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/session"
)

type executionIdentityGateSessionWriter struct {
	base         session.DurableSessionWriter
	identityRows chan session.SessionEntry
	releases     chan struct{}
	releaseAll   chan struct{}
	failIdentity bool
	mu           sync.Mutex
	seenActions  map[string]struct{}
}

func (writer *executionIdentityGateSessionWriter) AppendAndSync(ctx context.Context, entries ...session.SessionEntry) ([]session.SessionEntry, error) {
	var identityRows []session.SessionEntry
	for _, entry := range entries {
		if entry.Type != session.EntryTypeTool || entry.ActionState != session.ActionStateExecution || entry.ExecutionIdentity == nil {
			continue
		}
		if writer.failIdentity {
			return nil, errors.New("test: execution identity sync failed")
		}
		writer.mu.Lock()
		if _, seen := writer.seenActions[entry.ActionID]; !seen {
			writer.seenActions[entry.ActionID] = struct{}{}
			identityRows = append(identityRows, entry)
		}
		writer.mu.Unlock()
	}

	appended, err := writer.base.AppendAndSync(ctx, entries...)
	if err != nil {
		return appended, err
	}
	for _, entry := range identityRows {
		select {
		case writer.identityRows <- entry:
		case <-writer.releaseAll:
			return appended, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		select {
		case <-writer.releases:
		case <-writer.releaseAll:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return appended, nil
}

func newExecutionIdentityGateWriter(base session.DurableSessionWriter, identityCapacity int) *executionIdentityGateSessionWriter {
	return &executionIdentityGateSessionWriter{
		base:         base,
		identityRows: make(chan session.SessionEntry, identityCapacity),
		releases:     make(chan struct{}, identityCapacity),
		releaseAll:   make(chan struct{}),
		seenActions:  make(map[string]struct{}, identityCapacity),
	}
}

func prepareExecutionIdentitySession(t *testing.T, root, workspaceID, description string, writer *executionIdentityGateSessionWriter) (session.PreparedSession, core.RunCommitSink, *session.WorkspaceQuarantineIndex) {
	t.Helper()
	ctx := context.Background()
	index, err := session.NewWorkspaceQuarantineIndex()
	require.NoError(t, err)
	require.NoError(t, index.EnsureWorkspace(ctx, workspaceID))
	assertWorkspaceIndexUsesTempHome(t, root, workspaceID)
	prepared, err := session.NewDurableJSONLSessionPreparer(filepath.Join(root, "durable"), index).PrepareNew(ctx, description)
	require.NoError(t, err)
	writer.base = prepared.DurableWriter()
	preparedForSink := &receiptGatedPreparedSession{PreparedSession: prepared, writer: writer}
	sink, err := session.NewSessionRunCommitSink(preparedForSink, index, workspaceID, nil)
	if err != nil {
		_ = prepared.Close()
	}
	require.NoError(t, err)
	return prepared, sink, index
}

func waitForIdentityExecutionRow(t *testing.T, writer *executionIdentityGateSessionWriter) session.SessionEntry {
	t.Helper()
	select {
	case row := <-writer.identityRows:
		return row
	case <-time.After(5 * time.Second):
		t.Fatal("managed Bash did not durably record its process identity before release")
		return session.SessionEntry{}
	}
}

func assertEffectAbsent(t *testing.T, path string) {
	t.Helper()
	_, err := os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist, "Bash user code must remain gated until its identity row is synced")
}

func awaitIdentityRun(t *testing.T, stream core.EventStream, events <-chan []core.AgentEvent) {
	t.Helper()
	select {
	case <-events:
	case <-time.After(10 * time.Second):
		t.Fatal("durable Bash run did not finish")
	}
	result, err := stream.Result()
	require.NoError(t, err)
	require.Equal(t, "done", result.Content)
}

func TestRunCommitPersistsManagedBashIdentityBeforeSequentialUserCode(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	writer := newExecutionIdentityGateWriter(nil, 1)
	prepared, sink, index := prepareExecutionIdentitySession(t, root, "workspace-v1-identity-sequential", "identity sequential", writer)
	defer prepared.Close()
	defer close(writer.releaseAll)

	effectPath := filepath.Join(root, "sequential-effect")
	stream, err := newIdentityRun(t, prepared, sink, "identity-sequential", "run sequential Bash", []mock.ExpectedToolCall{{
		ID: "bash-sequential-1", Name: "bash", Args: map[string]any{"command": fmt.Sprintf("printf effect > %s", effectPath)},
	}}, false)
	require.NoError(t, err)
	events := collectAgentEvents(stream)
	identityRow := waitForIdentityExecutionRow(t, writer)
	assertEffectAbsent(t, effectPath)
	require.Equal(t, session.ActionStateExecution, identityRow.ActionState)
	require.Equal(t, core.DispatchStateDispatched, identityRow.DispatchState)
	require.Equal(t, core.RunExecutionCleanupPending, identityRow.ResourceState)
	require.NotNil(t, identityRow.ExecutionIdentity)
	require.NoError(t, identityRow.ExecutionIdentity.Validate())
	requirePlatformPIDNamespaceToken(t, identityRow.ExecutionIdentity.PIDNamespaceToken)
	writer.releases <- struct{}{}

	awaitIdentityRun(t, stream, events)
	require.FileExists(t, effectPath)
	require.Equal(t, "bash-sequential-1", identityRow.ToolCallID)
	requireIdentityRowMatchesIntent(t, prepared, identityRow)
	sessionID := prepared.View().SessionID
	require.NoError(t, prepared.Close())
	reopened, err := session.NewDurableJSONLSessionPreparer(filepath.Join(root, "durable"), index).PrepareResume(context.Background(), sessionID)
	require.NoError(t, err)
	defer reopened.Close()
	_, err = session.NewSessionRunCommitSink(reopened, index, "workspace-v1-identity-sequential", nil)
	require.NoError(t, err, "new PID namespace tokens must remain readable through the normal session recovery path")
	reopenedSnapshot, err := reopened.Snapshot()
	require.NoError(t, err)
	var readBackIdentity *session.ExecutionIdentity
	for _, row := range reopenedSnapshot.DurableEntries {
		if row.ID == identityRow.ID {
			readBackIdentity = row.ExecutionIdentity
			break
		}
	}
	require.NotNil(t, readBackIdentity)
	require.Equal(t, identityRow.ExecutionIdentity, readBackIdentity)
}

func TestRunCommitPersistsEachManagedBashIdentityBeforeParallelUserCode(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	writer := newExecutionIdentityGateWriter(nil, 2)
	prepared, sink, _ := prepareExecutionIdentitySession(t, root, "workspace-v1-identity-parallel", "identity parallel", writer)
	defer prepared.Close()
	defer close(writer.releaseAll)

	effects := map[string]string{
		"bash-parallel-a": filepath.Join(root, "parallel-a-effect"),
		"bash-parallel-b": filepath.Join(root, "parallel-b-effect"),
	}
	calls := []mock.ExpectedToolCall{
		{ID: "bash-parallel-a", Name: "bash", Args: map[string]any{"command": fmt.Sprintf("printf effect > %s", effects["bash-parallel-a"])}},
		{ID: "bash-parallel-b", Name: "bash", Args: map[string]any{"command": fmt.Sprintf("printf effect > %s", effects["bash-parallel-b"])}},
	}
	stream, err := newIdentityRun(t, prepared, sink, "identity-parallel", "run parallel Bash", calls, true)
	require.NoError(t, err)
	events := collectAgentEvents(stream)
	seen := make(map[string]session.SessionEntry, len(calls))
	for len(seen) < len(calls) {
		row := waitForIdentityExecutionRow(t, writer)
		effectPath, ok := effects[row.ToolCallID]
		require.True(t, ok, "identity row must be bound to one exact Bash tool call")
		assertEffectAbsent(t, effectPath)
		require.Equal(t, core.DispatchStateDispatched, row.DispatchState)
		require.Equal(t, core.RunExecutionCleanupPending, row.ResourceState)
		require.NotNil(t, row.ExecutionIdentity)
		require.NoError(t, row.ExecutionIdentity.Validate())
		requirePlatformPIDNamespaceToken(t, row.ExecutionIdentity.PIDNamespaceToken)
		seen[row.ToolCallID] = row
		writer.releases <- struct{}{}
	}

	awaitIdentityRun(t, stream, events)
	for callID, effectPath := range effects {
		require.FileExists(t, effectPath)
		requireIdentityRowMatchesIntent(t, prepared, seen[callID])
	}
}

func requirePlatformPIDNamespaceToken(t *testing.T, token string) {
	t.Helper()
	if runtime.GOOS == "linux" {
		want, err := core.CurrentPIDNamespaceToken()
		require.NoError(t, err)
		require.NotEmpty(t, want)
		require.Equal(t, want, token, "the exact Linux PID namespace must be synced before Bash starts")
		return
	}
	require.Empty(t, token, "Darwin does not support PID namespace tokens")
}

func TestRunCommitKeepsManagedBashGatedWhenIdentitySyncFails(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	writer := newExecutionIdentityGateWriter(nil, 1)
	writer.failIdentity = true
	prepared, sink, index := prepareExecutionIdentitySession(t, root, "workspace-v1-identity-failure", "identity failure", writer)
	defer prepared.Close()
	defer close(writer.releaseAll)

	effectPath := filepath.Join(root, "failed-identity-effect")
	stream, err := newIdentityRun(t, prepared, sink, "identity-failure", "run gated Bash", []mock.ExpectedToolCall{{
		ID: "bash-identity-failure", Name: "bash", Args: map[string]any{"command": fmt.Sprintf("printf effect > %s", effectPath)},
	}}, false)
	require.NoError(t, err)
	events := collectAgentEvents(stream)
	select {
	case <-events:
	case <-time.After(10 * time.Second):
		t.Fatal("failed identity persistence did not stop the run")
	}
	_, runErr := stream.Result()
	require.ErrorIs(t, runErr, core.ErrRunCommitQuiescenceUnproven)
	assertEffectAbsent(t, effectPath)
	select {
	case row := <-writer.identityRows:
		t.Fatalf("failed identity sync must not be reported as durable: %+v", row)
	default:
	}
	snapshot, err := prepared.Snapshot()
	require.NoError(t, err)
	var intent *session.SessionEntry
	for _, row := range snapshot.DurableEntries {
		require.Nil(t, row.ExecutionIdentity, "the failed identity row must not leak into the durable journal")
		if row.ActionState == session.ActionStateIntent && row.ToolCallID == "bash-identity-failure" {
			intent = &row
		}
	}
	require.NotNil(t, intent)
	require.NotNil(t, intent.AssistantTranscriptOrdinal)
	decisions, err := index.InspectDispatchMarkers(context.Background(), []session.PendingActionRef{{
		SessionID: intent.SessionID, WorkspaceID: intent.WorkspaceID, RunID: intent.RunID,
		ActionBatchID: intent.ActionBatchID, AssistantTranscriptOrdinal: *intent.AssistantTranscriptOrdinal,
		ToolCallID: intent.ToolCallID, ActionID: intent.ActionID,
	}})
	require.NoError(t, err)
	require.Len(t, decisions, 1)
	require.Equal(t, session.DispatchMarkerPresent, decisions[0].MarkerState)
}

func newIdentityRun(t *testing.T, prepared session.PreparedSession, sink core.RunCommitSink, agentName, prompt string, expected []mock.ExpectedToolCall, parallel bool) (core.EventStream, error) {
	t.Helper()
	ctx := context.Background()
	registry := newRunCommitStreamingRegistry(ctx, t, nil)
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"r2-process-identity-"+agentName,
		"durable process identity",
		mock.ConversationTurn{AssistantToolCalls: expected},
		mock.ConversationTurn{AssistantContent: "done"},
	))
	options := []core.LoopOption{core.WithLLM(model), core.WithTools(registry)}
	if parallel {
		options = append(options, core.WithExecutionMode(core.ExecutionModeParallel))
	}
	agent := core.NewAgentImpl(agentName, core.NewLoopAgent(options...))
	harness := core.NewHarnessImpl(agent, core.WithHarnessEventBus(core.NewMemoryEventBus()))
	stream, err := harness.Submit(core.WithRunCommit(ctx, sink, core.RunCommitBinding{
		SessionID: prepared.View().SessionID, WorkspaceID: "workspace-v1-" + agentName,
	}), prompt)
	if err != nil {
		return nil, err
	}
	return stream, nil
}

func requireIdentityRowMatchesIntent(t *testing.T, prepared session.PreparedSession, identityRow session.SessionEntry) {
	t.Helper()
	snapshot, err := prepared.Snapshot()
	require.NoError(t, err)
	var intent *session.SessionEntry
	for index := range snapshot.DurableEntries {
		row := &snapshot.DurableEntries[index]
		if row.ActionState == session.ActionStateIntent && row.ToolCallID == identityRow.ToolCallID {
			intent = row
			break
		}
	}
	require.NotNil(t, intent, "the identity row must correspond to a synced intent")
	require.Equal(t, intent.RunID, identityRow.RunID)
	require.Equal(t, intent.ActionBatchID, identityRow.ActionBatchID)
	require.Equal(t, intent.ActionID, identityRow.ActionID)
	require.Equal(t, intent.OwnerGeneration, identityRow.OwnerGeneration)
	require.Equal(t, intent.AssistantTranscriptOrdinal, identityRow.AssistantTranscriptOrdinal)
}
