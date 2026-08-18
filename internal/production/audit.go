package production

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// AuditEntry is a single audit record captured by an AuditLog.
type AuditEntry struct {
	// Timestamp is when the audited operation occurred.
	Timestamp time.Time
	// Operation names the audited operation (e.g. "config.set", "tool.run").
	Operation string
	// ToolName is the tool that performed the operation, if any.
	ToolName string
	// Args holds the operation arguments.
	Args map[string]any
	// Result holds the operation result.
	Result map[string]any
	// UserID identifies the acting user, if known.
	UserID string
	// SessionID identifies the acting session, if known.
	SessionID string
	// PrevHash is the hash of the previous entry, forming a tamper-evident
	// hash chain.
	PrevHash string `json:"prev_hash,omitempty"`
	// Hash is the SHA-256 hash of this entry, computed over PrevHash and the
	// JSON payload (excluding the Hash field itself).
	Hash string `json:"hash,omitempty"`
}

// AuditFilter narrows an AuditLog.Query to a subset of entries.
type AuditFilter struct {
	// From, when set, includes only entries at or after this time.
	From time.Time
	// To, when set, includes only entries at or before this time.
	To time.Time
	// Operation, when non-empty, matches at least one of the comma-separated
	// operation names.
	Operation string
	// ToolName, when non-empty, matches entries whose ToolName equals it.
	ToolName string
}

// AuditLog records and queries time-stamped, immutable audit entries.
type AuditLog interface {
	// Log appends an entry to the log.
	Log(ctx context.Context, entry AuditEntry) error
	// Query reads entries that match filter, ordered oldest first.
	Query(ctx context.Context, filter AuditFilter) ([]AuditEntry, error)
	// Name returns the log identifier.
	Name() string
}

// DefaultAuditLog is a JSONL file-backed AuditLog. Each entry is appended as a
// single JSON line; Query re-reads the file and filters in memory.
type DefaultAuditLog struct {
	mu       sync.RWMutex
	path     string
	name     string
	lastHash string
	maxSize  int64
}

// Compile-time assertion that DefaultAuditLog satisfies AuditLog.
var _ AuditLog = (*DefaultAuditLog)(nil)

// AuditLogOption configures a DefaultAuditLog. It keeps the audit constructor
// signature focused on the persistence path while mirroring the Option style
// used by the other production components.
type AuditLogOption func(*DefaultAuditLog)

// defaultAuditMaxSize is the default rotation threshold (100 MB).
const defaultAuditMaxSize = 100 * 1024 * 1024 // 100MB

// WithAuditName overrides the identifier returned by Name.
func WithAuditName(name string) AuditLogOption {
	return func(a *DefaultAuditLog) { a.name = name }
}

// WithMaxSize sets the rotation threshold in bytes. When the log file exceeds
// this size after an append, it is rotated to path+".1" and a new hash chain
// starts in the fresh file.
func WithMaxSize(size int64) AuditLogOption {
	return func(a *DefaultAuditLog) { a.maxSize = size }
}

// NewDefaultAuditLog returns a DefaultAuditLog that appends JSON-lines to path.
func NewDefaultAuditLog(path string, opts ...AuditLogOption) AuditLog {
	a := &DefaultAuditLog{
		path:    path,
		name:    "default-audit-log",
		maxSize: defaultAuditMaxSize,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(a)
		}
	}
	return a
}

// Log appends a JSON representation of entry to the log file, emitting an
// audit.log span and an info-level log. Each entry is chained to the previous
// one via a SHA-256 hash for tamper-evidence.
func (l *DefaultAuditLog) Log(ctx context.Context, entry AuditEntry) error {
	span, ctx := tracing.SpanFromContext(ctx, "audit.log", tracing.SpanKindInternal)
	defer span.End()

	if entry.Timestamp.IsZero() {
		entry.Timestamp = time.Now()
	}

	l.mu.Lock()
	// Build hash chain: link to previous entry, then compute this entry's hash
	// over PrevHash + JSON payload (excluding the Hash field itself).
	entry.PrevHash = l.lastHash
	payload, err := json.Marshal(entry)
	if err != nil {
		l.mu.Unlock()
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		span.SetStatus(tracing.SpanStatusError, err.Error())
		return err
	}
	h := sha256.Sum256([]byte(entry.PrevHash + string(payload)))
	entry.Hash = hex.EncodeToString(h[:])
	l.lastHash = entry.Hash
	data, err := json.Marshal(entry)
	if err != nil {
		l.mu.Unlock()
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		span.SetStatus(tracing.SpanStatusError, err.Error())
		return err
	}
	err = l.appendLine(data)
	l.mu.Unlock()

	span.SetAttributes(
		tracing.Attribute{Key: "operation", Value: entry.Operation},
		tracing.Attribute{Key: "success", Value: err == nil},
	)
	logger := tracing.NewTraceLogger(span, slog.Default())
	logger.InfoContext(ctx, "audit.log",
		"operation", entry.Operation,
		"tool", entry.ToolName,
		"path", l.path,
		"err", err,
	)
	if err != nil {
		span.SetStatus(tracing.SpanStatusError, err.Error())
		return err
	}
	span.SetStatus(tracing.SpanStatusOK, "")
	return nil
}

// appendLine writes a single JSON line to the log file, creating the file and
// parent directory if needed. After writing, it checks whether the file exceeds
// maxSize and rotates it (single generation: path → path+".1") if so. It must
// be called while l.mu is held.
func (l *DefaultAuditLog) appendLine(data []byte) error {
	if dir := filepath.Dir(l.path); dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	line := append(data, '\n')
	if _, err := f.Write(line); err != nil {
		_ = f.Close()
		return err
	}
	// Rotate when the file exceeds maxSize.
	if info, statErr := f.Stat(); statErr == nil && info.Size() > l.maxSize {
		if cerr := f.Close(); cerr != nil {
			return cerr
		}
		if rerr := os.Rename(l.path, l.path+".1"); rerr != nil {
			return rerr
		}
		l.lastHash = "" // new chain starts in the fresh file
		return nil
	}
	return f.Close()
}

// VerifyChain reads the log file and verifies the SHA-256 hash chain: each
// entry's Hash must equal sha256(PrevHash + payload-without-Hash) and each
// PrevHash must equal the previous entry's Hash. It returns an error
// identifying the offending line number on mismatch.
func (l *DefaultAuditLog) VerifyChain() error {
	l.mu.RLock()
	defer l.mu.RUnlock()

	f, err := os.Open(l.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer func() { _ = f.Close() }() //nolint:errcheck // best-effort close during scan.

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	lineNum := 0
	expectedPrev := ""
	for scanner.Scan() {
		lineNum++
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var entry AuditEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			return fmt.Errorf("audit chain: line %d: %w", lineNum, err)
		}
		if entry.PrevHash != expectedPrev {
			return fmt.Errorf("audit chain: line %d: prev_hash mismatch", lineNum)
		}
		savedHash := entry.Hash
		entry.Hash = ""
		payload, err := json.Marshal(entry)
		if err != nil {
			return fmt.Errorf("audit chain: line %d: %w", lineNum, err)
		}
		h := sha256.Sum256([]byte(entry.PrevHash + string(payload)))
		if hex.EncodeToString(h[:]) != savedHash {
			return fmt.Errorf("audit chain: line %d: hash mismatch", lineNum)
		}
		expectedPrev = savedHash
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("audit chain: %w", err)
	}
	return nil
}

// Query streams the log file line by line and returns entries that match
// filter, oldest first. Only matching entries are held in memory, avoiding
// loading the entire file for large logs.
func (l *DefaultAuditLog) Query(ctx context.Context, filter AuditFilter) ([]AuditEntry, error) {
	span, _ := tracing.SpanFromContext(ctx, "audit.query", tracing.SpanKindInternal)
	defer span.End()

	l.mu.RLock()
	defer l.mu.RUnlock()

	f, err := os.Open(l.path)
	if err != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		span.SetStatus(tracing.SpanStatusError, err.Error())
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = f.Close() }() //nolint:errcheck // best-effort close during scan.

	var filtered []AuditEntry
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var entry AuditEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
			span.SetStatus(tracing.SpanStatusError, err.Error())
			return nil, err
		}
		if filter.Matches(entry) {
			filtered = append(filtered, entry)
		}
	}
	if err := scanner.Err(); err != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		span.SetStatus(tracing.SpanStatusError, err.Error())
		return nil, err
	}

	span.SetAttributes(
		tracing.Attribute{Key: "matched", Value: len(filtered)},
		tracing.Attribute{Key: "success", Value: true},
	)
	span.SetStatus(tracing.SpanStatusOK, "")
	return filtered, nil
}

// Name returns the log identifier.
func (l *DefaultAuditLog) Name() string { return l.name }

// Matches reports whether entry satisfies every set field of filter.
func (f AuditFilter) Matches(entry AuditEntry) bool {
	if !f.From.IsZero() && entry.Timestamp.Before(f.From) {
		return false
	}
	if !f.To.IsZero() && entry.Timestamp.After(f.To) {
		return false
	}
	if f.Operation != "" && f.Operation != entry.Operation {
		return false
	}
	if f.ToolName != "" && f.ToolName != entry.ToolName {
		return false
	}
	return true
}
