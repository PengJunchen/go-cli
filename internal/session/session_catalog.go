package session

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const maxSessionInspectionBytes = 64 << 20
const maxSessionInspectionRows = 100000
const maxSessionCatalogFiles = 4096

// CatalogSession describes persisted history without opening a mutable session.
type CatalogSession struct {
	SessionID  string
	Label      string
	BranchID   string
	Preview    string
	EntryCount int
	Modified   time.Time
	Status     string
	Diagnostic string
}

// ListSessionCatalog inspects bounded files read-only. It never repairs tails,
// creates directories or opens a prepared writer. Busy/invalid entries remain
// visible so a compatibility resume cannot silently skip a failed latest file.
func ListSessionCatalog(ctx context.Context, directory string) ([]CatalogSession, error) {
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	directory = filepath.Join(directory, "sessions")
	root, err := openSessionDirectoryNoFollow(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	files, err := root.ReadDir(maxSessionCatalogFiles + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(files) > maxSessionCatalogFiles {
		return nil, errors.New("session: catalog file limit exceeded")
	}
	remainingBytes := int64(maxSessionInspectionBytes)
	var result []CatalogSession
	for _, entry := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		item := CatalogSession{SessionID: strings.TrimSuffix(entry.Name(), ".jsonl"), Status: "invalid"}
		file, err := openSessionFileNoFollow(directory, entry.Name(), os.O_RDONLY, 0)
		if err != nil {
			item.Diagnostic = err.Error()
			result = append(result, item)
			continue
		}
		info, statErr := file.Stat()
		if statErr != nil {
			item.Diagnostic = statErr.Error()
			_ = file.Close()
			result = append(result, item)
			continue
		}
		item.Modified = info.ModTime()
		locked, lockErr := trySessionReadLock(file)
		if lockErr != nil {
			item.Diagnostic = lockErr.Error()
		} else if !locked {
			item.Status = "busy"
		} else if info.Size() > remainingBytes {
			item.Status = "limited"
			item.Diagnostic = "catalog inspection byte budget exceeded"
			_ = flockUnlock(file)
		} else {
			remainingBytes -= info.Size()
			rows, readErr := readSessionInspection(ctx, file)
			if readErr == nil && validPhysicalSessionID(item.SessionID) {
				snapshot, inspectErr := snapshotFromInspectedRows(rows, item.SessionID)
				if inspectErr == nil {
					item.Status = "ready"
					item.Label = catalogPreview(snapshot.SessionLabel)
					item.BranchID = snapshot.ActiveBranchID
					item.EntryCount = len(rows)
					if err := ValidateHistoryAdministration(snapshot); err != nil {
						item.Status = "unresolved"
						item.Diagnostic = err.Error()
					}
					for _, row := range rows {
						if row.Type == EntryTypeUser {
							item.Preview = catalogPreview(row.Content)
							break
						}
					}
				} else {
					item.Diagnostic = inspectErr.Error()
				}
			} else if readErr != nil {
				item.Diagnostic = readErr.Error()
			}
			_ = flockUnlock(file)
		}
		_ = file.Close()
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Modified.Equal(result[j].Modified) {
			return result[i].SessionID < result[j].SessionID
		}
		return result[i].Modified.After(result[j].Modified)
	})
	return result, nil
}

func catalogPreview(content string) string {
	count := 0
	for index := range content {
		if count == 120 {
			return strings.Clone(content[:index])
		}
		count++
	}
	return strings.Clone(content)
}

func readSessionInspection(ctx context.Context, reader io.Reader) ([]SessionEntry, error) {
	limited := &io.LimitedReader{R: reader, N: maxSessionInspectionBytes + 1}
	scanner := bufio.NewReaderSize(limited, 64*1024)
	var rows []SessionEntry
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		line, err := readInspectionLine(scanner)
		if len(line) > maxDurableJSONLineBytes {
			return nil, errors.New("session: inspection line limit exceeded")
		}
		if limited.N <= 0 {
			return nil, errors.New("session: inspection byte limit exceeded")
		}
		if len(line) > 0 {
			if err != nil {
				return nil, errors.New("session: unterminated session row; inspection does not repair it")
			}
			if len(bytes.TrimSpace(line)) == 0 {
				return nil, errors.New("session: empty session row")
			}
			var row SessionEntry
			if jsonErr := json.Unmarshal(line, &row); jsonErr != nil {
				return nil, fmt.Errorf("session: invalid JSON row: %w", jsonErr)
			}
			if row.ID == "" {
				return nil, errors.New("session: row has no ID")
			}
			rows = append(rows, row)
			if len(rows) > maxSessionInspectionRows {
				return nil, errors.New("session: inspection row limit exceeded")
			}
		}
		if errors.Is(err, io.EOF) {
			return rows, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

func readInspectionLine(reader *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		part, err := reader.ReadSlice('\n')
		if len(line)+len(part) > maxDurableJSONLineBytes {
			return nil, errors.New("session: inspection line limit exceeded")
		}
		line = append(line, part...)
		if err == bufio.ErrBufferFull {
			continue
		}
		return line, err
	}
}

func snapshotFromInspectedRows(rows []SessionEntry, physicalID string) (PreparedSessionSnapshot, error) {
	byID, maxSeq, label, branch, leaf, err := validateLoadedDurableEntries(rows, physicalID)
	if err != nil {
		return PreparedSessionSnapshot{}, err
	}
	ordered, err := orderedDurableBranch(leaf, byID)
	if err != nil {
		return PreparedSessionSnapshot{}, err
	}
	view, err := durableSessionView(physicalID, branch, maxSeq, ordered)
	if err != nil {
		return PreparedSessionSnapshot{}, err
	}
	if _, err := analyzeDurableActionJournal(rows, physicalID); err != nil {
		return PreparedSessionSnapshot{}, err
	}
	snapshot := PreparedSessionSnapshot{SessionID: physicalID, SessionLabel: label, DurableEntries: cloneSessionEntries(rows), ActiveBranchID: branch, ActiveLeafEntryID: leaf, MaxSeq: maxSeq, PublishedView: view}
	if _, err := NewDefaultSessionTreeBuilder().BuildFromSnapshot(context.Background(), snapshot); err != nil {
		return PreparedSessionSnapshot{}, err
	}
	return snapshot, nil
}

// InspectPhysicalSession reads one exact file without recovery or repair.
func InspectPhysicalSession(ctx context.Context, directory, physicalID string) (PreparedSessionSnapshot, error) {
	if !validPhysicalSessionID(physicalID) {
		return PreparedSessionSnapshot{}, errors.New("session: invalid physical session ID")
	}
	sessionsDir := filepath.Join(directory, "sessions")
	filename, err := resolvePhysicalSessionFilename(sessionsDir, physicalID)
	if err != nil {
		return PreparedSessionSnapshot{}, err
	}
	file, err := openSessionFileNoFollow(sessionsDir, filename, os.O_RDONLY, 0)
	if err != nil {
		return PreparedSessionSnapshot{}, err
	}
	defer func() { _ = file.Close() }()
	locked, err := trySessionReadLock(file)
	if err != nil {
		return PreparedSessionSnapshot{}, err
	}
	if !locked {
		return PreparedSessionSnapshot{}, errors.New("session: physical session is busy")
	}
	defer func() { _ = flockUnlock(file) }()
	rows, err := readSessionInspection(ctx, file)
	if err != nil {
		return PreparedSessionSnapshot{}, err
	}
	return snapshotFromInspectedRows(rows, physicalID)
}

// LatestPhysicalSession selects deterministically and never skips a failed latest file.
func LatestPhysicalSession(ctx context.Context, directory string) (string, error) {
	entries, err := ListSessionCatalog(ctx, directory)
	if err != nil {
		return "", err
	}
	if len(entries) == 0 {
		return "", errors.New("session: no previous physical session; use --resume-session <physical-id> after creating one")
	}
	if entries[0].Status != "ready" {
		return "", fmt.Errorf("session: latest physical session %s is %s; inspect it explicitly", entries[0].SessionID, entries[0].Status)
	}
	return entries[0].SessionID, nil
}

// PhysicalSessionForEntry resolves historical bare-entry fork arguments only
// when all inspected candidates prove a unique physical source.
func PhysicalSessionForEntry(ctx context.Context, directory, entryID string) (string, error) {
	entries, err := ListSessionCatalog(ctx, directory)
	if err != nil {
		return "", err
	}
	var found string
	for _, entry := range entries {
		if entry.Status != "ready" {
			return "", fmt.Errorf("session: cannot resolve bare entry across %s session %s; specify --resume-session", entry.Status, entry.SessionID)
		}
		snapshot, err := InspectPhysicalSession(ctx, directory, entry.SessionID)
		if err != nil {
			return "", err
		}
		for _, row := range snapshot.DurableEntries {
			if row.ID == entryID {
				if found != "" {
					return "", errors.New("session: ambiguous entry ID; specify --resume-session")
				}
				found = entry.SessionID
				break
			}
		}
	}
	if found == "" {
		return "", errors.New("session: fork source entry not found; specify --resume-session <physical-id>")
	}
	return found, nil
}
