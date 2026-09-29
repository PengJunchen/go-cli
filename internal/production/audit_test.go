package production

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/privatefile"
	"github.com/pengjunchen/go-cli/internal/verify"
)

func TestAuditLogLogAndQueryRoundTrip(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	path := auditLogTestPath(t, "audit.jsonl")
	l := NewDefaultAuditLog(path)

	now := time.Now()
	entry := AuditEntry{
		Timestamp: now,
		Operation: "config.set",
		ToolName:  "settings",
		Args:      map[string]any{"key": "max_tokens"},
		Result:    map[string]any{"ok": true},
		UserID:    "u1",
		SessionID: "s1",
	}
	require.NoError(t, l.Log(ctx, entry))

	entries, err := l.Query(ctx, AuditFilter{})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "config.set", entries[0].Operation)
	assert.Equal(t, "settings", entries[0].ToolName)
	assert.Equal(t, "u1", entries[0].UserID)
}

func TestAuditLogDefaultPathSupportsOwnerSharedConfigDirectory(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("advisory file locks are supported on Darwin and Linux")
	}
	dir := filepath.Join(t.TempDir(), ".go-cli")
	require.NoError(t, os.Mkdir(dir, 0o755))
	require.NoError(t, os.Chmod(dir, 0o755))
	path := filepath.Join(dir, "audit.jsonl")
	first := NewDefaultAuditLog(path)
	second := NewDefaultAuditLog(path)
	ctx := context.Background()

	require.NoError(t, first.Log(ctx, AuditEntry{Operation: "first"}))
	require.NoError(t, second.Log(ctx, AuditEntry{Operation: "second"}))
	entries, err := first.Query(ctx, AuditFilter{})
	require.NoError(t, err)
	require.Len(t, entries, 2)

	parentInfo, err := os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o755), parentInfo.Mode().Perm(), "the config directory remains unchanged")
	lockDirInfo, err := os.Stat(filepath.Join(dir, ".go-cli-audit-locks"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), lockDirInfo.Mode().Perm())
}

func TestAuditLogFilters(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	path := auditLogTestPath(t, "audit.jsonl")
	l := NewDefaultAuditLog(path)

	base := time.Now()
	require.NoError(t, l.Log(ctx, AuditEntry{Timestamp: base, Operation: "opA", ToolName: "toolX"}))
	require.NoError(t, l.Log(ctx, AuditEntry{Timestamp: base.Add(time.Second), Operation: "opB", ToolName: "toolY"}))
	require.NoError(t, l.Log(ctx, AuditEntry{Timestamp: base.Add(2 * time.Second), Operation: "opA", ToolName: "toolY"}))

	// Filter by operation.
	res, err := l.Query(ctx, AuditFilter{Operation: "opA"})
	require.NoError(t, err)
	assert.Len(t, res, 2)

	// Filter by tool name.
	res, err = l.Query(ctx, AuditFilter{ToolName: "toolY"})
	require.NoError(t, err)
	assert.Len(t, res, 2)

	// Filter by time range.
	res, err = l.Query(ctx, AuditFilter{From: base.Add(1500 * time.Millisecond)})
	require.NoError(t, err)
	assert.Len(t, res, 1)
	assert.Equal(t, "opA", res[0].Operation)

	// Combined filter excludes everything.
	res, err = l.Query(ctx, AuditFilter{Operation: "opB", ToolName: "toolX"})
	require.NoError(t, err)
	assert.Len(t, res, 0)
}

func TestAuditLogMissingFileReturnsEmpty(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	path := auditLogTestPath(t, "does-not-exist.jsonl")
	l := NewDefaultAuditLog(path)

	entries, err := l.Query(ctx, AuditFilter{})
	require.NoError(t, err)
	assert.Len(t, entries, 0)
}

func TestAuditLogAutoTimestamp(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	path := auditLogTestPath(t, "audit.jsonl")
	l := NewDefaultAuditLog(path)

	require.NoError(t, l.Log(ctx, AuditEntry{Operation: "op"}))
	entries, err := l.Query(ctx, AuditFilter{})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.False(t, entries[0].Timestamp.IsZero(), "missing timestamp should default to now")
}

func TestAuditLogConcurrentWrites(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	path := auditLogTestPath(t, "audit.jsonl")
	l := NewDefaultAuditLog(path)

	var wg sync.WaitGroup
	const writers = 8
	const perWriter = 20
	for g := 0; g < writers; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				discardErr(l.Log(ctx, AuditEntry{Operation: "op", UserID: "u"}))
			}
		}(g)
	}
	wg.Wait()

	entries, err := l.Query(ctx, AuditFilter{})
	require.NoError(t, err)
	assert.Len(t, entries, writers*perWriter)
}

func TestAuditLogNameAndOption(t *testing.T) {
	l := NewDefaultAuditLog("")
	assert.Equal(t, "default-audit-log", l.Name())
	assert.Equal(t, "custom", NewDefaultAuditLog("", WithAuditName("custom")).Name())
}

func TestAuditLogContextCancellation(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	path := auditLogTestPath(t, "audit.jsonl")
	l := NewDefaultAuditLog(path)

	require.NoError(t, l.Log(ctx, AuditEntry{Operation: "op"}))
	entries, err := l.Query(ctx, AuditFilter{})
	require.NoError(t, err)
	assert.Len(t, entries, 1)
}

func TestAuditLogStreamingFilterOnLargeFile(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	path := auditLogTestPath(t, "audit.jsonl")
	l := NewDefaultAuditLog(path)

	// Write a large number of entries; only a subset matches the filter.
	const total = 5000
	const matchingOp = "match.me"
	base := time.Now()
	started := time.Now()
	for i := 0; i < total; i++ {
		if time.Since(started) > 60*time.Second {
			t.Fatalf("appending %d audit entries exceeded the 60s bound", i)
		}
		op := "other"
		if i%10 == 0 {
			op = matchingOp
		}
		require.NoError(t, l.Log(ctx, AuditEntry{
			Timestamp: base.Add(time.Duration(i) * time.Millisecond),
			Operation: op,
			ToolName:  "bulk",
		}))
	}

	// Streaming filter should return only matching entries without loading
	// the entire file into an intermediate slice.
	entries, err := l.Query(ctx, AuditFilter{Operation: matchingOp})
	require.NoError(t, err)
	assert.Len(t, entries, total/10)
	for _, e := range entries {
		assert.Equal(t, matchingOp, e.Operation)
	}

	// No-filter query returns all entries.
	all, err := l.Query(ctx, AuditFilter{})
	require.NoError(t, err)
	assert.Len(t, all, total)
}

func TestAuditLogStreamingHandlesLargeLines(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	path := auditLogTestPath(t, "audit.jsonl")
	l := NewDefaultAuditLog(path)

	// Create an entry with large Args payload (> 64 KB default scanner buffer).
	big := make(map[string]any)
	big["data"] = strings.Repeat("x", 128*1024)
	require.NoError(t, l.Log(ctx, AuditEntry{
		Operation: "big.entry",
		Args:      big,
	}))
	require.NoError(t, l.Log(ctx, AuditEntry{Operation: "small.entry"}))

	entries, err := l.Query(ctx, AuditFilter{})
	require.NoError(t, err)
	require.Len(t, entries, 2)
	assert.Equal(t, "big.entry", entries[0].Operation)
	assert.Equal(t, "small.entry", entries[1].Operation)

	// Filter should also work with large lines.
	filtered, err := l.Query(ctx, AuditFilter{Operation: "big.entry"})
	require.NoError(t, err)
	require.Len(t, filtered, 1)
	assert.Equal(t, "big.entry", filtered[0].Operation)
}

func TestAuditLogHashChain(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	path := auditLogTestPath(t, "audit.jsonl")
	l := NewDefaultAuditLog(path)

	require.NoError(t, l.Log(ctx, AuditEntry{Operation: "op1"}))
	require.NoError(t, l.Log(ctx, AuditEntry{Operation: "op2"}))
	require.NoError(t, l.Log(ctx, AuditEntry{Operation: "op3"}))

	entries, err := l.Query(ctx, AuditFilter{})
	require.NoError(t, err)
	require.Len(t, entries, 3)

	// First entry has empty PrevHash (genesis of the chain).
	assert.Empty(t, entries[0].PrevHash, "first entry should have empty prev_hash")

	// Each entry's PrevHash equals the previous entry's Hash.
	for i := 1; i < len(entries); i++ {
		assert.Equal(t, entries[i-1].Hash, entries[i].PrevHash,
			"entry %d prev_hash should equal entry %d hash", i, i-1)
	}

	// Every entry should have a non-empty Hash.
	for i, e := range entries {
		assert.NotEmpty(t, e.Hash, "entry %d should have a hash", i)
	}

	// VerifyChain should pass on the intact chain.
	al := l.(*DefaultAuditLog)
	assert.NoError(t, al.VerifyChain())
}

func TestAuditHashAdvancesOnlyAfterAppend(t *testing.T) {
	for _, tc := range []struct {
		name      string
		writeMode string
		syncErr   error
		wantErr   error
	}{
		{name: "short write", writeMode: "short", wantErr: ErrAuditLogAppend},
		{name: "sync failure", syncErr: errors.New("sync sentinel"), wantErr: ErrAuditLogSync},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := auditLogTestPath(t, "audit.jsonl")
			log, ok := NewDefaultAuditLog(path).(*DefaultAuditLog)
			require.True(t, ok)
			ctx := context.Background()
			require.NoError(t, log.Log(ctx, AuditEntry{Operation: "first"}))
			before, err := log.Query(ctx, AuditFilter{})
			require.NoError(t, err)
			require.Len(t, before, 1)
			previousHash := before[0].Hash

			log.openFile = faultAuditFileOpener(tc.writeMode, tc.syncErr, nil)
			err = log.Log(ctx, AuditEntry{Operation: "failed"})
			require.Error(t, err)
			assert.ErrorIs(t, err, tc.wantErr)
			if tc.writeMode == "short" {
				assert.ErrorIs(t, err, io.ErrShortWrite)
			} else {
				assert.ErrorIs(t, err, tc.syncErr)
			}

			log.openFile = openAuditLogFile
			require.NoError(t, log.Log(ctx, AuditEntry{Operation: "second"}))
			after, err := log.Query(ctx, AuditFilter{})
			require.NoError(t, err)
			require.Len(t, after, 2)
			assert.Equal(t, previousHash, after[1].PrevHash)
			require.NoError(t, log.VerifyChain())
		})
	}
}

func TestAuditLogRollbackFailurePoisonsWriter(t *testing.T) {
	path := auditLogTestPath(t, "audit.jsonl")
	log, ok := NewDefaultAuditLog(path).(*DefaultAuditLog)
	require.True(t, ok)
	syncErr := errors.New("sync sentinel")
	rollbackErr := errors.New("truncate sentinel")
	log.openFile = faultAuditFileOpener("", syncErr, rollbackErr)

	err := log.Log(context.Background(), AuditEntry{Operation: "failed"})
	assert.ErrorIs(t, err, ErrAuditLogSync)
	assert.ErrorIs(t, err, ErrAuditLogRollback)
	assert.ErrorIs(t, err, syncErr)
	assert.ErrorIs(t, err, rollbackErr)

	err = log.Log(context.Background(), AuditEntry{Operation: "blocked"})
	assert.ErrorIs(t, err, ErrAuditLogRollback)
}

func TestAuditLogContinuesHashChainAfterRestart(t *testing.T) {
	path := auditLogTestPath(t, "audit.jsonl")
	ctx := context.Background()
	first, ok := NewDefaultAuditLog(path).(*DefaultAuditLog)
	require.True(t, ok)
	require.NoError(t, first.Log(ctx, AuditEntry{Operation: "first"}))

	reopened, ok := NewDefaultAuditLog(path).(*DefaultAuditLog)
	require.True(t, ok)
	require.NoError(t, reopened.Log(ctx, AuditEntry{Operation: "second"}))
	entries, err := reopened.Query(ctx, AuditFilter{})
	require.NoError(t, err)
	require.Len(t, entries, 2)
	assert.Equal(t, entries[0].Hash, entries[1].PrevHash)
	require.NoError(t, reopened.VerifyChain())
}

func TestAuditLogMultipleInstancesRefreshHashChain(t *testing.T) {
	path := auditLogTestPath(t, "audit.jsonl")
	ctx := context.Background()
	first, ok := NewDefaultAuditLog(path).(*DefaultAuditLog)
	require.True(t, ok)
	second, ok := NewDefaultAuditLog(path).(*DefaultAuditLog)
	require.True(t, ok)
	for index := 0; index < 20; index++ {
		log := first
		if index%2 == 1 {
			log = second
		}
		require.NoError(t, log.Log(ctx, AuditEntry{Operation: fmt.Sprintf("operation-%02d", index)}))
	}

	entries, err := first.Query(ctx, AuditFilter{})
	require.NoError(t, err)
	require.Len(t, entries, 20)
	for index := 1; index < len(entries); index++ {
		assert.Equal(t, entries[index-1].Hash, entries[index].PrevHash, "entry %d must continue the latest durable chain", index)
	}
	require.NoError(t, first.VerifyChain())
}

func TestProductionAuditCrossProcessAppendHelper(t *testing.T) {
	mode := os.Getenv("GOCLI_PROD_AUDIT_HELPER_MODE")
	if mode == "" {
		return
	}
	log, ok := NewDefaultAuditLog(os.Getenv("GOCLI_PROD_AUDIT_HELPER_PATH")).(*DefaultAuditLog)
	if !ok {
		t.Fatal("default audit log implementation unavailable")
	}
	if mode == "block" {
		openFile := log.openFile
		log.openFile = func(path string, flag int, perm os.FileMode) (auditFile, error) {
			file, err := openFile(path, flag, perm)
			if err != nil {
				return nil, err
			}
			if err := writeProductionAuditMarker(os.Getenv("GOCLI_PROD_AUDIT_HELPER_OPENED")); err != nil {
				_ = file.Close()
				return nil, err
			}
			if err := waitForProductionAuditMarker(os.Getenv("GOCLI_PROD_AUDIT_HELPER_RELEASE"), 5*time.Second); err != nil {
				_ = file.Close()
				return nil, err
			}
			return file, nil
		}
	} else if mode == "waiter" {
		if err := writeProductionAuditMarker(os.Getenv("GOCLI_PROD_AUDIT_HELPER_ATTEMPTED")); err != nil {
			t.Errorf("write attempted marker: %v", err)
		}
	}

	err := log.Log(context.Background(), AuditEntry{Operation: os.Getenv("GOCLI_PROD_AUDIT_HELPER_OPERATION")})
	if err != nil {
		t.Errorf("record audit entry: %v", err)
	}
	if mode == "waiter" {
		if err := writeProductionAuditMarker(os.Getenv("GOCLI_PROD_AUDIT_HELPER_COMPLETE")); err != nil {
			t.Errorf("write complete marker: %v", err)
		}
	}
}

func TestAuditLogSerializesCrossProcessAppend(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("cross-process advisory locks are supported on Darwin and Linux")
	}
	path := auditLogTestPath(t, "audit.jsonl")
	opened := path + ".opened"
	attempted := path + ".attempted"
	complete := path + ".complete"
	release := path + ".release"
	firstOutput := &bytes.Buffer{}
	first := startProductionAuditProcess(t, firstOutput, path, "block", "first", opened, attempted, complete, release)
	if err := waitForProductionAuditMarker(opened, 5*time.Second); err != nil {
		_ = first.Process.Kill()
		_ = first.Wait()
		t.Fatalf("first process did not enter its open hook: %v; output: %s", err, firstOutput.String())
	}
	secondOutput := &bytes.Buffer{}
	second := startProductionAuditProcess(t, secondOutput, path, "waiter", "second", opened, attempted, complete, release)
	if err := waitForProductionAuditMarker(attempted, 5*time.Second); err != nil {
		_ = writeProductionAuditMarker(release)
		_ = first.Wait()
		_ = second.Process.Kill()
		_ = second.Wait()
		t.Fatalf("second process did not attempt its append: %v; output: %s", err, secondOutput.String())
	}
	time.Sleep(200 * time.Millisecond)
	_, completeErr := os.Stat(complete)
	completedBeforeRelease := completeErr == nil
	require.True(t, errors.Is(completeErr, os.ErrNotExist) || completedBeforeRelease, "inspect second process completion")
	require.NoError(t, writeProductionAuditMarker(release))
	firstErr := first.Wait()
	secondErr := second.Wait()
	require.NoError(t, firstErr, firstOutput.String())
	require.NoError(t, secondErr, secondOutput.String())
	assert.False(t, completedBeforeRelease, "the second process must wait while the first owns the audit append lock")

	log, ok := NewDefaultAuditLog(path).(*DefaultAuditLog)
	require.True(t, ok)
	entries, err := log.Query(context.Background(), AuditFilter{})
	require.NoError(t, err)
	require.Len(t, entries, 2)
	assert.Equal(t, entries[0].Hash, entries[1].PrevHash)
	require.NoError(t, log.VerifyChain())
}

func startProductionAuditProcess(t *testing.T, output *bytes.Buffer, path, mode, operation, opened, attempted, complete, release string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestProductionAuditCrossProcessAppendHelper$")
	cmd.Env = append(os.Environ(),
		"GOCLI_PROD_AUDIT_HELPER_MODE="+mode,
		"GOCLI_PROD_AUDIT_HELPER_PATH="+path,
		"GOCLI_PROD_AUDIT_HELPER_OPERATION="+operation,
		"GOCLI_PROD_AUDIT_HELPER_OPENED="+opened,
		"GOCLI_PROD_AUDIT_HELPER_ATTEMPTED="+attempted,
		"GOCLI_PROD_AUDIT_HELPER_COMPLETE="+complete,
		"GOCLI_PROD_AUDIT_HELPER_RELEASE="+release,
	)
	cmd.Stdout = output
	cmd.Stderr = output
	require.NoError(t, cmd.Start())
	return cmd
}

func writeProductionAuditMarker(path string) error {
	return os.WriteFile(path, []byte("ready"), 0o600)
}

func waitForProductionAuditMarker(path string, timeout time.Duration) error {
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

func TestAuditLogRejectsLegacyUnhashedFile(t *testing.T) {
	path := auditLogTestPath(t, "audit.jsonl")
	legacy := []byte(`{"Operation":"legacy"}` + "\n")
	require.NoError(t, os.WriteFile(path, legacy, 0o600))
	log, ok := NewDefaultAuditLog(path).(*DefaultAuditLog)
	require.True(t, ok)

	err := log.Log(context.Background(), AuditEntry{Operation: "new"})
	assert.ErrorIs(t, err, ErrAuditLogCorrupt)
	got, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, legacy, got, "a legacy file must be rejected without migration or append")
}

func TestAuditLogFailsClosedWhenLockUnavailable(t *testing.T) {
	path := auditLogTestPath(t, "audit.jsonl")
	require.NoError(t, os.WriteFile(path+".lock", nil, 0o644))
	require.NoError(t, os.Chmod(path+".lock", 0o644))
	log, ok := NewDefaultAuditLog(path).(*DefaultAuditLog)
	require.True(t, ok)

	err := log.Log(context.Background(), AuditEntry{Operation: "new"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "privatefile")
	_, statErr := os.Stat(path)
	assert.ErrorIs(t, statErr, os.ErrNotExist, "no audit record is appended without the lock")
}

func TestAuditLogTamperDetection(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	path := auditLogTestPath(t, "audit.jsonl")
	l := NewDefaultAuditLog(path)

	require.NoError(t, l.Log(ctx, AuditEntry{Operation: "op1", UserID: "u1"}))
	require.NoError(t, l.Log(ctx, AuditEntry{Operation: "op2", UserID: "u2"}))
	require.NoError(t, l.Log(ctx, AuditEntry{Operation: "op3", UserID: "u3"}))

	al := l.(*DefaultAuditLog)
	require.NoError(t, al.VerifyChain())

	// Tamper: modify a field in the second entry.
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	tampered := strings.Replace(string(raw), "op2", "opX", 1)
	require.NotEqual(t, string(raw), tampered, "tamper should change file content")
	require.NoError(t, os.WriteFile(path, []byte(tampered), 0o600))

	// VerifyChain should detect the tampering.
	err = al.VerifyChain()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "hash mismatch")
}

func TestAuditLogRejectsSameSizeHistoricalTamperingBeforeAppend(t *testing.T) {
	path := auditLogTestPath(t, "audit.jsonl")
	log, ok := NewDefaultAuditLog(path).(*DefaultAuditLog)
	require.True(t, ok)
	ctx := context.Background()
	require.NoError(t, log.Log(ctx, AuditEntry{Operation: "alpha"}))
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	tampered := bytes.Replace(raw, []byte(`"Operation":"alpha"`), []byte(`"Operation":"bravo"`), 1)
	require.NotEqual(t, raw, tampered)
	require.Equal(t, len(raw), len(tampered))
	require.NoError(t, os.WriteFile(path, tampered, 0o600))

	err = log.Log(ctx, AuditEntry{Operation: "later"})
	assert.ErrorIs(t, err, ErrAuditLogCorrupt)
}

func TestAuditLogLockWaitIsBoundedAfterCallerCancellation(t *testing.T) {
	path := auditLogTestPath(t, "audit.jsonl")
	log, ok := NewDefaultAuditLog(path).(*DefaultAuditLog)
	require.True(t, ok)
	log.lockTimeout = 50 * time.Millisecond
	release := holdProductionAuditLock(t, path)
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := log.Log(ctx, AuditEntry{Operation: "blocked"})
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	_, statErr := os.Stat(path)
	assert.ErrorIs(t, statErr, os.ErrNotExist, "a record is not appended after the lock wait expires")
}

func holdProductionAuditLock(t *testing.T, path string) func() {
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

func TestAuditLogRotation(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	path := auditLogTestPath(t, "audit.jsonl")
	l := NewDefaultAuditLog(path, WithMaxSize(512))

	// Write enough entries to trigger at least one rotation.
	for i := 0; i < 10; i++ {
		require.NoError(t, l.Log(ctx, AuditEntry{Operation: "op", UserID: "u"}))
	}

	// Rotation should have created path+".1".
	_, err := os.Stat(path + ".1")
	assert.NoError(t, err, "rotated file path.1 should exist")

	// The current file should still exist and be queryable.
	entries, err := l.Query(ctx, AuditFilter{})
	require.NoError(t, err)
	assert.NotEmpty(t, entries, "current file should have entries")

	// VerifyChain should pass on the current file (new chain after rotation).
	al := l.(*DefaultAuditLog)
	assert.NoError(t, al.VerifyChain())
}

func TestAuditLogConcurrentWrite(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	path := auditLogTestPath(t, "audit.jsonl")
	l := NewDefaultAuditLog(path)

	var wg sync.WaitGroup
	const goroutines = 100
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			discardErr(l.Log(ctx, AuditEntry{Operation: "op"}))
		}()
	}
	wg.Wait()

	// All 100 entries should be present.
	entries, err := l.Query(ctx, AuditFilter{})
	require.NoError(t, err)
	assert.Len(t, entries, goroutines)

	// Hash chain should be intact despite concurrent writes.
	al := l.(*DefaultAuditLog)
	assert.NoError(t, al.VerifyChain())
}

type auditFaultFile struct {
	*os.File
	writeMode   string
	syncErr     error
	syncCalls   int
	truncateErr error
}

func (f *auditFaultFile) Write(data []byte) (int, error) {
	if f.writeMode == "short" {
		return len(data) / 2, nil
	}
	return f.File.Write(data)
}

func (f *auditFaultFile) Sync() error {
	if f.syncErr != nil && f.syncCalls == 0 {
		f.syncCalls++
		return f.syncErr
	}
	f.syncCalls++
	return f.File.Sync()
}

func (f *auditFaultFile) Truncate(size int64) error {
	if f.truncateErr != nil {
		return f.truncateErr
	}
	return f.File.Truncate(size)
}

func faultAuditFileOpener(writeMode string, syncErr, truncateErr error) auditFileOpener {
	return func(path string, flag int, perm os.FileMode) (auditFile, error) {
		file, err := os.OpenFile(path, flag, perm)
		if err != nil {
			return nil, err
		}
		return &auditFaultFile{File: file, writeMode: writeMode, syncErr: syncErr, truncateErr: truncateErr}, nil
	}
}

func auditLogTestPath(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o700))
	return filepath.Join(dir, name)
}
