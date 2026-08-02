package production

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
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
	mu   sync.RWMutex
	path string
	name string
}

// Compile-time assertion that DefaultAuditLog satisfies AuditLog.
var _ AuditLog = (*DefaultAuditLog)(nil)

// AuditLogOption configures a DefaultAuditLog. It keeps the audit constructor
// signature focused on the persistence path while mirroring the Option style
// used by the other production components.
type AuditLogOption func(*auditLogOptions)

type auditLogOptions struct {
	name string
}

// WithAuditName overrides the identifier returned by Name.
func WithAuditName(name string) AuditLogOption {
	return func(o *auditLogOptions) { o.name = name }
}

// NewDefaultAuditLog returns a DefaultAuditLog that appends JSON-lines to path.
func NewDefaultAuditLog(path string, opts ...AuditLogOption) AuditLog {
	o := &auditLogOptions{}
	for _, opt := range opts {
		if opt != nil {
			opt(o)
		}
	}
	name := o.name
	if name == "" {
		name = "default-audit-log"
	}
	return &DefaultAuditLog{path: path, name: name}
}

// Log appends a JSON representation of entry to the log file, emitting an
// audit.log span and an info-level log.
func (l *DefaultAuditLog) Log(ctx context.Context, entry AuditEntry) error {
	span, ctx := tracing.SpanFromContext(ctx, "audit.log", tracing.SpanKindInternal)
	defer span.End()

	if entry.Timestamp.IsZero() {
		entry.Timestamp = time.Now()
	}

	data, err := json.Marshal(entry)
	if err != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		span.SetStatus(tracing.SpanStatusError, err.Error())
		return err
	}

	l.mu.Lock()
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
// parent directory if needed. It must be called while l.mu is held.
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
	defer func() { _ = f.Close() }() //nolint:errcheck // best-effort close after append.
	line := append(data, '\n')
	_, err = f.Write(line)
	return err
}

// Query reads the log file and returns entries that match filter, oldest first.
func (l *DefaultAuditLog) Query(ctx context.Context, filter AuditFilter) ([]AuditEntry, error) {
	span, _ := tracing.SpanFromContext(ctx, "audit.query", tracing.SpanKindInternal)
	defer span.End()

	l.mu.RLock()
	entries, err := l.readAll()
	l.mu.RUnlock()

	if err != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		span.SetStatus(tracing.SpanStatusError, err.Error())
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	filtered := make([]AuditEntry, 0, len(entries))
	for _, e := range entries {
		if filter.Matches(e) {
			filtered = append(filtered, e)
		}
	}

	span.SetAttributes(
		tracing.Attribute{Key: "matched", Value: len(filtered)},
		tracing.Attribute{Key: "success", Value: true},
	)
	span.SetStatus(tracing.SpanStatusOK, "")
	return filtered, nil
}

// readAll reads and decodes every JSON line. It must be called while l.mu is held.
func (l *DefaultAuditLog) readAll() ([]AuditEntry, error) {
	f, err := os.Open(l.path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }() //nolint:errcheck // best-effort close during scan.

	var entries []AuditEntry
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var entry AuditEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return entries, nil
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
