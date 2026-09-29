package approval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/privatefile"
)

// TestAuditTrail_Record verifies that a single entry is written to the daily
// JSONL file with all required fields.
func TestAuditTrail_Record(t *testing.T) {
	dir := privateAuditTestDir(t)
	trail := NewAuditTrail(dir)

	ts := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	entry := AuditEntry{
		Timestamp:      ts,
		Tool:           "read_file",
		ArgsSummary:    "path",
		Decision:       "allow",
		Classifier:     "allow_all",
		PermissionMode: "default",
		SessionID:      "sess-1",
	}
	require.NoError(t, trail.Record(entry))

	// AC-3: file written to {dir}/{date}.jsonl
	filename := filepath.Join(dir, "2026-08-09.jsonl")
	data, err := os.ReadFile(filename)
	require.NoError(t, err)

	// AC-4: JSON contains tool/decision/classifier/mode/timestamp fields
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(data, &decoded))
	assert.Equal(t, "read_file", decoded["tool"])
	assert.Equal(t, "allow", decoded["decision"])
	assert.Equal(t, "allow_all", decoded["classifier"])
	assert.Equal(t, "default", decoded["permission_mode"])
	assert.Equal(t, "sess-1", decoded["session_id"])
	assert.NotEmpty(t, decoded["timestamp"])
	assert.Equal(t, "redacted", decoded["args_summary"])
	assert.NotEmpty(t, decoded["hash"])
}

// TestAuditTrail_AppendOnly verifies that multiple records are appended to the
// same file (AC-5: append-only design).
func TestAuditTrail_AppendOnly(t *testing.T) {
	dir := privateAuditTestDir(t)
	trail := NewAuditTrail(dir)

	ts := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		entry := AuditEntry{
			Timestamp:      ts,
			Tool:           "read_file",
			Decision:       "allow",
			Classifier:     "allow_all",
			PermissionMode: "default",
		}
		require.NoError(t, trail.Record(entry))
	}

	filename := filepath.Join(dir, "2026-08-09.jsonl")
	data, err := os.ReadFile(filename)
	require.NoError(t, err)

	// 3 entries → 3 lines (each ending with \n)
	lines := splitLines(string(data))
	assert.Len(t, lines, 3)

	// Verify each line is valid JSON
	for i, line := range lines {
		var decoded map[string]any
		require.NoErrorf(t, json.Unmarshal([]byte(line), &decoded), "line %d", i)
		assert.Equal(t, "read_file", decoded["tool"])
	}
}

// TestAuditTrail_NilSafe verifies that a nil AuditTrail does not panic.
func TestAuditTrail_NilSafe(t *testing.T) {
	var trail *AuditTrail
	err := trail.Record(AuditEntry{Tool: "read_file"})
	assert.NoError(t, err)
}

// TestAuditTrail_DirectoryCreated verifies that Record creates the directory
// if it does not exist.
func TestAuditTrail_DirectoryCreated(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "audit")
	trail := NewAuditTrail(dir)

	ts := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	entry := AuditEntry{Timestamp: ts, Tool: "bash", Decision: "deny"}
	require.NoError(t, trail.Record(entry))

	filename := filepath.Join(dir, "2026-08-09.jsonl")
	_, err := os.Stat(filename)
	require.NoError(t, err)
}

func TestAuditTrail_ReadsAndContinuesHashChainAfterRestart(t *testing.T) {
	dir := privateAuditTestDir(t)
	ts := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	first := NewAuditTrail(dir)
	require.NoError(t, first.Record(AuditEntry{Timestamp: ts, Tool: "read_file", Decision: "allow"}))

	reopened := NewAuditTrail(dir)
	require.NoError(t, reopened.Record(AuditEntry{Timestamp: ts, Tool: "bash", Decision: "deny"}))

	entries, err := reopened.ReadEntries(ts)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	assert.Empty(t, entries[0].PrevHash)
	assert.Equal(t, entries[0].Hash, entries[1].PrevHash)
	assert.NotEmpty(t, entries[1].Hash)
}

func TestAuditTrail_MultipleInstancesRefreshHashChain(t *testing.T) {
	dir := privateAuditTestDir(t)
	ts := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	first := NewAuditTrail(dir)
	second := NewAuditTrail(dir)
	for index := 0; index < 20; index++ {
		trail := first
		if index%2 == 1 {
			trail = second
		}
		require.NoError(t, trail.Record(AuditEntry{Timestamp: ts, Tool: fmt.Sprintf("tool-%02d", index)}))
	}

	entries, err := first.ReadEntries(ts)
	require.NoError(t, err)
	require.Len(t, entries, 20)
	for index := 1; index < len(entries); index++ {
		assert.Equal(t, entries[index-1].Hash, entries[index].PrevHash, "entry %d must continue the latest durable chain", index)
	}
}

func TestAuditTrailRejectsSameSizeHistoricalTamperingBeforeAppend(t *testing.T) {
	dir := privateAuditTestDir(t)
	ts := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	trail := NewAuditTrail(dir)
	require.NoError(t, trail.Record(AuditEntry{Timestamp: ts, Tool: "alpha", Decision: "allow"}))
	path := filepath.Join(dir, "2026-08-09.jsonl")
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	tampered := bytes.Replace(raw, []byte(`"tool":"alpha"`), []byte(`"tool":"bravo"`), 1)
	require.NotEqual(t, raw, tampered)
	require.Equal(t, len(raw), len(tampered))
	require.NoError(t, os.WriteFile(path, tampered, 0o600))

	err = trail.Record(AuditEntry{Timestamp: ts, Tool: "later", Decision: "allow"})
	assert.ErrorIs(t, err, ErrAuditTrailCorrupt)
	entries, readErr := trail.ReadEntries(ts)
	assert.ErrorIs(t, readErr, ErrAuditTrailCorrupt)
	assert.Empty(t, entries)
}

func TestAuditTrailLockWaitIsBounded(t *testing.T) {
	dir := privateAuditTestDir(t)
	ts := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	trail := NewAuditTrail(dir)
	trail.lockTimeout = 50 * time.Millisecond
	path := filepath.Join(dir, "2026-08-09.jsonl")
	release := holdApprovalAuditLock(t, path)
	defer release()

	err := trail.Record(AuditEntry{Timestamp: ts, Tool: "blocked", Decision: "allow"})
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	_, statErr := os.Stat(path)
	assert.ErrorIs(t, statErr, os.ErrNotExist, "a record is not appended after the lock wait expires")
}

func holdApprovalAuditLock(t *testing.T, path string) func() {
	t.Helper()
	locked := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- privatefile.WithExclusiveLock(context.Background(), path, func() error {
			close(locked)
			<-release
			return nil
		})
	}()
	select {
	case <-locked:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("test process did not acquire the audit lock")
	}
	var once sync.Once
	finish := func() {
		once.Do(func() {
			close(release)
			require.NoError(t, <-done)
		})
	}
	t.Cleanup(finish)
	return finish
}

func TestAuditTrailCrossProcessAppendHelper(t *testing.T) {
	mode := os.Getenv("GOCLI_AUDIT_TRAIL_HELPER_MODE")
	if mode == "" {
		return
	}
	trail := NewAuditTrail(os.Getenv("GOCLI_AUDIT_TRAIL_HELPER_DIR"))
	if mode == "block" {
		openFile := trail.openFile
		trail.openFile = func(path string, flag int, perm os.FileMode) (auditTrailFile, error) {
			file, err := openFile(path, flag, perm)
			if err != nil {
				return nil, err
			}
			if err := writeAuditTestMarker(os.Getenv("GOCLI_AUDIT_TRAIL_HELPER_OPENED")); err != nil {
				_ = file.Close()
				return nil, err
			}
			if err := waitForAuditTestMarker(os.Getenv("GOCLI_AUDIT_TRAIL_HELPER_RELEASE"), 5*time.Second); err != nil {
				_ = file.Close()
				return nil, err
			}
			return file, nil
		}
	} else if mode == "waiter" {
		if err := writeAuditTestMarker(os.Getenv("GOCLI_AUDIT_TRAIL_HELPER_ATTEMPTED")); err != nil {
			t.Errorf("write attempted marker: %v", err)
		}
	}

	err := trail.Record(AuditEntry{
		Timestamp: time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC),
		Tool:      os.Getenv("GOCLI_AUDIT_TRAIL_HELPER_TOOL"),
	})
	if err != nil {
		t.Errorf("record audit entry: %v", err)
	}
	if mode == "waiter" {
		if err := writeAuditTestMarker(os.Getenv("GOCLI_AUDIT_TRAIL_HELPER_COMPLETE")); err != nil {
			t.Errorf("write complete marker: %v", err)
		}
	}
}

func TestAuditTrailSerializesCrossProcessAppend(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("cross-process advisory locks are supported on Darwin and Linux")
	}
	dir := privateAuditTestDir(t)
	opened := filepath.Join(dir, "opened")
	attempted := filepath.Join(dir, "attempted")
	complete := filepath.Join(dir, "complete")
	release := filepath.Join(dir, "release")
	firstOutput := &bytes.Buffer{}
	first := startAuditTrailProcess(t, firstOutput, dir, "block", "first", opened, attempted, complete, release)
	if err := waitForAuditTestMarker(opened, 5*time.Second); err != nil {
		_ = first.Process.Kill()
		_ = first.Wait()
		t.Fatalf("first process did not enter its open hook: %v; output: %s", err, firstOutput.String())
	}
	secondOutput := &bytes.Buffer{}
	second := startAuditTrailProcess(t, secondOutput, dir, "waiter", "second", opened, attempted, complete, release)
	if err := waitForAuditTestMarker(attempted, 5*time.Second); err != nil {
		_ = writeAuditTestMarker(release)
		_ = first.Wait()
		_ = second.Process.Kill()
		_ = second.Wait()
		t.Fatalf("second process did not attempt its append: %v; output: %s", err, secondOutput.String())
	}
	time.Sleep(200 * time.Millisecond)
	_, completeErr := os.Stat(complete)
	completedBeforeRelease := completeErr == nil
	require.True(t, errors.Is(completeErr, os.ErrNotExist) || completedBeforeRelease, "inspect second process completion")
	require.NoError(t, writeAuditTestMarker(release))
	firstErr := first.Wait()
	secondErr := second.Wait()
	require.NoError(t, firstErr, firstOutput.String())
	require.NoError(t, secondErr, secondOutput.String())
	assert.False(t, completedBeforeRelease, "the second process must wait while the first owns the audit append lock")

	entries, err := NewAuditTrail(dir).ReadEntries(time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	require.Len(t, entries, 2)
	assert.Equal(t, entries[0].Hash, entries[1].PrevHash)
}

func startAuditTrailProcess(t *testing.T, output *bytes.Buffer, dir, mode, tool, opened, attempted, complete, release string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestAuditTrailCrossProcessAppendHelper$")
	cmd.Env = append(os.Environ(),
		"GOCLI_AUDIT_TRAIL_HELPER_MODE="+mode,
		"GOCLI_AUDIT_TRAIL_HELPER_DIR="+dir,
		"GOCLI_AUDIT_TRAIL_HELPER_TOOL="+tool,
		"GOCLI_AUDIT_TRAIL_HELPER_OPENED="+opened,
		"GOCLI_AUDIT_TRAIL_HELPER_ATTEMPTED="+attempted,
		"GOCLI_AUDIT_TRAIL_HELPER_COMPLETE="+complete,
		"GOCLI_AUDIT_TRAIL_HELPER_RELEASE="+release,
	)
	cmd.Stdout = output
	cmd.Stderr = output
	require.NoError(t, cmd.Start())
	return cmd
}

func writeAuditTestMarker(path string) error {
	return os.WriteFile(path, []byte("ready"), 0o600)
}

func waitForAuditTestMarker(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, err := os.Stat(path)
		if err == nil {
			return nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		time.Sleep(time.Millisecond)
	}
	return fmt.Errorf("marker %q was not created before timeout", path)
}

func TestAuditTrailRejectsLegacyUnhashedFile(t *testing.T) {
	dir := privateAuditTestDir(t)
	ts := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(dir, "2026-08-09.jsonl")
	legacy := []byte(`{"tool":"legacy","decision":"allow"}` + "\n")
	require.NoError(t, os.WriteFile(path, legacy, 0o600))

	err := NewAuditTrail(dir).Record(AuditEntry{Timestamp: ts, Tool: "new"})
	assert.ErrorIs(t, err, ErrAuditTrailCorrupt)
	got, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, legacy, got, "a legacy file must be rejected without migration or append")
}

func TestAuditTrailFailsClosedWhenLockUnavailable(t *testing.T) {
	dir := privateAuditTestDir(t)
	ts := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(dir, "2026-08-09.jsonl")
	require.NoError(t, os.WriteFile(path+".lock", nil, 0o644))
	require.NoError(t, os.Chmod(path+".lock", 0o644))

	err := NewAuditTrail(dir).Record(AuditEntry{Timestamp: ts, Tool: "new"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "privatefile")
	_, statErr := os.Stat(path)
	assert.ErrorIs(t, statErr, os.ErrNotExist, "no audit record is appended without the lock")
}

func TestAuditTrail_FailedAppendDoesNotAdvanceHash(t *testing.T) {
	for _, tc := range []struct {
		name      string
		writeMode string
		syncErr   error
		wantErr   error
	}{
		{name: "short write", writeMode: "short", wantErr: io.ErrShortWrite},
		{name: "sync failure", syncErr: errors.New("sync sentinel"), wantErr: ErrAuditTrailSync},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := privateAuditTestDir(t)
			trail := NewAuditTrail(dir)
			ts := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
			require.NoError(t, trail.Record(AuditEntry{Timestamp: ts, Tool: "first"}))
			before, err := os.ReadFile(filepath.Join(dir, "2026-08-09.jsonl"))
			require.NoError(t, err)

			trail.openFile = func(path string, flag int, perm os.FileMode) (auditTrailFile, error) {
				file, openErr := os.OpenFile(path, flag, perm)
				if openErr != nil {
					return nil, openErr
				}
				return &auditTrailFaultFile{File: file, writeMode: tc.writeMode, syncErr: tc.syncErr}, nil
			}
			recordErr := trail.Record(AuditEntry{Timestamp: ts, Tool: "failed"})
			require.Error(t, recordErr)
			if tc.wantErr == ErrAuditTrailSync {
				assert.ErrorIs(t, recordErr, tc.wantErr)
				assert.ErrorIs(t, recordErr, tc.syncErr)
			} else {
				assert.ErrorIs(t, recordErr, tc.wantErr)
				assert.ErrorIs(t, recordErr, ErrAuditTrailAppend)
			}
			trail.openFile = openAuditTrailFile

			after, err := os.ReadFile(filepath.Join(dir, "2026-08-09.jsonl"))
			require.NoError(t, err)
			assert.Equal(t, before, after, "failed append must be rolled back")
			require.NoError(t, trail.Record(AuditEntry{Timestamp: ts, Tool: "second"}))
			entries, err := trail.ReadEntries(ts)
			require.NoError(t, err)
			require.Len(t, entries, 2)
			assert.Equal(t, entries[0].Hash, entries[1].PrevHash)
		})
	}
}

func TestAuditTrail_RollbackFailurePoisonsTrail(t *testing.T) {
	dir := privateAuditTestDir(t)
	trail := NewAuditTrail(dir)
	ts := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	syncErr := errors.New("sync sentinel")
	rollbackErr := errors.New("truncate sentinel")
	trail.openFile = func(path string, flag int, perm os.FileMode) (auditTrailFile, error) {
		file, err := os.OpenFile(path, flag, perm)
		if err != nil {
			return nil, err
		}
		return &auditTrailFaultFile{File: file, syncErr: syncErr, truncateErr: rollbackErr}, nil
	}

	err := trail.Record(AuditEntry{Timestamp: ts, Tool: "failed"})
	assert.ErrorIs(t, err, ErrAuditTrailSync)
	assert.ErrorIs(t, err, ErrAuditTrailRollback)
	assert.ErrorIs(t, err, syncErr)
	assert.ErrorIs(t, err, rollbackErr)

	err = trail.Record(AuditEntry{Timestamp: ts, Tool: "blocked"})
	assert.ErrorIs(t, err, ErrAuditTrailRollback)
}

func TestAuditTrail_RedactsAndBoundsArgumentSummary(t *testing.T) {
	dir := privateAuditTestDir(t)
	trail := NewAuditTrail(dir)
	ts := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	require.NoError(t, trail.Record(AuditEntry{
		Timestamp:   ts,
		Tool:        "bash",
		ArgsSummary: "prompt-secret-should-not-be-written",
	}))

	raw, err := os.ReadFile(filepath.Join(dir, "2026-08-09.jsonl"))
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "prompt-secret-should-not-be-written")
	assert.Contains(t, string(raw), `"args_summary":"redacted"`)
}

type auditTrailFaultFile struct {
	*os.File
	writeMode   string
	syncErr     error
	syncCalls   int
	truncateErr error
}

func (f *auditTrailFaultFile) Write(data []byte) (int, error) {
	if f.writeMode == "short" {
		return len(data) / 2, nil
	}
	return f.File.Write(data)
}

func (f *auditTrailFaultFile) Sync() error {
	if f.syncErr != nil && f.syncCalls == 0 {
		f.syncCalls++
		return f.syncErr
	}
	f.syncCalls++
	return f.File.Sync()
}

func (f *auditTrailFaultFile) Truncate(size int64) error {
	if f.truncateErr != nil {
		return f.truncateErr
	}
	return f.File.Truncate(size)
}

// TestAuditTrail_ClassifierRecordsDecision verifies that AuditClassifier wraps
// an inner classifier, returns its decision, and records the decision to the
// audit trail.
func TestAuditTrail_ClassifierRecordsDecision(t *testing.T) {
	dir := privateAuditTestDir(t)
	trail := NewAuditTrail(dir)

	inner := &AllowAllClassifier{}
	wrapped := NewAuditClassifier(inner, trail, PermissionAutoFull, "sess-42")

	result := wrapped.Classify(ctx(), call("read_file"))
	assert.Equal(t, Allow, result)
	assert.Equal(t, "allow_all", wrapped.Name())

	// Verify the audit file was written
	ts := time.Now().UTC()
	filename := filepath.Join(dir, ts.Format("2006-01-02")+".jsonl")
	data, err := os.ReadFile(filename)
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(data, &decoded))
	assert.Equal(t, "read_file", decoded["tool"])
	assert.Equal(t, "allow", decoded["decision"])
	assert.Equal(t, "allow_all", decoded["classifier"])
	assert.Equal(t, "auto_full", decoded["permission_mode"])
	assert.Equal(t, "sess-42", decoded["session_id"])
}

// TestAuditTrail_ClassifierNilAudit verifies that AuditClassifier with a nil
// AuditTrail still returns the correct classification without panicking.
func TestAuditTrail_ClassifierNilAudit(t *testing.T) {
	inner := &DenyAllClassifier{}
	wrapped := NewAuditClassifier(inner, nil, PermissionDefault, "")

	result := wrapped.Classify(ctx(), call("dangerous"))
	assert.Equal(t, Deny, result)
	assert.Equal(t, "deny_all", wrapped.Name())
}

// TestAuditResolver_WrapsResolvedClassifier verifies that AuditResolver wraps
// every classifier returned by Resolve with an AuditClassifier so the resolver
// path (used by TUI interactive mode) still records audit entries. This is the
// regression guard for the HIGH-1 bypass where effectiveClassifier returned a
// bare classifier from the resolver, skipping the AuditClassifier decorator.
func TestAuditResolver_WrapsResolvedClassifier(t *testing.T) {
	dir := privateAuditTestDir(t)
	trail := NewAuditTrail(dir)

	resolver := NewAuditResolver(NewDefaultPermissionModeResolver(), trail, "sess-r")
	assert.Equal(t, "permission_mode", resolver.Name())

	// Resolve under auto_full so the inner classifier is AllowAllClassifier.
	c := resolver.Resolve(PermissionAutoFull)
	result := c.Classify(ctx(), call("read_file"))
	assert.Equal(t, Allow, result)
	// Name is preserved through the AuditClassifier decorator.
	assert.Equal(t, "allow_all", c.Name())

	// Verify the audit file was written with the resolved mode and session.
	ts := time.Now().UTC()
	filename := filepath.Join(dir, ts.Format("2006-01-02")+".jsonl")
	data, err := os.ReadFile(filename)
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(data, &decoded))
	assert.Equal(t, "read_file", decoded["tool"])
	assert.Equal(t, "allow", decoded["decision"])
	assert.Equal(t, "allow_all", decoded["classifier"])
	assert.Equal(t, "auto_full", decoded["permission_mode"])
	assert.Equal(t, "sess-r", decoded["session_id"])
}

// TestAuditResolver_NilAuditPassesThrough verifies that with a nil AuditTrail
// the resolver returns the bare inner classifier unchanged.
func TestAuditResolver_NilAuditPassesThrough(t *testing.T) {
	resolver := NewAuditResolver(NewDefaultPermissionModeResolver(), nil, "")
	c := resolver.Resolve(PermissionAutoFull)
	// Without audit, the returned classifier is the bare AllowAllClassifier,
	// not an AuditClassifier wrapper.
	_, ok := c.(*AuditClassifier)
	assert.False(t, ok)
	assert.Equal(t, "allow_all", c.Name())
}

// TestAuditTrail_SummarizeArgs verifies the args summary helper.
func TestAuditTrail_SummarizeArgs(t *testing.T) {
	assert.Equal(t, "", summarizeArgs(nil))
	assert.Equal(t, "", summarizeArgs(map[string]any{}))
	assert.Equal(t, "redacted", summarizeArgs(map[string]any{
		"c": 3, "a": 1, "b": 2,
	}))
}

// splitLines splits the JSONL file content into lines, trimming the trailing
// empty element produced by a final newline.
func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

func privateAuditTestDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o700))
	return dir
}

// TestAuditTrail_DirectoryPermissions verifies that the audit directory is
// created with 0o700 permissions (owner-only access) for security (MD-11).
func TestAuditTrail_DirectoryPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "audit")
	trail := NewAuditTrail(dir)

	ts := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	entry := AuditEntry{Timestamp: ts, Tool: "bash", Decision: "deny"}
	require.NoError(t, trail.Record(entry))

	info, err := os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm(),
		"audit directory should have 0700 permissions")
}
