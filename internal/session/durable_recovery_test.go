package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"
)

type recoveryAuthorityFunc struct {
	apply   func(context.Context, []ValidatedExecutionResolution) error
	inspect func(context.Context, []PendingActionRef) ([]PendingActionDecision, error)
}

func (a recoveryAuthorityFunc) ApplyDurableResolutions(ctx context.Context, resolutions []ValidatedExecutionResolution) error {
	if a.apply == nil {
		return nil
	}
	return a.apply(ctx, resolutions)
}

func (a recoveryAuthorityFunc) InspectDispatchMarkers(ctx context.Context, refs []PendingActionRef) ([]PendingActionDecision, error) {
	if a.inspect == nil {
		return nil, nil
	}
	return a.inspect(ctx, refs)
}

type recoveryFixture struct {
	directory  string
	sessionID  string
	assistant  SessionEntry
	intent     SessionEntry
	execution  SessionEntry
	receipt    SessionEntry
	resolution SessionEntry
}

func createRecoveryFixture(t *testing.T, withIntent bool) recoveryFixture {
	t.Helper()
	directory := t.TempDir()
	prepared, err := NewDurableJSONLSessionPreparer(directory, nil).PrepareNew(context.Background(), "recovery fixture")
	if err != nil {
		t.Fatal(err)
	}
	writer := prepared.DurableWriter()
	zero, index0, index1 := uint64(0), uint64(0), uint64(1)
	user, err := writer.AppendAndSync(context.Background(), SessionEntry{
		ID: "user-1", Type: EntryTypeUser, RunID: "run-1", TranscriptIndex: &index0,
		Timestamp: time.Unix(10, 0).UTC(), Content: "do the work",
	})
	if err != nil {
		t.Fatalf("append user: %v", err)
	}
	assistant, err := writer.AppendAndSync(context.Background(), SessionEntry{
		ID: "assistant-1", ParentID: user[0].ID, Type: EntryTypeAssistant, RunID: "run-1",
		WorkspaceID: "workspace-1", AssistantTranscriptOrdinal: &zero, TranscriptIndex: &index1,
		Timestamp: time.Unix(11, 0).UTC(), ToolCalls: []llm.ToolCall{{ID: "call-1", Name: "read", Args: map[string]any{"path": "README.md"}}},
	})
	if err != nil {
		t.Fatalf("append assistant: %v", err)
	}
	fixture := recoveryFixture{directory: directory, sessionID: prepared.View().SessionID, assistant: assistant[0]}
	parentID := assistant[0].ID
	if withIntent {
		intent, err := writer.AppendAndSync(context.Background(), SessionEntry{
			ID: "intent-1", ParentID: parentID, Type: EntryTypeTool, RunID: "run-1", WorkspaceID: "workspace-1",
			ActionBatchID: "batch-1", ActionID: "action-1", ActionState: ActionStateIntent,
			AssistantTranscriptOrdinal: &zero, ToolCallID: "call-1", ToolName: "read",
			ArgumentDigest: strings.Repeat("a", 64), SchemaDigest: strings.Repeat("b", 64),
			ToolIdentity: &tools.TrustedToolIdentitySnapshot{
				RegistrationSource: "builtin", ToolName: "read", RegistrationGeneration: 1,
				ExecutorFingerprint: strings.Repeat("c", 64), ScopeFingerprint: strings.Repeat("d", 64),
			},
			EffectSet: []tools.TrustedToolEffect{tools.EffectWorkspaceRead}, Timestamp: time.Unix(12, 0).UTC(),
		})
		if err != nil {
			t.Fatalf("append intent: %v", err)
		}
		fixture.intent = intent[0]
		parentID = intent[0].ID
	}
	if err := prepared.Close(); err != nil {
		t.Fatalf("close initial session: %v", err)
	}
	return fixture
}

func TestPrepareResumeSyncFailureStopsBeforeAuthorityAndReceiptRecovery(t *testing.T) {
	fixture := createRecoveryFixture(t, true)
	var applyCalls, inspectCalls, syncCalls int
	authority := recoveryAuthorityFunc{
		apply: func(context.Context, []ValidatedExecutionResolution) error {
			applyCalls++
			return nil
		},
		inspect: func(_ context.Context, refs []PendingActionRef) ([]PendingActionDecision, error) {
			inspectCalls++
			decisions := make([]PendingActionDecision, len(refs))
			for i, ref := range refs {
				decisions[i] = PendingActionDecision{Action: ref, MarkerState: DispatchMarkerAbsent}
			}
			return decisions, nil
		},
	}
	syncFailure := errors.New("injected resume file sync failure")
	prepared, err := newDurableJSONLSessionPreparer(fixture.directory, authority, durableJSONLIO{
		sync: func(*os.File) error {
			syncCalls++
			return syncFailure
		},
	}).PrepareResume(context.Background(), fixture.sessionID)
	if err == nil {
		if prepared != nil {
			_ = prepared.Close()
		}
		t.Fatal("resume succeeded after the validated session file Sync failed")
	}
	if !errors.Is(err, syncFailure) {
		t.Fatalf("resume error = %v, want injected Sync error", err)
	}
	if prepared != nil {
		_ = prepared.Close()
		t.Fatal("failed resume returned a usable prepared session")
	}
	if applyCalls != 0 || inspectCalls != 0 {
		t.Fatalf("authority calls before failed Sync returned: apply=%d inspect=%d, want 0/0", applyCalls, inspectCalls)
	}
	if syncCalls != 1 {
		t.Fatalf("resume file Sync calls = %d, want exactly one failed pre-authority Sync", syncCalls)
	}
	rows := readFixtureRows(t, fixture.directory, fixture.sessionID)
	if got := countReceipts(rows, "call-1"); got != 0 {
		t.Fatalf("failed resume wrote %d synthesized receipts before file Sync succeeded", got)
	}
}

func TestPrepareResumeReconcilesAbsentReservedMarkerAsInterrupted(t *testing.T) {
	fixture := createRecoveryFixture(t, true)
	var applyCalls, inspectCalls int
	authority := recoveryAuthorityFunc{
		apply: func(_ context.Context, resolutions []ValidatedExecutionResolution) error {
			applyCalls++
			if len(resolutions) != 0 {
				t.Fatalf("unexpected resolution proofs: %#v", resolutions)
			}
			return nil
		},
		inspect: func(_ context.Context, refs []PendingActionRef) ([]PendingActionDecision, error) {
			inspectCalls++
			if len(refs) != 1 || refs[0].ActionID != "action-1" || refs[0].ToolCallID != "call-1" {
				t.Fatalf("unexpected pending refs: %#v", refs)
			}
			return []PendingActionDecision{{Action: refs[0], MarkerState: DispatchMarkerAbsent}}, nil
		},
	}
	prepared, err := NewDurableJSONLSessionPreparer(fixture.directory, authority).PrepareResume(context.Background(), fixture.sessionID)
	if err != nil {
		t.Fatalf("resume after absent marker: %v", err)
	}
	view := prepared.View()
	if len(view.Messages) != 3 || view.Messages[2].Role != "tool" || view.Messages[2].ReceiptOutcome != core.ReceiptOutcomeInterrupted {
		t.Fatalf("recovered history does not contain an interrupted receipt: %#v", view.Messages)
	}
	if len(view.SourceEntryIDs) != 3 || view.SourceEntryIDs[0] != "user-1" || view.SourceEntryIDs[1] != fixture.assistant.ID || view.SourceEntryIDs[2] == "" || view.Revision == "" {
		t.Fatalf("recovered view did not publish complete provenance and revision: %#v", view)
	}
	if applyCalls != 1 || inspectCalls != 1 {
		t.Fatalf("authority calls = apply:%d inspect:%d, want 1/1", applyCalls, inspectCalls)
	}
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}
	rows := readFixtureRows(t, fixture.directory, fixture.sessionID)
	if got := countReceipts(rows, "call-1"); got != 1 {
		t.Fatalf("synthesized receipt count = %d, want 1", got)
	}
	for _, row := range rows {
		if row.Type == EntryTypeTool && row.ActionState == "" && row.ToolCallID == "call-1" {
			if row.ActionID != "action-1" || row.ActionBatchID != "batch-1" || row.DispatchState != DispatchStateNotDispatched || row.ReceiptOutcome != core.ReceiptOutcomeInterrupted {
				t.Fatalf("incorrect absent-marker receipt: %#v", row)
			}
		}
	}
}

func TestPrepareResumeReconcilesPresentMarkerAsUnknownWithoutReplay(t *testing.T) {
	for _, marker := range []DispatchMarkerState{DispatchMarkerPresent, DispatchMarkerResolved} {
		t.Run(fmt.Sprintf("marker-%d", marker), func(t *testing.T) {
			fixture := createRecoveryFixture(t, true)
			authority := recoveryAuthorityFunc{inspect: func(_ context.Context, refs []PendingActionRef) ([]PendingActionDecision, error) {
				return []PendingActionDecision{{Action: refs[0], MarkerState: marker}}, nil
			}}
			prepared, err := NewDurableJSONLSessionPreparer(fixture.directory, authority).PrepareResume(context.Background(), fixture.sessionID)
			if err != nil {
				t.Fatalf("resume after marker: %v", err)
			}
			view := prepared.View()
			if len(view.Messages) != 3 || view.Messages[2].ReceiptOutcome != core.ReceiptOutcomeUnknown {
				t.Fatalf("marker %d must synthesize an unknown receipt: %#v", marker, view.Messages)
			}
			if len(view.SourceEntryIDs) != 3 || view.SourceEntryIDs[2] == "" || view.Revision == "" {
				t.Fatalf("unknown receipt view lacks complete provenance and revision: %#v", view)
			}
			if err := prepared.Close(); err != nil {
				t.Fatal(err)
			}
			rows := readFixtureRows(t, fixture.directory, fixture.sessionID)
			if got := countReceipts(rows, "call-1"); got != 1 {
				t.Fatalf("unknown receipt count after recovery = %d, want 1", got)
			}
			for _, row := range rows {
				if row.Type == EntryTypeTool && row.ActionState == "" && row.ToolCallID == "call-1" {
					if row.DispatchState != DispatchStateDispatched || row.ReceiptOutcome != string(core.ReceiptOutcomeUnknown) {
						t.Fatalf("marker %d produced an incorrect recovery receipt: %#v", marker, row)
					}
				}
			}
			prepared, err = NewDurableJSONLSessionPreparer(fixture.directory, authority).PrepareResume(context.Background(), fixture.sessionID)
			if err != nil {
				t.Fatalf("repeat resume must be idempotent: %v", err)
			}
			if err := prepared.Close(); err != nil {
				t.Fatal(err)
			}
			if got := countReceipts(readFixtureRows(t, fixture.directory, fixture.sessionID), "call-1"); got != 1 {
				t.Fatalf("repeat recovery duplicated receipt, count=%d", got)
			}
		})
	}
}

func TestPrepareResumeRequiresAuthorityBeforeExposingOpenActionHistory(t *testing.T) {
	fixture := createRecoveryFixture(t, true)
	if prepared, err := NewDurableJSONLSessionPreparer(fixture.directory, nil).PrepareResume(context.Background(), fixture.sessionID); err == nil {
		_ = prepared.Close()
		t.Fatal("resume with an unresolved intent and no recovery authority must fail closed")
	}
}

func TestPrepareResumeFailsClosedOnUnreservedOccurrenceWithMarker(t *testing.T) {
	fixture := createRecoveryFixture(t, false)
	authority := recoveryAuthorityFunc{inspect: func(_ context.Context, refs []PendingActionRef) ([]PendingActionDecision, error) {
		return []PendingActionDecision{{Action: refs[0], MarkerState: DispatchMarkerPresent}}, nil
	}}
	if prepared, err := NewDurableJSONLSessionPreparer(fixture.directory, authority).PrepareResume(context.Background(), fixture.sessionID); err == nil {
		_ = prepared.Close()
		t.Fatal("unreserved occurrence cannot be correlated with an action marker")
	}
}

func TestPrepareResumeClosesUnreservedOccurrenceWithoutInventingReservation(t *testing.T) {
	fixture := createRecoveryFixture(t, false)
	var inspected PendingActionRef
	authority := recoveryAuthorityFunc{inspect: func(_ context.Context, refs []PendingActionRef) ([]PendingActionDecision, error) {
		if len(refs) != 1 {
			t.Fatalf("unreserved pending references = %#v, want one", refs)
		}
		inspected = refs[0]
		return []PendingActionDecision{{Action: refs[0], MarkerState: DispatchMarkerAbsent}}, nil
	}}
	prepared, err := NewDurableJSONLSessionPreparer(fixture.directory, authority).PrepareResume(context.Background(), fixture.sessionID)
	if err != nil {
		t.Fatalf("resume unreserved absent occurrence: %v", err)
	}
	if inspected.ActionID != "" || inspected.ActionBatchID != "" {
		t.Fatalf("recovery fabricated reservation identity: %#v", inspected)
	}
	if got := prepared.View().Messages[2].ReceiptOutcome; got != core.ReceiptOutcomeInterrupted {
		t.Fatalf("unreserved absent marker outcome = %q, want interrupted", got)
	}
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}
	rows := readFixtureRows(t, fixture.directory, fixture.sessionID)
	for _, row := range rows {
		if row.Type == EntryTypeTool && row.ActionState == "" && row.ToolCallID == "call-1" {
			if row.ActionID != "" || row.ActionBatchID != "" || row.DispatchState != DispatchStateNotDispatched {
				t.Fatalf("recovered unreserved receipt invented identity: %#v", row)
			}
			return
		}
	}
	t.Fatal("recovery did not persist the unreserved interrupted receipt")
}

func TestPrepareResumeRejectsReorderedUnreservedDecisionsBeforeWriting(t *testing.T) {
	directory := t.TempDir()
	prepared, err := NewDurableJSONLSessionPreparer(directory, nil).PrepareNew(context.Background(), "decision order")
	if err != nil {
		t.Fatal(err)
	}
	zero, one, ordinal := uint64(0), uint64(1), uint64(0)
	if _, err := prepared.DurableWriter().AppendAndSync(context.Background(), SessionEntry{
		ID: "user-1", Type: EntryTypeUser, RunID: "run-1", TranscriptIndex: &zero,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := prepared.DurableWriter().AppendAndSync(context.Background(), SessionEntry{
		ID: "assistant-1", ParentID: "user-1", Type: EntryTypeAssistant, RunID: "run-1", WorkspaceID: "workspace-1",
		TranscriptIndex: &one, AssistantTranscriptOrdinal: &ordinal,
		ToolCalls: []llm.ToolCall{{ID: "call-1", Name: "read"}, {ID: "call-2", Name: "read"}},
	}); err != nil {
		t.Fatal(err)
	}
	sessionID := prepared.View().SessionID
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "sessions", sessionID+".jsonl")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	authority := recoveryAuthorityFunc{inspect: func(_ context.Context, refs []PendingActionRef) ([]PendingActionDecision, error) {
		if len(refs) != 2 || refs[0].ActionID != "" || refs[1].ActionID != "" || refs[0].ToolCallID != "call-1" || refs[1].ToolCallID != "call-2" {
			t.Fatalf("unexpected unreserved references: %#v", refs)
		}
		return []PendingActionDecision{
			{Action: refs[1], MarkerState: DispatchMarkerUnverifiable},
			{Action: refs[0], MarkerState: DispatchMarkerAbsent},
		}, nil
	}}
	if resumed, err := NewDurableJSONLSessionPreparer(directory, authority).PrepareResume(context.Background(), sessionID); err == nil {
		_ = resumed.Close()
		t.Fatal("reordered unreserved marker decisions must fail closed")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("mismatched recovery decisions wrote receipts: before=%d after=%d", len(before), len(after))
	}
}

func TestPrepareResumeAppliesValidatedResolutionBeforeInspectingMarkers(t *testing.T) {
	fixture := createRecoveryFixture(t, true)
	zero := uint64(0)
	secondOrdinal, secondTranscriptIndex := uint64(1), uint64(3)
	appended := appendFixtureRows(t, fixture, SessionEntry{
		ID: "execution-1", ParentID: fixture.intent.ID, Type: EntryTypeTool, RunID: "run-1", WorkspaceID: "workspace-1",
		ActionBatchID: "batch-1", ActionID: "action-1", ActionState: ActionStateExecution,
		AssistantTranscriptOrdinal: &zero, ToolCallID: "call-1", ToolName: "read", ExecutionIdentity: testProcessGroupIdentity(), Timestamp: time.Unix(13, 0).UTC(),
	}, SessionEntry{
		ID: "receipt-1", ParentID: "execution-1", Type: EntryTypeTool, RunID: "run-1", WorkspaceID: "workspace-1",
		ActionBatchID: "batch-1", ActionID: "action-1", AssistantTranscriptOrdinal: &zero,
		TranscriptIndex: ptrUint64(2), ToolCallID: "call-1", ToolName: "read", DispatchState: DispatchStateDispatched,
		ReceiptOutcome: core.ReceiptOutcomeError, Content: "known failure", Timestamp: time.Unix(14, 0).UTC(),
	}, SessionEntry{
		ID: "resolution-1", ParentID: "receipt-1", Type: EntryTypeCheckpoint, RunID: "run-1", WorkspaceID: "workspace-1",
		ActionBatchID: "batch-1", ActionID: "action-1", AssistantTranscriptOrdinal: &zero, ToolCallID: "call-1",
		CheckpointKind:      CheckpointExecutionResolution,
		ExecutionResolution: &core.ExecutionResolution{RunID: "run-1", ActionID: "action-1", Evidence: core.ResolutionLocalResourcesStopped, EvidenceRef: "execution-1"},
		Timestamp:           time.Unix(15, 0).UTC(),
	}, SessionEntry{
		ID: "assistant-2", ParentID: "resolution-1", Type: EntryTypeAssistant, RunID: "run-1",
		WorkspaceID: "workspace-1", AssistantTranscriptOrdinal: &secondOrdinal,
		TranscriptIndex: &secondTranscriptIndex, ToolCalls: []llm.ToolCall{{ID: "call-2", Name: "read"}},
		Timestamp: time.Unix(16, 0).UTC(),
	})
	fixture.execution, fixture.receipt, fixture.resolution = appended[0], appended[1], appended[2]

	var events []string
	authority := recoveryAuthorityFunc{
		apply: func(_ context.Context, resolutions []ValidatedExecutionResolution) error {
			events = append(events, "apply")
			if len(resolutions) != 1 || resolutions[0].CheckpointEntryID != fixture.resolution.ID || resolutions[0].CheckpointSeq != fixture.resolution.Seq || resolutions[0].Action.ActionID != "action-1" {
				t.Fatalf("authority received wrong validated resolution: %#v", resolutions)
			}
			return nil
		},
		inspect: func(_ context.Context, refs []PendingActionRef) ([]PendingActionDecision, error) {
			events = append(events, "inspect")
			if len(refs) != 1 || refs[0].ActionID != "" || refs[0].ToolCallID != "call-2" {
				t.Fatalf("unexpected later pending occurrence: %#v", refs)
			}
			return []PendingActionDecision{{Action: refs[0], MarkerState: DispatchMarkerAbsent}}, nil
		},
	}
	prepared, err := NewDurableJSONLSessionPreparer(fixture.directory, authority).PrepareResume(context.Background(), fixture.sessionID)
	if err != nil {
		t.Fatalf("resume resolved action: %v", err)
	}
	defer prepared.Close()
	if len(events) != 2 || events[0] != "apply" || events[1] != "inspect" {
		t.Fatalf("recovery authority order = %#v, want apply then inspect", events)
	}
}

func TestPrepareResumeRejectsInvalidLocalResolutionBeforeAuthority(t *testing.T) {
	fixture := createRecoveryFixture(t, true)
	zero := uint64(0)
	appendRawFixtureRow(t, fixture,
		SessionEntry{
			ID: "execution-1", ParentID: fixture.intent.ID, Type: EntryTypeTool, RunID: "run-1", WorkspaceID: "workspace-1",
			ActionBatchID: "batch-1", ActionID: "action-1", ActionState: ActionStateExecution,
			AssistantTranscriptOrdinal: &zero, ToolCallID: "call-1", ToolName: "read",
		},
	)
	appendRawFixtureRow(t, fixture, SessionEntry{
		ID: "receipt-1", ParentID: "execution-1", Type: EntryTypeTool, RunID: "run-1", WorkspaceID: "workspace-1",
		ActionBatchID: "batch-1", ActionID: "action-1", AssistantTranscriptOrdinal: &zero,
		TranscriptIndex: ptrUint64(2), ToolCallID: "call-1", ToolName: "read", DispatchState: DispatchStateDispatched,
		ReceiptOutcome: core.ReceiptOutcomeError,
	})
	appendRawFixtureRow(t, fixture, SessionEntry{
		ID: "resolution-1", ParentID: "receipt-1", Type: EntryTypeCheckpoint, RunID: "run-1", WorkspaceID: "workspace-1",
		ActionBatchID: "batch-1", ActionID: "action-1", AssistantTranscriptOrdinal: &zero, ToolCallID: "call-1",
		CheckpointKind:      CheckpointExecutionResolution,
		ExecutionResolution: &core.ExecutionResolution{RunID: "run-1", ActionID: "action-1", Evidence: core.ResolutionLocalResourcesStopped, EvidenceRef: "missing-execution"},
	})
	var applyCalls int
	authority := recoveryAuthorityFunc{apply: func(context.Context, []ValidatedExecutionResolution) error {
		applyCalls++
		return nil
	}}
	if prepared, err := NewDurableJSONLSessionPreparer(fixture.directory, authority).PrepareResume(context.Background(), fixture.sessionID); err == nil {
		_ = prepared.Close()
		t.Fatal("invalid local resolution evidence must fail closed")
	}
	if applyCalls != 0 {
		t.Fatalf("global authority was called %d times before local validation failed", applyCalls)
	}
}

func TestPrepareResumeRejectsLocalResolutionWithoutExecutionIdentity(t *testing.T) {
	fixture := createRecoveryFixture(t, true)
	zero := uint64(0)
	appendRawFixtureRow(t, fixture,
		SessionEntry{
			ID: "execution-1", ParentID: fixture.intent.ID, Type: EntryTypeTool, RunID: "run-1", WorkspaceID: "workspace-1",
			ActionBatchID: "batch-1", ActionID: "action-1", ActionState: ActionStateExecution,
			AssistantTranscriptOrdinal: &zero, ToolCallID: "call-1", ToolName: "read",
		},
	)
	appendRawFixtureRow(t, fixture, SessionEntry{
		ID: "receipt-1", ParentID: "execution-1", Type: EntryTypeTool, RunID: "run-1", WorkspaceID: "workspace-1",
		ActionBatchID: "batch-1", ActionID: "action-1", AssistantTranscriptOrdinal: &zero,
		TranscriptIndex: ptrUint64(2), ToolCallID: "call-1", ToolName: "read", DispatchState: DispatchStateDispatched,
		ReceiptOutcome: core.ReceiptOutcomeError,
	})
	appendRawFixtureRow(t, fixture, SessionEntry{
		ID: "resolution-1", ParentID: "receipt-1", Type: EntryTypeCheckpoint, RunID: "run-1", WorkspaceID: "workspace-1",
		ActionBatchID: "batch-1", ActionID: "action-1", AssistantTranscriptOrdinal: &zero, ToolCallID: "call-1",
		CheckpointKind:      CheckpointExecutionResolution,
		ExecutionResolution: &core.ExecutionResolution{RunID: "run-1", ActionID: "action-1", Evidence: core.ResolutionLocalResourcesStopped, EvidenceRef: "execution-1"},
	})
	var applyCalls int
	authority := recoveryAuthorityFunc{apply: func(context.Context, []ValidatedExecutionResolution) error {
		applyCalls++
		return nil
	}}
	if prepared, err := NewDurableJSONLSessionPreparer(fixture.directory, authority).PrepareResume(context.Background(), fixture.sessionID); err == nil {
		_ = prepared.Close()
		t.Fatal("local-resource resolution requires an execution row with verifiable identity")
	}
	if applyCalls != 0 {
		t.Fatalf("global authority was called %d times for unprovable local identity", applyCalls)
	}
}

func testProcessGroupIdentity() *ExecutionIdentity {
	return &ExecutionIdentity{
		Kind:              ExecutionIdentityProcessGroup,
		Platform:          "linux",
		HostGeneration:    "boot-test",
		LeaderPID:         123,
		ProcessStartToken: "start-token",
		ProcessGroupID:    123,
	}
}

func appendFixtureRows(t *testing.T, fixture recoveryFixture, rows ...SessionEntry) []SessionEntry {
	t.Helper()
	path := filepath.Join(fixture.directory, "sessions", fixture.sessionID+".jsonl")
	file, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := flockExclusiveContext(context.Background(), file); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	loaded, err := readDurableJSONL(file, filepath.Dir(path))
	if err != nil {
		_ = flockUnlock(file)
		_ = file.Close()
		t.Fatal(err)
	}
	entryMap, maxSeq, label, branch, leaf, err := validateLoadedDurableEntries(loaded, fixture.sessionID)
	if err != nil {
		_ = flockUnlock(file)
		_ = file.Close()
		t.Fatal(err)
	}
	store := newDurableJSONLStore(file, fixture.sessionID, label, branch, entryMap, maxSeq, durableEntryIDs(loaded), durableJSONLIO{})
	store.activeLeafID = leaf
	canonical, err := store.AppendAndSync(context.Background(), rows...)
	if err != nil {
		_ = store.close()
		t.Fatal(err)
	}
	if err := store.close(); err != nil {
		t.Fatal(err)
	}
	return canonical
}

func ptrUint64(value uint64) *uint64 {
	return &value
}

func readFixtureRows(t *testing.T, directory, sessionID string) []SessionEntry {
	t.Helper()
	path := filepath.Join(directory, "sessions", sessionID+".jsonl")
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	rows, err := readDurableJSONL(file, filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func countReceipts(entries []SessionEntry, toolCallID string) int {
	count := 0
	for _, entry := range entries {
		if entry.Type == EntryTypeTool && entry.ActionState == "" && entry.ToolCallID == toolCallID {
			count++
		}
	}
	return count
}

func TestPrepareResumeAuthorityErrorKeepsSessionClosed(t *testing.T) {
	fixture := createRecoveryFixture(t, true)
	wantErr := errors.New("index unavailable")
	authority := recoveryAuthorityFunc{inspect: func(context.Context, []PendingActionRef) ([]PendingActionDecision, error) {
		return nil, wantErr
	}}
	prepared, err := NewDurableJSONLSessionPreparer(fixture.directory, authority).PrepareResume(context.Background(), fixture.sessionID)
	if err == nil || !strings.Contains(err.Error(), wantErr.Error()) {
		if prepared != nil {
			_ = prepared.Close()
		}
		t.Fatalf("authority error must fail closed, got prepared=%T err=%v", prepared, err)
	}
}

func TestPrepareResumeRejectsInvalidRowsBeforeCallingAuthority(t *testing.T) {
	tests := []struct {
		name string
		row  func(recoveryFixture) SessionEntry
	}{
		{
			name: "unknown record version",
			row: func(fixture recoveryFixture) SessionEntry {
				return SessionEntry{ID: "future-row", ParentID: fixture.intent.ID, Type: EntryTypeSystem, RecordVersion: 2, Content: "future"}
			},
		},
		{
			name: "version one transcript row missing index",
			row: func(fixture recoveryFixture) SessionEntry {
				return SessionEntry{ID: "malformed-user", ParentID: fixture.intent.ID, Type: EntryTypeUser, RecordVersion: 1, RunID: "run-1"}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := createRecoveryFixture(t, true)
			appendRawFixtureRow(t, fixture, test.row(fixture))
			var applyCalls, inspectCalls int
			authority := recoveryAuthorityFunc{
				apply: func(context.Context, []ValidatedExecutionResolution) error {
					applyCalls++
					return nil
				},
				inspect: func(_ context.Context, refs []PendingActionRef) ([]PendingActionDecision, error) {
					inspectCalls++
					decisions := make([]PendingActionDecision, len(refs))
					for i, ref := range refs {
						decisions[i] = PendingActionDecision{Action: ref, MarkerState: DispatchMarkerAbsent}
					}
					return decisions, nil
				},
			}
			if prepared, err := NewDurableJSONLSessionPreparer(fixture.directory, authority).PrepareResume(context.Background(), fixture.sessionID); err == nil {
				_ = prepared.Close()
				t.Fatal("invalid durable row must fail before recovery authority is called")
			}
			if applyCalls != 0 || inspectCalls != 0 {
				t.Fatalf("authority calls before invalid-row rejection = apply:%d inspect:%d, want 0/0", applyCalls, inspectCalls)
			}
		})
	}
}

func TestPrepareResumeRejectsProjectedHistoryBeforeCallingAuthority(t *testing.T) {
	fixture := createRecoveryFixture(t, true)
	zero := uint64(0)
	appendRawFixtureRow(t, fixture, SessionEntry{
		ID: "hidden-receipt", ParentID: fixture.intent.ID, Type: EntryTypeTool, RunID: "run-1", WorkspaceID: "workspace-1",
		ActionBatchID: "batch-1", ActionID: "action-1", AssistantTranscriptOrdinal: &zero,
		TranscriptIndex: ptrUint64(2), ToolCallID: "call-1", ToolName: "read",
		DispatchState: DispatchStateNotDispatched, ReceiptOutcome: core.ReceiptOutcomeInterrupted, SurfaceOp: SurfaceOpHidden,
	})
	path := filepath.Join(fixture.directory, "sessions", fixture.sessionID+".jsonl")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var applyCalls, inspectCalls int
	authority := recoveryAuthorityFunc{
		apply: func(context.Context, []ValidatedExecutionResolution) error {
			applyCalls++
			return nil
		},
		inspect: func(_ context.Context, refs []PendingActionRef) ([]PendingActionDecision, error) {
			inspectCalls++
			return []PendingActionDecision{{Action: refs[0], MarkerState: DispatchMarkerAbsent}}, nil
		},
	}
	if prepared, err := NewDurableJSONLSessionPreparer(fixture.directory, authority).PrepareResume(context.Background(), fixture.sessionID); err == nil {
		_ = prepared.Close()
		t.Fatal("inconsistent projected history must fail before recovery authority is called")
	}
	if applyCalls != 0 || inspectCalls != 0 {
		t.Fatalf("authority calls before projected-history rejection = apply:%d inspect:%d, want 0/0", applyCalls, inspectCalls)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("projected-history rejection changed durable bytes: before=%d after=%d", len(before), len(after))
	}
}

func appendRawFixtureRow(t *testing.T, fixture recoveryFixture, row SessionEntry) {
	t.Helper()
	entries := readFixtureRows(t, fixture.directory, fixture.sessionID)
	if row.RecordVersion == 0 {
		row.RecordVersion = 1
	}
	row.SessionID = fixture.sessionID
	row.SessionLabel = entries[0].SessionLabel
	row.BranchID = entries[len(entries)-1].BranchID
	row.Seq = entries[len(entries)-1].Seq + 1
	if row.ParentID == "" {
		row.ParentID = entries[len(entries)-1].ID
	}
	_, digest, err := canonicalEntryPayload(row)
	if err != nil {
		t.Fatal(err)
	}
	row.PayloadDigest = digest
	encoded, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(fixture.directory, "sessions", fixture.sessionID+".jsonl")
	file, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(append(encoded, '\n')); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}
