package approval

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAuditTrail_Record verifies that a single entry is written to the daily
// JSONL file with all required fields.
func TestAuditTrail_Record(t *testing.T) {
	dir := t.TempDir()
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
	dir := t.TempDir()
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
	dir := t.TempDir()
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
			dir := t.TempDir()
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
	dir := t.TempDir()
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
	dir := t.TempDir()
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
	dir := t.TempDir()
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
	dir := t.TempDir()
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
