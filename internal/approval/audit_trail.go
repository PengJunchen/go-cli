package approval

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

	"github.com/pengjunchen/go-cli/internal/privatefile"
)

var (
	// ErrAuditTrailAppend identifies an incomplete approval audit append.
	ErrAuditTrailAppend = errors.New("approval audit: append failed")
	// ErrAuditTrailSync identifies a failed durability sync for an audit append.
	ErrAuditTrailSync = errors.New("approval audit: sync failed")
	// ErrAuditTrailRollback identifies a failed rollback after an incomplete append.
	ErrAuditTrailRollback = errors.New("approval audit: rollback failed")
	// ErrAuditTrailCorrupt identifies a malformed or tampered audit chain.
	ErrAuditTrailCorrupt = errors.New("approval audit: chain corrupt")
	// ErrAuditTrailClose identifies a failed file close after an audit append.
	ErrAuditTrailClose = errors.New("approval audit: close failed")
)

// AuditTrailErrorClass is a bounded, non-sensitive category for audit health.
// It never contains a path, operating-system message, or event payload.
type AuditTrailErrorClass string

const (
	AuditTrailErrorClassLockUnavailable AuditTrailErrorClass = "lock_unavailable"
	AuditTrailErrorClassAppendFailed    AuditTrailErrorClass = "append_failed"
	AuditTrailErrorClassSyncFailed      AuditTrailErrorClass = "sync_failed"
	AuditTrailErrorClassRollbackFailed  AuditTrailErrorClass = "rollback_failed"
	AuditTrailErrorClassCloseFailed     AuditTrailErrorClass = "close_failed"
	AuditTrailErrorClassChainCorrupt    AuditTrailErrorClass = "chain_corrupt"
	AuditTrailErrorClassStorageFailed   AuditTrailErrorClass = "storage_failed"
)

// AuditTrailHealth reports whether audit writes have failed in this process.
// ErrorClass is sticky after the first failure and is safe to expose to callers.
type AuditTrailHealth struct {
	Enabled    bool                 `json:"enabled"`
	Healthy    bool                 `json:"healthy"`
	ErrorClass AuditTrailErrorClass `json:"error_class,omitempty"`
}

const maxAuditTrailLineBytes = 64 * 1024
const defaultAuditTrailLockTimeout = 5 * time.Second

// AuditEntry records one approval audit event. Decision is the classifier's
// proposal; UserChoice and ToolOutcome are separate fields for later stages.
// Argument values are never persisted by the default trail.
type AuditEntry struct {
	// Timestamp is when the event occurred (UTC).
	Timestamp time.Time `json:"timestamp"`
	// Tool is the name of the tool associated with the event.
	Tool string `json:"tool"`
	// ArgsSummary is a redacted marker when the event included arguments.
	ArgsSummary string `json:"args_summary,omitempty"`
	// Decision is the classifier proposal, such as "allow", "deny", or "ask".
	Decision string `json:"decision,omitempty"`
	// Classifier names the component that proposed the decision.
	Classifier string `json:"classifier,omitempty"`
	// PermissionMode is the active permission mode when the proposal was made.
	PermissionMode string `json:"permission_mode,omitempty"`
	// SessionID identifies the session associated with the event, if known.
	SessionID string `json:"session_id,omitempty"`
	// Stage names the audit lifecycle stage represented by this record.
	Stage string `json:"stage,omitempty"`
	// UserChoice records an actual approval response independently of Decision.
	UserChoice string `json:"user_choice,omitempty"`
	// Resolution identifies how the approval decision was resolved.
	Resolution string `json:"resolution,omitempty"`
	// ToolOutcome records the result of tool execution independently of approval.
	ToolOutcome string `json:"tool_outcome,omitempty"`
	// ToolCallID is a stable fingerprint of the tool call identifier.
	ToolCallID string `json:"tool_call_id,omitempty"`
	// DispatchCommitted records whether execution crossed its commit boundary.
	DispatchCommitted bool `json:"dispatch_committed,omitempty"`
	// ResultPresent records whether dispatch returned a tool result.
	ResultPresent bool `json:"result_present,omitempty"`
	// ResultIsError records the structured error bit on the tool result.
	ResultIsError bool `json:"result_is_error,omitempty"`
	// PrevHash links this entry to the preceding durable entry for the day.
	PrevHash string `json:"prev_hash,omitempty"`
	// Hash is the SHA-256 digest of this entry and PrevHash.
	Hash string `json:"hash,omitempty"`
}

type auditTrailFile interface {
	io.Writer
	Sync() error
	Stat() (os.FileInfo, error)
	Truncate(size int64) error
	Close() error
}

type auditTrailOpenFileFunc func(path string, flag int, perm os.FileMode) (auditTrailFile, error)

// AuditTrail is a durable append-only approval audit log. Each record is
// written to {dir}/{date}.jsonl and linked to the preceding record by hash.
type AuditTrail struct {
	dir         string
	lockTimeout time.Duration
	mu          sync.Mutex
	lastHash    map[string]string
	cursors     map[string]auditTrailCursor
	poisoned    map[string]error
	healthClass AuditTrailErrorClass
	openFile    auditTrailOpenFileFunc
}

type auditTrailCursor struct {
	fingerprint privatefile.FileFingerprint
	valid       bool
}

// NewAuditTrail creates an AuditTrail that writes daily JSONL files under dir.
// The directory is created lazily on the first Record call.
func NewAuditTrail(dir string) *AuditTrail {
	return &AuditTrail{
		dir:         dir,
		lockTimeout: defaultAuditTrailLockTimeout,
		lastHash:    make(map[string]string),
		cursors:     make(map[string]auditTrailCursor),
		poisoned:    make(map[string]error),
		openFile:    openAuditTrailFile,
	}
}

func openAuditTrailFile(path string, flag int, perm os.FileMode) (auditTrailFile, error) {
	return os.OpenFile(path, flag, perm)
}

// Health returns a process-local, redacted view of audit append health. Once
// an append has failed, successful later records do not clear the missing-event
// signal; a new AuditTrail instance starts with healthy process-local state.
func (a *AuditTrail) Health() AuditTrailHealth {
	if a == nil {
		return AuditTrailHealth{Healthy: true}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return AuditTrailHealth{
		Enabled:    true,
		Healthy:    a.healthClass == "",
		ErrorClass: a.healthClass,
	}
}

func (a *AuditTrail) markUnhealthyLocked(class AuditTrailErrorClass) {
	if a.healthClass == "" {
		a.healthClass = class
	}
}

func classifyAuditTrailError(err error, lockCallbackEntered bool) AuditTrailErrorClass {
	if err == nil {
		return ""
	}
	if !lockCallbackEntered {
		return AuditTrailErrorClassLockUnavailable
	}
	switch {
	case errors.Is(err, ErrAuditTrailRollback):
		return AuditTrailErrorClassRollbackFailed
	case errors.Is(err, ErrAuditTrailCorrupt):
		return AuditTrailErrorClassChainCorrupt
	case errors.Is(err, ErrAuditTrailSync):
		return AuditTrailErrorClassSyncFailed
	case errors.Is(err, ErrAuditTrailAppend):
		return AuditTrailErrorClassAppendFailed
	case errors.Is(err, ErrAuditTrailClose):
		return AuditTrailErrorClassCloseFailed
	default:
		return AuditTrailErrorClassStorageFailed
	}
}

// Record durably appends an audit entry. The chain advances only after a
// complete line write and successful Sync. A nil receiver disables auditing.
func (a *AuditTrail) Record(entry AuditEntry) error {
	if a == nil {
		return nil
	}
	if entry.Timestamp.IsZero() {
		entry.Timestamp = time.Now().UTC()
	} else {
		entry.Timestamp = entry.Timestamp.UTC()
	}
	entry = sanitizeAuditEntry(entry)

	a.mu.Lock()
	defer a.mu.Unlock()

	filename := filepath.Join(a.dir, entry.Timestamp.Format("2006-01-02")+".jsonl")
	if poison := a.poisoned[filename]; poison != nil {
		a.markUnhealthyLocked(AuditTrailErrorClassRollbackFailed)
		return fmt.Errorf("%w: writer quarantined: %w", ErrAuditTrailRollback, poison)
	}
	lockCtx, cancel := context.WithTimeout(context.Background(), a.auditLockTimeout())
	defer cancel()
	lockCallbackEntered := false
	err := privatefile.WithAuditExclusiveLock(lockCtx, filename, func() error {
		lockCallbackEntered = true
		if err := a.refreshChain(filename); err != nil {
			return err
		}

		entry.PrevHash = a.lastHash[filename]
		hash, err := approvalAuditHash(entry)
		if err != nil {
			return fmt.Errorf("approval audit: hash: %w", err)
		}
		entry.Hash = hash
		data, err := json.Marshal(entry)
		if err != nil {
			return fmt.Errorf("approval audit: marshal: %w", err)
		}
		line := append(data, '\n')
		f, err := a.openFile(filename, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("approval audit: open: %w", err)
		}
		info, err := f.Stat()
		if err != nil {
			return errors.Join(fmt.Errorf("approval audit: stat: %w", err), wrapAuditTrailClose(f.Close()))
		}

		written, writeErr := f.Write(line)
		if writeErr == nil && written != len(line) {
			writeErr = io.ErrShortWrite
		}
		if writeErr != nil {
			return a.rollback(filename, f, info.Size(), ErrAuditTrailAppend, writeErr)
		}
		if err := f.Sync(); err != nil {
			return a.rollback(filename, f, info.Size(), ErrAuditTrailSync, err)
		}

		// Sync is the durability boundary: state advances only after it succeeds.
		a.lastHash[filename] = entry.Hash
		if err := f.Close(); err != nil {
			delete(a.cursors, filename)
			return fmt.Errorf("%w: %w", ErrAuditTrailClose, err)
		}
		a.captureCursor(filename)
		return nil
	})
	if err != nil {
		a.markUnhealthyLocked(classifyAuditTrailError(err, lockCallbackEntered))
		return fmt.Errorf("approval audit: locked append: %w", err)
	}
	slog.Info("approval.audit.appended", "stage", entry.Stage, "tool", entry.Tool, "hash", entry.Hash)
	return nil
}

// ReadEntries reads and verifies every record for the UTC day containing day.
func (a *AuditTrail) ReadEntries(day time.Time) ([]AuditEntry, error) {
	if a == nil {
		return nil, nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	filename := filepath.Join(a.dir, day.UTC().Format("2006-01-02")+".jsonl")
	var entries []AuditEntry
	var lastHash string
	var fingerprint privatefile.FileFingerprint
	var fingerprintValid bool
	lockCtx, cancel := context.WithTimeout(context.Background(), a.auditLockTimeout())
	defer cancel()
	lockCallbackEntered := false
	err := privatefile.WithAuditExclusiveLock(lockCtx, filename, func() error {
		lockCallbackEntered = true
		before, beforeErr := privatefile.FingerprintFile(filename)
		var readErr error
		entries, lastHash, readErr = readApprovalAuditFile(filename)
		if readErr != nil {
			return readErr
		}
		after, afterErr := privatefile.FingerprintFile(filename)
		if beforeErr == nil && afterErr == nil && before != after {
			return fmt.Errorf("%w: file changed while verifying chain", ErrAuditTrailCorrupt)
		}
		if beforeErr == nil && afterErr == nil {
			fingerprint = after
			fingerprintValid = true
		}
		return nil
	})
	if err != nil {
		a.markUnhealthyLocked(classifyAuditTrailError(err, lockCallbackEntered))
		return nil, fmt.Errorf("approval audit: locked read: %w", err)
	}
	a.lastHash[filename] = lastHash
	if fingerprintValid {
		a.cursors[filename] = auditTrailCursor{fingerprint: fingerprint, valid: true}
	} else {
		delete(a.cursors, filename)
	}
	return entries, nil
}

func (a *AuditTrail) auditLockTimeout() time.Duration {
	if a.lockTimeout <= 0 {
		return defaultAuditTrailLockTimeout
	}
	return a.lockTimeout
}

func (a *AuditTrail) refreshChain(filename string) error {
	before, beforeErr := privatefile.FingerprintFile(filename)
	if beforeErr == nil {
		if cursor := a.cursors[filename]; cursor.valid && cursor.fingerprint == before {
			return nil
		}
	}

	_, lastHash, err := readApprovalAuditFile(filename)
	if err != nil {
		return err
	}
	after, afterErr := privatefile.FingerprintFile(filename)
	if beforeErr == nil && afterErr == nil && before != after {
		return fmt.Errorf("%w: file changed while verifying chain", ErrAuditTrailCorrupt)
	}
	a.lastHash[filename] = lastHash
	if beforeErr == nil && afterErr == nil {
		a.cursors[filename] = auditTrailCursor{fingerprint: after, valid: true}
	} else {
		delete(a.cursors, filename)
	}
	return nil
}

func (a *AuditTrail) captureCursor(filename string) {
	fingerprint, err := privatefile.FingerprintFile(filename)
	if err != nil {
		delete(a.cursors, filename)
		return
	}
	a.cursors[filename] = auditTrailCursor{fingerprint: fingerprint, valid: true}
}

func (a *AuditTrail) rollback(filename string, f auditTrailFile, originalSize int64, sentinel, cause error) error {
	delete(a.cursors, filename)
	rollbackErr := f.Truncate(originalSize)
	if rollbackErr == nil {
		rollbackErr = f.Sync()
	}
	closeErr := f.Close()
	appendErr := fmt.Errorf("%w: %w", sentinel, cause)
	if rollbackErr != nil {
		a.poisoned[filename] = rollbackErr
		return errors.Join(appendErr, fmt.Errorf("%w: %w", ErrAuditTrailRollback, rollbackErr), wrapAuditTrailClose(closeErr))
	}
	return errors.Join(appendErr, wrapAuditTrailClose(closeErr))
}

func wrapAuditTrailClose(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrAuditTrailClose, err)
}

func readApprovalAuditFile(filename string) ([]AuditEntry, string, error) {
	f, err := os.Open(filename)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, "", nil
		}
		return nil, "", fmt.Errorf("approval audit: read: %w", err)
	}

	reader := bufio.NewReaderSize(f, maxAuditTrailLineBytes+1)
	entries := make([]AuditEntry, 0)
	expectedPrev := ""
	lineNum := 0
	for {
		line, readErr := reader.ReadSlice('\n')
		if errors.Is(readErr, bufio.ErrBufferFull) {
			closeErr := f.Close()
			return nil, "", errors.Join(fmt.Errorf("%w: line %d exceeds %d bytes", ErrAuditTrailCorrupt, lineNum+1, maxAuditTrailLineBytes), wrapAuditTrailClose(closeErr))
		}
		if errors.Is(readErr, io.EOF) {
			if len(line) != 0 {
				closeErr := f.Close()
				return nil, "", errors.Join(fmt.Errorf("%w: unterminated line %d", ErrAuditTrailCorrupt, lineNum+1), wrapAuditTrailClose(closeErr))
			}
			break
		}
		if readErr != nil {
			closeErr := f.Close()
			return nil, "", errors.Join(fmt.Errorf("approval audit: read line: %w", readErr), wrapAuditTrailClose(closeErr))
		}
		lineNum++
		if len(line) > maxAuditTrailLineBytes {
			closeErr := f.Close()
			return nil, "", errors.Join(fmt.Errorf("%w: line %d exceeds %d bytes", ErrAuditTrailCorrupt, lineNum, maxAuditTrailLineBytes), wrapAuditTrailClose(closeErr))
		}
		line = line[:len(line)-1]
		if len(line) == 0 {
			continue
		}
		var entry AuditEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			closeErr := f.Close()
			return nil, "", errors.Join(fmt.Errorf("%w: line %d: %w", ErrAuditTrailCorrupt, lineNum, err), wrapAuditTrailClose(closeErr))
		}
		if entry.PrevHash != expectedPrev {
			closeErr := f.Close()
			return nil, "", errors.Join(fmt.Errorf("%w: line %d previous hash mismatch", ErrAuditTrailCorrupt, lineNum), wrapAuditTrailClose(closeErr))
		}
		savedHash := entry.Hash
		computedHash, err := approvalAuditHash(entry)
		if err != nil {
			closeErr := f.Close()
			return nil, "", errors.Join(fmt.Errorf("%w: line %d: %w", ErrAuditTrailCorrupt, lineNum, err), wrapAuditTrailClose(closeErr))
		}
		if savedHash == "" || computedHash != savedHash {
			closeErr := f.Close()
			return nil, "", errors.Join(fmt.Errorf("%w: line %d hash mismatch", ErrAuditTrailCorrupt, lineNum), wrapAuditTrailClose(closeErr))
		}
		entry.Hash = savedHash
		entries = append(entries, entry)
		expectedPrev = savedHash
	}
	if err := f.Close(); err != nil {
		return nil, "", fmt.Errorf("%w: %w", ErrAuditTrailClose, err)
	}
	return entries, expectedPrev, nil
}

func approvalAuditHash(entry AuditEntry) (string, error) {
	entry.Hash = ""
	payload, err := json.Marshal(entry)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(entry.PrevHash + string(payload)))
	return hex.EncodeToString(sum[:]), nil
}

func sanitizeAuditEntry(entry AuditEntry) AuditEntry {
	entry.Tool = boundAuditText(entry.Tool, 128)
	if strings.TrimSpace(entry.ArgsSummary) != "" {
		entry.ArgsSummary = "redacted"
	}
	entry.Decision = boundAuditText(entry.Decision, 32)
	entry.Classifier = boundAuditText(entry.Classifier, 128)
	entry.PermissionMode = boundAuditText(entry.PermissionMode, 64)
	entry.SessionID = boundAuditText(entry.SessionID, 128)
	entry.Stage = boundAuditText(entry.Stage, 32)
	entry.UserChoice = boundAuditText(entry.UserChoice, 32)
	entry.Resolution = boundAuditText(entry.Resolution, 32)
	entry.ToolOutcome = boundAuditText(entry.ToolOutcome, 32)
	entry.ToolCallID = safeAuditToolCallID(entry.ToolCallID)
	if entry.Stage == "tool_outcome" {
		entry.ToolOutcome = safeAuditToolOutcome(entry.ToolOutcome)
	}
	return entry
}

func safeAuditToolCallID(value string) string {
	if value == "" {
		return ""
	}
	const prefix = "sha256:"
	if len(value) == len(prefix)+sha256.Size*2 && strings.HasPrefix(value, prefix) {
		if _, err := hex.DecodeString(value[len(prefix):]); err == nil {
			return prefix + strings.ToLower(value[len(prefix):])
		}
	}
	digest := sha256.Sum256([]byte(value))
	return prefix + hex.EncodeToString(digest[:])
}

func safeAuditToolOutcome(value string) string {
	switch value {
	case "success", "dispatch_rejected", "cleanup_pending", "context_canceled", "executor_error", "result_error", "unknown_error":
		return value
	default:
		return "unknown_error"
	}
}

func boundAuditText(value string, maxBytes int) string {
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

// summarizeArgs intentionally records no argument keys or values.
func summarizeArgs(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	return "redacted"
}
