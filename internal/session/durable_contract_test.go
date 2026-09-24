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
	ordinal := uint64(2)
	transcriptIndex := uint64(5)
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
		ArgumentDigest:    "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		SchemaDigest:      "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
		ExecutionIdentity: &ExecutionIdentity{
			Kind:              ExecutionIdentityProcessGroup,
			Platform:          "linux",
			HostGeneration:    "boot-1",
			LeaderPID:         42,
			ProcessStartToken: "start-42",
			ProcessGroupID:    42,
		},
		AssistantTranscriptOrdinal: &ordinal,
		TranscriptIndex:            &transcriptIndex,
		CompactedThroughEntryID:    "entry-3",
		RunOutcome:                 core.RunOutcomeCancelled,
		CheckpointKind:             CheckpointExecutionResolution,
		Usage:                      &llm.Usage{InputTokens: 7, OutputTokens: 3, TotalTokens: 10},
		ExecutionResolution: &core.ExecutionResolution{
			RunID:       "run-1",
			ActionID:    "action-1",
			Evidence:    core.ResolutionLocalResourcesStopped,
			EvidenceRef: "entry-proof",
		},
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
	cloned := decoded.clone()
	cloned.ExecutionResolution.EvidenceRef = "changed"
	if decoded.ExecutionResolution.EvidenceRef != "entry-proof" {
		t.Fatal("SessionEntry.clone aliases the typed execution resolution")
	}
	cloned.Usage.InputTokens = 99
	if decoded.Usage.InputTokens != 7 {
		t.Fatal("SessionEntry.clone aliases token usage")
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

func TestVersion1AssistantAndFirstReceiptRequirePresentZeroCoordinates(t *testing.T) {
	zero := uint64(0)
	assistant := SessionEntry{
		ID:                         "assistant-entry",
		Type:                       EntryTypeAssistant,
		RecordVersion:              1,
		RunID:                      "run-1",
		AssistantTranscriptOrdinal: &zero,
		TranscriptIndex:            &zero,
		ToolCalls:                  []llm.ToolCall{{ID: "call-1", Name: "read"}},
	}
	encoded, err := json.Marshal(assistant)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"assistant_transcript_ordinal", "transcript_index"} {
		if _, exists := fields[field]; !exists {
			t.Fatalf("explicit zero coordinate %q was omitted", field)
		}
	}
	var decoded SessionEntry
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.AssistantTranscriptOrdinal == nil || *decoded.AssistantTranscriptOrdinal != 0 || decoded.TranscriptIndex == nil || *decoded.TranscriptIndex != 0 {
		t.Fatal("explicit zero coordinates did not survive JSON round trip")
	}
	if err := validateVersion1EntryCoordinates(decoded); err != nil {
		t.Fatalf("valid first assistant coordinate was rejected: %v", err)
	}

	receiptIndex := uint64(1)
	receipt := SessionEntry{
		ID:                         "receipt-entry",
		Type:                       EntryTypeTool,
		RecordVersion:              1,
		RunID:                      "run-1",
		AssistantTranscriptOrdinal: &zero,
		TranscriptIndex:            &receiptIndex,
		ToolCallID:                 "call-1",
		ToolName:                   "read",
		ReceiptOutcome:             core.ReceiptOutcomeSuccess,
		DispatchState:              DispatchStateDispatched,
	}
	if err := validateVersion1EntryCoordinates(receipt); err != nil {
		t.Fatalf("valid first receipt coordinate was rejected: %v", err)
	}

	missingOrdinal := assistant
	missingOrdinal.AssistantTranscriptOrdinal = nil
	if err := validateVersion1EntryCoordinates(missingOrdinal); err == nil {
		t.Fatal("version 1 assistant with missing ordinal must be rejected")
	}
	missingIndex := receipt
	missingIndex.TranscriptIndex = nil
	if err := validateVersion1EntryCoordinates(missingIndex); err == nil {
		t.Fatal("version 1 receipt with missing transcript index must be rejected")
	}

	journalIntent := SessionEntry{
		Type: EntryTypeTool, RecordVersion: 1, ActionState: ActionStateIntent,
		RunID: "run-1", ActionBatchID: "batch-1", ActionID: "action-1",
		ToolCallID: "call-1", ToolName: "read", AssistantTranscriptOrdinal: &zero,
	}
	if err := validateVersion1EntryCoordinates(journalIntent); err != nil {
		t.Fatalf("journal-only intent with no transcript index was rejected: %v", err)
	}
	journalIntent.TranscriptIndex = &zero
	if err := validateVersion1EntryCoordinates(journalIntent); err == nil {
		t.Fatal("journal-only intent must not carry a transcript index")
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

func TestSessionViewValidateRequiresBranchUniqueSourcesAndNonemptyRevision(t *testing.T) {
	valid := SessionView{
		SessionID:      "physical-1",
		BranchID:       "branch-1",
		Revision:       "revision-1",
		Messages:       []core.AgentMessage{{Role: "user", Content: "first"}, {Role: "user", Content: "second"}},
		SourceEntryIDs: []string{"entry-1", "entry-2"},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid view was rejected: %v", err)
	}

	tests := []struct {
		name string
		view SessionView
	}{
		{name: "missing branch", view: SessionView{SessionID: "physical-1", Messages: valid.Messages, SourceEntryIDs: valid.SourceEntryIDs, Revision: "revision-1"}},
		{name: "missing revision on history", view: SessionView{SessionID: "physical-1", BranchID: "branch-1", Messages: valid.Messages, SourceEntryIDs: valid.SourceEntryIDs}},
		{name: "duplicate source", view: SessionView{SessionID: "physical-1", BranchID: "branch-1", Revision: "revision-1", Messages: valid.Messages, SourceEntryIDs: []string{"entry-1", "entry-1"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.view.Validate(); err == nil {
				t.Fatal("expected invalid view to be rejected")
			}
		})
	}

	empty := SessionView{SessionID: "physical-1", BranchID: "branch-1"}
	if err := empty.Validate(); err != nil {
		t.Fatalf("empty new-session view may have no revision yet: %v", err)
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
