package approval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
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

// TrustEntryReader is the optional read-only capability for inspecting stored
// trust metadata. It preserves load errors and exposes legacy entries without
// treating them as authorization.
type TrustEntryReader interface {
	LoadTrustEntries() (map[string]TrustEntry, error)
}

// DefaultTrustManager is the default TrustManager. It wraps a TrustStore by
// interface so the persistence backend can be swapped. It is safety-aware: an
// unknown or expired project is not trusted.
type DefaultTrustManager struct {
	store TrustStore
}

var _ TrustManager = (*DefaultTrustManager)(nil)
var _ TrustEntryReader = (*DefaultTrustManager)(nil)

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
	if entry, ok, err := m.lookup(projectPath); err == nil && ok && !entry.Revoked && !expired(entry, time.Now()) {
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
// the change. The fingerprint is computed from the content of the project's
// .go-cli/mcp.json config file so that any config change is detectable.
func (m *DefaultTrustManager) TrustProject(ctx context.Context, projectPath string) error {
	if ctx == nil {
		return errors.New("approval: trust context is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if m == nil || m.store == nil {
		return errors.New("approval: trust store is unavailable")
	}
	root, err := canonicalOrCleanProjectRoot(projectPath)
	if err != nil {
		return fmt.Errorf("approval: canonicalize project trust root: %w", err)
	}
	configPath := filepath.Join(root, ".go-cli", "mcp.json")
	var generation uint64
	err = m.update(ctx, func(entries map[string]TrustEntry) (bool, error) {
		maxGeneration := uint64(0)
		for key, existing := range entries {
			if trustEntryIdentifiesRoot(key, existing, root) {
				maxGeneration = maxTrustGeneration(maxGeneration, effectiveTrustGeneration(existing))
			}
		}
		if maxGeneration == ^uint64(0) {
			return false, errTrustGenerationOverflow
		}
		generation = maxGeneration + 1
		entries[root] = TrustEntry{
			Path:            root,
			CanonicalRoot:   root,
			Fingerprint:     contentFingerprint(configPath),
			TrustedAt:       time.Now().UTC().Format(time.RFC3339),
			TrustGeneration: generation,
		}
		return true, nil
	})
	if err != nil {
		return err
	}
	slog.Info("approval.trust_project", "path", root, "generation", generation)
	return nil
}

// RevokeTrust removes the project from the trusted set and logs the change.
func (m *DefaultTrustManager) RevokeTrust(ctx context.Context, projectPath string) error {
	if ctx == nil {
		return errors.New("approval: revoke context is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if m == nil || m.store == nil {
		return errors.New("approval: trust store is unavailable")
	}
	root, err := canonicalProjectRootForRevocation(projectPath)
	if err != nil {
		return fmt.Errorf("approval: canonicalize revoke root: %w", err)
	}
	var generation uint64
	err = m.update(ctx, func(entries map[string]TrustEntry) (bool, error) {
		maxGeneration := uint64(0)
		aliases := make([]string, 0)
		for key, entry := range entries {
			if !trustEntryIdentifiesRoot(key, entry, root) {
				continue
			}
			maxGeneration = maxTrustGeneration(maxGeneration, effectiveTrustGeneration(entry))
			aliases = append(aliases, key)
		}
		if maxGeneration == ^uint64(0) {
			return false, errTrustGenerationOverflow
		}
		generation = maxGeneration + 1
		for _, alias := range aliases {
			delete(entries, alias)
		}
		entries[root] = trustTombstone(root, generation)
		return true, nil
	})
	if err != nil {
		return err
	}
	slog.Info("approval.revoke_trust", "path", root, "generation", generation)
	return nil
}

// TrustedProjects returns the trusted paths sorted ascending.
func (m *DefaultTrustManager) TrustedProjects() []string {
	entries, err := m.store.Load()
	if err != nil {
		return nil
	}
	paths := make([]string, 0, len(entries))
	for path, entry := range entries {
		if !entry.Revoked && !expired(entry, time.Now()) {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	return paths
}

func (m *DefaultTrustManager) update(ctx context.Context, mutate func(map[string]TrustEntry) (bool, error)) error {
	store, ok := m.store.(TransactionalTrustStore)
	if !ok {
		return ErrTransactionalTrustStoreUnavailable
	}
	return store.Update(ctx, mutate)
}

func effectiveTrustGeneration(entry TrustEntry) uint64 {
	if entry.TrustGeneration > 0 {
		return entry.TrustGeneration
	}
	if entry.ManifestVersion > 0 && !entry.Revoked {
		return 1
	}
	return 0
}

func maxTrustGeneration(left, right uint64) uint64 {
	if right > left {
		return right
	}
	return left
}

func trustEntryIdentifiesRoot(key string, entry TrustEntry, root string) bool {
	if key == root || entry.Path == root || entry.CanonicalRoot == root {
		return true
	}
	for _, candidate := range []string{key, entry.Path, entry.CanonicalRoot} {
		if candidate == "" || !filepath.IsAbs(candidate) {
			continue
		}
		if canonical, err := canonicalProjectRoot(candidate); err == nil && canonical == root {
			return true
		}
	}
	return false
}

func canonicalOrCleanProjectRoot(root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", errors.New("empty root")
	}
	if canonical, err := canonicalProjectRoot(root); err == nil {
		return canonical, nil
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	return filepath.Clean(absolute), nil
}

func canonicalProjectRootForRevocation(root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", errors.New("empty root")
	}
	if canonical, err := canonicalProjectRoot(root); err == nil {
		return canonical, nil
	}
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return "", errors.New("deleted revoke root must be absolute and clean")
	}
	if info, err := os.Lstat(root); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("revoke root is an unresolved symlink")
		}
		if !info.IsDir() {
			return "", errors.New("revoke root is not a directory")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	missing := make([]string, 0)
	ancestor := root
	for {
		info, err := os.Lstat(ancestor)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return "", errors.New("revoke root ancestor is not canonical directory")
			}
			canonical, evalErr := filepath.EvalSymlinks(ancestor)
			if evalErr != nil || filepath.Clean(canonical) != ancestor {
				return "", errors.New("revoke root ancestor is not canonical")
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		base := filepath.Base(ancestor)
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return "", errors.New("revoke root has no existing canonical ancestor")
		}
		missing = append([]string{base}, missing...)
		ancestor = parent
	}
	canonicalRoot := filepath.Clean(ancestor)
	for _, component := range missing {
		canonicalRoot = filepath.Join(canonicalRoot, component)
	}
	return canonicalRoot, nil
}

// LoadTrustEntries returns a defensive copy of stored entries and propagates
// store errors so read-only trust listing cannot mistake corruption for emptiness.
func (m *DefaultTrustManager) LoadTrustEntries() (map[string]TrustEntry, error) {
	if m == nil || m.store == nil {
		return nil, errors.New("approval: trust store is unavailable")
	}
	entries, err := m.store.Load()
	if err != nil {
		return nil, fmt.Errorf("approval: load trust entries: %w", err)
	}
	copied := make(map[string]TrustEntry, len(entries))
	for path, entry := range entries {
		copied[path] = cloneTrustEntry(entry)
	}
	return copied, nil
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

// contentFingerprint returns a SHA-256 fingerprint of the content of the file
// at filePath. If the file cannot be read, it falls back to hashing the path
// string itself so a fingerprint is always produced.
func contentFingerprint(filePath string) string {
	data, err := os.ReadFile(filePath)
	if err != nil {
		sum := sha256.Sum256([]byte(filePath))
		return hex.EncodeToString(sum[:])
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ValidateGlobalConfigFile validates that a global config file (e.g.
// ~/.config/go-cli/mcp.json) is owned by the current user and has 0600
// permissions (owner read/write only). This prevents tampering via
// world-writable or foreign-owned config files. When ownership cannot be established on a platform, validation fails closed.
func ValidateGlobalConfigFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Mode().Perm() != 0o600 {
		return fmt.Errorf("global config %s has permissions %o, expected 0600", path, info.Mode().Perm())
	}
	return validateFileOwner(info, path)
}
