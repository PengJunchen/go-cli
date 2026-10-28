package session

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// LegacySessionInspection is a bounded, read-only validation result. No
// execution authority, schema upgrade or source repair is implied.
type LegacySessionInspection struct {
	SHA256     string
	EntryCount int
	LeafCount  int
}

// InspectLegacySession captures and strictly validates a legacy file read-only.
func InspectLegacySession(ctx context.Context, path string) (LegacySessionInspection, error) {
	_, _, inspection, err := captureLegacySession(ctx, path)
	return inspection, err
}

func captureLegacySession(ctx context.Context, path string) ([]byte, []SessionEntry, LegacySessionInspection, error) {
	if err := contextErr(ctx); err != nil {
		return nil, nil, LegacySessionInspection{}, err
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, nil, LegacySessionInspection{}, err
	}
	file, err := openSessionFileNoFollow(filepath.Dir(absolute), filepath.Base(absolute), os.O_RDONLY, 0)
	if err != nil {
		return nil, nil, LegacySessionInspection{}, err
	}
	defer func() { _ = file.Close() }()
	locked, err := trySessionReadLock(file)
	if err != nil {
		return nil, nil, LegacySessionInspection{}, err
	}
	if !locked {
		return nil, nil, LegacySessionInspection{}, errors.New("session: legacy source is busy")
	}
	defer func() { _ = flockUnlock(file) }()
	raw, err := io.ReadAll(io.LimitReader(file, maxSessionInspectionBytes+1))
	if err != nil {
		return nil, nil, LegacySessionInspection{}, err
	}
	if len(raw) > maxSessionInspectionBytes {
		return nil, nil, LegacySessionInspection{}, errors.New("session: legacy source exceeds byte limit")
	}
	rows, err := readSessionInspection(ctx, bytes.NewReader(raw))
	if err != nil {
		return nil, nil, LegacySessionInspection{}, err
	}
	if len(rows) == 0 {
		return nil, nil, LegacySessionInspection{}, errors.New("session: legacy source is empty")
	}
	// Import accepts exactly the transcript fields emitted by the original
	// version-zero schema. A later execution field remains non-legacy evidence
	// even when its value is null or zero; never silently discard or downgrade it.
	decoder := json.NewDecoder(bytes.NewReader(raw))
	for index := range rows {
		var rawRow json.RawMessage
		if err := decoder.Decode(&rawRow); err != nil {
			return nil, nil, LegacySessionInspection{}, err
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(rawRow, &fields); err != nil {
			return nil, nil, LegacySessionInspection{}, err
		}
		for field := range fields {
			switch field {
			case "id", "parent_id", "type", "content", "content_blocks", "timestamp", "summary", "is_summary", "tool_calls", "tool_call_id", "tool_name", "surface_op", "seq":
			default:
				return nil, nil, LegacySessionInspection{}, fmt.Errorf("session: field %q is outside the legacy transcript schema; execution/version evidence requires its original durable session", field)
			}
		}
		strict := json.NewDecoder(bytes.NewReader(rawRow))
		strict.DisallowUnknownFields()
		if err := strict.Decode(&rows[index]); err != nil {
			return nil, nil, LegacySessionInspection{}, err
		}
		switch rows[index].Type {
		case EntryTypeUser, EntryTypeAssistant, EntryTypeTool, EntryTypeCompaction, EntryTypeSystem:
		default:
			return nil, nil, LegacySessionInspection{}, errors.New("session: entry type is outside the legacy transcript schema")
		}
	}
	byID, _, _, _, _, err := validateLoadedDurableEntries(rows, "legacy-inspection")
	if err != nil {
		return nil, nil, LegacySessionInspection{}, err
	}
	parents := make(map[string]bool)
	for _, row := range rows {
		if row.ParentID != "" {
			parents[row.ParentID] = true
		}
	}
	leaves := 0
	for _, row := range rows {
		if parents[row.ID] {
			continue
		}
		leaves++
		branch, err := orderedDurableBranch(row.ID, byID)
		if err != nil {
			return nil, nil, LegacySessionInspection{}, err
		}
		if err := validateProjectedToolGroups(branch); err != nil {
			return nil, nil, LegacySessionInspection{}, fmt.Errorf("session: incomplete legacy branch %s: %w", row.ID, err)
		}
	}
	digest := sha256.Sum256(raw)
	return raw, rows, LegacySessionInspection{SHA256: hex.EncodeToString(digest[:]), EntryCount: len(rows), LeafCount: leaves}, nil
}

// ImportLegacySession publishes immutable legacy bytes and a provenance row
// into a new no-clobber physical file. Version-zero transcript values (with missing sequence numbers normalized) are never
// passed through the version-one execution writer or given invented receipts.
func ImportLegacySession(ctx context.Context, source, directory, physicalID string) (string, error) {
	_, rows, inspection, err := captureLegacySession(ctx, source)
	if err != nil {
		return "", err
	}
	if physicalID == "" {
		physicalID, err = randomPhysicalSessionID()
		if err != nil {
			return "", err
		}
	}
	if !validPhysicalSessionID(physicalID) {
		return "", errors.New("session: invalid import physical session ID")
	}
	maxSeq := uint64(0)
	for index := range rows {
		if rows[index].Seq == 0 {
			rows[index].Seq = maxSeq + 1
		}
		if rows[index].Seq <= maxSeq {
			return "", errors.New("session: legacy sequences cannot be normalized without changing explicit evidence")
		}
		maxSeq = rows[index].Seq
	}
	provenance := SessionEntry{ID: "import-" + physicalID, ParentID: rows[len(rows)-1].ID, Type: EntryTypeSystem, SurfaceOp: SurfaceOpHidden, Timestamp: time.Now().UTC(), Seq: maxSeq + 1, SessionLabel: "legacy import", Content: fmt.Sprintf("Legacy transcript imported from %s; source_sha256=%s", filepath.Base(source), inspection.SHA256)}
	var payload bytes.Buffer
	for _, row := range rows {
		if err := json.NewEncoder(&payload).Encode(row); err != nil {
			return "", err
		}
	}
	if err := json.NewEncoder(&payload).Encode(provenance); err != nil {
		return "", err
	}
	candidate := append(cloneSessionEntries(rows), provenance)
	if _, err := snapshotFromInspectedRows(candidate, physicalID); err != nil {
		return "", fmt.Errorf("session: import cannot preserve this transcript: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	sessionsDir := filepath.Join(directory, "sessions")
	if err := mkdirDurably(sessionsDir, 0700); err != nil {
		return "", err
	}
	if err := publishImportedSession(ctx, sessionsDir, physicalID+".jsonl", payload.Bytes()); err != nil {
		return "", err
	}
	return physicalID, nil
}
