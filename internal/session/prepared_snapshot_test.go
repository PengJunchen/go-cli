package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"
)

func TestPreparedSessionSnapshotRetainsDurableOpenGroupAndPublishedView(t *testing.T) {
	ctx := context.Background()
	processStartToken := newSessionTestCanary(t)
	prepared, err := NewDurableJSONLSessionPreparer(t.TempDir(), nil).PrepareNew(ctx, "snapshot open tool group")
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()

	zero, one := uint64(0), uint64(1)
	user, err := prepared.DurableWriter().AppendAndSync(ctx, SessionEntry{
		ID: "user-entry", Type: EntryTypeUser, RunID: "run-snapshot", TranscriptIndex: &zero,
		Content:       "read a file",
		ContentBlocks: []llm.ContentBlock{{Type: "image_url", ImageURL: &llm.ImageURL{URL: "data:image/png;base64,abc", Detail: "high"}}},
	})
	if err != nil {
		t.Fatalf("append user: %v", err)
	}
	ordinal := uint64(0)
	_, err = prepared.DurableWriter().AppendAndSync(ctx,
		SessionEntry{
			ID: "assistant-entry", ParentID: user[0].ID, Type: EntryTypeAssistant,
			RunID: "run-snapshot", AssistantTranscriptOrdinal: &ordinal, TranscriptIndex: &one,
			Usage:     &llm.Usage{InputTokens: 5, OutputTokens: 3, TotalTokens: 8},
			ToolCalls: []llm.ToolCall{{ID: "call-snapshot", Name: "read", Args: map[string]any{"nested": []any{map[string]any{"path": "README.md"}}}}},
		},
		SessionEntry{
			ID: "intent-entry", ParentID: "assistant-entry", Type: EntryTypeTool,
			RunID: "run-snapshot", WorkspaceID: "workspace-snapshot", ActionBatchID: "batch-snapshot", ActionID: "action-snapshot",
			ActionState: ActionStateIntent, AssistantTranscriptOrdinal: &ordinal,
			ToolCallID: "call-snapshot", ToolName: "read", ArgumentDigest: strings.Repeat("a", 64), SchemaDigest: strings.Repeat("b", 64),
			ToolIdentity: &tools.TrustedToolIdentitySnapshot{
				RegistrationSource: "builtin", ToolName: "read", RegistrationGeneration: 1,
				ExecutorFingerprint: strings.Repeat("c", 64), ScopeFingerprint: strings.Repeat("d", 64),
			},
			EffectSet: []tools.TrustedToolEffect{tools.EffectWorkspaceRead},
		},
		SessionEntry{
			ID: "execution-entry", ParentID: "intent-entry", Type: EntryTypeTool,
			RunID: "run-snapshot", WorkspaceID: "workspace-snapshot", ActionBatchID: "batch-snapshot", ActionID: "action-snapshot",
			ActionState: ActionStateExecution, AssistantTranscriptOrdinal: &ordinal,
			ToolCallID: "call-snapshot", ToolName: "read",
			ExecutionIdentity: &ExecutionIdentity{
				Kind: ExecutionIdentityProcessGroup, Platform: "linux", HostGeneration: "boot-1",
				LeaderPID: 42, ProcessStartToken: processStartToken, ProcessGroupID: 42,
			},
		},
	)
	if err != nil {
		t.Fatalf("append open tool-call group: %v", err)
	}

	snapshot, err := prepared.Snapshot()
	if err != nil {
		t.Fatalf("snapshot prepared session: %v", err)
	}
	if got, want := len(snapshot.DurableEntries), 4; got != want {
		t.Fatalf("durable row count = %d, want %d", got, want)
	}
	wantIDs := []string{"user-entry", "assistant-entry", "intent-entry", "execution-entry"}
	for index, want := range wantIDs {
		if got := snapshot.DurableEntries[index].ID; got != want {
			t.Fatalf("durable row %d ID = %q, want physical append order %q", index, got, want)
		}
		if got := snapshot.DurableEntries[index].Seq; got != uint64(index+1) {
			t.Fatalf("durable row %q sequence = %d, want %d", want, got, index+1)
		}
	}
	if snapshot.SessionID != user[0].SessionID || snapshot.SessionLabel != "snapshot open tool group" {
		t.Fatalf("snapshot identity = (%q, %q), want prepared physical identity and label", snapshot.SessionID, snapshot.SessionLabel)
	}
	if snapshot.ActiveBranchID != "main" || snapshot.ActiveLeafEntryID != "execution-entry" || snapshot.MaxSeq != 4 {
		t.Fatalf("snapshot durable cursor = branch:%q leaf:%q maxSeq:%d", snapshot.ActiveBranchID, snapshot.ActiveLeafEntryID, snapshot.MaxSeq)
	}
	if len(snapshot.PublishedView.Messages) != 1 || snapshot.PublishedView.SourceEntryIDs[0] != "user-entry" || snapshot.PublishedView.Revision != prepared.View().Revision {
		t.Fatalf("open tool group must leave a separate prior complete published view: %#v", snapshot.PublishedView)
	}

	// Mutating every nested value represented in the persisted open group must
	// not mutate the handle's canonical durable prefix or its published view.
	snapshot.DurableEntries[0].ContentBlocks[0].ImageURL.URL = "mutated"
	args := snapshot.DurableEntries[1].ToolCalls[0].Args.(map[string]any)
	args["nested"].([]any)[0].(map[string]any)["path"] = "mutated"
	snapshot.DurableEntries[1].Usage.InputTokens = 999
	snapshot.DurableEntries[2].ToolIdentity.ExecutorFingerprint = "mutated"
	snapshot.DurableEntries[2].EffectSet[0] = tools.EffectUnknown
	snapshot.DurableEntries[3].ExecutionIdentity.ProcessGroupID = 999
	snapshot.PublishedView.Messages[0].ContentBlocks[0].ImageURL.URL = "mutated-view"
	snapshot.PublishedView.SourceEntryIDs[0] = "mutated-view"

	repeated, err := prepared.Snapshot()
	if err != nil {
		t.Fatalf("snapshot after caller mutation: %v", err)
	}
	if repeated.DurableEntries[0].ContentBlocks[0].ImageURL.URL != "data:image/png;base64,abc" || repeated.DurableEntries[1].ToolCalls[0].Args.(map[string]any)["nested"].([]any)[0].(map[string]any)["path"] != "README.md" {
		t.Fatal("snapshot aliases nested content-block or tool-call payload")
	}
	if repeated.DurableEntries[1].Usage.InputTokens != 5 || repeated.DurableEntries[2].ToolIdentity.ExecutorFingerprint != strings.Repeat("c", 64) || repeated.DurableEntries[2].EffectSet[0] != tools.EffectWorkspaceRead {
		t.Fatal("snapshot aliases usage or trusted intent metadata")
	}
	if repeated.DurableEntries[3].ExecutionIdentity.ProcessGroupID != 42 || repeated.PublishedView.Messages[0].ContentBlocks[0].ImageURL.URL != "data:image/png;base64,abc" || repeated.PublishedView.SourceEntryIDs[0] != "user-entry" {
		t.Fatal("snapshot aliases execution identity or published view")
	}
}

func TestPreparedSessionSnapshotPreservesLegacyPhysicalAppendOrder(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	sessionsDir := filepath.Join(root, "sessions")
	if err := os.MkdirAll(sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	const sessionID = "legacy-order"
	rows := []SessionEntry{
		{ID: "z-root", Type: EntryTypeSystem, Content: "root", RecordVersion: 0},
		{ID: "a-child", ParentID: "z-root", Type: EntryTypeSystem, Content: "child", RecordVersion: 0},
	}
	var data bytes.Buffer
	for _, row := range rows {
		encoded, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		data.Write(encoded)
		data.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(sessionsDir, sessionID+".jsonl"), data.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	prepared, err := NewDurableJSONLSessionPreparer(root, nil).PrepareResume(ctx, sessionID)
	if err != nil {
		t.Fatalf("resume legacy physical session: %v", err)
	}
	defer prepared.Close()

	snapshot, err := prepared.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.DurableEntries) != 2 || snapshot.DurableEntries[0].ID != "z-root" || snapshot.DurableEntries[1].ID != "a-child" {
		t.Fatalf("legacy snapshot reordered physical rows: %#v", snapshot.DurableEntries)
	}
	if snapshot.MaxSeq != 0 || snapshot.ActiveLeafEntryID != "a-child" {
		t.Fatalf("legacy snapshot fabricated sequence or lost physical active leaf: maxSeq=%d leaf=%q", snapshot.MaxSeq, snapshot.ActiveLeafEntryID)
	}
}

func TestPreparedSessionSnapshotIncludesBranchesCompactionAndHiddenRows(t *testing.T) {
	ctx := context.Background()
	prepared, err := NewDurableJSONLSessionPreparer(t.TempDir(), nil).PrepareNew(ctx, "complete physical history")
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	writer := prepared.DurableWriter()
	zero, one, two := uint64(0), uint64(1), uint64(2)
	root, err := writer.AppendAndSync(ctx, SessionEntry{
		ID: "main-root", Type: EntryTypeUser, RunID: "run-branches", TranscriptIndex: &zero, Content: "root",
	})
	if err != nil {
		t.Fatal(err)
	}
	hidden, err := writer.AppendAndSync(ctx, SessionEntry{
		ID: "main-hidden", ParentID: root[0].ID, Type: EntryTypeUser, BranchID: "main",
		RunID: "run-branches", TranscriptIndex: &one, SurfaceOp: SurfaceOpHidden, Content: "hidden source row",
	})
	if err != nil {
		t.Fatal(err)
	}
	compaction, err := writer.AppendAndSync(ctx, SessionEntry{
		ID: "main-compaction", ParentID: hidden[0].ID, Type: EntryTypeCompaction, BranchID: "main",
		Summary: "summary through root", CompactedThroughEntryID: root[0].ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	branchRoot, err := writer.AppendAndSync(ctx, SessionEntry{
		ID: "feature-root", ParentID: root[0].ID, Type: EntryTypeCheckpoint,
		BranchID: "feature", ParentBranchID: "main", BaseLeafEntryID: root[0].ID,
		ActiveLeafEntryID: "feature-root", CheckpointKind: CheckpointBranch,
	})
	if err != nil {
		t.Fatal(err)
	}
	feature, err := writer.AppendAndSync(ctx, SessionEntry{
		ID: "feature-user", ParentID: branchRoot[0].ID, Type: EntryTypeUser, BranchID: "feature",
		RunID: "run-branches", TranscriptIndex: &two, Content: "feature continuation",
	})
	if err != nil {
		t.Fatal(err)
	}

	snapshot, err := prepared.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	wantIDs := []string{"main-root", "main-hidden", "main-compaction", "feature-root", "feature-user"}
	if len(snapshot.DurableEntries) != len(wantIDs) {
		t.Fatalf("snapshot rows = %d, want all %d durable rows", len(snapshot.DurableEntries), len(wantIDs))
	}
	for index, want := range wantIDs {
		if got := snapshot.DurableEntries[index].ID; got != want {
			t.Fatalf("physical row %d ID = %q, want %q", index, got, want)
		}
	}
	if snapshot.DurableEntries[1].SurfaceOp != SurfaceOpHidden || snapshot.DurableEntries[2].CompactedThroughEntryID != root[0].ID || snapshot.DurableEntries[3].CheckpointKind != CheckpointBranch || snapshot.DurableEntries[3].ParentBranchID != "main" {
		t.Fatalf("snapshot lost hidden, compaction, or branch metadata: %#v", snapshot.DurableEntries)
	}
	if snapshot.ActiveBranchID != "feature" || snapshot.ActiveLeafEntryID != feature[0].ID || snapshot.MaxSeq != 5 {
		t.Fatalf("snapshot cursor = branch:%q leaf:%q maxSeq:%d", snapshot.ActiveBranchID, snapshot.ActiveLeafEntryID, snapshot.MaxSeq)
	}
	if len(snapshot.PublishedView.Messages) != 2 || snapshot.PublishedView.SourceEntryIDs[0] != root[0].ID || snapshot.PublishedView.SourceEntryIDs[1] != feature[0].ID {
		t.Fatalf("published view did not remain a projection of the selected branch: %#v", snapshot.PublishedView)
	}
	if snapshot.DurableEntries[2].ID != compaction[0].ID {
		t.Fatal("snapshot lost the compaction row outside the active branch")
	}
}

func TestPreparedSessionSnapshotCloneDeepCopiesEveryEntryPayload(t *testing.T) {
	ordinal, transcriptIndex := uint64(0), uint64(0)
	source := PreparedSessionSnapshot{
		SessionID: "physical", SessionLabel: "label", ActiveBranchID: "feature", ActiveLeafEntryID: "leaf", MaxSeq: 9,
		DurableEntries: []SessionEntry{{
			ID: "checkpoint", Type: EntryTypeCheckpoint, ContentBlocks: []llm.ContentBlock{{Type: "image_url", ImageURL: &llm.ImageURL{URL: "original", Detail: "low"}}},
			Usage: &llm.Usage{InputTokens: 1}, ToolCalls: []llm.ToolCall{{ID: "call", Name: "read", Args: map[string]any{"key": []any{"original"}}}},
			ToolIdentity:               &tools.TrustedToolIdentitySnapshot{ToolName: "read", ExecutorFingerprint: "original"},
			EffectSet:                  []tools.TrustedToolEffect{tools.EffectWorkspaceRead},
			ExecutionIdentity:          &ExecutionIdentity{ProcessGroupID: 7},
			ExecutionResolution:        &core.ExecutionResolution{RunID: "run", ActionID: "action", Evidence: core.ResolutionLocalResourcesStopped, EvidenceRef: "evidence"},
			AssistantTranscriptOrdinal: &ordinal, TranscriptIndex: &transcriptIndex,
		}},
		PublishedView: SessionView{SessionID: "physical", BranchID: "feature", Revision: "rev", Messages: []core.AgentMessage{{
			Role: "assistant", ContentBlocks: []llm.ContentBlock{{Type: "image_url", ImageURL: &llm.ImageURL{URL: "view"}}},
			Usage: &llm.Usage{OutputTokens: 2}, ToolCalls: []llm.ToolCall{{ID: "view-call", Name: "read", Args: map[string]any{"key": "view"}}},
		}}, SourceEntryIDs: []string{"checkpoint"}},
	}
	cloned := source.Clone()
	entry := &cloned.DurableEntries[0]
	entry.ContentBlocks[0].ImageURL.URL = "changed"
	entry.Usage.InputTokens = 99
	entry.ToolCalls[0].Args.(map[string]any)["key"].([]any)[0] = "changed"
	entry.ToolIdentity.ExecutorFingerprint = "changed"
	entry.EffectSet[0] = tools.EffectUnknown
	entry.ExecutionIdentity.ProcessGroupID = 99
	entry.ExecutionResolution.EvidenceRef = "changed"
	*entry.AssistantTranscriptOrdinal = 99
	*entry.TranscriptIndex = 99
	cloned.PublishedView.Messages[0].ContentBlocks[0].ImageURL.URL = "changed"
	cloned.PublishedView.Messages[0].Usage.OutputTokens = 99
	cloned.PublishedView.Messages[0].ToolCalls[0].Args.(map[string]any)["key"] = "changed"
	cloned.PublishedView.SourceEntryIDs[0] = "changed"

	if source.DurableEntries[0].ContentBlocks[0].ImageURL.URL != "original" || source.DurableEntries[0].Usage.InputTokens != 1 || source.DurableEntries[0].ToolCalls[0].Args.(map[string]any)["key"].([]any)[0] != "original" {
		t.Fatal("PreparedSessionSnapshot.Clone aliases canonical entry payload")
	}
	if source.DurableEntries[0].ToolIdentity.ExecutorFingerprint != "original" || source.DurableEntries[0].EffectSet[0] != tools.EffectWorkspaceRead || source.DurableEntries[0].ExecutionIdentity.ProcessGroupID != 7 || source.DurableEntries[0].ExecutionResolution.EvidenceRef != "evidence" || *source.DurableEntries[0].AssistantTranscriptOrdinal != 0 || *source.DurableEntries[0].TranscriptIndex != 0 {
		t.Fatal("PreparedSessionSnapshot.Clone aliases typed entry metadata")
	}
	if source.PublishedView.Messages[0].ContentBlocks[0].ImageURL.URL != "view" || source.PublishedView.Messages[0].Usage.OutputTokens != 2 || source.PublishedView.Messages[0].ToolCalls[0].Args.(map[string]any)["key"] != "view" || source.PublishedView.SourceEntryIDs[0] != "checkpoint" {
		t.Fatal("PreparedSessionSnapshot.Clone aliases published view")
	}
}

func TestPreparedSessionSnapshotRejectsClosedAndPoisonedHandles(t *testing.T) {
	t.Run("closed", func(t *testing.T) {
		prepared, err := NewDurableJSONLSessionPreparer(t.TempDir(), nil).PrepareNew(context.Background(), "closed")
		if err != nil {
			t.Fatal(err)
		}
		if err := prepared.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := prepared.Snapshot(); !errors.Is(err, ErrDurableWriterClosed) {
			t.Fatalf("Snapshot after Close = %v, want closed error", err)
		}
	})

	t.Run("poisoned", func(t *testing.T) {
		preparer := newDurableJSONLSessionPreparer(t.TempDir(), nil, durableJSONLIO{
			write: func(_ *os.File, _ []byte) (int, error) { return 0, errors.New("injected partial write failure") },
		})
		prepared, err := preparer.PrepareNew(context.Background(), "poisoned")
		if err != nil {
			t.Fatal(err)
		}
		defer prepared.Close()
		zero := uint64(0)
		if _, err := prepared.DurableWriter().AppendAndSync(context.Background(), SessionEntry{
			ID: "user", Type: EntryTypeUser, RunID: "run", TranscriptIndex: &zero,
		}); err == nil {
			t.Fatal("injected write failure unexpectedly succeeded")
		}
		if _, err := prepared.Snapshot(); !errors.Is(err, ErrDurableWriterPoisoned) {
			t.Fatalf("Snapshot after write failure = %v, want poisoned error", err)
		}
	})
}
