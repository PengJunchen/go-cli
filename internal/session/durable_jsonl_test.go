package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"
)

func TestReadDurableJSONLPreservesValidUnterminatedFinalRecord(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	entry := SessionEntry{ID: "entry-1", Type: EntryTypeUser, Content: "hello", Timestamp: time.Unix(10, 0).UTC()}
	line, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, line, 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	got, err := readDurableJSONL(file, dir)
	if err != nil {
		t.Fatalf("read valid unterminated row: %v", err)
	}
	if len(got) != 1 || got[0].ID != entry.ID {
		t.Fatalf("unexpected recovered entries: %#v", got)
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, append(line, '\n')) {
		t.Fatalf("valid final record was not preserved and delimited: %q", stored)
	}
}

func TestReadDurableJSONLQuarantinesOnlyTruncatedFinalSuffix(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	entry := SessionEntry{ID: "entry-1", Type: EntryTypeUser, Content: "complete"}
	line, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	suffix := []byte(`{"id":"torn","type":"user","content":"unfinished`)
	original := append(append(append([]byte(nil), line...), '\n'), suffix...)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	got, err := readDurableJSONL(file, dir)
	if err != nil {
		t.Fatalf("repair truncated final suffix: %v", err)
	}
	if len(got) != 1 || got[0].ID != entry.ID {
		t.Fatalf("unexpected recovered entries: %#v", got)
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, append(line, '\n')) {
		t.Fatalf("source was not truncated at last complete row: %q", stored)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "session.jsonl.torn-*.tail"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected one quarantined suffix sidecar, found %d", len(matches))
	}
	quarantined, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(quarantined, suffix) {
		t.Fatalf("quarantined suffix changed: got %q want %q", quarantined, suffix)
	}
}

func TestReadDurableJSONLRejectsCompleteMalformedFinalRow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	contents := []byte("{\"id\":\"entry-1\",\"type\":\"user\"}\n{bad-json}\n")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := readDurableJSONL(file, dir); err == nil {
		t.Fatal("complete malformed row must fail closed")
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, contents) {
		t.Fatal("complete malformed row was modified during failed recovery")
	}
}

func TestDurableJSONLReaderUsesSymmetricExactLineLimit(t *testing.T) {
	for _, targetBytes := range []int{maxDurableJSONLineBytes - 1, maxDurableJSONLineBytes, maxDurableJSONLineBytes + 1} {
		t.Run(strconv.Itoa(targetBytes), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "session.jsonl")
			entry := SessionEntry{ID: "entry-1", Type: EntryTypeUser}
			base, err := json.Marshal(entry)
			if err != nil {
				t.Fatal(err)
			}
			entry.Content = strings.Repeat("x", targetBytes-len(base))
			line, err := json.Marshal(entry)
			if err != nil {
				t.Fatal(err)
			}
			if len(line) != targetBytes {
				t.Fatalf("fixture line has %d bytes, want %d", len(line), targetBytes)
			}
			if err := os.WriteFile(path, append(line, '\n'), 0o600); err != nil {
				t.Fatal(err)
			}
			file, err := os.OpenFile(path, os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			got, err := readDurableJSONL(file, dir)
			if targetBytes <= maxDurableJSONLineBytes {
				if err != nil || len(got) != 1 {
					t.Fatalf("accepted boundary row: entries=%d err=%v", len(got), err)
				}
			} else if err == nil {
				t.Fatal("row larger than the configured limit must fail")
			}
		})
	}
}

func TestDurableWriterAssignsSequenceAndMakesExactRetryIdempotent(t *testing.T) {
	preparer := NewDurableJSONLSessionPreparer(t.TempDir(), nil)
	prepared, err := preparer.PrepareNew(context.Background(), "work label")
	if err != nil {
		t.Fatalf("prepare new session: %v", err)
	}
	defer prepared.Close()

	zero := uint64(0)
	input := SessionEntry{ID: "entry-1", Type: EntryTypeUser, RunID: "run-1", TranscriptIndex: &zero, Content: "hello"}
	first, err := prepared.DurableWriter().AppendAndSync(context.Background(), input)
	if err != nil {
		t.Fatalf("append first row: %v", err)
	}
	if len(first) != 1 || first[0].Seq != 1 || first[0].SessionID != prepared.View().SessionID || first[0].SessionLabel != "work label" || first[0].BranchID != "main" {
		t.Fatalf("writer did not return canonical durable row: %#v", first)
	}
	if !strings.HasPrefix(first[0].PayloadDigest, "sha256:") || !isHexDigest(strings.TrimPrefix(first[0].PayloadDigest, "sha256:")) {
		t.Fatalf("canonical row has invalid payload digest %q", first[0].PayloadDigest)
	}

	retry, err := prepared.DurableWriter().AppendAndSync(context.Background(), input)
	if err != nil {
		t.Fatalf("append exact retry: %v", err)
	}
	if len(retry) != 1 || retry[0].Seq != first[0].Seq || retry[0].PayloadDigest != first[0].PayloadDigest {
		t.Fatalf("exact retry changed canonical row: first=%#v retry=%#v", first, retry)
	}
	conflict := input
	conflict.Content = "different"
	if _, err := prepared.DurableWriter().AppendAndSync(context.Background(), conflict); err == nil {
		t.Fatal("same EntryID with different payload must fail")
	}
}

func TestDurableWriterPoisonsAfterPartialBatchWriteAndReopenRetriesByID(t *testing.T) {
	var failed atomic.Bool
	preparer := newDurableJSONLSessionPreparer(t.TempDir(), nil, durableJSONLIO{
		write: func(file *os.File, payload []byte) (int, error) {
			if failed.CompareAndSwap(false, true) {
				firstLF := bytes.IndexByte(payload, '\n')
				if firstLF < 0 {
					return 0, errors.New("injected append failure")
				}
				written, err := file.Write(payload[:firstLF+1])
				if err != nil {
					return written, err
				}
				return written, errors.New("injected append failure after complete prefix")
			}
			return file.Write(payload)
		},
	})
	prepared, err := preparer.PrepareNew(context.Background(), "retry label")
	if err != nil {
		t.Fatalf("prepare new session: %v", err)
	}
	sessionID := prepared.View().SessionID
	zero, one := uint64(0), uint64(1)
	first := SessionEntry{ID: "entry-1", Type: EntryTypeUser, RunID: "run-1", TranscriptIndex: &zero, Content: "first"}
	second := SessionEntry{ID: "entry-2", ParentID: "entry-1", Type: EntryTypeUser, RunID: "run-1", TranscriptIndex: &one, Content: "second"}
	if _, err := prepared.DurableWriter().AppendAndSync(context.Background(), first, second); err == nil {
		t.Fatal("injected partial batch write must not be acknowledged")
	}
	if _, err := prepared.DurableWriter().AppendAndSync(context.Background(), first); !errors.Is(err, ErrDurableWriterPoisoned) {
		t.Fatalf("writer remained usable after append failure: %v", err)
	}
	if err := prepared.Close(); err != nil {
		t.Fatalf("close poisoned writer: %v", err)
	}

	reopened, err := NewDurableJSONLSessionPreparer(preparer.directory, nil).PrepareResume(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("reopen partial prefix: %v", err)
	}
	defer reopened.Close()
	canonical, err := reopened.DurableWriter().AppendAndSync(context.Background(), first, second)
	if err != nil {
		t.Fatalf("retry exact batch after recovery: %v", err)
	}
	if len(canonical) != 2 || canonical[0].Seq != 1 || canonical[1].Seq != 2 {
		t.Fatalf("reopened batch did not preserve prefix and assign next sequence: %#v", canonical)
	}
}

func TestDurableWriterPoisonsAfterSyncFailure(t *testing.T) {
	preparer := newDurableJSONLSessionPreparer(t.TempDir(), nil, durableJSONLIO{
		sync: func(*os.File) error { return errors.New("injected sync failure") },
	})
	prepared, err := preparer.PrepareNew(context.Background(), "sync label")
	if err != nil {
		t.Fatalf("prepare new session: %v", err)
	}
	sessionID := prepared.View().SessionID
	zero := uint64(0)
	entry := SessionEntry{ID: "entry-1", Type: EntryTypeUser, RunID: "run-1", TranscriptIndex: &zero, Content: "durable?"}
	if _, err := prepared.DurableWriter().AppendAndSync(context.Background(), entry); err == nil {
		t.Fatal("sync failure must withhold the durable acknowledgment")
	}
	if _, err := prepared.DurableWriter().AppendAndSync(context.Background(), entry); !errors.Is(err, ErrDurableWriterPoisoned) {
		t.Fatalf("writer did not poison after sync failure: %v", err)
	}
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewDurableJSONLSessionPreparer(preparer.directory, nil).PrepareResume(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("reopen after sync failure: %v", err)
	}
	defer reopened.Close()
	canonical, err := reopened.DurableWriter().AppendAndSync(context.Background(), entry)
	if err != nil {
		t.Fatalf("exact retry after sync failure: %v", err)
	}
	if len(canonical) != 1 || canonical[0].Seq != 1 {
		t.Fatalf("retry did not recover the existing persisted row: %#v", canonical)
	}
}

func TestDurableWriterAcceptsExactLineLimitAndRejectsLimitPlusOne(t *testing.T) {
	prepared, err := NewDurableJSONLSessionPreparer(t.TempDir(), nil).PrepareNew(context.Background(), "line limit")
	if err != nil {
		t.Fatalf("prepare new session: %v", err)
	}
	defer prepared.Close()
	store := prepared.(*durablePreparedSession).store
	first := durableUserRow("entry-1", "", 0)
	first.Content = strings.Repeat("x", exactContentLengthForDurableRow(t, store, first, 1, maxDurableJSONLineBytes))
	if got := encodedDurableRowLength(t, store, first, 1); got != maxDurableJSONLineBytes {
		t.Fatalf("exact-limit fixture has %d bytes, want %d", got, maxDurableJSONLineBytes)
	}
	canonical, err := prepared.DurableWriter().AppendAndSync(context.Background(), first)
	if err != nil {
		t.Fatalf("writer rejected exact-limit row: %v", err)
	}
	if len(canonical) != 1 || canonical[0].Seq != 1 {
		t.Fatalf("exact-limit row was not stored: %#v", canonical)
	}

	tooLarge := durableUserRow("entry-2", "", 1)
	tooLarge.ParentID = "entry-1"
	tooLarge.Content = strings.Repeat("x", exactContentLengthForDurableRow(t, store, tooLarge, 2, maxDurableJSONLineBytes+1))
	if got := encodedDurableRowLength(t, store, tooLarge, 2); got != maxDurableJSONLineBytes+1 {
		t.Fatalf("over-limit fixture has %d bytes, want %d", got, maxDurableJSONLineBytes+1)
	}
	if _, err := prepared.DurableWriter().AppendAndSync(context.Background(), tooLarge); err == nil {
		t.Fatal("writer accepted a serialized row one byte over the limit")
	}
	second := durableUserRow("entry-2", "small", 1)
	second.ParentID = "entry-1"
	canonical, err = prepared.DurableWriter().AppendAndSync(context.Background(), second)
	if err != nil {
		t.Fatalf("prevalidation size rejection poisoned the writer: %v", err)
	}
	if canonical[0].Seq != 2 {
		t.Fatalf("rejected row consumed a sequence number: %#v", canonical[0])
	}
}

func TestPrepareNewUsesUniquePhysicalIDsAndDoesNotReplacePriorHandle(t *testing.T) {
	root := t.TempDir()
	preparer := NewDurableJSONLSessionPreparer(root, nil)
	first, err := preparer.PrepareNew(context.Background(), "same display label")
	if err != nil {
		t.Fatalf("prepare first session: %v", err)
	}
	second, err := preparer.PrepareNew(context.Background(), "same display label")
	if err != nil {
		t.Fatalf("prepare second session: %v", err)
	}
	defer first.Close()
	defer second.Close()
	if first.View().SessionID == second.View().SessionID {
		t.Fatal("two physical sessions reused one ID")
	}
	if len(first.View().Messages) != 0 || len(first.View().SourceEntryIDs) != 0 || first.View().BranchID == "" {
		t.Fatalf("new session did not expose an empty root view: %#v", first.View())
	}
	if _, err := first.DurableWriter().AppendAndSync(context.Background(), durableUserRow("entry-1", "one", 0)); err != nil {
		t.Fatalf("prior handle stopped working after preparing another session: %v", err)
	}
	files, err := filepath.Glob(filepath.Join(root, "sessions", "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("expected two independent physical files, found %d", len(files))
	}
}

func TestPrepareResumeLocksBeforeLoadingTheSelectedFile(t *testing.T) {
	root := t.TempDir()
	preparer := NewDurableJSONLSessionPreparer(root, nil)
	prepared, err := preparer.PrepareNew(context.Background(), "lock test")
	if err != nil {
		if errors.Is(err, ErrUnsupportedSessionLock) {
			t.Skip("OS-backed flock is unavailable on this platform")
		}
		t.Fatalf("prepare new session: %v", err)
	}
	sessionID := prepared.View().SessionID
	path := filepath.Join(root, "sessions", sessionID+".jsonl")
	if err := os.WriteFile(path, []byte("{broken}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		resumed, err := preparer.PrepareResume(context.Background(), sessionID)
		if resumed != nil {
			_ = resumed.Close()
		}
		result <- err
	}()
	select {
	case err := <-result:
		t.Fatalf("resume read the selected file before acquiring its lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("resume accepted malformed file after acquiring its lock")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("resume did not continue after the owning handle released its lock")
	}
}

func TestPrepareResumeLockWaitHonorsContextCancellation(t *testing.T) {
	root := t.TempDir()
	preparer := NewDurableJSONLSessionPreparer(root, nil)
	prepared, err := preparer.PrepareNew(context.Background(), "cancel lock test")
	if err != nil {
		if errors.Is(err, ErrUnsupportedSessionLock) {
			t.Skip("OS-backed flock is unavailable on this platform")
		}
		t.Fatalf("prepare new session: %v", err)
	}
	defer prepared.Close()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		resumed, err := preparer.PrepareResume(ctx, prepared.View().SessionID)
		if resumed != nil {
			_ = resumed.Close()
		}
		result <- err
	}()
	time.Sleep(40 * time.Millisecond)
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("lock wait returned %v, want context cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("lock wait did not honor context cancellation")
	}
}

func TestPrepareResumeRejectsSymlinkWithoutRepairingItsTarget(t *testing.T) {
	root := t.TempDir()
	sessionsDir := filepath.Join(root, "sessions")
	if err := os.Mkdir(sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	outsideDir := t.TempDir()
	target := filepath.Join(outsideDir, "external.jsonl")
	complete, err := json.Marshal(SessionEntry{ID: "entry-1", Type: EntryTypeUser})
	if err != nil {
		t.Fatal(err)
	}
	targetContents := append(append(append([]byte(nil), complete...), '\n'), []byte(`{"id":"torn","type":"user","content":"unfinished`)...)
	if err := os.WriteFile(target, targetContents, 0o600); err != nil {
		t.Fatal(err)
	}
	const physicalID = "s_safe-id"
	if err := os.Symlink(target, filepath.Join(sessionsDir, physicalID+".jsonl")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := NewDurableJSONLSessionPreparer(root, nil).PrepareResume(context.Background(), physicalID); err == nil {
		t.Fatal("resume followed a physical session symlink")
	}
	after, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, targetContents) {
		t.Fatal("rejecting a symlink modified the external target")
	}
}

func TestPrepareResumeRejectsSymlinkedSessionsDirectory(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "sessions")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := NewDurableJSONLSessionPreparer(root, nil).PrepareResume(context.Background(), "s_valid"); err == nil {
		t.Fatal("resume followed a symlinked sessions directory")
	}
}

func TestPrepareResumeRejectsNonRegularPhysicalSessionEntry(t *testing.T) {
	root := t.TempDir()
	sessionsDir := filepath.Join(root, "sessions")
	if err := os.MkdirAll(sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	const physicalID = "s_directory"
	if err := os.Mkdir(filepath.Join(sessionsDir, physicalID+".jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := NewDurableJSONLSessionPreparer(root, nil).PrepareResume(context.Background(), physicalID); err == nil {
		t.Fatal("resume accepted a non-regular physical session entry")
	}
}

func TestPrepareResumeResolvesLegacyTimestampFilename(t *testing.T) {
	root := t.TempDir()
	sessionsDir := filepath.Join(root, "sessions")
	if err := os.MkdirAll(sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	const physicalID = "legacy-session"
	filename := time.Date(2026, time.September, 25, 12, 13, 14, 0, time.UTC).Format(sessionTimestampFmt) + "-" + physicalID + ".jsonl"
	if err := os.WriteFile(filepath.Join(sessionsDir, filename), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	prepared, err := NewDurableJSONLSessionPreparer(root, nil).PrepareResume(context.Background(), physicalID)
	if err != nil {
		t.Fatalf("resume legacy timestamp filename: %v", err)
	}
	defer prepared.Close()
	if got := prepared.View().SessionID; got != physicalID {
		t.Fatalf("physical session ID = %q, want %q", got, physicalID)
	}
}

func durableUserRow(id, content string, index uint64) SessionEntry {
	return SessionEntry{
		ID: id, Type: EntryTypeUser, RunID: "run-1",
		TranscriptIndex: &index, Content: content,
	}
}

func exactContentLengthForDurableRow(t *testing.T, store *durableJSONLStore, entry SessionEntry, seq uint64, target int) int {
	t.Helper()
	entry.SessionID = store.sessionID
	entry.SessionLabel = store.sessionLabel
	entry.BranchID = store.activeBranchID
	entry.RecordVersion = 1
	entry.Seq = 0
	entry.Content = ""
	_, digest, err := canonicalEntryPayload(entry)
	if err != nil {
		t.Fatal(err)
	}
	entry.PayloadDigest = digest
	entry.Seq = seq
	encoded, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	return target - len(encoded)
}

func encodedDurableRowLength(t *testing.T, store *durableJSONLStore, entry SessionEntry, seq uint64) int {
	t.Helper()
	entry.SessionID = store.sessionID
	entry.SessionLabel = store.sessionLabel
	entry.BranchID = store.activeBranchID
	entry.RecordVersion = 1
	entry.Seq = 0
	_, digest, err := canonicalEntryPayload(entry)
	if err != nil {
		t.Fatal(err)
	}
	entry.PayloadDigest = digest
	entry.Seq = seq
	encoded, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	return len(encoded)
}

func TestDurableWriterPersistsOpenToolCallWithoutPublishingPartialView(t *testing.T) {
	prepared, err := NewDurableJSONLSessionPreparer(t.TempDir(), nil).PrepareNew(context.Background(), "open tool group")
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	writer := prepared.DurableWriter()

	userIndex := uint64(0)
	if _, err := writer.AppendAndSync(context.Background(), SessionEntry{
		ID: "user-entry", Type: EntryTypeUser, RunID: "run-open-group",
		TranscriptIndex: &userIndex, Content: "run the tool",
	}); err != nil {
		t.Fatalf("append user input: %v", err)
	}
	beforeAssistant := prepared.View()
	ordinal := uint64(0)
	assistantIndex := uint64(1)
	if _, err := writer.AppendAndSync(context.Background(), SessionEntry{
		ID: "assistant-entry", ParentID: "user-entry", Type: EntryTypeAssistant,
		RunID: "run-open-group", AssistantTranscriptOrdinal: &ordinal,
		TranscriptIndex: &assistantIndex, ToolCalls: []llm.ToolCall{{ID: "call-1", Name: "read"}},
	}, SessionEntry{
		ID: "intent-entry", ParentID: "assistant-entry", Type: EntryTypeTool,
		RunID: "run-open-group", WorkspaceID: "workspace-open-group", ActionBatchID: "batch-open-group", ActionID: "action-open-group",
		ActionState: ActionStateIntent, AssistantTranscriptOrdinal: &ordinal, ToolCallID: "call-1", ToolName: "read",
		ArgumentDigest: strings.Repeat("a", 64), SchemaDigest: strings.Repeat("b", 64),
		ToolIdentity: &tools.TrustedToolIdentitySnapshot{
			RegistrationSource: "builtin", ToolName: "read", RegistrationGeneration: 1,
			ExecutorFingerprint: strings.Repeat("c", 64), ScopeFingerprint: strings.Repeat("d", 64),
		},
		EffectSet: []tools.TrustedToolEffect{tools.EffectWorkspaceRead},
	}); err != nil {
		t.Fatalf("persist assistant tool call and intent before its receipt: %v", err)
	}
	afterAssistant := prepared.View()
	if len(afterAssistant.Messages) != len(beforeAssistant.Messages) || afterAssistant.Revision != beforeAssistant.Revision {
		t.Fatalf("incomplete tool-call group was published to the model view: before=%#v after=%#v", beforeAssistant, afterAssistant)
	}

	receiptIndex := uint64(2)
	if _, err := writer.AppendAndSync(context.Background(), SessionEntry{
		ID: "receipt-entry", ParentID: "intent-entry", Type: EntryTypeTool,
		RunID: "run-open-group", AssistantTranscriptOrdinal: &ordinal,
		TranscriptIndex: &receiptIndex, ToolCallID: "call-1", ToolName: "read",
		WorkspaceID: "workspace-open-group", ActionBatchID: "batch-open-group", ActionID: "action-open-group",
		ReceiptOutcome: "error", DispatchState: DispatchStateNotDispatched,
		Content: "denied before dispatch",
	}); err != nil {
		t.Fatalf("append terminal receipt: %v", err)
	}
	closedView := prepared.View()
	if len(closedView.Messages) != 3 || closedView.SourceEntryIDs[1] != "assistant-entry" || closedView.SourceEntryIDs[2] != "receipt-entry" {
		t.Fatalf("closed tool-call group did not publish its complete view: %#v", closedView)
	}
	if closedView.Revision == beforeAssistant.Revision {
		t.Fatal("complete receipt group must advance the published view revision")
	}
}

func TestDurableWriterRejectsInvalidHistoryBeforeWriting(t *testing.T) {
	tests := []struct {
		name  string
		build func(root, assistant SessionEntry) []SessionEntry
	}{
		{
			name: "intervening user",
			build: func(root, assistant SessionEntry) []SessionEntry {
				index := uint64(2)
				return []SessionEntry{{ID: "later-user", ParentID: assistant.ID, Type: EntryTypeUser, RunID: "run-invalid", TranscriptIndex: &index}}
			},
		},
		{
			name: "second assistant before receipt",
			build: func(root, assistant SessionEntry) []SessionEntry {
				index, ordinal := uint64(2), uint64(1)
				return []SessionEntry{{ID: "second-assistant", ParentID: assistant.ID, Type: EntryTypeAssistant, RunID: "run-invalid", TranscriptIndex: &index, AssistantTranscriptOrdinal: &ordinal}}
			},
		},
		{
			name: "orphan receipt",
			build: func(root, assistant SessionEntry) []SessionEntry {
				index, ordinal := uint64(2), uint64(0)
				return []SessionEntry{{ID: "orphan-receipt", ParentID: assistant.ID, Type: EntryTypeTool, RunID: "run-invalid", TranscriptIndex: &index, AssistantTranscriptOrdinal: &ordinal, ToolCallID: "other-call", ToolName: "read", ReceiptOutcome: "error", DispatchState: DispatchStateDispatched}}
			},
		},
		{
			name: "mismatched tool name",
			build: func(root, assistant SessionEntry) []SessionEntry {
				index, ordinal := uint64(2), uint64(0)
				return []SessionEntry{{ID: "wrong-name", ParentID: assistant.ID, Type: EntryTypeTool, RunID: "run-invalid", TranscriptIndex: &index, AssistantTranscriptOrdinal: &ordinal, ToolCallID: "call-1", ToolName: "write", ReceiptOutcome: "error", DispatchState: DispatchStateDispatched}}
			},
		},
		{
			name: "duplicate receipt",
			build: func(root, assistant SessionEntry) []SessionEntry {
				index2, index3, ordinal := uint64(2), uint64(3), uint64(0)
				first := SessionEntry{ID: "receipt-one", ParentID: assistant.ID, Type: EntryTypeTool, RunID: "run-invalid", TranscriptIndex: &index2, AssistantTranscriptOrdinal: &ordinal, ToolCallID: "call-1", ToolName: "read", ReceiptOutcome: "error", DispatchState: DispatchStateDispatched}
				second := SessionEntry{ID: "receipt-two", ParentID: first.ID, Type: EntryTypeTool, RunID: "run-invalid", TranscriptIndex: &index3, AssistantTranscriptOrdinal: &ordinal, ToolCallID: "call-1", ToolName: "read", ReceiptOutcome: "error", DispatchState: DispatchStateDispatched}
				return []SessionEntry{first, second}
			},
		},
		{
			name: "duplicate transcript index",
			build: func(root, assistant SessionEntry) []SessionEntry {
				index := *root.TranscriptIndex
				return []SessionEntry{{ID: "duplicate-index", ParentID: assistant.ID, Type: EntryTypeUser, RunID: "run-invalid", TranscriptIndex: &index}}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			preparer := NewDurableJSONLSessionPreparer(t.TempDir(), nil)
			prepared, err := preparer.PrepareNew(context.Background(), "invalid history")
			if err != nil {
				t.Fatal(err)
			}
			sessionID := prepared.View().SessionID
			zero, one := uint64(0), uint64(1)
			root, err := prepared.DurableWriter().AppendAndSync(context.Background(), SessionEntry{
				ID: "root-user", Type: EntryTypeUser, RunID: "run-invalid", TranscriptIndex: &zero,
			})
			if err != nil {
				t.Fatal(err)
			}
			ordinal := uint64(0)
			assistant, err := prepared.DurableWriter().AppendAndSync(context.Background(), SessionEntry{
				ID: "open-assistant", ParentID: root[0].ID, Type: EntryTypeAssistant, RunID: "run-invalid",
				TranscriptIndex: &one, AssistantTranscriptOrdinal: &ordinal,
				ToolCalls: []llm.ToolCall{{ID: "call-1", Name: "read"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(preparer.directory, "sessions", sessionID+".jsonl")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := prepared.DurableWriter().AppendAndSync(context.Background(), test.build(root[0], assistant[0])...); err == nil {
				t.Fatal("invalid or non-tail history update must be rejected")
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatalf("rejected append changed durable bytes: before=%d after=%d", len(before), len(after))
			}
			if err := prepared.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

type failingMarshaler struct {
	calls  *atomic.Int32
	failAt int32
}

func (m *failingMarshaler) MarshalJSON() ([]byte, error) {
	if m.calls.Add(1) == m.failAt {
		return nil, errors.New("injected late marshal failure")
	}
	return []byte(`"ok"`), nil
}

func TestDurableWriterSerializesWholeBatchBeforeWriting(t *testing.T) {
	preparer := NewDurableJSONLSessionPreparer(t.TempDir(), nil)
	prepared, err := preparer.PrepareNew(context.Background(), "batch preflight")
	if err != nil {
		t.Fatal(err)
	}
	sessionID := prepared.View().SessionID
	calls := &atomic.Int32{}
	zero, one := uint64(0), uint64(1)
	first := SessionEntry{ID: "first", Type: EntryTypeUser, RunID: "run-1", TranscriptIndex: &zero, Content: "first"}
	second := SessionEntry{
		ID: "second", ParentID: first.ID, Type: EntryTypeUser, RunID: "run-1", TranscriptIndex: &one,
		ToolCalls: []llm.ToolCall{{ID: "unused", Name: "read", Args: map[string]any{"value": &failingMarshaler{calls: calls, failAt: 3}}}},
	}
	if _, err := prepared.DurableWriter().AppendAndSync(context.Background(), first, second); err == nil {
		t.Fatal("unsupported or late-failing JSON value must fail before writing the batch")
	}
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(preparer.directory, "sessions", sessionID+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Fatalf("failed batch left %d bytes on disk", info.Size())
	}
}

func TestDurableWriterRejectsBranchCheckpointsWithUnknownMetadata(t *testing.T) {
	t.Run("unknown parent branch", func(t *testing.T) {
		prepared, err := NewDurableJSONLSessionPreparer(t.TempDir(), nil).PrepareNew(context.Background(), "branch proof")
		if err != nil {
			t.Fatal(err)
		}
		defer prepared.Close()
		zero := uint64(0)
		root, err := prepared.DurableWriter().AppendAndSync(context.Background(), SessionEntry{
			ID: "root", Type: EntryTypeUser, RunID: "run-1", TranscriptIndex: &zero,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := prepared.DurableWriter().AppendAndSync(context.Background(), SessionEntry{
			ID: "branch-feature", ParentID: root[0].ID, Type: EntryTypeCheckpoint,
			BranchID: "feature", ParentBranchID: "ghost", BaseLeafEntryID: root[0].ID,
			ActiveLeafEntryID: "branch-feature", CheckpointKind: CheckpointBranch,
		}); err == nil {
			t.Fatal("branch checkpoint with unknown parent branch must be rejected")
		}
	})

	t.Run("unknown active branch", func(t *testing.T) {
		prepared, err := NewDurableJSONLSessionPreparer(t.TempDir(), nil).PrepareNew(context.Background(), "branch proof")
		if err != nil {
			t.Fatal(err)
		}
		defer prepared.Close()
		zero := uint64(0)
		root, err := prepared.DurableWriter().AppendAndSync(context.Background(), SessionEntry{
			ID: "root", Type: EntryTypeUser, RunID: "run-1", TranscriptIndex: &zero,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := prepared.DurableWriter().AppendAndSync(context.Background(), SessionEntry{
			ID: "active-ghost", ParentID: root[0].ID, Type: EntryTypeCheckpoint,
			BranchID: "ghost", ActiveLeafEntryID: root[0].ID, CheckpointKind: CheckpointActiveLeaf,
		}); err == nil {
			t.Fatal("active-leaf checkpoint cannot name an unknown branch")
		}
	})
}

func TestDurableWriterAcceptsSharedPrefixFromAncestorBranch(t *testing.T) {
	preparer := NewDurableJSONLSessionPreparer(t.TempDir(), nil)
	prepared, err := preparer.PrepareNew(context.Background(), "shared branch")
	if err != nil {
		t.Fatal(err)
	}
	sessionID := prepared.View().SessionID
	zero, one := uint64(0), uint64(1)
	root, err := prepared.DurableWriter().AppendAndSync(context.Background(), SessionEntry{
		ID: "main-root", Type: EntryTypeUser, RunID: "run-1", TranscriptIndex: &zero, Content: "root",
	})
	if err != nil {
		t.Fatal(err)
	}
	branch, err := prepared.DurableWriter().AppendAndSync(context.Background(), SessionEntry{
		ID: "feature-base", ParentID: root[0].ID, Type: EntryTypeCheckpoint,
		BranchID: "feature", ParentBranchID: "main", BaseLeafEntryID: root[0].ID,
		ActiveLeafEntryID: "feature-base", CheckpointKind: CheckpointBranch,
	})
	if err != nil {
		t.Fatalf("append branch checkpoint over shared main prefix: %v", err)
	}
	if _, err := prepared.DurableWriter().AppendAndSync(context.Background(), SessionEntry{
		ID: "feature-user", ParentID: branch[0].ID, Type: EntryTypeUser, BranchID: "feature",
		RunID: "run-1", TranscriptIndex: &one, Content: "feature continuation",
	}); err != nil {
		t.Fatalf("append child of a valid shared-prefix branch: %v", err)
	}
	if got := prepared.View().BranchID; got != "feature" {
		t.Fatalf("active branch = %q, want feature", got)
	}
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := preparer.PrepareResume(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("resume valid shared-prefix branch: %v", err)
	}
	defer reopened.Close()
	if got := reopened.View(); got.BranchID != "feature" || len(got.Messages) != 2 || got.Messages[0].Content != "root" || got.Messages[1].Content != "feature continuation" {
		t.Fatalf("shared-prefix branch did not restore: %#v", got)
	}
}
