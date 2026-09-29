package production

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

var (
	// ErrAuditLogAppend identifies an incomplete audit-log append.
	ErrAuditLogAppend = errors.New("audit log: append failed")
	// ErrAuditLogSync identifies a failed durability sync for an audit append.
	ErrAuditLogSync = errors.New("audit log: sync failed")
	// ErrAuditLogRollback identifies a failed rollback after an incomplete append.
	ErrAuditLogRollback = errors.New("audit log: rollback failed")
	// ErrAuditLogCorrupt identifies a malformed or tampered audit hash chain.
	ErrAuditLogCorrupt = errors.New("audit log: chain corrupt")
	// ErrAuditLogClose identifies a failed audit file close.
	ErrAuditLogClose = errors.New("audit log: close failed")
)

const maxAuditLogLineBytes = 64 * 1024

type auditFile interface {
	io.Writer
	Sync() error
	Stat() (os.FileInfo, error)
	Truncate(size int64) error
	Close() error
}

type auditFileOpener func(path string, flag int, perm os.FileMode) (auditFile, error)

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
	mu          sync.RWMutex
	path        string
	name        string
	lastHash    string
	chainLoaded bool
	poisoned    error
	maxSize     int64
	openFile    auditFileOpener
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

func sanitizeProductionAuditEntry(entry AuditEntry) AuditEntry {
	entry.Operation = boundAuditLogText(entry.Operation, 256)
	entry.ToolName = boundAuditLogText(entry.ToolName, 128)
	entry.UserID = boundAuditLogText(entry.UserID, 128)
	entry.SessionID = boundAuditLogText(entry.SessionID, 128)
	entry.Args = redactedAuditMap(entry.Args)
	entry.Result = redactedAuditMap(entry.Result)
	return entry
}

func redactedAuditMap(values map[string]any) map[string]any {
	if values == nil {
		return nil
	}
	return map[string]any{"redacted": true, "field_count": len(values)}
}

func boundAuditLogText(value string, maxBytes int) string {
	value = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, value)
	if len(value) <= maxBytes {
		return value
	}
	value = value[:maxBytes]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

// NewDefaultAuditLog returns a DefaultAuditLog that appends JSON-lines to path.
func NewDefaultAuditLog(path string, opts ...AuditLogOption) AuditLog {
	a := &DefaultAuditLog{
		path:     path,
		name:     "default-audit-log",
		maxSize:  defaultAuditMaxSize,
		openFile: openAuditLogFile,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(a)
		}
	}
	return a
}

func openAuditLogFile(path string, flag int, perm os.FileMode) (auditFile, error) {
	return os.OpenFile(path, flag, perm)
}

// Log appends a JSON representation of entry to the log file, emitting an
// audit.log span and an info-level log. Each entry is chained to the previous
// one via a SHA-256 hash for tamper-evidence.
func (l *DefaultAuditLog) Log(ctx context.Context, entry AuditEntry) error {
	span, ctx := tracing.SpanFromContext(ctx, "audit.log", tracing.SpanKindInternal)
	defer span.End()

	if entry.Timestamp.IsZero() {
		entry.Timestamp = time.Now().UTC()
	} else {
		entry.Timestamp = entry.Timestamp.UTC()
	}
	entry = sanitizeProductionAuditEntry(entry)

	l.mu.Lock()
	if l.poisoned != nil {
		l.mu.Unlock()
		err := fmt.Errorf("%w: writer quarantined: %w", ErrAuditLogRollback, l.poisoned)
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		span.SetStatus(tracing.SpanStatusError, err.Error())
		return err
	}
	if err := l.loadChain(); err != nil {
		l.mu.Unlock()
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		span.SetStatus(tracing.SpanStatusError, err.Error())
		return err
	}
	// Build the next hash without changing in-memory chain state.
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
	data, err := json.Marshal(entry)
	if err != nil {
		l.mu.Unlock()
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		span.SetStatus(tracing.SpanStatusError, err.Error())
		return err
	}
	durable, rotated, appendErr := l.appendLine(data)
	if durable {
		l.lastHash = entry.Hash
		if rotated {
			l.lastHash = ""
		}
	}
	l.mu.Unlock()

	span.SetAttributes(
		tracing.Attribute{Key: "operation", Value: entry.Operation},
		tracing.Attribute{Key: "success", Value: appendErr == nil},
	)
	logger := tracing.NewTraceLogger(span, slog.Default())
	logger.InfoContext(ctx, "audit.log",
		"operation", entry.Operation,
		"tool", entry.ToolName,
		"path", l.path,
		"err", appendErr,
	)
	if appendErr != nil {
		span.SetStatus(tracing.SpanStatusError, appendErr.Error())
		return appendErr
	}
	span.SetStatus(tracing.SpanStatusOK, "")
	return nil
}

// appendLine writes a single JSON line to the log file, creating the file and
// parent directory if needed. After writing, it checks whether the file exceeds
// maxSize and rotates it (single generation: path → path+".1") if so. It must
// be called while l.mu is held.
func (l *DefaultAuditLog) appendLine(data []byte) (bool, bool, error) {
	if dir := filepath.Dir(l.path); dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return false, false, fmt.Errorf("audit log: mkdir: %w", err)
		}
	}
	f, err := l.openFile(l.path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return false, false, fmt.Errorf("audit log: open: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		return false, false, errors.Join(fmt.Errorf("audit log: stat: %w", err), wrapAuditLogClose(f.Close()))
	}
	line := append(data, '\n')
	written, writeErr := f.Write(line)
	if writeErr == nil && written != len(line) {
		writeErr = io.ErrShortWrite
	}
	if writeErr != nil {
		return l.rollbackAppend(f, info.Size(), ErrAuditLogAppend, writeErr)
	}
	if err := f.Sync(); err != nil {
		return l.rollbackAppend(f, info.Size(), ErrAuditLogSync, err)
	}

	// The line is durable after Sync succeeds, even if a later close or rotate fails.
	info, statErr := f.Stat()
	if statErr != nil {
		return true, false, errors.Join(fmt.Errorf("audit log: stat after append: %w", statErr), wrapAuditLogClose(f.Close()))
	}
	// Rotate when the file exceeds maxSize.
	if info.Size() > l.maxSize {
		if cerr := f.Close(); cerr != nil {
			return true, false, fmt.Errorf("%w: %w", ErrAuditLogClose, cerr)
		}
		if rerr := os.Rename(l.path, l.path+".1"); rerr != nil {
			return true, false, fmt.Errorf("audit log: rotate: %w", rerr)
		}
		return true, true, nil
	}
	if err := f.Close(); err != nil {
		return true, false, fmt.Errorf("%w: %w", ErrAuditLogClose, err)
	}
	return true, false, nil
}

func (l *DefaultAuditLog) rollbackAppend(f auditFile, originalSize int64, sentinel, cause error) (bool, bool, error) {
	rollbackErr := f.Truncate(originalSize)
	if rollbackErr == nil {
		rollbackErr = f.Sync()
	}
	closeErr := f.Close()
	appendErr := fmt.Errorf("%w: %w", sentinel, cause)
	if rollbackErr != nil {
		l.poisoned = rollbackErr
		return false, false, errors.Join(appendErr, fmt.Errorf("%w: %w", ErrAuditLogRollback, rollbackErr), wrapAuditLogClose(closeErr))
	}
	return false, false, errors.Join(appendErr, wrapAuditLogClose(closeErr))
}

func wrapAuditLogClose(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrAuditLogClose, err)
}

func (l *DefaultAuditLog) loadChain() error {
	if l.chainLoaded {
		return nil
	}
	lastHash, _, err := walkAuditLog(l.path, nil)
	if err != nil {
		return err
	}
	l.lastHash = lastHash
	l.chainLoaded = true
	return nil
}

// VerifyChain reads the log file and verifies the SHA-256 hash chain: each
// entry's Hash must equal sha256(PrevHash + payload-without-Hash) and each
// PrevHash must equal the previous entry's Hash. It returns an error
// identifying the offending line number on mismatch.
func (l *DefaultAuditLog) VerifyChain() error {
	l.mu.RLock()
	defer l.mu.RUnlock()
	_, _, err := walkAuditLog(l.path, nil)
	return err
}

// Query streams the log file line by line and returns entries that match
// filter, oldest first. Only matching entries are held in memory, avoiding
// loading the entire file for large logs.
func (l *DefaultAuditLog) Query(ctx context.Context, filter AuditFilter) ([]AuditEntry, error) {
	span, _ := tracing.SpanFromContext(ctx, "audit.query", tracing.SpanKindInternal)
	defer span.End()

	l.mu.RLock()
	defer l.mu.RUnlock()

	var filtered []AuditEntry
	_, _, err := walkAuditLog(l.path, func(entry AuditEntry) error {
		if filter.Matches(entry) {
			filtered = append(filtered, entry)
		}
		return nil
	})
	if err != nil {
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

func walkAuditLog(path string, visit func(AuditEntry) error) (string, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("audit log: open for read: %w", err)
	}
	reader := bufio.NewReaderSize(f, maxAuditLogLineBytes+1)
	expectedPrev := ""
	lineNum := 0
	for {
		line, readErr := reader.ReadSlice('\n')
		if errors.Is(readErr, bufio.ErrBufferFull) {
			closeErr := f.Close()
			return "", false, errors.Join(fmt.Errorf("%w: line %d exceeds %d bytes", ErrAuditLogCorrupt, lineNum+1, maxAuditLogLineBytes), wrapAuditLogClose(closeErr))
		}
		if errors.Is(readErr, io.EOF) {
			if len(line) != 0 {
				closeErr := f.Close()
				return "", false, errors.Join(fmt.Errorf("%w: unterminated line %d", ErrAuditLogCorrupt, lineNum+1), wrapAuditLogClose(closeErr))
			}
			break
		}
		if readErr != nil {
			closeErr := f.Close()
			return "", false, errors.Join(fmt.Errorf("audit log: read line: %w", readErr), wrapAuditLogClose(closeErr))
		}
		lineNum++
		if len(line) > maxAuditLogLineBytes {
			closeErr := f.Close()
			return "", false, errors.Join(fmt.Errorf("%w: line %d exceeds %d bytes", ErrAuditLogCorrupt, lineNum, maxAuditLogLineBytes), wrapAuditLogClose(closeErr))
		}
		line = line[:len(line)-1]
		if len(line) == 0 {
			continue
		}
		var entry AuditEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			closeErr := f.Close()
			return "", false, errors.Join(fmt.Errorf("%w: line %d: %w", ErrAuditLogCorrupt, lineNum, err), wrapAuditLogClose(closeErr))
		}
		if entry.PrevHash != expectedPrev {
			closeErr := f.Close()
			return "", false, errors.Join(fmt.Errorf("%w: line %d previous hash mismatch", ErrAuditLogCorrupt, lineNum), wrapAuditLogClose(closeErr))
		}
		savedHash := entry.Hash
		entry.Hash = ""
		payload, err := json.Marshal(entry)
		if err != nil {
			closeErr := f.Close()
			return "", false, errors.Join(fmt.Errorf("%w: line %d: %w", ErrAuditLogCorrupt, lineNum, err), wrapAuditLogClose(closeErr))
		}
		h := sha256.Sum256([]byte(entry.PrevHash + string(payload)))
		if savedHash == "" || hex.EncodeToString(h[:]) != savedHash {
			closeErr := f.Close()
			return "", false, errors.Join(fmt.Errorf("%w: line %d hash mismatch", ErrAuditLogCorrupt, lineNum), wrapAuditLogClose(closeErr))
		}
		entry.Hash = savedHash
		if visit != nil {
			if err := visit(entry); err != nil {
				closeErr := f.Close()
				return "", false, errors.Join(err, wrapAuditLogClose(closeErr))
			}
		}
		expectedPrev = savedHash
	}
	if err := f.Close(); err != nil {
		return "", false, fmt.Errorf("%w: %w", ErrAuditLogClose, err)
	}
	return expectedPrev, true, nil
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
