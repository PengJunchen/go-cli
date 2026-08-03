package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/verify"
)

func TestSnapshotStore_SaveAndGet(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	s := NewDefaultSnapshotStore()
	snap := TurnSnapshot{
		TurnID: "turn-1",
		Messages: []core.AgentMessage{
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: "hi there"},
		},
		Config: map[string]any{"model": "gpt-4", "temperature": 0.7},
	}

	require.NoError(t, s.Save(snap))
	assert.Equal(t, 1, s.Len())

	got, ok := s.Get("turn-1")
	require.True(t, ok)
	assert.Equal(t, "turn-1", got.TurnID)
	require.Len(t, got.Messages, 2)
	assert.Equal(t, "hello", got.Messages[0].Content)
	assert.Equal(t, "gpt-4", got.Config["model"])
	assert.False(t, got.CreatedAt.IsZero(), "CreatedAt should be set when zero")
}

func TestSnapshotStore_GetNotFound(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	s := NewDefaultSnapshotStore()
	_, ok := s.Get("missing")
	require.False(t, ok)
}

func TestSnapshotStore_OverwriteByTurnID(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	s := NewDefaultSnapshotStore()
	require.NoError(t, s.Save(TurnSnapshot{
		TurnID:   "turn-1",
		Messages: []core.AgentMessage{{Role: "user", Content: "original"}},
	}))
	require.NoError(t, s.Save(TurnSnapshot{
		TurnID:   "turn-1",
		Messages: []core.AgentMessage{{Role: "user", Content: "overwritten"}},
	}))

	assert.Equal(t, 1, s.Len(), "overwriting should not increase count")
	got, ok := s.Get("turn-1")
	require.True(t, ok)
	assert.Equal(t, "overwritten", got.Messages[0].Content)
}

func TestSnapshotStore_List(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	s := NewDefaultSnapshotStore()
	require.NoError(t, s.Save(TurnSnapshot{TurnID: "t1", Messages: []core.AgentMessage{{Role: "user", Content: "a"}}}))
	require.NoError(t, s.Save(TurnSnapshot{TurnID: "t2", Messages: []core.AgentMessage{{Role: "user", Content: "b"}}}))
	require.NoError(t, s.Save(TurnSnapshot{TurnID: "t3", Messages: []core.AgentMessage{{Role: "user", Content: "c"}}}))

	list := s.List()
	require.Len(t, list, 3)
	// List should be sorted by CreatedAt; since all are set to now, order may
	// vary, but all TurnIDs should be present.
	ids := map[string]bool{list[0].TurnID: true, list[1].TurnID: true, list[2].TurnID: true}
	assert.True(t, ids["t1"] && ids["t2"] && ids["t3"])
}

func TestSnapshotStore_ListEmpty(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	s := NewDefaultSnapshotStore()
	list := s.List()
	assert.Empty(t, list)
}

func TestSnapshotStore_ImmutabilityOnSave(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	s := NewDefaultSnapshotStore()
	original := TurnSnapshot{
		TurnID:   "turn-1",
		Messages: []core.AgentMessage{{Role: "user", Content: "original"}},
		Config:   map[string]any{"key": "value"},
	}
	require.NoError(t, s.Save(original))

	// Mutate the original after save.
	original.Messages[0].Content = "mutated"
	original.Config["key"] = "changed"

	// The stored snapshot should be unaffected.
	got, ok := s.Get("turn-1")
	require.True(t, ok)
	assert.Equal(t, "original", got.Messages[0].Content)
	assert.Equal(t, "value", got.Config["key"])
}

func TestSnapshotStore_ImmutabilityOnGet(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	s := NewDefaultSnapshotStore()
	require.NoError(t, s.Save(TurnSnapshot{
		TurnID:   "turn-1",
		Messages: []core.AgentMessage{{Role: "user", Content: "original"}},
		Config:   map[string]any{"key": "value"},
	}))

	got1, ok := s.Get("turn-1")
	require.True(t, ok)
	got1.Messages[0].Content = "mutated"
	got1.Config["key"] = "changed"

	// A second get should return the unmutated snapshot.
	got2, ok := s.Get("turn-1")
	require.True(t, ok)
	assert.Equal(t, "original", got2.Messages[0].Content)
	assert.Equal(t, "value", got2.Config["key"])
}

func TestSnapshotStore_NilMessagesAndConfig(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	s := NewDefaultSnapshotStore()
	require.NoError(t, s.Save(TurnSnapshot{TurnID: "turn-1"}))

	got, ok := s.Get("turn-1")
	require.True(t, ok)
	assert.Nil(t, got.Messages)
	assert.Nil(t, got.Config)
}
