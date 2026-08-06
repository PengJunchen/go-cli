package core

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSystemReminderManagerImplementsInterface(t *testing.T) {
	var _ SystemReminderManager = (*DefaultSystemReminderManager)(nil)
}

func TestSystemReminderAddGetRemove(t *testing.T) {
	m := NewDefaultSystemReminderManager()

	m.AddReminder(SystemReminder{ID: "r1", Content: "hello", Interval: time.Second})
	m.AddReminder(SystemReminder{ID: "r2", Content: "world", Interval: 0})

	active := m.GetActiveReminders()
	assert.Len(t, active, 2)

	m.RemoveReminder("r1")
	active = m.GetActiveReminders()
	require.Len(t, active, 1)
	assert.Equal(t, "r2", active[0].ID)

	m.RemoveReminder("missing") // no-op, must not panic
	assert.Len(t, m.GetActiveReminders(), 1)
}

func TestSystemReminderAddEmptyIDIgnored(t *testing.T) {
	m := NewDefaultSystemReminderManager()
	m.AddReminder(SystemReminder{ID: "", Content: "no id"})
	assert.Empty(t, m.GetActiveReminders())
}

func TestSystemReminderOneTimeFiresOnce(t *testing.T) {
	m := NewDefaultSystemReminderManager()
	m.AddReminder(SystemReminder{ID: "once", Content: "once-content", Interval: 0})

	first := m.CheckAndCollect(context.Background())
	require.Len(t, first, 1)
	assert.Equal(t, "once-content", first[0])

	second := m.CheckAndCollect(context.Background())
	assert.Empty(t, second, "one-time reminder must not fire again")
}

func TestSystemReminderPeriodicFiresOnInterval(t *testing.T) {
	m := NewDefaultSystemReminderManager()
	m.AddReminder(SystemReminder{ID: "periodic", Content: "tick", Interval: 50 * time.Millisecond})

	// First check fires immediately (never fired before).
	first := m.CheckAndCollect(context.Background())
	require.Len(t, first, 1)

	// Immediately after: not due yet.
	second := m.CheckAndCollect(context.Background())
	assert.Empty(t, second)

	// After the interval elapses: due again.
	time.Sleep(60 * time.Millisecond)
	third := m.CheckAndCollect(context.Background())
	require.Len(t, third, 1)
	assert.Equal(t, "tick", third[0])
}

func TestSystemReminderCheckAndCollectMultiple(t *testing.T) {
	m := NewDefaultSystemReminderManager()
	m.AddReminder(SystemReminder{ID: "a", Content: "A", Interval: 0})
	m.AddReminder(SystemReminder{ID: "b", Content: "B", Interval: 0})

	got := m.CheckAndCollect(context.Background())
	assert.Len(t, got, 2)
	assert.ElementsMatch(t, []string{"A", "B"}, got)

	// Both one-time, so a second collect is empty.
	assert.Empty(t, m.CheckAndCollect(context.Background()))
}

func TestSystemReminderAddOverwrites(t *testing.T) {
	m := NewDefaultSystemReminderManager()
	m.AddReminder(SystemReminder{ID: "r", Content: "old", Interval: 0})
	m.AddReminder(SystemReminder{ID: "r", Content: "new", Interval: 0})

	active := m.GetActiveReminders()
	require.Len(t, active, 1)
	assert.Equal(t, "new", active[0].Content)
}

func TestSystemReminderStoresCondition(t *testing.T) {
	m := NewDefaultSystemReminderManager()
	m.AddReminder(SystemReminder{ID: "c", Content: "cond", Interval: 0, Condition: "every-turn"})

	active := m.GetActiveReminders()
	require.Len(t, active, 1)
	assert.Equal(t, "every-turn", active[0].Condition)

	// Condition is stored/logged; eligibility still follows the interval rule,
	// so a one-time reminder fires once.
	got := m.CheckAndCollect(context.Background())
	require.Len(t, got, 1)
}
