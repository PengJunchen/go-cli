package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// HistoryStore manages command history with optional JSONL persistence.
// It is a simple in-memory ring buffer that can optionally load from and save
// to a file in JSONL format (one JSON-encoded string per line).
type HistoryStore struct {
	mu       sync.RWMutex
	entries  []string
	maxLen   int
	filePath string
}

// NewHistoryStore creates a HistoryStore with the given maximum length and
// optional file path. If filePath is empty, no persistence is used.
func NewHistoryStore(maxLen int, filePath string) *HistoryStore {
	if maxLen <= 0 {
		maxLen = 1000
	}
	return &HistoryStore{
		maxLen:   maxLen,
		filePath: filePath,
	}
}

// Add appends an entry to the history. Empty strings and consecutive
// duplicates are skipped. When the buffer exceeds maxLen, the oldest entry
// is evicted (FIFO).
func (h *HistoryStore) Add(entry string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if strings.TrimSpace(entry) == "" {
		return
	}
	if len(h.entries) > 0 && h.entries[len(h.entries)-1] == entry {
		return
	}
	h.entries = append(h.entries, entry)
	if len(h.entries) > h.maxLen {
		h.entries = h.entries[len(h.entries)-h.maxLen:]
	}
}

// Set replaces all history entries. The slice is copied.
func (h *HistoryStore) Set(entries []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.entries = make([]string, len(entries))
	copy(h.entries, entries)
	if len(h.entries) > h.maxLen {
		h.entries = h.entries[len(h.entries)-h.maxLen:]
	}
}

// List returns a copy of the current history entries.
func (h *HistoryStore) List() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()

	out := make([]string, len(h.entries))
	copy(out, h.entries)
	return out
}

// Save writes the history to the configured file in JSONL format. If no file
// path is set, it is a no-op. The parent directory is created if necessary.
func (h *HistoryStore) Save() error {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if h.filePath == "" {
		return nil
	}
	dir := filepath.Dir(h.filePath)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0750); err != nil {
			return err
		}
	}
	var buf bytes.Buffer
	for _, entry := range h.entries {
		b, err := json.Marshal(entry)
		if err != nil {
			continue
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	return os.WriteFile(h.filePath, buf.Bytes(), 0o600)
}

// Load reads the history from the configured file. If no file path is set or
// the file does not exist, it is a no-op. Existing entries are replaced.
func (h *HistoryStore) Load() error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.filePath == "" {
		return nil
	}
	data, err := os.ReadFile(h.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var loaded []string
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var entry string
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}
		loaded = append(loaded, entry)
	}
	if len(loaded) > h.maxLen {
		loaded = loaded[len(loaded)-h.maxLen:]
	}
	h.entries = loaded
	return nil
}
