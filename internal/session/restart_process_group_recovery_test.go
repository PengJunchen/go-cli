package session

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"
)

func TestPrepareResumeDiagnosesProcessGroupWithoutResolvingOrReplaying(t *testing.T) {
	for _, status := range []core.ProcessGroupExitStatus{
		core.ProcessGroupExitPresent,
		core.ProcessGroupExitUnknown,
		core.ProcessGroupExitAbsent,
	} {
		t.Run(string(status), func(t *testing.T) {
			fixture := createDispatchedBashRecoveryFixture(t, "owner-generation-1", true)
			var applyCalls, inspected int
			authority := recoveryAuthorityFunc{
				apply: func(_ context.Context, resolutions []ValidatedExecutionResolution) error {
					applyCalls++
					if len(resolutions) != 0 {
						t.Fatalf("resume must not manufacture local resolution from process-group status: %#v", resolutions)
					}
					return nil
				},
				inspect: func(_ context.Context, refs []PendingActionRef) ([]PendingActionDecision, error) {
					if len(refs) != 1 || refs[0].ActionID != fixture.intent.ActionID || refs[0].ToolCallID != fixture.intent.ToolCallID {
						t.Fatalf("recovery inspected a different action occurrence: %#v", refs)
					}
					return []PendingActionDecision{{Action: refs[0], MarkerState: DispatchMarkerPresent}}, nil
				},
			}
			preparer := NewDurableJSONLSessionPreparer(fixture.directory, authority)
			preparer.inspectExecutionIdentity = func(identity core.ExecutionIdentity) core.ProcessGroupExitInspection {
				inspected++
				if identity != *fixture.execution.ExecutionIdentity {
					t.Fatalf("inspector received an identity from another action: got %#v want %#v", identity, *fixture.execution.ExecutionIdentity)
				}
				return core.ProcessGroupExitInspection{Status: status, Reason: "test observation", ObservedAt: time.Unix(123, 0).UTC()}
			}

			prepared, err := preparer.PrepareResume(context.Background(), fixture.sessionID)
			if err != nil {
				t.Fatalf("resume session: %v", err)
			}
			view := prepared.View()
			if len(view.Messages) == 0 || view.Messages[len(view.Messages)-1].ReceiptOutcome != core.ReceiptOutcomeUnknown {
				t.Fatalf("process-group status %q must retain the unknown receipt: %#v", status, view.Messages)
			}
			if len(view.ExecutionRecoveryDiagnostics) != 1 {
				t.Fatalf("diagnostics = %#v, want one exact action observation", view.ExecutionRecoveryDiagnostics)
			}
			diagnostic := view.ExecutionRecoveryDiagnostics[0]
			if diagnostic.Action.ActionID != fixture.intent.ActionID || diagnostic.Action.ToolCallID != fixture.intent.ToolCallID ||
				diagnostic.IntentEntryID != fixture.intent.ID || diagnostic.ExecutionEntryID != fixture.execution.ID ||
				diagnostic.OwnerGeneration != fixture.intent.OwnerGeneration || diagnostic.Status != status ||
				diagnostic.Reason != "test observation" {
				t.Fatalf("recovery diagnostic lost exact action/owner coordinates: %#v", diagnostic)
			}
			if got := diagnostic.ObservedAt; !got.Equal(time.Unix(123, 0).UTC()) {
				t.Fatalf("diagnostic observation time = %s", got)
			}

			// A returned view is defensive; callers cannot rewrite the prepared
			// handle's recovery diagnosis.
			view.ExecutionRecoveryDiagnostics[0].Reason = "caller mutation"
			if got := prepared.View().ExecutionRecoveryDiagnostics[0].Reason; got != "test observation" {
				t.Fatalf("view mutation changed prepared diagnostic: %q", got)
			}
			if err := prepared.Close(); err != nil {
				t.Fatal(err)
			}

			rows := readFixtureRows(t, fixture.directory, fixture.sessionID)
			if got := countReceipts(rows, fixture.intent.ToolCallID); got != 1 {
				t.Fatalf("receipt count = %d, want one non-replayed unknown receipt", got)
			}
			var unknownReceipt *SessionEntry
			for index := range rows {
				row := &rows[index]
				if row.Type == EntryTypeTool && row.ActionState == "" && row.ToolCallID == fixture.intent.ToolCallID {
					unknownReceipt = row
				}
				if row.CheckpointKind == CheckpointExecutionResolution {
					t.Fatalf("status %q wrote a local execution-resolution checkpoint: %#v", status, row)
				}
			}
			if unknownReceipt == nil || unknownReceipt.ReceiptOutcome != string(core.ReceiptOutcomeUnknown) || unknownReceipt.DispatchState != DispatchStateDispatched {
				t.Fatalf("status %q did not preserve a dispatched unknown receipt: %#v", status, unknownReceipt)
			}
			if applyCalls != 1 || inspected != 1 {
				t.Fatalf("recovery calls apply=%d process-inspections=%d, want 1/1", applyCalls, inspected)
			}

			// Reopening an already receipted occurrence must keep its receipt ID
			// and rerun only the read-only diagnostic.
			prepared, err = preparer.PrepareResume(context.Background(), fixture.sessionID)
			if err != nil {
				t.Fatalf("resume session again: %v", err)
			}
			if len(prepared.View().ExecutionRecoveryDiagnostics) != 1 || prepared.View().ExecutionRecoveryDiagnostics[0].Status != status {
				t.Fatalf("second resume lost process-group diagnostic: %#v", prepared.View().ExecutionRecoveryDiagnostics)
			}
			if err := prepared.Close(); err != nil {
				t.Fatal(err)
			}
			rows = readFixtureRows(t, fixture.directory, fixture.sessionID)
			if got := countReceipts(rows, fixture.intent.ToolCallID); got != 1 {
				t.Fatalf("second resume replayed the action receipt: got %d rows", got)
			}
			if applyCalls != 2 || inspected != 2 {
				t.Fatalf("second resume calls apply=%d process-inspections=%d, want 2/2", applyCalls, inspected)
			}
		})
	}
}

func TestPrepareResumeReportsMissingProcessIdentityAsUnknown(t *testing.T) {
	fixture := createDispatchedBashRecoveryFixture(t, "owner-generation-1", false)
	var inspectorCalls int
	authority := recoveryAuthorityFunc{inspect: func(_ context.Context, refs []PendingActionRef) ([]PendingActionDecision, error) {
		return []PendingActionDecision{{Action: refs[0], MarkerState: DispatchMarkerPresent}}, nil
	}}
	preparer := NewDurableJSONLSessionPreparer(fixture.directory, authority)
	preparer.inspectExecutionIdentity = func(core.ExecutionIdentity) core.ProcessGroupExitInspection {
		inspectorCalls++
		t.Fatal("inspector must not receive a missing identity")
		return core.ProcessGroupExitInspection{}
	}

	prepared, err := preparer.PrepareResume(context.Background(), fixture.sessionID)
	if err != nil {
		t.Fatalf("resume session with missing identity: %v", err)
	}
	defer prepared.Close()
	view := prepared.View()
	if len(view.ExecutionRecoveryDiagnostics) != 1 {
		t.Fatalf("diagnostics = %#v, want one missing-identity diagnostic", view.ExecutionRecoveryDiagnostics)
	}
	diagnostic := view.ExecutionRecoveryDiagnostics[0]
	if diagnostic.Status != core.ProcessGroupExitUnknown || diagnostic.ExecutionEntryID != "" || diagnostic.Reason == "" {
		t.Fatalf("missing identity must remain Unknown with an explanation: %#v", diagnostic)
	}
	if len(view.Messages) == 0 || view.Messages[len(view.Messages)-1].ReceiptOutcome != core.ReceiptOutcomeUnknown {
		t.Fatalf("missing identity must retain unknown receipt: %#v", view.Messages)
	}
	if inspectorCalls != 0 {
		t.Fatalf("inspector calls = %d, want 0 without a persisted identity", inspectorCalls)
	}
}

func TestPrepareResumeRejectsAbsentMarkerWhenExecutionWasDurablyDispatched(t *testing.T) {
	fixture := createDispatchedBashRecoveryFixture(t, "owner-generation-1", true)
	authority := recoveryAuthorityFunc{
		inspect: func(_ context.Context, refs []PendingActionRef) ([]PendingActionDecision, error) {
			return []PendingActionDecision{{Action: refs[0], MarkerState: DispatchMarkerAbsent}}, nil
		},
	}
	prepared, err := NewDurableJSONLSessionPreparer(fixture.directory, authority).PrepareResume(context.Background(), fixture.sessionID)
	if err == nil {
		if prepared != nil {
			_ = prepared.Close()
		}
		t.Fatal("resume accepted a missing global marker despite a durable dispatched execution row")
	}
	if !strings.Contains(err.Error(), "durable dispatched execution") {
		t.Fatalf("resume error = %v, want contradictory dispatch evidence", err)
	}
	rows := readFixtureRows(t, fixture.directory, fixture.sessionID)
	if got := countReceipts(rows, fixture.intent.ToolCallID); got != 0 {
		t.Fatalf("contradictory marker recovery wrote %d receipts, want no synthesized result", got)
	}
	for _, row := range rows {
		if row.CheckpointKind == CheckpointExecutionResolution {
			t.Fatalf("contradictory marker recovery wrote a resolution: %#v", row)
		}
	}
}

func TestPrepareResumeKeepsMismatchedExecutionOwnerUnknown(t *testing.T) {
	fixture := createDispatchedBashRecoveryFixture(t, "different-owner-generation", true)
	var inspectorCalls int
	authority := recoveryAuthorityFunc{inspect: func(_ context.Context, refs []PendingActionRef) ([]PendingActionDecision, error) {
		return []PendingActionDecision{{Action: refs[0], MarkerState: DispatchMarkerPresent}}, nil
	}}
	preparer := NewDurableJSONLSessionPreparer(fixture.directory, authority)
	preparer.inspectExecutionIdentity = func(core.ExecutionIdentity) core.ProcessGroupExitInspection {
		inspectorCalls++
		t.Fatal("inspector must not receive an identity from another owner generation")
		return core.ProcessGroupExitInspection{}
	}
	prepared, err := preparer.PrepareResume(context.Background(), fixture.sessionID)
	if err != nil {
		t.Fatalf("resume with owner-mismatched execution identity: %v", err)
	}
	defer prepared.Close()
	view := prepared.View()
	if len(view.ExecutionRecoveryDiagnostics) != 1 {
		t.Fatalf("diagnostics = %#v, want owner-mismatch diagnosis", view.ExecutionRecoveryDiagnostics)
	}
	diagnostic := view.ExecutionRecoveryDiagnostics[0]
	if diagnostic.Status != core.ProcessGroupExitUnknown || diagnostic.ExecutionEntryID != "" ||
		diagnostic.OwnerGeneration != fixture.intent.OwnerGeneration || !strings.Contains(diagnostic.Reason, "owner generation") {
		t.Fatalf("owner mismatch must not bind or inspect the other execution row: %#v", diagnostic)
	}
	if len(view.Messages) == 0 || view.Messages[len(view.Messages)-1].ReceiptOutcome != core.ReceiptOutcomeUnknown {
		t.Fatalf("owner mismatch must retain unknown receipt: %#v", view.Messages)
	}
	if inspectorCalls != 0 {
		t.Fatalf("inspector calls = %d, want 0 for owner-mismatched identity", inspectorCalls)
	}
}

type dispatchedBashRecoveryFixture struct {
	directory string
	sessionID string
	intent    SessionEntry
	execution SessionEntry
}

func createDispatchedBashRecoveryFixture(t *testing.T, executionOwner string, withIdentity bool) dispatchedBashRecoveryFixture {
	t.Helper()
	directory := t.TempDir()
	prepared, err := NewDurableJSONLSessionPreparer(directory, nil).PrepareNew(context.Background(), "process-group recovery")
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	writer := prepared.DurableWriter()
	zero, inputIndex, assistantIndex := uint64(0), uint64(0), uint64(1)
	user, err := writer.AppendAndSync(context.Background(), SessionEntry{
		ID: "recovery-user", Type: EntryTypeUser, RunID: "recovery-run", TranscriptIndex: &inputIndex,
		Timestamp: time.Unix(10, 0).UTC(), Content: "run bash",
	})
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := writer.AppendAndSync(context.Background(), SessionEntry{
		ID: "recovery-assistant", ParentID: user[0].ID, Type: EntryTypeAssistant, RunID: "recovery-run",
		WorkspaceID: "workspace-recovery", AssistantTranscriptOrdinal: &zero, TranscriptIndex: &assistantIndex,
		Timestamp: time.Unix(11, 0).UTC(), ToolCalls: []llm.ToolCall{{ID: "call-bash-restart", Name: "bash", Args: map[string]any{"command": "printf side-effect"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	identity := core.ExecutionIdentity{
		Kind: core.ExecutionIdentityProcessGroup, Platform: "linux", HostGeneration: "linux:test-host",
		PIDNamespaceToken: "linux:pidns:v1:test", LeaderPID: 41, ProcessStartToken: "linux:41", ProcessGroupID: 41,
	}
	intent, err := writer.AppendAndSync(context.Background(), SessionEntry{
		ID: "recovery-intent", ParentID: assistant[0].ID, Type: EntryTypeTool, RunID: "recovery-run", WorkspaceID: "workspace-recovery",
		ActionBatchID: "recovery-batch", ActionID: "recovery-action", OwnerGeneration: "owner-generation-1", ActionState: ActionStateIntent,
		AssistantTranscriptOrdinal: &zero, ToolCallID: "call-bash-restart", ToolName: "bash",
		ArgumentDigest: strings.Repeat("a", 64), SchemaDigest: strings.Repeat("b", 64),
		ToolIdentity: &tools.TrustedToolIdentitySnapshot{
			RegistrationSource: tools.RegistrationSourceBuiltin, ToolName: "bash", RegistrationGeneration: 1,
			ExecutorFingerprint: strings.Repeat("c", 64), ScopeFingerprint: strings.Repeat("d", 64),
		},
		EffectSet: []tools.TrustedToolEffect{tools.EffectWorkspaceWrite}, Timestamp: time.Unix(12, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture := dispatchedBashRecoveryFixture{directory: directory, sessionID: prepared.View().SessionID, intent: intent[0]}
	if withIdentity {
		execution, err := writer.AppendAndSync(context.Background(), SessionEntry{
			ID: "recovery-execution", ParentID: intent[0].ID, Type: EntryTypeTool, RunID: "recovery-run", WorkspaceID: "workspace-recovery",
			ActionBatchID: "recovery-batch", ActionID: "recovery-action", OwnerGeneration: executionOwner, ActionState: ActionStateExecution,
			AssistantTranscriptOrdinal: &zero, ToolCallID: "call-bash-restart", ToolName: "bash",
			DispatchState: DispatchStateDispatched, ResourceState: core.RunExecutionCleanupPending,
			ExecutionIdentity: &identity, Timestamp: time.Unix(13, 0).UTC(),
		})
		if err != nil {
			t.Fatal(err)
		}
		fixture.execution = execution[0]
	}
	return fixture
}
