package session

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/tools"
)

func TestSessionCompactionSummaryRendersOnlyActiveProcessSessionIDs(t *testing.T) {
	ref := core.ActiveProcessSessionRef{
		OwnerID: "private-owner", WorkspaceID: "workspace", RunID: "run", SessionID: "opaque-session-id",
		OwnerGeneration: "generation", StartActionID: "start-action",
	}
	message, ok := sessionEntryToAgentMessage(SessionEntry{
		Type: EntryTypeCompaction, Summary: compaction.DurableTruncationMarker,
		ActiveProcessSessions: []core.ActiveProcessSessionRef{ref},
	})
	require.True(t, ok)
	require.Contains(t, message.Content, `- "opaque-session-id"`)
	require.NotContains(t, message.Content, "private-owner")
	require.NotContains(t, message.Content, "generation")
}

func TestActiveProcessSessionCompactionRequiresExactLatestLeaseRoster(t *testing.T) {
	makeLeaseRow := func(phase tools.ProcessSessionLeasePhase) SessionEntry {
		lease := tools.ProcessSessionLeaseSnapshot{
			SessionID: "opaque-session-id", OwnerID: "owner", WorkspaceID: "workspace", RunID: "run", OwnerGeneration: "generation",
			Action: tools.ProcessSessionActionRef{
				SessionID: "physical-session", WorkspaceID: "workspace", RunID: "run", ActionBatchID: "batch",
				ActionID: "start-action", OwnerGeneration: "generation", ToolCallID: "start-call",
				SchemaDigest: [32]byte{1}, ArgumentDigest: [32]byte{2},
			},
			Phase: phase,
			Identity: &tools.ProcessExecutionIdentity{
				Platform: "linux", HostGeneration: "host", LeaderPID: 123, ProcessStartToken: "birth-a", ProcessGroupID: 123,
			},
		}
		return SessionEntry{
			ID: "lease-row", RecordVersion: 1, Seq: 1, Type: EntryTypeCheckpoint, CheckpointKind: CheckpointProcessSessionLease,
			SessionID: "physical-session", WorkspaceID: "workspace", RunID: "run", ActionBatchID: "batch",
			ActionID: "start-action", ToolCallID: "start-call", OwnerGeneration: "generation", ProcessSessionLease: &lease,
		}
	}
	validRef := core.ActiveProcessSessionRef{
		OwnerID: "owner", WorkspaceID: "workspace", RunID: "run", SessionID: "opaque-session-id",
		OwnerGeneration: "generation", StartActionID: "start-action",
	}
	for _, test := range []struct {
		name  string
		phase tools.ProcessSessionLeasePhase
		refs  []core.ActiveProcessSessionRef
	}{
		{name: "active lease omitted", phase: tools.ProcessSessionLeaseActive},
		{name: "missing action proof", phase: tools.ProcessSessionLeaseActive, refs: []core.ActiveProcessSessionRef{validRef}},
		{name: "forged session selector", phase: tools.ProcessSessionLeaseActive, refs: []core.ActiveProcessSessionRef{{OwnerID: "owner", WorkspaceID: "workspace", RunID: "run", SessionID: "forged-id", OwnerGeneration: "generation", StartActionID: "start-action"}}},
		{name: "stale closed lease", phase: tools.ProcessSessionLeaseStopped, refs: []core.ActiveProcessSessionRef{validRef}},
	} {
		t.Run(test.name, func(t *testing.T) {
			marker := SessionEntry{RunID: "run", WorkspaceID: "workspace", ActiveProcessSessions: test.refs}
			_, err := validateActiveProcessSessionReferences([]SessionEntry{makeLeaseRow(test.phase)}, marker, 1, "physical-session")
			require.Error(t, err)
		})
	}
}
