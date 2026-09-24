package session

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
)

const maxDurableJSONLineBytes = 1 << 20

var (
	// ErrUnsupportedSessionLock is returned when the platform cannot provide
	// the OS-backed session-file lock required for durable writes.
	ErrUnsupportedSessionLock = errors.New("session: durable session locks are unsupported on this platform")
	// ErrUnsupportedSessionNoFollowOpen is returned when the platform cannot
	// safely open a physical session file without following symlinks.
	ErrUnsupportedSessionNoFollowOpen = errors.New("session: durable no-follow session opens are unsupported on this platform")
	// ErrDurableWriterPoisoned indicates a write/flush/sync failure after which
	// the same handle can no longer acknowledge writes.
	ErrDurableWriterPoisoned = errors.New("session: durable writer is poisoned until reopened")
	// ErrDurableWriterClosed indicates that the owning prepared session closed.
	ErrDurableWriterClosed = errors.New("session: durable writer is closed")
)

type durableJSONLIO struct {
	write func(*os.File, []byte) (int, error)
	sync  func(*os.File) error
}

// DurableJSONLSessionPreparer opens independently locked physical session
// files. directory is a store root; session files live under its sessions
// child and are named only by opaque physical IDs.
type DurableJSONLSessionPreparer struct {
	directory string
	authority RecoveryAuthority
	io        durableJSONLIO
}

// NewDurableJSONLSessionPreparer returns a durable per-session JSONL opener.
// RecoveryAuthority may be nil only for sessions with no action journal or
// execution-resolution checkpoints.
func NewDurableJSONLSessionPreparer(directory string, authority RecoveryAuthority) *DurableJSONLSessionPreparer {
	return newDurableJSONLSessionPreparer(directory, authority, durableJSONLIO{})
}

func newDurableJSONLSessionPreparer(directory string, authority RecoveryAuthority, ioOps durableJSONLIO) *DurableJSONLSessionPreparer {
	return &DurableJSONLSessionPreparer{directory: directory, authority: authority, io: ioOps}
}

// PrepareNew creates a unique physical session file with exclusive creation,
// locks it before exposing the handle, and returns an empty root view. label is
// display metadata and is never used in the file path.
func (p *DurableJSONLSessionPreparer) PrepareNew(ctx context.Context, label string) (PreparedSession, error) {
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	if p == nil || p.directory == "" {
		return nil, errors.New("session: durable session directory is required")
	}
	sessionsDir := filepath.Join(p.directory, "sessions")
	if err := mkdirDurably(sessionsDir, 0o700); err != nil {
		return nil, fmt.Errorf("session: create sessions directory: %w", err)
	}
	for attempts := 0; attempts < 8; attempts++ {
		sessionID, err := randomPhysicalSessionID()
		if err != nil {
			return nil, err
		}
		filename := sessionID + ".jsonl"
		path := filepath.Join(sessionsDir, filename)
		file, err := openSessionFileNoFollow(sessionsDir, filename, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("session: create physical session file: %w", err)
		}
		if err := flockExclusiveContext(ctx, file); err != nil {
			_ = file.Close()
			_ = os.Remove(path)
			return nil, fmt.Errorf("session: lock new physical session file: %w", err)
		}
		if err := file.Sync(); err != nil {
			_ = flockUnlock(file)
			_ = file.Close()
			_ = os.Remove(path)
			return nil, fmt.Errorf("session: sync new physical session file: %w", err)
		}
		if err := syncDirectory(sessionsDir); err != nil {
			_ = flockUnlock(file)
			_ = file.Close()
			_ = os.Remove(path)
			return nil, fmt.Errorf("session: sync sessions directory: %w", err)
		}
		store := newDurableJSONLStore(file, sessionID, label, "main", nil, 0, p.io)
		store.view = SessionView{SessionID: sessionID, BranchID: "main"}
		return &durablePreparedSession{store: store}, nil
	}
	return nil, errors.New("session: unable to allocate unique physical session ID")
}

// PrepareResume resolves the opaque physical ID to one file, acquires its OS
// lock, and only then reads or repairs its JSONL contents.
func (p *DurableJSONLSessionPreparer) PrepareResume(ctx context.Context, physicalSessionID string) (PreparedSession, error) {
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	if p == nil || p.directory == "" {
		return nil, errors.New("session: durable session directory is required")
	}
	if !validPhysicalSessionID(physicalSessionID) {
		return nil, errors.New("session: invalid physical session ID")
	}
	sessionsDir := filepath.Join(p.directory, "sessions")
	filename, err := resolvePhysicalSessionFilename(sessionsDir, physicalSessionID)
	if err != nil {
		return nil, fmt.Errorf("session: resolve physical session file: %w", err)
	}
	file, err := openSessionFileNoFollow(sessionsDir, filename, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("session: open physical session file: %w", err)
	}
	if err := flockExclusiveContext(ctx, file); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("session: lock physical session file: %w", err)
	}
	failed := true
	defer func() {
		if failed {
			_ = flockUnlock(file)
			_ = file.Close()
		}
	}()

	entries, err := readDurableJSONL(file, sessionsDir)
	if err != nil {
		return nil, err
	}
	entryMap, maxSeq, label, activeBranch, activeLeaf, err := validateLoadedDurableEntries(entries, physicalSessionID)
	if err != nil {
		return nil, err
	}
	if hasRecoveryEvidence(entries) && p.authority == nil {
		return nil, errors.New("session: action history requires a recovery authority")
	}
	if hasRecoveryEvidence(entries) {
		return nil, errors.New("session: recovery authority adapter is required to resolve action history")
	}
	if _, err := file.Seek(0, io.SeekEnd); err != nil {
		return nil, fmt.Errorf("session: seek append position: %w", err)
	}
	branchEntries, err := orderedDurableBranch(activeLeaf, entryMap)
	if err != nil {
		return nil, err
	}
	view, err := durableSessionView(physicalSessionID, activeBranch, maxSeq, branchEntries)
	if err != nil {
		return nil, err
	}
	store := newDurableJSONLStore(file, physicalSessionID, label, activeBranch, entryMap, maxSeq, p.io)
	store.activeLeafID = activeLeaf
	store.view = view
	failed = false
	return &durablePreparedSession{store: store}, nil
}

type durablePreparedSession struct {
	store *durableJSONLStore
}

func (p *durablePreparedSession) View() SessionView {
	if p == nil || p.store == nil {
		return SessionView{}
	}
	p.store.mu.RLock()
	defer p.store.mu.RUnlock()
	return p.store.view.Clone()
}

func (p *durablePreparedSession) DurableWriter() DurableSessionWriter {
	if p == nil {
		return closedDurableWriter{}
	}
	return p.store
}

func (p *durablePreparedSession) Close() error {
	if p == nil || p.store == nil {
		return nil
	}
	return p.store.close()
}

type closedDurableWriter struct{}

func (closedDurableWriter) AppendAndSync(context.Context, ...SessionEntry) ([]SessionEntry, error) {
	return nil, ErrDurableWriterClosed
}

type durableJSONLStore struct {
	mu             sync.RWMutex
	file           *os.File
	buffered       *bufio.Writer
	sessionID      string
	sessionLabel   string
	activeBranchID string
	activeLeafID   string
	entries        map[string]*SessionEntry
	maxSeq         uint64
	view           SessionView
	poisoned       bool
	closed         bool
	io             durableJSONLIO
}

func newDurableJSONLStore(file *os.File, sessionID, label, branchID string, entries map[string]*SessionEntry, maxSeq uint64, ioOps durableJSONLIO) *durableJSONLStore {
	if entries == nil {
		entries = make(map[string]*SessionEntry)
	}
	if branchID == "" {
		branchID = "main"
	}
	store := &durableJSONLStore{
		file:           file,
		sessionID:      sessionID,
		sessionLabel:   label,
		activeBranchID: branchID,
		entries:        entries,
		maxSeq:         maxSeq,
		io:             ioOps,
	}
	store.buffered = bufio.NewWriterSize(durableFileWriter{file: file, write: ioOps.write}, 64*1024)
	return store
}

type durableFileWriter struct {
	file  *os.File
	write func(*os.File, []byte) (int, error)
}

func (w durableFileWriter) Write(payload []byte) (int, error) {
	if w.write != nil {
		return w.write(w.file, payload)
	}
	return w.file.Write(payload)
}

func (s *durableJSONLStore) AppendAndSync(ctx context.Context, entries ...SessionEntry) ([]SessionEntry, error) {
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.file == nil {
		return nil, ErrDurableWriterClosed
	}
	if s.poisoned {
		return nil, ErrDurableWriterPoisoned
	}
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return []SessionEntry{}, nil
	}

	available := make(map[string]*SessionEntry, len(s.entries)+len(entries))
	for id, entry := range s.entries {
		available[id] = entry
	}
	staged := make(map[string]*SessionEntry, len(entries))
	canonical := make([]SessionEntry, len(entries))
	newRows := make([]SessionEntry, 0, len(entries))
	var nextSeq = s.maxSeq
	for i, input := range entries {
		if err := validateDurableEntryJSONValues(input); err != nil {
			return nil, fmt.Errorf("session: durable entry at index %d has unsupported JSON values: %w", i, err)
		}
		row := *input.clone()
		if row.ID == "" || row.Type == "" {
			return nil, fmt.Errorf("session: durable entry at index %d requires ID and type", i)
		}
		if !validSessionEntryType(row.Type) {
			return nil, fmt.Errorf("session: durable entry %q has unsupported type %q", row.ID, row.Type)
		}
		if row.SessionID != "" && row.SessionID != s.sessionID {
			return nil, fmt.Errorf("session: entry %q belongs to a different physical session", row.ID)
		}
		row.SessionID = s.sessionID
		if row.SessionLabel == "" {
			row.SessionLabel = s.sessionLabel
		}
		if row.BranchID == "" {
			row.BranchID = s.activeBranchID
		}
		if row.BranchID == "" {
			row.BranchID = "main"
		}
		if row.RecordVersion == 0 {
			row.RecordVersion = 1
		}
		if row.RecordVersion != 1 {
			return nil, fmt.Errorf("session: writer cannot append record version %d", row.RecordVersion)
		}
		row.Seq = 0
		providedDigest := row.PayloadDigest
		_, digest, err := canonicalEntryPayload(row)
		if err != nil {
			return nil, err
		}
		if providedDigest != "" && providedDigest != digest {
			return nil, fmt.Errorf("session: entry %q carries a mismatched payload digest", row.ID)
		}
		row.PayloadDigest = digest
		if err := validateVersion1EntryCoordinates(row); err != nil {
			return nil, fmt.Errorf("session: invalid durable entry %q: %w", row.ID, err)
		}

		if previous, exists := staged[row.ID]; exists {
			previousDigest, digestErr := storedPayloadDigest(previous)
			if digestErr != nil || previousDigest != digest {
				return nil, fmt.Errorf("session: conflicting duplicate entry ID %q in batch", row.ID)
			}
			canonical[i] = *previous.clone()
			continue
		}
		if previous, exists := available[row.ID]; exists {
			previousDigest, digestErr := storedPayloadDigest(previous)
			if digestErr != nil || previousDigest != digest {
				return nil, fmt.Errorf("session: entry ID %q already exists with different content", row.ID)
			}
			canonical[i] = *previous.clone()
			continue
		}
		if row.ParentID != "" {
			if _, exists := available[row.ParentID]; !exists {
				return nil, fmt.Errorf("session: entry %q references unknown parent %q", row.ID, row.ParentID)
			}
		} else if len(available) > 0 {
			return nil, fmt.Errorf("session: non-root entry %q has no parent", row.ID)
		}

		nextSeq++
		row.Seq = nextSeq
		staged[row.ID] = row.clone()
		available[row.ID] = row.clone()
		newRows = append(newRows, row)
		canonical[i] = row
	}
	if len(newRows) == 0 {
		return cloneSessionEntries(canonical), nil
	}
	existingRows, err := entriesInSequence(s.entries)
	if err != nil {
		return nil, err
	}
	branchRows := append(existingRows, newRows...)
	if err := validateDurableBranchJournal(branchRows); err != nil {
		return nil, err
	}
	if err := validateDurableTranscriptCoordinates(branchRows); err != nil {
		return nil, err
	}
	candidateActiveLeafID := s.activeLeafID
	candidateActiveBranchID := s.activeBranchID
	for _, row := range newRows {
		candidateActiveLeafID = row.ID
		if row.CheckpointKind == CheckpointActiveLeaf && row.ActiveLeafEntryID != "" {
			candidateActiveLeafID = row.ActiveLeafEntryID
		}
		candidateActiveBranchID = row.BranchID
	}
	branchEntries, err := orderedDurableBranch(candidateActiveLeafID, available)
	if err != nil {
		return nil, fmt.Errorf("session: validate durable branch projection: %w", err)
	}
	candidateView, err := durableSessionView(s.sessionID, candidateActiveBranchID, nextSeq, branchEntries)
	deferViewUpdate := errors.Is(err, errUnclosedToolCallGroup)
	if err != nil && !deferViewUpdate {
		return nil, fmt.Errorf("session: validate durable session view: %w", err)
	}
	newByID := make(map[string]*SessionEntry, len(newRows))
	for i := range newRows {
		newByID[newRows[i].ID] = newRows[i].clone()
	}
	var serializedBatch bytes.Buffer
	for _, row := range newRows {
		encoded, err := json.Marshal(row)
		if err != nil {
			return nil, fmt.Errorf("session: serialize durable entry %q: %w", row.ID, err)
		}
		if len(encoded) > maxDurableJSONLineBytes {
			return nil, fmt.Errorf("session: serialized entry %q exceeds %d bytes", row.ID, maxDurableJSONLineBytes)
		}
		serializedBatch.Write(encoded)
		serializedBatch.WriteByte('\n')
	}
	if written, err := s.buffered.Write(serializedBatch.Bytes()); err != nil {
		s.poisoned = true
		return nil, fmt.Errorf("session: append durable entry batch: %w", err)
	} else if written != serializedBatch.Len() {
		s.poisoned = true
		return nil, fmt.Errorf("session: append durable entry batch: %w", io.ErrShortWrite)
	}
	if err := s.buffered.Flush(); err != nil {
		s.poisoned = true
		return nil, fmt.Errorf("session: flush durable entry batch: %w", err)
	}
	if err := s.syncFile(); err != nil {
		s.poisoned = true
		return nil, fmt.Errorf("session: sync durable entry batch: %w", err)
	}

	for id, entry := range newByID {
		s.entries[id] = entry
	}
	s.maxSeq = nextSeq
	s.activeLeafID = candidateActiveLeafID
	s.activeBranchID = candidateActiveBranchID
	if !deferViewUpdate {
		s.view = candidateView
	}
	return cloneSessionEntries(canonical), nil
}

func (s *durableJSONLStore) syncFile() error {
	if s.io.sync != nil {
		return s.io.sync(s.file)
	}
	return s.file.Sync()
}

func (s *durableJSONLStore) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	var flushErr error
	if !s.poisoned && s.buffered != nil {
		flushErr = s.buffered.Flush()
	}
	unlockErr := flockUnlock(s.file)
	closeErr := s.file.Close()
	s.file = nil
	if flushErr != nil {
		s.poisoned = true
	}
	return errors.Join(flushErr, unlockErr, closeErr)
}

func cloneSessionEntries(entries []SessionEntry) []SessionEntry {
	cloned := make([]SessionEntry, len(entries))
	for i := range entries {
		cloned[i] = *entries[i].clone()
	}
	return cloned
}

func contextErr(ctx context.Context) error {
	if ctx == nil {
		return errors.New("session: context is required")
	}
	return ctx.Err()
}

func randomPhysicalSessionID() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("session: generate physical session ID: %w", err)
	}
	return "s_" + hex.EncodeToString(random[:]), nil
}

func validPhysicalSessionID(id string) bool {
	if id == "" || len(id) > 96 || id == "." || id == ".." {
		return false
	}
	for _, character := range id {
		if !((character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '_' || character == '-') {
			return false
		}
	}
	return true
}

func resolvePhysicalSessionFilename(sessionsDir, physicalSessionID string) (string, error) {
	direct := physicalSessionID + ".jsonl"
	if _, err := os.Lstat(filepath.Join(sessionsDir, direct)); err == nil {
		return direct, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		return "", err
	}
	suffix := "-" + physicalSessionID + ".jsonl"
	var match string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), suffix) {
			continue
		}
		if match != "" {
			return "", fmt.Errorf("session: physical session ID %q is ambiguous", physicalSessionID)
		}
		match = entry.Name()
	}
	if match == "" {
		return "", os.ErrNotExist
	}
	return match, nil
}

func mkdirDurably(path string, mode os.FileMode) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	var missing []string
	current := absolute
	for {
		info, statErr := os.Stat(current)
		if statErr == nil {
			if !info.IsDir() {
				return fmt.Errorf("%q is not a directory", current)
			}
			break
		}
		if !errors.Is(statErr, os.ErrNotExist) {
			return statErr
		}
		missing = append(missing, current)
		parent := filepath.Dir(current)
		if parent == current {
			return fmt.Errorf("no existing parent directory for %q", path)
		}
		current = parent
	}
	if err := os.MkdirAll(absolute, mode); err != nil {
		return err
	}
	for i := len(missing) - 1; i >= 0; i-- {
		if err := syncDirectory(filepath.Dir(missing[i])); err != nil {
			return err
		}
	}
	return nil
}

func validateLoadedDurableEntries(entries []SessionEntry, sessionID string) (map[string]*SessionEntry, uint64, string, string, string, error) {
	byID := make(map[string]*SessionEntry, len(entries))
	var maxSeq uint64
	var sessionLabel string
	activeBranch := "main"
	activeLeaf := ""
	rootSeen := false
	for i := range entries {
		entry := entries[i]
		if !validSessionEntryType(entry.Type) {
			return nil, 0, "", "", "", fmt.Errorf("session: row %d has unsupported entry type %q", i, entry.Type)
		}
		if entry.SessionID != "" && entry.SessionID != sessionID {
			return nil, 0, "", "", "", fmt.Errorf("session: row %q belongs to physical session %q", entry.ID, entry.SessionID)
		}
		if entry.RecordVersion == 1 && (entry.SessionID != sessionID || entry.BranchID == "" || entry.Seq == 0 || entry.PayloadDigest == "") {
			return nil, 0, "", "", "", fmt.Errorf("session: version 1 row %q lacks physical identity, branch, sequence, or payload digest", entry.ID)
		}
		if _, exists := byID[entry.ID]; exists {
			return nil, 0, "", "", "", fmt.Errorf("session: duplicate entry ID %q in durable file", entry.ID)
		}
		if entry.ParentID == "" {
			if rootSeen && entry.RecordVersion == 1 {
				return nil, 0, "", "", "", fmt.Errorf("session: version 1 row %q introduces a second root", entry.ID)
			}
			rootSeen = true
		} else if _, exists := byID[entry.ParentID]; !exists {
			return nil, 0, "", "", "", fmt.Errorf("session: row %q has missing or forward parent %q", entry.ID, entry.ParentID)
		}
		if entry.Seq != 0 {
			if entry.Seq <= maxSeq {
				return nil, 0, "", "", "", fmt.Errorf("session: sequence %d is not increasing at row %q", entry.Seq, entry.ID)
			}
			maxSeq = entry.Seq
		} else if maxSeq != 0 {
			return nil, 0, "", "", "", fmt.Errorf("session: row %q has missing sequence after sequenced data", entry.ID)
		}
		if entry.PayloadDigest != "" {
			_, digest, err := canonicalEntryPayload(entry)
			if err != nil {
				return nil, 0, "", "", "", err
			}
			if digest != entry.PayloadDigest {
				return nil, 0, "", "", "", fmt.Errorf("session: row %q has mismatched payload digest", entry.ID)
			}
		}
		if sessionLabel == "" && entry.SessionLabel != "" {
			sessionLabel = entry.SessionLabel
		}
		if entry.BranchID != "" {
			activeBranch = entry.BranchID
		}
		if entry.ID != "" {
			activeLeaf = entry.ID
		}
		if entry.CheckpointKind == CheckpointActiveLeaf && entry.ActiveLeafEntryID != "" {
			activeLeaf = entry.ActiveLeafEntryID
			if entry.BranchID != "" {
				activeBranch = entry.BranchID
			}
		}
		byID[entry.ID] = entry.clone()
	}
	if err := validateDurableBranchJournal(entries); err != nil {
		return nil, 0, "", "", "", err
	}
	if err := validateDurableTranscriptCoordinates(entries); err != nil {
		return nil, 0, "", "", "", err
	}
	return byID, maxSeq, sessionLabel, activeBranch, activeLeaf, nil
}

func hasRecoveryEvidence(entries []SessionEntry) bool {
	for _, entry := range entries {
		if entry.ActionState != "" || entry.CheckpointKind == CheckpointExecutionResolution {
			return true
		}
	}
	return false
}

func orderedDurableBranch(leafID string, entries map[string]*SessionEntry) ([]SessionEntry, error) {
	if leafID == "" {
		return nil, nil
	}
	ordered := make([]SessionEntry, 0)
	seen := make(map[string]struct{}, len(entries))
	for leafID != "" {
		if _, exists := seen[leafID]; exists {
			return nil, errors.New("session: durable parent chain contains a cycle")
		}
		seen[leafID] = struct{}{}
		entry, exists := entries[leafID]
		if !exists {
			return nil, fmt.Errorf("session: active leaf %q is missing from durable entries", leafID)
		}
		ordered = append(ordered, *entry.clone())
		leafID = entry.ParentID
	}
	for left, right := 0, len(ordered)-1; left < right; left, right = left+1, right-1 {
		ordered[left], ordered[right] = ordered[right], ordered[left]
	}
	return ordered, nil
}

func durableSessionView(sessionID, branchID string, maxSeq uint64, branchEntries []SessionEntry) (SessionView, error) {
	if branchID == "" {
		branchID = "main"
	}
	if len(branchEntries) == 0 {
		return SessionView{SessionID: sessionID, BranchID: branchID}, nil
	}
	hash := sha256.New()
	for _, entry := range branchEntries {
		_, _ = fmt.Fprintf(hash, "%s\x00%d\x00%s\n", entry.ID, entry.Seq, entry.PayloadDigest)
	}
	revision := fmt.Sprintf("sha256:%x:%d", hash.Sum(nil), maxSeq)
	view, err := SessionViewFromEntries(sessionID, branchID, revision, branchEntries)
	if err != nil {
		return SessionView{}, err
	}
	return view, nil
}

func storedPayloadDigest(entry *SessionEntry) (string, error) {
	if entry == nil {
		return "", errors.New("session: nil stored entry")
	}
	if entry.PayloadDigest != "" {
		return entry.PayloadDigest, nil
	}
	_, digest, err := canonicalEntryPayload(*entry)
	return digest, err
}

func validSessionEntryType(entryType EntryType) bool {
	switch entryType {
	case EntryTypeUser, EntryTypeAssistant, EntryTypeTool, EntryTypeCompaction, EntryTypeSystem, EntryTypeCheckpoint:
		return true
	default:
		return false
	}
}

var _ SessionPreparer = (*DurableJSONLSessionPreparer)(nil)
var _ PreparedSession = (*durablePreparedSession)(nil)
var _ DurableSessionWriter = (*durableJSONLStore)(nil)

// readDurableJSONL reads complete bounded rows from an already-open and
// exclusively locked session file. It preserves a valid unterminated final
// row by appending and syncing its delimiter. Only a syntactically truncated
// final JSON value is copied to a synced sidecar before the source is trimmed.
func readDurableJSONL(file *os.File, sidecarDir string) ([]SessionEntry, error) {
	if file == nil {
		return nil, errors.New("session: cannot read a nil durable file")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("session: seek durable file: %w", err)
	}

	reader := bufio.NewReaderSize(file, 64*1024)
	entries := make([]SessionEntry, 0)
	var row []byte
	var offset int64
	var completeOffset int64
	for {
		fragment, readErr := reader.ReadSlice('\n')
		if len(row)+len(fragment) > maxDurableJSONLineBytes+1 {
			return nil, fmt.Errorf("session: JSONL row exceeds %d bytes", maxDurableJSONLineBytes)
		}
		row = append(row, fragment...)
		offset += int64(len(fragment))

		switch readErr {
		case nil:
			line := row[:len(row)-1]
			if len(line) > maxDurableJSONLineBytes {
				return nil, fmt.Errorf("session: JSONL row exceeds %d bytes", maxDurableJSONLineBytes)
			}
			entry, err := decodeDurableEntry(line)
			if err != nil {
				return nil, fmt.Errorf("session: decode JSONL row ending at byte %d: %w", offset, err)
			}
			entries = append(entries, entry)
			completeOffset = offset
			row = row[:0]
		case bufio.ErrBufferFull:
			if len(row) > maxDurableJSONLineBytes {
				return nil, fmt.Errorf("session: JSONL row exceeds %d bytes", maxDurableJSONLineBytes)
			}
		case io.EOF:
			if len(row) == 0 {
				return entries, nil
			}
			if len(row) > maxDurableJSONLineBytes {
				return nil, fmt.Errorf("session: JSONL row exceeds %d bytes", maxDurableJSONLineBytes)
			}
			entry, err := decodeDurableEntry(row)
			if err == nil {
				if _, seekErr := file.Seek(0, io.SeekEnd); seekErr != nil {
					return nil, fmt.Errorf("session: seek unterminated final row: %w", seekErr)
				}
				if err := writeFileBytes(file, []byte{'\n'}); err != nil {
					return nil, fmt.Errorf("session: delimit valid unterminated final row: %w", err)
				}
				if err := file.Sync(); err != nil {
					return nil, fmt.Errorf("session: sync final row delimiter: %w", err)
				}
				entries = append(entries, entry)
				return entries, nil
			}
			if !isTruncatedJSON(err) {
				return nil, fmt.Errorf("session: malformed final JSONL row at byte %d: %w", offset, err)
			}
			if sidecarDir == "" {
				sidecarDir = filepath.Dir(file.Name())
			}
			if err := quarantineTornSuffix(file.Name(), sidecarDir, row); err != nil {
				return nil, fmt.Errorf("session: quarantine truncated final row: %w", err)
			}
			if err := file.Truncate(completeOffset); err != nil {
				return nil, fmt.Errorf("session: truncate torn final row: %w", err)
			}
			if err := file.Sync(); err != nil {
				return nil, fmt.Errorf("session: sync repaired session file: %w", err)
			}
			return entries, nil
		default:
			return nil, fmt.Errorf("session: read durable JSONL: %w", readErr)
		}
	}
}

func decodeDurableEntry(line []byte) (SessionEntry, error) {
	var entry SessionEntry
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return entry, errors.New("session: JSONL row must be an object")
	}
	if err := json.Unmarshal(trimmed, &entry); err != nil {
		return entry, err
	}
	if entry.ID == "" {
		return entry, errors.New("session: durable entry ID is required")
	}
	if entry.Type == "" {
		return entry, errors.New("session: durable entry type is required")
	}
	if err := validateVersion1EntryCoordinates(entry); err != nil {
		return entry, err
	}
	return entry, nil
}

func isTruncatedJSON(err error) bool {
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var syntaxErr *json.SyntaxError
	return errors.As(err, &syntaxErr) && syntaxErr.Error() == "unexpected end of JSON input"
}

func quarantineTornSuffix(sourcePath, sidecarDir string, suffix []byte) error {
	if err := mkdirDurably(sidecarDir, 0o700); err != nil {
		return err
	}
	for attempts := 0; attempts < 8; attempts++ {
		var random [12]byte
		if _, err := rand.Read(random[:]); err != nil {
			return fmt.Errorf("generate sidecar identity: %w", err)
		}
		name := filepath.Base(sourcePath) + ".torn-" + hex.EncodeToString(random[:]) + ".tail"
		path := filepath.Join(sidecarDir, name)
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return err
		}
		writeErr := writeFileBytes(file, suffix)
		if writeErr == nil {
			writeErr = file.Sync()
		}
		closeErr := file.Close()
		if writeErr != nil {
			return errors.Join(writeErr, closeErr)
		}
		if closeErr != nil {
			return closeErr
		}
		if err := syncDirectory(sidecarDir); err != nil {
			return err
		}
		return nil
	}
	return errors.New("session: unable to allocate unique torn-tail sidecar")
}

func writeFileBytes(file *os.File, content []byte) error {
	for len(content) > 0 {
		written, err := file.Write(content)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		content = content[written:]
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open directory for sync: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync directory: %w", err)
	}
	return nil
}

func canonicalEntryPayload(entry SessionEntry) ([]byte, string, error) {
	entry.Seq = 0
	entry.PayloadDigest = ""
	encoded, err := json.Marshal(entry)
	if err != nil {
		return nil, "", fmt.Errorf("session: encode canonical entry payload: %w", err)
	}
	if len(encoded) > maxDurableJSONLineBytes {
		return nil, "", fmt.Errorf("session: serialized entry exceeds %d bytes", maxDurableJSONLineBytes)
	}
	digest := sha256Bytes(encoded)
	return encoded, "sha256:" + hex.EncodeToString(digest[:]), nil
}

func validateDurableEntryJSONValues(entry SessionEntry) error {
	for callIndex, call := range entry.ToolCalls {
		if err := validateProtocolJSONArgument(call.Args); err != nil {
			return fmt.Errorf("tool call %d arguments: %w", callIndex, err)
		}
	}
	return nil
}

func validateProtocolJSONArgument(value any) error {
	nodes := 0
	return validateProtocolJSONValue(reflect.ValueOf(value), 0, &nodes)
}

func validateProtocolJSONValue(value reflect.Value, depth int, nodes *int) error {
	if !value.IsValid() {
		return nil
	}
	(*nodes)++
	if *nodes > maxDurableJSONLineBytes || depth > 128 {
		return errors.New("argument graph exceeds supported JSON depth or node limit")
	}
	if value.Kind() == reflect.Interface {
		if value.IsNil() {
			return nil
		}
		return validateProtocolJSONValue(value.Elem(), depth+1, nodes)
	}
	valueType := value.Type()
	jsonMarshalerType := reflect.TypeOf((*json.Marshaler)(nil)).Elem()
	textMarshalerType := reflect.TypeOf((*encoding.TextMarshaler)(nil)).Elem()
	if valueType.Implements(jsonMarshalerType) || valueType.Implements(textMarshalerType) {
		return fmt.Errorf("custom marshaler %s is not a protocol JSON value", valueType)
	}
	if valueType == reflect.TypeOf(json.Number("")) {
		return nil
	}
	switch value.Kind() {
	case reflect.Map:
		if value.IsNil() {
			return nil
		}
		if valueType.Key() != reflect.TypeOf("") {
			return fmt.Errorf("map key type %s is not a JSON object key", valueType.Key())
		}
		iterator := value.MapRange()
		for iterator.Next() {
			if err := validateProtocolJSONValue(iterator.Value(), depth+1, nodes); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		if value.Kind() == reflect.Slice && value.IsNil() {
			return nil
		}
		if value.Kind() == reflect.Slice && value.Type().Elem().Kind() == reflect.Uint8 {
			return fmt.Errorf("byte slice %s is not a protocol JSON array", valueType)
		}
		for index := 0; index < value.Len(); index++ {
			if err := validateProtocolJSONValue(value.Index(index), depth+1, nodes); err != nil {
				return err
			}
		}
	case reflect.Bool, reflect.String,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return nil
	default:
		return fmt.Errorf("value type %s is not a protocol JSON value", valueType)
	}
	return nil
}

func sha256Bytes(payload []byte) [32]byte {
	return sha256.Sum256(payload)
}
