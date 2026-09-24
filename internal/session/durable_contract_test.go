package session

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
)

func TestSessionEntryV1JSONRoundTrip(t *testing.T) {
	entry := SessionEntry{
		ID:                "entry-1",
		ParentID:          "entry-0",
		Type:              EntryTypeCheckpoint,
		Content:           "checkpoint",
		Timestamp:         time.Date(2026, 9, 25, 8, 0, 0, 0, time.UTC),
		RecordVersion:     1,
		SessionID:         "physical-1",
		SessionLabel:      "display-label",
		BranchID:          "branch-1",
		ParentBranchID:    "branch-0",
		BaseLeafEntryID:   "entry-0",
		ActiveLeafEntryID: "entry-1",
		RunID:             "run-1",
		ActionBatchID:     "batch-1",
		ActionID:          "action-1",
		OwnerGeneration:   "owner-1",
		WorkspaceID:       "workspace-digest",
		InputKind:         core.InputKindFollowUp,
		ActionState:       ActionStateIntent,
		DispatchState:     DispatchStateDispatched,
		ReceiptOutcome:    core.ReceiptOutcomeUnknown,
		PayloadDigest:     "sha256:payload",
		ExecutionIdentity: &ExecutionIdentity{
			Kind:              ExecutionIdentityProcessGroup,
			Platform:          "linux",
			HostGeneration:    "boot-1",
			LeaderPID:         42,
			ProcessStartToken: "start-42",
			ProcessGroupID:    42,
		},
		AssistantTranscriptOrdinal: 2,
		TranscriptIndex:            5,
		CompactedThroughEntryID:    "entry-3",
		RunOutcome:                 core.RunOutcomeCancelled,
		CheckpointKind:             CheckpointExecutionResolution,
	}

	encoded, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal version 1 entry: %v", err)
	}
	var decoded SessionEntry
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal version 1 entry: %v", err)
	}
	if !reflect.DeepEqual(decoded, entry) {
		t.Fatalf("version 1 entry changed after round trip:\n got: %#v\nwant: %#v", decoded, entry)
	}
	if err := decoded.ExecutionIdentity.Validate(); err != nil {
		t.Fatalf("valid process-group identity shape was rejected: %v", err)
	}
}

func TestExecutionIdentityValidateRejectsIncompleteOrUnsupportedIdentity(t *testing.T) {
	tests := []struct {
		name     string
		identity ExecutionIdentity
	}{
		{name: "missing host generation", identity: ExecutionIdentity{Kind: ExecutionIdentityProcessGroup, Platform: "linux", LeaderPID: 1, ProcessStartToken: "start", ProcessGroupID: 1}},
		{name: "missing start token", identity: ExecutionIdentity{Kind: ExecutionIdentityProcessGroup, Platform: "linux", HostGeneration: "boot", LeaderPID: 1, ProcessGroupID: 1}},
		{name: "unsupported platform", identity: ExecutionIdentity{Kind: ExecutionIdentityProcessGroup, Platform: "plan9", HostGeneration: "boot", LeaderPID: 1, ProcessStartToken: "start", ProcessGroupID: 1}},
		{name: "missing windows job", identity: ExecutionIdentity{Kind: ExecutionIdentityWindowsJob, Platform: "windows", HostGeneration: "boot", LeaderPID: 1, ProcessStartToken: "start"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.identity.Validate(); err == nil {
				t.Fatal("expected incomplete or unsupported identity shape to be rejected")
			}
		})
	}
}

func TestSessionViewValidateRequiresMessageSourceAlignment(t *testing.T) {
	view := SessionView{
		SessionID:      "physical-1",
		BranchID:       "branch-1",
		Revision:       "revision-1",
		Messages:       []core.AgentMessage{{Role: "user", Content: "hello"}},
		SourceEntryIDs: nil,
	}
	if err := view.Validate(); err == nil {
		t.Fatal("expected a view with missing source entry IDs to be rejected")
	}

	view.SourceEntryIDs = []string{"entry-1"}
	if err := view.Validate(); err != nil {
		t.Fatalf("valid aligned view was rejected: %v", err)
	}
}

func TestSessionViewCloneDoesNotAliasMessagePayload(t *testing.T) {
	view := SessionView{
		SessionID: "physical-1",
		BranchID:  "branch-1",
		Revision:  "revision-1",
		Messages: []core.AgentMessage{{
			Role: "assistant",
			ToolCalls: []llm.ToolCall{{
				ID:   "call-1",
				Name: "read",
				Args: map[string]any{"nested": []any{map[string]any{"value": "original"}}},
			}},
			ContentBlocks: []llm.ContentBlock{{Type: "text", Text: "original"}},
			Usage:         &llm.Usage{InputTokens: 1, OutputTokens: 2},
		}},
		SourceEntryIDs: []string{"entry-1"},
	}

	cloned := view.Clone()
	cloned.SourceEntryIDs[0] = "changed"
	cloned.Messages[0].ContentBlocks[0].Text = "changed"
	cloned.Messages[0].Usage.InputTokens = 9
	args := cloned.Messages[0].ToolCalls[0].Args.(map[string]any)
	args["nested"].([]any)[0].(map[string]any)["value"] = "changed"

	originalArgs := view.Messages[0].ToolCalls[0].Args.(map[string]any)
	if view.SourceEntryIDs[0] != "entry-1" ||
		view.Messages[0].ContentBlocks[0].Text != "original" ||
		view.Messages[0].Usage.InputTokens != 1 ||
		originalArgs["nested"].([]any)[0].(map[string]any)["value"] != "original" {
		t.Fatal("SessionView.Clone aliases source IDs or canonical message payloads")
	}
}

func TestSessionEntryCloneDoesNotAliasNestedToolArguments(t *testing.T) {
	entry := &SessionEntry{
		ID:   "entry-1",
		Type: EntryTypeAssistant,
		ToolCalls: []llm.ToolCall{{
			ID:   "call-1",
			Name: "read",
			Args: map[string]any{"nested": []any{map[string]any{"value": "original"}}},
		}},
	}

	cloned := entry.clone()
	args := cloned.ToolCalls[0].Args.(map[string]any)
	args["nested"].([]any)[0].(map[string]any)["value"] = "changed"
	originalArgs := entry.ToolCalls[0].Args.(map[string]any)
	if originalArgs["nested"].([]any)[0].(map[string]any)["value"] != "original" {
		t.Fatal("SessionEntry.clone aliases nested canonical tool arguments")
	}
}

func TestPendingActionRefAllowsOnlyFullyUnreservedOpenOccurrence(t *testing.T) {
	ref := PendingActionRef{
		SessionID:                  "physical-1",
		WorkspaceID:                "workspace-1",
		RunID:                      "run-1",
		AssistantTranscriptOrdinal: 3,
		ToolCallID:                 "call-1",
	}
	if err := ref.Validate(); err != nil {
		t.Fatalf("open occurrence with no reservation should be inspectable: %v", err)
	}

	ref.ActionBatchID = "batch-1"
	if err := ref.Validate(); err == nil {
		t.Fatal("partial reservation identity must fail closed")
	}
	ref.ActionID = "action-1"
	if err := ref.Validate(); err != nil {
		t.Fatalf("complete reservation identity was rejected: %v", err)
	}
}

func TestValidatePendingActionDecisionsRequiresExactOrderedResults(t *testing.T) {
	refs := []PendingActionRef{
		{SessionID: "s", WorkspaceID: "w", RunID: "r", ActionBatchID: "b1", ActionID: "a1", AssistantTranscriptOrdinal: 0, ToolCallID: "call-1"},
		{SessionID: "s", WorkspaceID: "w", RunID: "r", AssistantTranscriptOrdinal: 1, ToolCallID: "call-1"},
	}
	valid := []PendingActionDecision{
		{Action: refs[0], MarkerState: DispatchMarkerPresent},
		{Action: refs[1], MarkerState: DispatchMarkerAbsent},
	}
	if err := validatePendingActionDecisions(refs, valid); err != nil {
		t.Fatalf("valid exact decisions were rejected: %v", err)
	}

	tests := []struct {
		name      string
		refs      []PendingActionRef
		decisions []PendingActionDecision
	}{
		{name: "missing", refs: refs, decisions: valid[:1]},
		{name: "extra", refs: refs, decisions: append(append([]PendingActionDecision(nil), valid...), PendingActionDecision{})},
		{name: "reordered reserved occurrences", refs: []PendingActionRef{
			{SessionID: "s", WorkspaceID: "w", RunID: "r", ActionBatchID: "b1", ActionID: "a1", AssistantTranscriptOrdinal: 0, ToolCallID: "call-1"},
			{SessionID: "s", WorkspaceID: "w", RunID: "r", ActionBatchID: "b2", ActionID: "a2", AssistantTranscriptOrdinal: 1, ToolCallID: "call-1"},
		}, decisions: []PendingActionDecision{{Action: PendingActionRef{SessionID: "s", WorkspaceID: "w", RunID: "r", ActionBatchID: "b2", ActionID: "a2", AssistantTranscriptOrdinal: 1, ToolCallID: "call-1"}, MarkerState: DispatchMarkerPresent}, {Action: PendingActionRef{SessionID: "s", WorkspaceID: "w", RunID: "r", ActionBatchID: "b1", ActionID: "a1", AssistantTranscriptOrdinal: 0, ToolCallID: "call-1"}, MarkerState: DispatchMarkerAbsent}}},
		{name: "mismatched action occurrence", refs: refs, decisions: []PendingActionDecision{{Action: PendingActionRef{SessionID: "s", WorkspaceID: "w", RunID: "r", ActionBatchID: "b1", ActionID: "other", AssistantTranscriptOrdinal: 0, ToolCallID: "call-1"}, MarkerState: DispatchMarkerPresent}, valid[1]}},
		{name: "unknown marker state", refs: refs, decisions: []PendingActionDecision{{Action: refs[0], MarkerState: DispatchMarkerState(99)}, valid[1]}},
		{name: "unreserved occurrence cannot claim a marker", refs: refs, decisions: []PendingActionDecision{valid[0], {Action: refs[1], MarkerState: DispatchMarkerPresent}}},
		{name: "duplicate occurrence reference", refs: []PendingActionRef{refs[0], refs[0]}, decisions: []PendingActionDecision{valid[0], valid[0]}},
		{name: "action ID reused across occurrences", refs: []PendingActionRef{
			{SessionID: "s", WorkspaceID: "w", RunID: "r", ActionBatchID: "b1", ActionID: "a1", AssistantTranscriptOrdinal: 0, ToolCallID: "call-1"},
			{SessionID: "s", WorkspaceID: "w", RunID: "r", ActionBatchID: "b2", ActionID: "a1", AssistantTranscriptOrdinal: 1, ToolCallID: "call-1"},
		}, decisions: []PendingActionDecision{{Action: PendingActionRef{SessionID: "s", WorkspaceID: "w", RunID: "r", ActionBatchID: "b1", ActionID: "a1", AssistantTranscriptOrdinal: 0, ToolCallID: "call-1"}, MarkerState: DispatchMarkerPresent}, {Action: PendingActionRef{SessionID: "s", WorkspaceID: "w", RunID: "r", ActionBatchID: "b2", ActionID: "a1", AssistantTranscriptOrdinal: 1, ToolCallID: "call-1"}, MarkerState: DispatchMarkerPresent}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validatePendingActionDecisions(test.refs, test.decisions); err == nil {
				t.Fatal("expected invalid marker decisions to fail closed")
			}
		})
	}

	unverifiable := append([]PendingActionDecision(nil), valid...)
	unverifiable[1].MarkerState = DispatchMarkerUnverifiable
	if err := validatePendingActionDecisions(refs, unverifiable); err != nil {
		t.Fatalf("unverifiable unreserved occurrence should remain fail-closed without inventing an ID: %v", err)
	}

	unreservedRefs := []PendingActionRef{refs[1], {SessionID: "s", WorkspaceID: "w", RunID: "r", AssistantTranscriptOrdinal: 2, ToolCallID: "call-1"}}
	unreservedDecisions := []PendingActionDecision{
		{Action: unreservedRefs[0], MarkerState: DispatchMarkerAbsent},
		{Action: unreservedRefs[1], MarkerState: DispatchMarkerUnverifiable},
	}
	if err := validatePendingActionDecisions(unreservedRefs, unreservedDecisions); err != nil {
		t.Fatalf("valid unreserved decisions were rejected: %v", err)
	}
	if err := validatePendingActionDecisions(unreservedRefs, []PendingActionDecision{unreservedDecisions[1], unreservedDecisions[0]}); err == nil {
		t.Fatal("reordered unreserved decisions must fail closed")
	}
}

type contractWriter struct{}

func (contractWriter) AppendAndSync(context.Context, ...SessionEntry) ([]SessionEntry, error) {
	return nil, nil
}

type contractPreparedSession struct{}

func (contractPreparedSession) View() SessionView { return SessionView{} }

func (contractPreparedSession) DurableWriter() DurableSessionWriter { return contractWriter{} }

func (contractPreparedSession) Close() error { return nil }

type contractPreparer struct{}

func (contractPreparer) PrepareNew(context.Context, string) (PreparedSession, error) {
	return contractPreparedSession{}, nil
}

func (contractPreparer) PrepareResume(context.Context, string) (PreparedSession, error) {
	return contractPreparedSession{}, nil
}

type contractRecoveryAuthority struct{}

func (contractRecoveryAuthority) ApplyDurableResolutions(context.Context, []ValidatedExecutionResolution) error {
	return nil
}

func (contractRecoveryAuthority) InspectDispatchMarkers(context.Context, []PendingActionRef) ([]PendingActionDecision, error) {
	return nil, nil
}

var (
	_ DurableSessionWriter = contractWriter{}
	_ PreparedSession      = contractPreparedSession{}
	_ SessionPreparer      = contractPreparer{}
	_ RecoveryAuthority    = contractRecoveryAuthority{}
)

func TestContractInterfaceFixturesAreUsable(t *testing.T) {
	prepared, err := (contractPreparer{}).PrepareNew(context.Background(), "display label")
	if err != nil {
		t.Fatal(err)
	}
	if prepared.DurableWriter() == nil {
		t.Fatal("prepared session must expose its durable writer")
	}
	if err := prepared.Close(); err != nil {
		t.Fatalf("unexpected close error: %v", err)
	}
}
