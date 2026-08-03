package session

import (
	"log/slog"
	"sync"
	"time"

	"github.com/pengjunchen/go-cli/internal/core"
)

// TurnSnapshot is an immutable snapshot of the session state at a specific turn.
type TurnSnapshot struct {
	TurnID    string
	Messages  []core.AgentMessage
	Config    map[string]any
	CreatedAt time.Time
}

// SnapshotStore stores and retrieves turn snapshots.
type SnapshotStore interface {
	// Save stores a snapshot. A snapshot with the same TurnID overwrites the
	// previous one.
	Save(snapshot TurnSnapshot) error
	// Get returns the snapshot for the given turnID. Returns ok=false when no
	// snapshot exists for that turn.
	Get(turnID string) (TurnSnapshot, bool)
	// List returns all stored snapshots ordered by creation time.
	List() []TurnSnapshot
}

// DefaultSnapshotStore is an in-memory SnapshotStore. Snapshots are immutable:
// the store copies the messages and config slices on save and on get so callers
// cannot mutate the stored data through the returned values.
type DefaultSnapshotStore struct {
	mu        sync.RWMutex
	snapshots map[string]TurnSnapshot
}

// Compile-time assertion that DefaultSnapshotStore satisfies SnapshotStore.
var _ SnapshotStore = (*DefaultSnapshotStore)(nil)

// NewDefaultSnapshotStore returns an empty in-memory snapshot store.
func NewDefaultSnapshotStore() *DefaultSnapshotStore {
	return &DefaultSnapshotStore{snapshots: make(map[string]TurnSnapshot)}
}

// cloneMessages returns a defensive copy of the messages slice.
func cloneMessages(msgs []core.AgentMessage) []core.AgentMessage {
	if msgs == nil {
		return nil
	}
	cp := make([]core.AgentMessage, len(msgs))
	copy(cp, msgs)
	return cp
}

// cloneConfig returns a defensive copy of the config map.
func cloneConfig(cfg map[string]any) map[string]any {
	if cfg == nil {
		return nil
	}
	cp := make(map[string]any, len(cfg))
	for k, v := range cfg {
		cp[k] = v
	}
	return cp
}

// Save stores an immutable copy of the snapshot. A snapshot with the same
// TurnID overwrites the previous one.
func (s *DefaultSnapshotStore) Save(snapshot TurnSnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	snap := TurnSnapshot{
		TurnID:    snapshot.TurnID,
		Messages:  cloneMessages(snapshot.Messages),
		Config:    cloneConfig(snapshot.Config),
		CreatedAt: snapshot.CreatedAt,
	}
	if snap.CreatedAt.IsZero() {
		snap.CreatedAt = time.Now().UTC()
	}
	s.snapshots[snap.TurnID] = snap

	slog.Info("session.snapshot.save",
		"turn_id", snap.TurnID,
		"message_count", len(snap.Messages),
	)
	return nil
}

// Get returns a defensive copy of the snapshot for the given turnID. Returns
// ok=false when no snapshot exists.
func (s *DefaultSnapshotStore) Get(turnID string) (TurnSnapshot, bool) {
	s.mu.RLock()
	snap, ok := s.snapshots[turnID]
	s.mu.RUnlock()
	if !ok {
		return TurnSnapshot{}, false
	}
	return TurnSnapshot{
		TurnID:    snap.TurnID,
		Messages:  cloneMessages(snap.Messages),
		Config:    cloneConfig(snap.Config),
		CreatedAt: snap.CreatedAt,
	}, true
}

// List returns all stored snapshots as defensive copies, ordered by creation
// time.
func (s *DefaultSnapshotStore) List() []TurnSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()

	list := make([]TurnSnapshot, 0, len(s.snapshots))
	for _, snap := range s.snapshots {
		list = append(list, TurnSnapshot{
			TurnID:    snap.TurnID,
			Messages:  cloneMessages(snap.Messages),
			Config:    cloneConfig(snap.Config),
			CreatedAt: snap.CreatedAt,
		})
	}
	// Sort by creation time for a stable order.
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j-1].CreatedAt.After(list[j].CreatedAt); j-- {
			list[j-1], list[j] = list[j], list[j-1]
		}
	}
	return list
}

// Len returns the number of stored snapshots.
func (s *DefaultSnapshotStore) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.snapshots)
}
