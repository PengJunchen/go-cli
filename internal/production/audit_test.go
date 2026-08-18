package production

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

func TestAuditLogLogAndQueryRoundTrip(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "audit.jsonl")
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

func TestAuditLogFilters(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "audit.jsonl")
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
	path := filepath.Join(t.TempDir(), "does-not-exist.jsonl")
	l := NewDefaultAuditLog(path)

	entries, err := l.Query(ctx, AuditFilter{})
	require.NoError(t, err)
	assert.Len(t, entries, 0)
}

func TestAuditLogAutoTimestamp(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "audit.jsonl")
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
	path := filepath.Join(t.TempDir(), "audit.jsonl")
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
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	l := NewDefaultAuditLog(path)

	require.NoError(t, l.Log(ctx, AuditEntry{Operation: "op"}))
	entries, err := l.Query(ctx, AuditFilter{})
	require.NoError(t, err)
	assert.Len(t, entries, 1)
}

func TestAuditLogStreamingFilterOnLargeFile(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	l := NewDefaultAuditLog(path)

	// Write a large number of entries; only a subset matches the filter.
	const total = 5000
	const matchingOp = "match.me"
	base := time.Now()
	for i := 0; i < total; i++ {
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
	path := filepath.Join(t.TempDir(), "audit.jsonl")
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
	path := filepath.Join(t.TempDir(), "audit.jsonl")
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

func TestAuditLogTamperDetection(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "audit.jsonl")
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

func TestAuditLogRotation(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "audit.jsonl")
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
	path := filepath.Join(t.TempDir(), "audit.jsonl")
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
