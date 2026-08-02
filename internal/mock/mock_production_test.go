//go:build mock

package mock

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/production"
)

// Compile-time assertions that the mocks satisfy the production interfaces.
var (
	_ production.IdempotentCache = (*MockIdempotentCache)(nil)
	_ production.AuditLog        = (*MockAuditLog)(nil)
)

func TestMockIdempotentCacheRecordsAndFIFO(t *testing.T) {
	c := NewMockIdempotentCache(2)

	// Cache hit/miss behavior.
	require.NoError(t, c.Set(context.Background(), "a", 1))
	require.NoError(t, c.Set(context.Background(), "b", 2))
	_, ok := c.Get(context.Background(), "a")
	assert.True(t, ok)

	// FIFO eviction at capacity.
	require.NoError(t, c.Set(context.Background(), "c", 3))
	_, evicted := c.Get(context.Background(), "a")
	assert.False(t, evicted, "oldest key should be evicted at capacity")
	_, remain := c.Get(context.Background(), "b")
	assert.True(t, remain)

	// Call recording.
	assert.Equal(t, 2, c.GetCallsCount("a"))
	assert.Equal(t, 1, c.GetCallsCount("b"))
	require.NoError(t, c.Delete(context.Background(), "b"))
	assert.Equal(t, 1, c.DeleteCallsCount("b"))

	// Programmed results / name.
	c.ProgramGet("z", "canned", true)
	v, ok := c.Get(context.Background(), "z")
	assert.True(t, ok)
	assert.Equal(t, "canned", v)
	assert.Equal(t, "mock-idempotent-cache", c.Name())
}

func TestMockAuditLogRecords(t *testing.T) {
	l := NewMockAuditLog()
	entry := production.AuditEntry{Operation: "config.set", ToolName: "settings"}
	require.NoError(t, l.Log(context.Background(), entry))

	logged := l.LoggedEntries()
	require.Len(t, logged, 1)
	assert.Equal(t, "config.set", logged[0].Operation)
	assert.Equal(t, 1, l.LogCount())

	entries, err := l.Query(context.Background(), production.AuditFilter{})
	require.NoError(t, err)
	assert.Len(t, entries, 1)
	assert.Len(t, l.LoggedFilters(), 1)

	// Error injection.
	l.SetQueryErr(errors.New("boom"))
	_, err = l.Query(context.Background(), production.AuditFilter{})
	assert.Error(t, err)
}

func TestMockAuditLogName(t *testing.T) {
	l := NewMockAuditLog()
	assert.Equal(t, "mock-audit-log", l.Name())
}

func TestMockAuditLogWithName(t *testing.T) {
	l := NewMockAuditLog()
	result := l.WithName("custom-name")
	assert.Same(t, l, result, "WithName should return the same instance for chaining")
	assert.Equal(t, "custom-name", l.Name())
}

func TestMockAuditLogSetErr(t *testing.T) {
	l := NewMockAuditLog()
	l.SetErr(errors.New("injected"))

	err := l.Log(context.Background(), production.AuditEntry{Operation: "test"})
	require.Error(t, err)
	assert.Equal(t, "injected", err.Error())

	// The entry should still be recorded.
	assert.Equal(t, 1, l.LogCount())
}

func TestMockAuditLogQueryReturnsCopy(t *testing.T) {
	l := NewMockAuditLog()
	require.NoError(t, l.Log(context.Background(), production.AuditEntry{Operation: "a"}))

	entries1, err := l.Query(context.Background(), production.AuditFilter{})
	require.NoError(t, err)
	entries1[0] = production.AuditEntry{} // mutate copy

	entries2, err := l.Query(context.Background(), production.AuditFilter{})
	require.NoError(t, err)
	assert.Equal(t, "a", entries2[0].Operation, "original should be unchanged")
}
