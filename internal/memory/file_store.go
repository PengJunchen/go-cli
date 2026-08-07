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
	"math"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

const memoryFilePerm = 0o600

// ErrMemoryNotFound is returned when a memory ID does not exist.
var ErrMemoryNotFound = errors.New("memory: not found")

// FileMemoryStore implements MemoryStore backed by a JSONL file. Entries are
// appended on Add and kept in memory for fast lookup and TF-IDF search. It is
// concurrency-safe.
type FileMemoryStore struct {
	path  string
	mu    sync.RWMutex
	items map[string]Memory
	file  *os.File
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
	}
	if err := s.load(); err != nil {
		return nil, fmt.Errorf("memory: load: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, memoryFilePerm)
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

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var m Memory
		if err := json.Unmarshal(line, &m); err != nil {
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
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("memory: read store file: %w", err)
	}
	return nil
}

// Add stores a memory entry. If mem.ID is empty a unique ID is generated. The
// entry is appended to the JSONL file and added to the in-memory index.
func (s *FileMemoryStore) Add(_ context.Context, mem Memory) error {
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

	if s.file == nil {
		return errors.New("memory: store is closed")
	}

	if _, exists := s.items[mem.ID]; exists {
		return fmt.Errorf("memory: entry %q already exists", mem.ID)
	}

	data, err := json.Marshal(mem)
	if err != nil {
		return fmt.Errorf("memory: encode entry: %w", err)
	}
	data = append(data, '\n')
	if _, err := s.file.Write(data); err != nil {
		return fmt.Errorf("memory: write entry: %w", err)
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
func (s *FileMemoryStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.items[id]; !ok {
		return ErrMemoryNotFound
	}
	delete(s.items, id)

	// Rebuild the search index from the remaining items.
	s.termIdx = make(map[string][]string)
	s.docLen = make(map[string]int)
	for _, m := range s.items {
		s.indexDocument(m)
	}
	return s.rewriteFileLocked()
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
		idf := math.Log(float64(n) / float64(1+df))
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
	if s.file == nil {
		return nil
	}
	err := s.file.Close()
	s.file = nil
	return err
}

// rewriteFileLocked truncates and rewrites the entire JSONL file from the
// in-memory items, then reopens the file for appending. It must be called while
// holding the write lock.
func (s *FileMemoryStore) rewriteFileLocked() error {
	if err := s.file.Close(); err != nil {
		return fmt.Errorf("memory: close for rewrite: %w", err)
	}
	var buf bytes.Buffer
	for _, m := range s.items {
		data, err := json.Marshal(m)
		if err != nil {
			return fmt.Errorf("memory: encode entry: %w", err)
		}
		buf.Write(data)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(s.path, buf.Bytes(), memoryFilePerm); err != nil {
		return fmt.Errorf("memory: rewrite file: %w", err)
	}
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, memoryFilePerm)
	if err != nil {
		return fmt.Errorf("memory: reopen store file: %w", err)
	}
	s.file = f
	return nil
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

// tokenize splits text into lowercase alphanumeric tokens.
func tokenize(text string) []string {
	text = strings.ToLower(text)
	var tokens []string
	var b strings.Builder
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else if b.Len() > 0 {
			tokens = append(tokens, b.String())
			b.Reset()
		}
	}
	if b.Len() > 0 {
		tokens = append(tokens, b.String())
	}
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
