package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/tracing"
)

// ErrHistoryChanged reports stale administrative session coordinates.
var ErrHistoryChanged = errors.New("session: history changed before administration")

// HistoryCoordinates identifies the complete journal state, including hidden rows.
type HistoryCoordinates struct {
	SessionID   string
	BranchID    string
	LeafEntryID string
	MaxSeq      uint64
}

// HistoryCoordinatesFromSnapshot captures a compare-and-swap token.
func HistoryCoordinatesFromSnapshot(snapshot PreparedSessionSnapshot) HistoryCoordinates {
	return HistoryCoordinates{snapshot.SessionID, snapshot.ActiveBranchID, snapshot.ActiveLeafEntryID, snapshot.MaxSeq}
}

// HistoryEditKind selects one closed administrative operation.
type HistoryEditKind string

const (
	// HistoryFork creates an independent continuation sharing an immutable prefix.
	HistoryFork HistoryEditKind = "fork"
	// HistorySelect selects an existing complete branch position.
	HistorySelect HistoryEditKind = "select"
	// HistoryCompact summarizes an exact complete source prefix.
	HistoryCompact HistoryEditKind = "compact"
)

// HistoryEdit describes a bounded administrative operation without execution authority.
type HistoryEdit struct {
	Expected       HistoryCoordinates
	Kind           HistoryEditKind
	TargetEntryID  string
	TargetBranchID string
	NewBranchID    string
	GitBranch      string
	Summary        string
}

// HistoryEditor applies an administrative change with validation and CAS under
// the same store mutex as journal append. Callers must additionally own idle
// runtime admission; this capability never starts or reconciles tool actions.
type HistoryEditor interface {
	EditHistory(context.Context, HistoryEdit) (PreparedSessionSnapshot, error)
}

var _ HistoryEditor = (*durablePreparedSession)(nil)

// EditHistory syncs a validated administrative checkpoint before publishing it.
func (p *durablePreparedSession) EditHistory(ctx context.Context, edit HistoryEdit) (PreparedSessionSnapshot, error) {
	span, ctx := tracing.SpanFromContext(ctx, "session.history.edit", tracing.SpanKindInternal)
	defer span.End()
	if err := contextErr(ctx); err != nil {
		return PreparedSessionSnapshot{}, err
	}
	if p == nil || p.store == nil {
		return PreparedSessionSnapshot{}, ErrDurableWriterClosed
	}
	s := p.store
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot, err := s.snapshotLocked()
	if err != nil {
		return PreparedSessionSnapshot{}, err
	}
	if edit.Expected != HistoryCoordinatesFromSnapshot(snapshot) {
		return PreparedSessionSnapshot{}, ErrHistoryChanged
	}
	if err := ValidateHistoryAdministration(snapshot); err != nil {
		return PreparedSessionSnapshot{}, err
	}
	currentRows, err := orderedDurableBranch(s.activeLeafID, s.entries)
	if err != nil {
		return PreparedSessionSnapshot{}, err
	}
	if _, err := durableSessionView(s.sessionID, s.activeBranchID, s.maxSeq, currentRows); err != nil {
		return PreparedSessionSnapshot{}, fmt.Errorf("session: incomplete current history: %w", err)
	}
	target, ok := s.entries[edit.TargetEntryID]
	if !ok {
		return PreparedSessionSnapshot{}, ErrLeafNotFound
	}
	if target.Type == EntryTypeTool && target.ActionState != "" {
		return PreparedSessionSnapshot{}, errors.New("session: an action journal row is not a navigable history position")
	}
	targetBranch := target.BranchID
	if targetBranch == "" {
		targetBranch = "main"
	}
	if edit.TargetBranchID != "" {
		targetBranch = edit.TargetBranchID
	}
	sourceRows, err := orderedDurableBranch(target.ID, s.entries)
	if err != nil {
		return PreparedSessionSnapshot{}, err
	}
	if _, err := durableSessionView(s.sessionID, targetBranch, s.maxSeq, sourceRows); err != nil {
		return PreparedSessionSnapshot{}, fmt.Errorf("session: incomplete target history: %w", err)
	}
	operationID, err := randomPhysicalSessionID()
	if err != nil {
		return PreparedSessionSnapshot{}, err
	}
	row := SessionEntry{ID: "admin-" + operationID, Type: EntryTypeCheckpoint, Timestamp: time.Now().UTC(), BranchID: targetBranch}
	switch edit.Kind {
	case HistoryFork:
		if strings.TrimSpace(edit.NewBranchID) == "" || len(edit.NewBranchID) > 128 || strings.ContainsAny(edit.NewBranchID, "/\\\x00\r\n\t ") {
			return PreparedSessionSnapshot{}, errors.New("session: invalid new branch ID")
		}
		row.ParentID = target.ID
		row.BranchID = edit.NewBranchID
		row.ParentBranchID = targetBranch
		row.BaseLeafEntryID = target.ID
		row.ActiveLeafEntryID = row.ID
		row.CheckpointKind = CheckpointBranch
		row.SessionGitBranch = edit.GitBranch
	case HistorySelect:
		row.ParentID = target.ID
		row.ActiveLeafEntryID = target.ID
		row.CheckpointKind = CheckpointActiveLeaf
	case HistoryCompact:
		if strings.TrimSpace(edit.Summary) == "" || len(edit.Summary) > maxDurableJSONLineBytes/2 {
			return PreparedSessionSnapshot{}, errors.New("session: compaction requires a bounded non-empty summary")
		}
		if targetBranch != s.activeBranchID {
			return PreparedSessionSnapshot{}, errors.New("session: compaction source is on a different branch")
		}
		found := false
		for _, entry := range currentRows {
			if entry.ID == target.ID {
				found = true
			}
		}
		if !found {
			return PreparedSessionSnapshot{}, errors.New("session: compaction source is not on the active history")
		}
		row.ParentID = s.activeLeafID
		row.Type = EntryTypeCompaction
		row.Content = edit.Summary
		row.Summary = edit.Summary
		row.CompactedThroughEntryID = target.ID
	default:
		return PreparedSessionSnapshot{}, errors.New("session: unsupported history operation")
	}
	// Validate the full candidate projection before append: ordinary journal
	// writes intentionally permit an open tool group, administrative edits do not.
	candidate := make(map[string]*SessionEntry, len(s.entries)+1)
	for id, entry := range s.entries {
		candidate[id] = entry
	}
	preview := *row.clone()
	preview.RecordVersion = 1
	preview.SessionID = s.sessionID
	preview.Seq = s.maxSeq + 1
	candidate[preview.ID] = &preview
	leaf := preview.ID
	if edit.Kind == HistorySelect {
		leaf = preview.ActiveLeafEntryID
	}
	ordered, err := orderedDurableBranch(leaf, candidate)
	if err != nil {
		return PreparedSessionSnapshot{}, err
	}
	if _, err := durableSessionView(s.sessionID, row.BranchID, s.maxSeq+1, ordered); err != nil {
		return PreparedSessionSnapshot{}, fmt.Errorf("session: invalid administrative projection: %w", err)
	}
	if _, err = s.appendAndSyncLocked(ctx, row); err != nil {
		return PreparedSessionSnapshot{}, err
	}
	return s.snapshotLocked()
}

// ValidateHistoryAdministration rejects unresolved or incomplete owning runs
// without interpreting transcript text as completion evidence.
func ValidateHistoryAdministration(snapshot PreparedSessionSnapshot) error {
	pending, err := InspectPendingActions(snapshot)
	if err != nil {
		return err
	}
	if len(pending) > 0 {
		return errors.New("session: unresolved actions prevent history changes")
	}
	runs := make(map[string]bool)
	terminals := make(map[string]bool)
	for _, row := range snapshot.DurableEntries {
		if row.RecordVersion == 1 && (row.Type == EntryTypeUser || row.Type == EntryTypeAssistant) && row.RunID != "" {
			runs[row.RunID] = true
		}
		if row.CheckpointKind == CheckpointRunTerminal {
			if row.RunOutcome == "" || row.RunOutcome == core.RunOutcomeUnknown {
				return errors.New("session: unknown run outcome prevents history changes")
			}
			terminals[row.RunID] = true
		}
	}
	for run := range runs {
		if !terminals[run] {
			return errors.New("session: incomplete owning run prevents history changes")
		}
	}
	return nil
}
