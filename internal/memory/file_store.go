package memory

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

const memoryFilePerm = 0o600
const maxMemoryRecordBytes = 1 << 20

// ErrMemoryStoreClosed is returned when a write is attempted after Close.
var ErrMemoryStoreClosed = errors.New("memory: store is closed")

// ErrMemoryStoreUnavailable is returned when a committed file replacement
// cannot be reopened for further writes.
var ErrMemoryStoreUnavailable = errors.New("memory: store is unavailable")

// ErrMemoryStorePoisoned marks a store whose last append could not be rolled
// back with confirmed file operations.
var ErrMemoryStorePoisoned = errors.New("memory: store state is unknown")

// ErrMemoryRecordTooLarge is returned when a serialized memory exceeds the
// maximum JSONL record size.
var ErrMemoryRecordTooLarge = errors.New("memory: record exceeds maximum size")

// MemoryStorePoisonedError describes an append failure whose rollback could
// not be confirmed. The store rejects later writes to avoid compounding an
// unknown on-disk state.
type MemoryStorePoisonedError struct {
	Cause error
}

// Error returns the poisoned store error and its rollback failure cause.
func (e *MemoryStorePoisonedError) Error() string {
	if e.Cause == nil {
		return ErrMemoryStorePoisoned.Error()
	}
	return fmt.Sprintf("%s: %v", ErrMemoryStorePoisoned, e.Cause)
}

// Unwrap exposes both the poisoned-state marker and the underlying failures.
func (e *MemoryStorePoisonedError) Unwrap() []error {
	if e.Cause == nil {
		return []error{ErrMemoryStorePoisoned}
	}
	return []error{ErrMemoryStorePoisoned, e.Cause}
}

type memoryFileIO struct {
	createTemp func(dir, pattern string) (*os.File, error)
	openFile   func(name string, flag int, perm os.FileMode) (*os.File, error)
	write      func(file *os.File, data []byte) (int, error)
	sync       func(file *os.File) error
	truncate   func(file *os.File, size int64) error
	rename     func(oldPath, newPath string) error
}

func defaultMemoryFileIO() memoryFileIO {
	return memoryFileIO{
		createTemp: os.CreateTemp,
		openFile:   os.OpenFile,
		write:      (*os.File).Write,
		sync:       (*os.File).Sync,
		truncate:   (*os.File).Truncate,
		rename:     os.Rename,
	}
}

// ErrMemoryNotFound is returned when a memory ID does not exist.
var ErrMemoryNotFound = errors.New("memory: not found")

// FileMemoryStore implements MemoryStore backed by a JSONL file. Entries are
// appended on Add and kept in memory for fast lookup and TF-IDF search. It is
// concurrency-safe.
type FileMemoryStore struct {
	path           string
	mu             sync.RWMutex
	items          map[string]Memory
	file           *os.File
	fileIO         memoryFileIO
	closed         bool
	poisoned       *MemoryStorePoisonedError
	unavailableErr error
	// TF-IDF index structures
	termIdx map[string][]string // term -> memory IDs (unique per term)
	docLen  map[string]int      // id -> token count
}

var _ MemoryStore = (*FileMemoryStore)(nil)

// NewFileMemoryStore opens or creates the JSONL file at path, loads existing
// entries into memory, and prepares the store for append-only writes.
func NewFileMemoryStore(path string) (*FileMemoryStore, error) {
	s := &FileMemoryStore{
		path:    path,
		items:   make(map[string]Memory),
		termIdx: make(map[string][]string),
		docLen:  make(map[string]int),
		fileIO:  defaultMemoryFileIO(),
	}
	if err := s.load(); err != nil {
		return nil, fmt.Errorf("memory: load: %w", err)
	}
	f, err := s.fileIO.openFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, memoryFilePerm)
	if err != nil {
		return nil, fmt.Errorf("memory: open store file: %w", err)
	}
	s.file = f
	return s, nil
}

// load reads existing JSONL entries into memory and builds the search index.
func (s *FileMemoryStore) load() error {
	f, err := os.Open(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // brand-new store; nothing to load
		}
		return fmt.Errorf("memory: open for reading: %w", err)
	}
	defer func() { _ = f.Close() }() //nolint:errcheck // best-effort close of read-only file

	reader := bufio.NewReaderSize(f, 64*1024)
	for {
		record, ok, err := readMemoryRecord(reader)
		if err != nil {
			return fmt.Errorf("memory: read store file: %w", err)
		}
		if !ok {
			break
		}
		line := bytes.TrimSpace(record)
		if len(line) == 0 {
			continue
		}
		var m Memory
		if err := json.Unmarshal(line, &m); err != nil {
			slog.Warn("memory.load_skip_corrupt_line", "path", s.path, "err", err)
			continue // skip corrupt lines, keep the rest
		}
		if m.ID == "" {
			continue
		}
		if _, exists := s.items[m.ID]; exists {
			continue
		}
		s.items[m.ID] = m
		s.indexDocument(m)
	}
	return nil
}

// Add stores a memory entry. If mem.ID is empty a unique ID is generated. The
// entry is appended to the JSONL file and added to the in-memory index.
func (s *FileMemoryStore) Add(ctx context.Context, mem Memory) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("memory: add canceled: %w", err)
	}
	if mem.ID == "" {
		id, err := newMemoryID()
		if err != nil {
			return fmt.Errorf("memory: generate id: %w", err)
		}
		mem.ID = id
	}
	now := time.Now()
	if mem.CreatedAt.IsZero() {
		mem.CreatedAt = now
	}
	if mem.UpdatedAt.IsZero() {
		mem.UpdatedAt = now
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.writeAvailabilityErrorLocked(); err != nil {
		return err
	}

	if _, exists := s.items[mem.ID]; exists {
		return fmt.Errorf("memory: entry %q already exists", mem.ID)
	}

	data, err := json.Marshal(mem)
	if err != nil {
		return fmt.Errorf("memory: encode entry: %w", err)
	}
	if len(data) > maxMemoryRecordBytes {
		return fmt.Errorf("%w: %d bytes exceeds %d", ErrMemoryRecordTooLarge, len(data), maxMemoryRecordBytes)
	}
	info, err := s.file.Stat()
	if err != nil {
		return fmt.Errorf("memory: stat store before append: %w", err)
	}
	data = append(data, '\n')
	appendErr := writeMemoryFileAll(s.fileIO.write, s.file, data)
	if appendErr == nil {
		appendErr = s.fileIO.sync(s.file)
	}
	if appendErr != nil {
		return s.rollbackAppendLocked(info.Size(), appendErr)
	}

	s.items[mem.ID] = mem
	s.indexDocument(mem)
	return nil
}

// Get returns the memory with the given ID.
func (s *FileMemoryStore) Get(_ context.Context, id string) (Memory, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	m, ok := s.items[id]
	if !ok {
		return Memory{}, ErrMemoryNotFound
	}
	return m, nil
}

// List returns all memories sorted by CreatedAt descending.
func (s *FileMemoryStore) List(_ context.Context) ([]Memory, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]Memory, 0, len(s.items))
	for _, m := range s.items {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

// Delete removes the memory with the given ID and rewrites the backing file.
func (s *FileMemoryStore) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("memory: delete canceled: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writeAvailabilityErrorLocked(); err != nil {
		return err
	}

	_, ok := s.items[id]
	if !ok {
		return ErrMemoryNotFound
	}
	candidate := make(map[string]Memory, len(s.items)-1)
	for itemID, item := range s.items {
		if itemID != id {
			candidate[itemID] = item
		}
	}
	return s.rewriteFileLocked(candidate)
}

// Search returns memories matching the query, scored with TF-IDF and sorted by
// relevance descending. At most limit results are returned. Documents whose
// TF-IDF score is not positive (e.g. terms that appear in nearly every
// document are not discriminating) are not returned.
func (s *FileMemoryStore) Search(_ context.Context, query string, limit int) ([]Memory, error) {
	if limit <= 0 {
		return nil, nil
	}
	qTerms := tokenize(query)
	if len(qTerms) == 0 {
		return nil, nil
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	n := len(s.items)
	if n == 0 {
		return nil, nil
	}
	effectiveN := n
	if effectiveN < 3 {
		effectiveN = 3
	}

	// Collect candidate document IDs that contain at least one query term.
	uniqueQ := dedup(qTerms)
	candSet := make(map[string]struct{})
	for _, term := range uniqueQ {
		for _, id := range s.termIdx[term] {
			candSet[id] = struct{}{}
		}
	}
	if len(candSet) == 0 {
		return nil, nil
	}

	type cand struct {
		id    string
		score float64
	}
	cands := make([]cand, 0, len(candSet))
	freqs := make(map[string]map[string]int, len(candSet))
	for id := range candSet {
		m, ok := s.items[id]
		if !ok {
			continue
		}
		freqs[id] = buildFreq(tokenize(m.Content))
		cands = append(cands, cand{id: id})
	}

	// Accumulate TF-IDF scores across query terms.
	for _, term := range uniqueQ {
		df := len(s.termIdx[term])
		if df == 0 {
			continue
		}
		idf := math.Log(float64(effectiveN) / float64(1+df))
		for i := range cands {
			id := cands[i].id
			dl := s.docLen[id]
			if dl == 0 {
				continue
			}
			tf := float64(freqs[id][term]) / float64(dl)
			cands[i].score += tf * idf
		}
	}

	// Keep only positively-scoring documents.
	maxScore := 0.0
	scored := make([]cand, 0, len(cands))
	for _, c := range cands {
		if c.score > 0 {
			scored = append(scored, c)
			if c.score > maxScore {
				maxScore = c.score
			}
		}
	}
	if len(scored) == 0 {
		return nil, nil
	}

	sort.Slice(scored, func(i, j int) bool {
		return scored[i].score > scored[j].score
	})

	if limit > len(scored) {
		limit = len(scored)
	}
	out := make([]Memory, 0, limit)
	for i := 0; i < limit; i++ {
		m := s.items[scored[i].id]
		m.Relevance = scored[i].score / maxScore
		out = append(out, m)
	}
	return out, nil
}

// Close closes the backing file. It is safe to call multiple times.
func (s *FileMemoryStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.file == nil {
		return nil
	}
	err := s.file.Close()
	s.file = nil
	return err
}

// rewriteFileLocked writes candidate to a synced temporary JSONL file and
// atomically replaces the store. The in-memory state changes only after rename
// commits the replacement. It must be called while holding the write lock.
func (s *FileMemoryStore) rewriteFileLocked(candidate map[string]Memory) error {
	var buf bytes.Buffer
	for _, m := range candidate {
		data, err := json.Marshal(m)
		if err != nil {
			return fmt.Errorf("memory: encode entry: %w", err)
		}
		if len(data) > maxMemoryRecordBytes {
			return fmt.Errorf("%w: %d bytes exceeds %d", ErrMemoryRecordTooLarge, len(data), maxMemoryRecordBytes)
		}
		buf.Write(data)
		buf.WriteByte('\n')
	}

	// Replace the store from a synced temporary file in the same directory.
	dir := filepath.Dir(s.path)
	tmp, err := s.fileIO.createTemp(dir, "memories-*.jsonl.tmp")
	if err != nil {
		return fmt.Errorf("memory: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() //nolint:errcheck

	if err := writeMemoryFileAll(s.fileIO.write, tmp, buf.Bytes()); err != nil {
		writeErr := fmt.Errorf("memory: write temp file: %w", err)
		if closeErr := tmp.Close(); closeErr != nil {
			return errors.Join(writeErr, fmt.Errorf("memory: close temp file: %w", closeErr))
		}
		return writeErr
	}
	if err := s.fileIO.sync(tmp); err != nil {
		syncErr := fmt.Errorf("memory: sync temp file: %w", err)
		if closeErr := tmp.Close(); closeErr != nil {
			return errors.Join(syncErr, fmt.Errorf("memory: close temp file: %w", closeErr))
		}
		return syncErr
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("memory: close temp file: %w", err)
	}
	oldFile := s.file
	if err := oldFile.Close(); err != nil {
		s.file = nil
		return s.restoreFileAfterPreRenameFailureLocked(fmt.Errorf("memory: close store file before replacement: %w", err))
	}
	s.file = nil
	if err := s.fileIO.rename(tmpName, s.path); err != nil {
		return s.restoreFileAfterPreRenameFailureLocked(fmt.Errorf("memory: rename temp file: %w", err))
	}

	s.items = candidate
	s.rebuildIndexLocked()
	f, err := s.fileIO.openFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, memoryFilePerm)
	if err != nil {
		s.file = nil
		s.unavailableErr = fmt.Errorf("memory: reopen store file after replacement: %w: %w", ErrMemoryStoreUnavailable, err)
		return s.unavailableErr
	}
	s.file = f
	return nil
}

func (s *FileMemoryStore) restoreFileAfterPreRenameFailureLocked(cause error) error {
	f, err := s.fileIO.openFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, memoryFilePerm)
	if err != nil {
		s.unavailableErr = fmt.Errorf("memory: restore store after failed replacement: %w: %w", ErrMemoryStoreUnavailable, err)
		return errors.Join(cause, s.unavailableErr)
	}
	s.file = f
	return cause
}

func (s *FileMemoryStore) writeAvailabilityErrorLocked() error {
	if s.closed {
		return ErrMemoryStoreClosed
	}
	if s.poisoned != nil {
		return s.poisoned
	}
	if s.unavailableErr != nil {
		return s.unavailableErr
	}
	if s.file == nil {
		return ErrMemoryStoreUnavailable
	}
	return nil
}

func (s *FileMemoryStore) rollbackAppendLocked(originalSize int64, appendErr error) error {
	truncateErr := s.fileIO.truncate(s.file, originalSize)
	syncErr := s.fileIO.sync(s.file)
	if truncateErr != nil || syncErr != nil {
		poisoned := &MemoryStorePoisonedError{Cause: errors.Join(appendErr, truncateErr, syncErr)}
		s.poisoned = poisoned
		return fmt.Errorf("memory: append failed and rollback could not be confirmed: %w", poisoned)
	}
	return fmt.Errorf("memory: append entry: %w", appendErr)
}

func writeMemoryFileAll(write func(*os.File, []byte) (int, error), file *os.File, data []byte) error {
	for len(data) > 0 {
		n, err := write(file, data)
		if n < 0 || n > len(data) {
			return fmt.Errorf("memory: invalid file write count %d", n)
		}
		data = data[n:]
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func (s *FileMemoryStore) rebuildIndexLocked() {
	s.termIdx = make(map[string][]string)
	s.docLen = make(map[string]int)
	for _, m := range s.items {
		s.indexDocument(m)
	}
}

func readMemoryRecord(reader *bufio.Reader) ([]byte, bool, error) {
	line := make([]byte, 0, 64*1024)
	for {
		part, err := reader.ReadSlice('\n')
		if len(line)+len(part) > maxMemoryRecordBytes+1 {
			return nil, false, ErrMemoryRecordTooLarge
		}
		line = append(line, part...)
		if err == nil {
			line = line[:len(line)-1]
			if len(line) > maxMemoryRecordBytes {
				return nil, false, ErrMemoryRecordTooLarge
			}
			return line, true, nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			if len(line) == 0 {
				return nil, false, nil
			}
			if len(line) > maxMemoryRecordBytes {
				return nil, false, ErrMemoryRecordTooLarge
			}
			return line, true, nil
		}
		return nil, false, err
	}
}

// indexDocument adds a document to the TF-IDF index. It must be called while
// holding the write lock (or before the store is shared).
func (s *FileMemoryStore) indexDocument(m Memory) {
	tokens := tokenize(m.Content)
	s.docLen[m.ID] = len(tokens)
	seen := make(map[string]struct{}, len(tokens))
	for _, t := range tokens {
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		s.termIdx[t] = append(s.termIdx[t], m.ID)
	}
}

// newMemoryID generates a unique memory ID using cryptographic randomness.
func newMemoryID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "mem_" + hex.EncodeToString(b[:]), nil
}

// tokenize lowercases alphanumeric runs, emitting a singleton for a one-rune
// Han run and overlapping bigrams for longer Han runs.
func tokenize(text string) []string {
	text = strings.ToLower(text)
	var tokens []string
	var run []rune
	runIsHan := false
	hanScript := unicode.Scripts["Han"]
	flush := func() {
		if len(run) == 0 {
			return
		}
		if runIsHan {
			if len(run) == 1 {
				tokens = append(tokens, string(run[0]))
			} else {
				for i := 0; i < len(run)-1; i++ {
					tokens = append(tokens, string(run[i:i+2]))
				}
			}
		} else {
			tokens = append(tokens, string(run))
		}
		run = nil
	}
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			isHan := unicode.Is(hanScript, r)
			if len(run) > 0 && isHan != runIsHan {
				flush()
			}
			run = append(run, r)
			runIsHan = isHan
		} else {
			flush()
		}
	}
	flush()
	return tokens
}

// buildFreq returns a token-to-count map for the given tokens.
func buildFreq(tokens []string) map[string]int {
	m := make(map[string]int, len(tokens))
	for _, t := range tokens {
		m[t]++
	}
	return m
}

// dedup returns the given tokens with duplicates removed, preserving order.
func dedup(tokens []string) []string {
	seen := make(map[string]struct{}, len(tokens))
	out := make([]string, 0, len(tokens))
	for _, t := range tokens {
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}
