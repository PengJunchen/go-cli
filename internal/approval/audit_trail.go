package approval

import (
	"bufio"
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

const maxAuditTrailLineBytes = 64 * 1024

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
	// ToolOutcome records the result of tool execution independently of approval.
	ToolOutcome string `json:"tool_outcome,omitempty"`
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
	dir      string
	mu       sync.Mutex
	lastHash map[string]string
	loaded   map[string]bool
	poisoned map[string]error
	openFile auditTrailOpenFileFunc
}

// NewAuditTrail creates an AuditTrail that writes daily JSONL files under dir.
// The directory is created lazily on the first Record call.
func NewAuditTrail(dir string) *AuditTrail {
	return &AuditTrail{
		dir:      dir,
		lastHash: make(map[string]string),
		loaded:   make(map[string]bool),
		poisoned: make(map[string]error),
		openFile: openAuditTrailFile,
	}
}

func openAuditTrailFile(path string, flag int, perm os.FileMode) (auditTrailFile, error) {
	return os.OpenFile(path, flag, perm)
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
		return fmt.Errorf("%w: writer quarantined: %w", ErrAuditTrailRollback, poison)
	}
	if err := a.loadChain(filename); err != nil {
		return err
	}
	if err := os.MkdirAll(a.dir, 0o700); err != nil {
		return fmt.Errorf("approval audit: mkdir: %w", err)
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
		return fmt.Errorf("%w: %w", ErrAuditTrailClose, err)
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
	entries, lastHash, err := readApprovalAuditFile(filename)
	if err != nil {
		return nil, err
	}
	a.lastHash[filename] = lastHash
	a.loaded[filename] = true
	return entries, nil
}

func (a *AuditTrail) loadChain(filename string) error {
	if a.loaded[filename] {
		return nil
	}
	_, lastHash, err := readApprovalAuditFile(filename)
	if err != nil {
		return err
	}
	a.lastHash[filename] = lastHash
	a.loaded[filename] = true
	return nil
}

func (a *AuditTrail) rollback(filename string, f auditTrailFile, originalSize int64, sentinel, cause error) error {
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
	entry.ToolOutcome = boundAuditText(entry.ToolOutcome, 32)
	return entry
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
