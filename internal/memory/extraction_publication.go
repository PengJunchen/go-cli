package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Provenance identifies already completed conversation text, never execution
// authority. Source IDs and revision bind extraction to an immutable view.
type Provenance struct {
	WorkspaceID string   `json:"workspace_id"`
	SessionID   string   `json:"session_id"`
	BranchID    string   `json:"branch_id,omitempty"`
	Revision    uint64   `json:"revision,omitempty"`
	SourceIDs   []string `json:"source_ids"`
}

// Key returns a stable processed-source identity, including zero-fact jobs.
func (p Provenance) Key() string {
	data, _ := json.Marshal(p)
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

type extractionRecord struct {
	Memory
	Facts []Memory `json:"facts,omitempty"`
}

// Processed checks fresh cross-process state before spending a model request.
func (s *FileMemoryStore) Processed(ctx context.Context, source Provenance) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.acquireFreshLocked(ctx, true)
	if err != nil {
		return false, err
	}
	defer unlock()
	_, exists := s.items["processed-"+source.Key()]
	return exists, nil
}

// PublishExtraction atomically syncs facts and their processed marker in one
// bounded JSONL record. A failed append publishes neither. Eligibility is
// checked after acquiring the publication lock, including cancellation waits.
func (s *FileMemoryStore) PublishExtraction(ctx context.Context, source Provenance, facts []Memory, eligible func() bool) (bool, error) {
	source.SourceIDs = append([]string(nil), source.SourceIDs...)
	if source.WorkspaceID == "" || source.SessionID == "" || len(source.SourceIDs) == 0 {
		return false, errors.New("memory: extraction provenance is incomplete")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.acquireFreshLocked(ctx, true)
	if err != nil {
		return false, err
	}
	defer unlock()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if eligible != nil && !eligible() {
		return false, nil
	}
	key := source.Key()
	markerID := "processed-" + key
	if _, exists := s.items[markerID]; exists {
		return false, nil
	}
	seen := make(map[string]bool)
	for _, previous := range s.items {
		if previous.WorkspaceID == source.WorkspaceID && previous.ProcessedSource == "" {
			seen[previous.Content] = true
		}
	}
	now := time.Now().UTC()
	var accepted []Memory
	for _, fact := range facts {
		if fact.Content == "" || len(fact.Content) > 2048 || seen[fact.Content] || len(accepted) >= 16 {
			continue
		}
		seen[fact.Content] = true
		id := sha256.Sum256([]byte(source.WorkspaceID + "\x00" + fact.Content))
		fact.ID = "auto-" + hex.EncodeToString(id[:])
		fact.WorkspaceID, fact.SourceKey, fact.Source, fact.ProcessedSource = source.WorkspaceID, key, "auto", ""
		fact.CreatedAt, fact.UpdatedAt = now, now
		accepted = append(accepted, fact)
	}
	marker := Memory{ID: markerID, WorkspaceID: source.WorkspaceID, ProcessedSource: key, Source: "processed", CreatedAt: now, UpdatedAt: now, Origin: &source}
	record := extractionRecord{Memory: marker, Facts: accepted}
	data, err := json.Marshal(record)
	if err != nil {
		return false, fmt.Errorf("memory: encode extraction: %w", err)
	}
	if len(data) > maxMemoryRecordBytes {
		return false, ErrMemoryRecordTooLarge
	}
	info, err := s.file.Stat()
	if err != nil {
		return false, err
	}
	if err := writeMemoryFileAll(s.fileIO.write, s.file, append(data, '\n')); err != nil {
		return false, s.rollbackAppendLocked(info.Size(), err)
	}
	if err := s.fileIO.sync(s.file); err != nil {
		return false, s.rollbackAppendLocked(info.Size(), err)
	}
	s.items[marker.ID] = marker
	for _, fact := range accepted {
		s.items[fact.ID] = fact
		s.indexDocument(fact)
	}
	s.rememberSnapshotLocked()
	return true, nil
}
