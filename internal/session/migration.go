package session

import (
	"fmt"
	"log/slog"
	"sync"
)

// SessionVersion represents the schema version of a session file.
type SessionVersion int

const (
	// SessionV1 is the initial schema version.
	SessionV1 SessionVersion = 1
	// SessionV2 adds the "metadata" field.
	SessionV2 SessionVersion = 2
	// SessionV3 adds the "trace_id" field.
	SessionV3 SessionVersion = 3
	// CurrentVersion is the latest supported session schema version.
	CurrentVersion SessionVersion = SessionV3
)

// String returns a stable identifier for the version.
func (v SessionVersion) String() string {
	return fmt.Sprintf("v%d", int(v))
}

// MigrationFunc transforms session data from one schema version to the next.
type MigrationFunc func(data map[string]any) (map[string]any, error)

// MigrationChain migrates session data from one version to the next through a
// chain of registered MigrationFuncs. It is safe for concurrent use.
type MigrationChain struct {
	mu         sync.RWMutex
	migrations map[SessionVersion]MigrationFunc
}

// Compile-time assertion that MigrationChain is a usable type.
var _ = (*MigrationChain)(nil)

// NewMigrationChain returns a MigrationChain pre-populated with the default
// migrations: v1->v2 (add "metadata") and v2->v3 (add "trace_id").
func NewMigrationChain() *MigrationChain {
	chain := &MigrationChain{
		migrations: make(map[SessionVersion]MigrationFunc),
	}
	chain.Register(SessionV1, migrateV1ToV2)
	chain.Register(SessionV2, migrateV2ToV3)
	return chain
}

// Register adds or replaces a migration from the given version to the next.
func (c *MigrationChain) Register(from SessionVersion, fn MigrationFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.migrations[from] = fn
	slog.Info("session.migration.register", "from", from.String())
}

// Migrate migrates data from the given version up to CurrentVersion. Each step
// applies the registered migration for the current version, advancing until the
// data is at CurrentVersion or an error occurs.
func (c *MigrationChain) Migrate(data map[string]any, from SessionVersion) (map[string]any, error) {
	if data == nil {
		data = make(map[string]any)
	}

	c.mu.RLock()
	defer c.mu.RUnlock()

	current := from
	for current < CurrentVersion {
		fn, ok := c.migrations[current]
		if !ok {
			slog.Error("session.migration.missing",
				"from", current.String(),
				"target", CurrentVersion.String(),
			)
			return nil, fmt.Errorf("session: no migration registered from %s", current.String())
		}
		next, err := fn(data)
		if err != nil {
			slog.Error("session.migration.failed",
				"from", current.String(),
				"err", err,
			)
			return nil, fmt.Errorf("session: migration %s->%s: %w", current.String(), (current + 1).String(), err)
		}
		data = next
		current++
		slog.Info("session.migration.step",
			"from", (current - 1).String(),
			"to", current.String(),
		)
	}

	slog.Info("session.migration.complete",
		"from", from.String(),
		"to", CurrentVersion.String(),
	)
	return data, nil
}

// migrateV1ToV2 adds the "metadata" field as an empty map when absent.
func migrateV1ToV2(data map[string]any) (map[string]any, error) {
	if _, ok := data["metadata"]; !ok {
		data["metadata"] = make(map[string]any)
	}
	return data, nil
}

// migrateV2ToV3 adds the "trace_id" field as an empty string when absent.
func migrateV2ToV3(data map[string]any) (map[string]any, error) {
	if _, ok := data["trace_id"]; !ok {
		data["trace_id"] = ""
	}
	return data, nil
}
