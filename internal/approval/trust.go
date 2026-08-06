package approval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"sort"
	"time"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// TrustManager gates whether a project (identified by path) is trusted enough
// for operator-approved configuration to apply. An untrusted project returns
// an untrusted (deny) decision so no config is applied until the operator opts
// in.
type TrustManager interface {
	// IsTrusted reports whether projectPath is currently trusted. Trust entries
	// whose ExpiresAt is set and has already passed are treated as untrusted.
	IsTrusted(ctx context.Context, projectPath string) bool
	// TrustProject marks projectPath as trusted, recording the current time.
	TrustProject(ctx context.Context, projectPath string) error
	// RevokeTrust removes projectPath from the trusted set.
	RevokeTrust(ctx context.Context, projectPath string) error
	// TrustedProjects returns the currently trusted paths, sorted ascending.
	TrustedProjects() []string
}

// DefaultTrustManager is the default TrustManager. It wraps a TrustStore by
// interface so the persistence backend can be swapped. It is safety-aware: an
// unknown or expired project is not trusted.
type DefaultTrustManager struct {
	store TrustStore
}

var _ TrustManager = (*DefaultTrustManager)(nil)

// NewDefaultTrustManager builds a DefaultTrustManager over the given store.
// A nil store is replaced with an in-memory trust store.
func NewDefaultTrustManager(store TrustStore) TrustManager {
	if store == nil {
		store = NewInMemoryTrustStore()
	}
	return &DefaultTrustManager{store: store}
}

// IsTrusted reports whether the project is currently trusted. It emits an
// approval.decision span (classifier=trust_manager) recording the outcome.
// Safety-aware: expiry past, unknown path and store errors all resolve to deny.
func (m *DefaultTrustManager) IsTrusted(ctx context.Context, projectPath string) bool {
	span, _ := tracing.SpanFromContext(ctx, "approval.decision", tracing.SpanKindInternal)
	defer span.End()

	trusted := false
	if entry, ok, err := m.lookup(projectPath); err == nil && ok && !expired(entry, time.Now()) {
		trusted = true
	}

	span.SetAttributes(
		tracing.Attribute{Key: "classifier", Value: "trust_manager"},
		tracing.Attribute{Key: "classification", Value: trustClassification(trusted)},
		tracing.Attribute{Key: "tool_name", Value: "project_config"},
	)
	if !trusted {
		span.SetStatus(tracing.SpanStatusError, "project not trusted")
	}
	return trusted
}

// lookup returns the trust entry for projectPath and whether it exists.
func (m *DefaultTrustManager) lookup(projectPath string) (TrustEntry, bool, error) {
	entries, err := m.store.Load()
	if err != nil {
		return TrustEntry{}, false, err
	}
	entry, ok := entries[projectPath]
	return entry, ok, nil
}

// TrustProject marks the project trusted with the current timestamp and logs
// the change.
func (m *DefaultTrustManager) TrustProject(_ context.Context, projectPath string) error {
	entry := TrustEntry{
		Path:        projectPath,
		Fingerprint: fingerprint(projectPath),
		TrustedAt:   time.Now().Format(time.RFC3339),
	}
	if err := m.store.Add(projectPath, entry); err != nil {
		return err
	}
	slog.Info("approval.trust_project", "path", projectPath)
	return nil
}

// RevokeTrust removes the project from the trusted set and logs the change.
func (m *DefaultTrustManager) RevokeTrust(_ context.Context, projectPath string) error {
	if err := m.store.Remove(projectPath); err != nil {
		return err
	}
	slog.Info("approval.revoke_trust", "path", projectPath)
	return nil
}

// TrustedProjects returns the trusted paths sorted ascending.
func (m *DefaultTrustManager) TrustedProjects() []string {
	entries, err := m.store.Load()
	if err != nil {
		return nil
	}
	paths := make([]string, 0, len(entries))
	for path := range entries {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

// expired reports whether the entry has a valid ExpiresAt that is past now.
func expired(entry TrustEntry, now time.Time) bool {
	if entry.ExpiresAt == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, entry.ExpiresAt)
	if err != nil {
		// An unparsable expiry is treated conservatively as expired.
		return true
	}
	return now.After(t)
}

// trustClassification maps a trust decision to its telemetry classification.
func trustClassification(trusted bool) string {
	if trusted {
		return "allow"
	}
	return "deny"
}

// fingerprint returns a stable SHA-256 fingerprint of path.
func fingerprint(path string) string {
	sum := sha256.Sum256([]byte(path))
	return hex.EncodeToString(sum[:])
}
