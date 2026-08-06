package core

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// SystemReminder represents a timed or conditional system message injection.
type SystemReminder struct {
	// ID uniquely identifies the reminder.
	ID string
	// Content is the message text injected as a system message.
	Content string
	// Interval is how often the reminder re-fires. Zero means one-time.
	Interval time.Duration
	// Condition is an optional predicate description. Empty means always
	// eligible (subject to Interval). Non-empty conditions are stored and
	// logged; predicate evaluation is reserved for a future hook.
	Condition string
}

// SystemReminderManager manages active reminders.
type SystemReminderManager interface {
	// AddReminder registers a reminder, overwriting any with the same ID.
	AddReminder(reminder SystemReminder)
	// RemoveReminder removes the reminder with the given ID.
	RemoveReminder(id string)
	// GetActiveReminders returns all registered reminders.
	GetActiveReminders() []SystemReminder
	// CheckAndCollect returns the contents of all reminders due for
	// injection, updating their last-fired timestamps.
	CheckAndCollect(ctx context.Context) []string
}

// DefaultSystemReminderManager is the default SystemReminderManager. It is
// thread-safe and tracks the last-fired time of each reminder so one-time
// reminders fire once and periodic reminders fire on their interval.
type DefaultSystemReminderManager struct {
	mu        sync.RWMutex
	reminders map[string]SystemReminder
	lastFired map[string]time.Time
}

var _ SystemReminderManager = (*DefaultSystemReminderManager)(nil)

// NewDefaultSystemReminderManager creates an empty manager.
func NewDefaultSystemReminderManager() *DefaultSystemReminderManager {
	return &DefaultSystemReminderManager{
		reminders: make(map[string]SystemReminder),
		lastFired: make(map[string]time.Time),
	}
}

// AddReminder registers reminder by ID, overwriting any previous entry.
func (m *DefaultSystemReminderManager) AddReminder(reminder SystemReminder) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if reminder.ID == "" {
		slog.Warn("core.system_reminder.add_empty_id", "content_len", len(reminder.Content))
		return
	}
	m.reminders[reminder.ID] = reminder
	slog.Debug("core.system_reminder.add",
		"id", reminder.ID,
		"interval", reminder.Interval.String(),
		"condition", reminder.Condition,
	)
}

// RemoveReminder removes the reminder with the given id.
func (m *DefaultSystemReminderManager) RemoveReminder(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.reminders, id)
	delete(m.lastFired, id)
	slog.Debug("core.system_reminder.remove", "id", id)
}

// GetActiveReminders returns a snapshot of all registered reminders.
func (m *DefaultSystemReminderManager) GetActiveReminders() []SystemReminder {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]SystemReminder, 0, len(m.reminders))
	for _, r := range m.reminders {
		out = append(out, r)
	}
	return out
}

// CheckAndCollect returns the contents of all due reminders and records their
// firing time. A one-time reminder (Interval == 0) fires only once. A periodic
// reminder fires when its interval has elapsed since the last firing (or on
// the first check).
func (m *DefaultSystemReminderManager) CheckAndCollect(ctx context.Context) []string {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()

	var due []string
	for id, r := range m.reminders {
		last, fired := m.lastFired[id]
		var isDue bool
		if r.Interval == 0 {
			// One-time: due only if never fired.
			isDue = !fired
		} else {
			// Periodic: due if never fired or interval elapsed.
			isDue = !fired || now.Sub(last) >= r.Interval
		}
		if !isDue {
			continue
		}
		due = append(due, r.Content)
		m.lastFired[id] = now
		slog.Debug("core.system_reminder.fired",
			"id", id,
			"content_len", len(r.Content),
			"interval", r.Interval.String(),
			"condition", r.Condition,
		)
	}

	slog.Debug("core.system_reminder.collect", "due", len(due))
	return due
}
