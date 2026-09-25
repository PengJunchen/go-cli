package session

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// EntryLister is an optional interface implemented by stores that can enumerate
// all stored entries. The tree builder uses it to reconstruct a SessionTree from
// persistent storage.
type EntryLister interface {
	List(ctx context.Context) ([]*SessionEntry, error)
}

var _ EntryLister = (*MemoryStore)(nil)

// SessionTreeBuilder builds a SessionTree from a SessionStore.
type SessionTreeBuilder interface {
	BuildFromStore(ctx context.Context, store SessionStore) (SessionTree, error)
}

// DefaultSessionTreeBuilder implements SessionTreeBuilder by reading all entries
// from the store and linking them via ParentID.
type DefaultSessionTreeBuilder struct{}

var _ SessionTreeBuilder = (*DefaultSessionTreeBuilder)(nil)

// NewDefaultSessionTreeBuilder returns a DefaultSessionTreeBuilder.
func NewDefaultSessionTreeBuilder() *DefaultSessionTreeBuilder {
	return &DefaultSessionTreeBuilder{}
}

// BuildFromStore reads all entries from the store and builds a SessionTree by
// linking entries via ParentID. The current leaf is set to the entry with the
// latest timestamp (i.e. the most recently appended entry). If the store is
// empty or the backing file does not exist, an empty tree is returned.
// Corrupted or orphaned entries are skipped so the rest of the tree is still
// rebuilt.
func (b *DefaultSessionTreeBuilder) BuildFromStore(ctx context.Context, store SessionStore) (SessionTree, error) {
	if store == nil {
		return NewDefaultSessionTree(), nil
	}

	lister, ok := store.(EntryLister)
	if !ok {
		return nil, fmt.Errorf("session: store %T does not implement EntryLister", store)
	}

	entries, err := lister.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("session: list entries from store: %w", err)
	}
	return b.buildFromEntries(ctx, entries)
}

func (b *DefaultSessionTreeBuilder) buildFromEntries(ctx context.Context, entries []*SessionEntry) (SessionTree, error) {
	tree := NewDefaultSessionTree()
	if len(entries) == 0 {
		return tree, nil
	}

	// Version one files are canonical recovery input: fail closed on malformed,
	// duplicate, or orphan rows instead of rebuilding a partial history.
	hasVersionOne := false
	for _, entry := range entries {
		if entry != nil && entry.RecordVersion == 1 {
			hasVersionOne = true
			break
		}
	}
	if hasVersionOne {
		return b.buildVersionOneTree(ctx, entries, tree)
	}

	// Legacy version zero stores retain their tolerant rebuild behavior.
	entryMap := make(map[string]*SessionEntry, len(entries))
	for _, e := range entries {
		if e == nil || e.ID == "" || e.Type == "" {
			continue
		}
		if _, exists := entryMap[e.ID]; !exists {
			entryMap[e.ID] = e
		}
	}

	if len(entryMap) == 0 {
		return tree, nil
	}

	// Topologically order entries so parents are appended before children.
	// This guarantees tree.Append (which validates parent existence) succeeds
	// for well-formed data even when timestamps are identical.
	ordered := topoSortEntries(entryMap)

	var latestID string
	var latestTS time.Time

	for _, e := range ordered {
		if err := tree.Append(ctx, e); err != nil {
			// Skip entries that cannot be appended (e.g. parent missing due to
			// corruption). Continue building the rest of the tree.
			continue
		}
		if e.Timestamp.After(latestTS) {
			latestTS = e.Timestamp
			latestID = e.ID
		}
	}

	// Set the current leaf to the most recently appended entry.
	if latestID != "" {
		if err := tree.MoveTo(ctx, latestID); err != nil {
			return nil, fmt.Errorf("session: set current leaf %q: %w", latestID, err)
		}
	}

	return tree, nil
}

// BuildFromSnapshot builds the navigational tree from one coherent prepared
// session snapshot without reopening its backing store. Durable entries retain
// structural journal rows; callers must use snapshot.PublishedView, rather
// than rebuilding model history or replay authority from those rows.
func (b *DefaultSessionTreeBuilder) BuildFromSnapshot(ctx context.Context, snapshot PreparedSessionSnapshot) (SessionTree, error) {
	snapshot = snapshot.Clone()
	entriesByID, err := validatePreparedSnapshotForTree(snapshot)
	if err != nil {
		return nil, err
	}
	entries := make([]*SessionEntry, len(snapshot.DurableEntries))
	for index := range snapshot.DurableEntries {
		entries[index] = &snapshot.DurableEntries[index]
	}
	tree, err := b.buildFromEntries(ctx, entries)
	if err != nil {
		return nil, err
	}
	if snapshot.ActiveLeafEntryID != "" {
		if _, exists := entriesByID[snapshot.ActiveLeafEntryID]; !exists {
			return nil, fmt.Errorf("session: snapshot active leaf %q is not in durable entries", snapshot.ActiveLeafEntryID)
		}
		if err := tree.MoveTo(ctx, snapshot.ActiveLeafEntryID); err != nil {
			return nil, fmt.Errorf("session: restore snapshot active leaf %q: %w", snapshot.ActiveLeafEntryID, err)
		}
	}
	return tree, nil
}

func validatePreparedSnapshotForTree(snapshot PreparedSessionSnapshot) (map[string]*SessionEntry, error) {
	if snapshot.SessionID == "" {
		return nil, fmt.Errorf("session: snapshot requires a physical session ID")
	}
	if snapshot.ActiveBranchID == "" {
		return nil, fmt.Errorf("session: snapshot requires an active branch ID")
	}
	if err := snapshot.PublishedView.Validate(); err != nil {
		return nil, fmt.Errorf("session: invalid snapshot published view: %w", err)
	}
	if snapshot.PublishedView.SessionID != snapshot.SessionID {
		return nil, fmt.Errorf("session: snapshot view physical session %q does not match %q", snapshot.PublishedView.SessionID, snapshot.SessionID)
	}
	if snapshot.PublishedView.BranchID != snapshot.ActiveBranchID {
		return nil, fmt.Errorf("session: snapshot view branch %q does not match active branch %q", snapshot.PublishedView.BranchID, snapshot.ActiveBranchID)
	}

	entriesByID := make(map[string]*SessionEntry, len(snapshot.DurableEntries))
	entryBranches := make(map[string]string, len(snapshot.DurableEntries))
	branchParents := map[string]durableBranch{"main": {}}
	seqs := make(map[uint64]string, len(snapshot.DurableEntries))
	var maxSeq uint64
	seqsStarted := false
	rows := make([]SessionEntry, len(snapshot.DurableEntries))
	for index := range snapshot.DurableEntries {
		entry := snapshot.DurableEntries[index]
		if entry.ID == "" || !validSessionEntryType(entry.Type) {
			return nil, fmt.Errorf("session: snapshot has invalid durable entry at index %d", index)
		}
		if _, exists := entriesByID[entry.ID]; exists {
			return nil, fmt.Errorf("session: snapshot repeats durable entry ID %q", entry.ID)
		}
		if entry.RecordVersion > 1 {
			return nil, fmt.Errorf("session: snapshot entry %q has unsupported record version %d", entry.ID, entry.RecordVersion)
		}
		if entry.SessionID != "" && entry.SessionID != snapshot.SessionID {
			return nil, fmt.Errorf("session: snapshot entry %q belongs to physical session %q, not %q", entry.ID, entry.SessionID, snapshot.SessionID)
		}
		if entry.RecordVersion == 1 && entry.SessionID != snapshot.SessionID {
			return nil, fmt.Errorf("session: version 1 snapshot entry %q has no matching physical session ID", entry.ID)
		}
		if entry.ParentID != "" {
			if _, exists := entriesByID[entry.ParentID]; !exists {
				return nil, fmt.Errorf("session: snapshot entry %q has missing or forward parent %q", entry.ID, entry.ParentID)
			}
		} else if index > 0 && entry.RecordVersion == 1 {
			return nil, fmt.Errorf("session: version 1 snapshot entry %q introduces a second root", entry.ID)
		}
		if err := validateVersion1EntryCoordinates(entry); err != nil {
			return nil, fmt.Errorf("session: invalid snapshot entry %q: %w", entry.ID, err)
		}
		if err := validateTreeCheckpointCoordinates(entry); err != nil {
			return nil, fmt.Errorf("session: invalid snapshot checkpoint %q: %w", entry.ID, err)
		}
		if entry.Seq == 0 {
			if seqsStarted {
				return nil, fmt.Errorf("session: snapshot entry %q has no sequence after sequenced data", entry.ID)
			}
		} else {
			if previous, exists := seqs[entry.Seq]; exists {
				return nil, fmt.Errorf("session: snapshot entries %q and %q repeat sequence %d", previous, entry.ID, entry.Seq)
			}
			if entry.Seq <= maxSeq {
				return nil, fmt.Errorf("session: snapshot sequence %d does not increase at entry %q", entry.Seq, entry.ID)
			}
			seqs[entry.Seq] = entry.ID
			maxSeq = entry.Seq
			seqsStarted = true
		}
		branchID := entry.BranchID
		if branchID == "" {
			branchID = "main"
		}
		entryBranches[entry.ID] = branchID
		if entry.RecordVersion == 1 && entry.Type == EntryTypeCheckpoint && entry.CheckpointKind == CheckpointBranch {
			branchParents[branchID] = durableBranch{parentBranch: entry.ParentBranchID}
		}
		entriesByID[entry.ID] = entry.clone()
		rows[index] = entry
	}
	if snapshot.MaxSeq != maxSeq {
		return nil, fmt.Errorf("session: snapshot maximum sequence %d does not match durable maximum %d", snapshot.MaxSeq, maxSeq)
	}
	if err := validateDurableBranchJournal(rows); err != nil {
		return nil, fmt.Errorf("session: invalid snapshot branch journal: %w", err)
	}
	if err := validateDurableTranscriptCoordinates(rows); err != nil {
		return nil, fmt.Errorf("session: invalid snapshot transcript coordinates: %w", err)
	}
	if _, exists := branchParents[snapshot.ActiveBranchID]; !exists {
		return nil, fmt.Errorf("session: snapshot active branch %q is not in durable branch metadata", snapshot.ActiveBranchID)
	}
	if snapshot.ActiveLeafEntryID == "" {
		if len(entriesByID) != 0 {
			return nil, fmt.Errorf("session: non-empty snapshot has no active leaf")
		}
		if len(snapshot.PublishedView.SourceEntryIDs) != 0 {
			return nil, fmt.Errorf("session: empty snapshot has published view sources")
		}
		return entriesByID, nil
	}
	leafBranch, exists := entryBranches[snapshot.ActiveLeafEntryID]
	if !exists {
		return nil, fmt.Errorf("session: snapshot active leaf %q is not in durable entries", snapshot.ActiveLeafEntryID)
	}
	if !durableBranchContains(branchParents, snapshot.ActiveBranchID, leafBranch) {
		return nil, fmt.Errorf("session: snapshot active leaf %q is outside active branch %q", snapshot.ActiveLeafEntryID, snapshot.ActiveBranchID)
	}
	for _, sourceID := range snapshot.PublishedView.SourceEntryIDs {
		source, exists := entriesByID[sourceID]
		if !exists {
			return nil, fmt.Errorf("session: snapshot published view source %q is not in durable entries", sourceID)
		}
		if _, ok := sessionEntryToAgentMessage(*source); !ok {
			return nil, fmt.Errorf("session: snapshot published view source %q is not model-visible", sourceID)
		}
		if !durableEntryOnPath(entriesByID, sourceID, snapshot.ActiveLeafEntryID) {
			return nil, fmt.Errorf("session: snapshot published view source %q is outside active leaf %q", sourceID, snapshot.ActiveLeafEntryID)
		}
	}
	return entriesByID, nil
}

func (b *DefaultSessionTreeBuilder) buildVersionOneTree(ctx context.Context, entries []*SessionEntry, tree SessionTree) (SessionTree, error) {
	entryMap := make(map[string]*SessionEntry, len(entries))
	seqIDs := make(map[uint64]string, len(entries))
	for index, entry := range entries {
		if entry == nil || entry.ID == "" || entry.Type == "" {
			return nil, fmt.Errorf("session: invalid versioned tree entry at index %d", index)
		}
		if _, exists := entryMap[entry.ID]; exists {
			return nil, fmt.Errorf("session: versioned tree repeats entry ID %q", entry.ID)
		}
		if entry.RecordVersion > 1 {
			return nil, fmt.Errorf("session: unsupported record version %d at entry %q", entry.RecordVersion, entry.ID)
		}
		if entry.RecordVersion == 1 {
			if err := validateVersion1EntryCoordinates(*entry); err != nil {
				return nil, fmt.Errorf("session: invalid version 1 entry %q: %w", entry.ID, err)
			}
			if err := validateTreeCheckpointCoordinates(*entry); err != nil {
				return nil, fmt.Errorf("session: invalid version 1 entry %q: %w", entry.ID, err)
			}
			if entry.Seq == 0 {
				return nil, fmt.Errorf("session: version 1 entry %q has no sequence", entry.ID)
			}
		}
		if entry.Seq != 0 {
			if previous, exists := seqIDs[entry.Seq]; exists {
				return nil, fmt.Errorf("session: entries %q and %q repeat sequence %d", previous, entry.ID, entry.Seq)
			}
			seqIDs[entry.Seq] = entry.ID
		}
		entryMap[entry.ID] = entry
	}

	for _, entry := range entryMap {
		if entry.ParentID != "" {
			parent, exists := entryMap[entry.ParentID]
			if !exists {
				return nil, fmt.Errorf("session: versioned tree entry %q has missing parent %q", entry.ID, entry.ParentID)
			}
			if parent.Seq != 0 && entry.Seq != 0 && parent.Seq >= entry.Seq {
				return nil, fmt.Errorf("session: entry %q sequence %d does not follow parent %q sequence %d", entry.ID, entry.Seq, parent.ID, parent.Seq)
			}
		}
		if entry.RecordVersion == 1 && entry.Type == EntryTypeCompaction {
			boundary, exists := entryMap[entry.CompactedThroughEntryID]
			if !exists || boundary.Seq >= entry.Seq {
				return nil, fmt.Errorf("session: compaction entry %q has an unmappable source boundary %q", entry.ID, entry.CompactedThroughEntryID)
			}
		}
		if entry.RecordVersion == 1 && entry.Type == EntryTypeCheckpoint && entry.ActiveLeafEntryID != "" && entry.ActiveLeafEntryID != entry.ID {
			activeLeaf, exists := entryMap[entry.ActiveLeafEntryID]
			if !exists || (activeLeaf.Seq != 0 && activeLeaf.Seq >= entry.Seq) {
				return nil, fmt.Errorf("session: checkpoint entry %q has an unmappable active leaf %q", entry.ID, entry.ActiveLeafEntryID)
			}
		}
	}

	ordered, err := strictTopoSortEntries(entryMap)
	if err != nil {
		return nil, err
	}
	branchJournal := make([]SessionEntry, len(ordered))
	for index, entry := range ordered {
		branchJournal[index] = *entry
	}
	if err := validateDurableBranchJournal(branchJournal); err != nil {
		return nil, fmt.Errorf("session: invalid recovered branch metadata: %w", err)
	}
	for _, entry := range ordered {
		if err := tree.Append(ctx, entry); err != nil {
			return nil, fmt.Errorf("session: append recovered entry %q: %w", entry.ID, err)
		}
	}
	concreteTree, ok := tree.(*DefaultSessionTree)
	if !ok {
		return nil, fmt.Errorf("session: versioned recovery requires a DefaultSessionTree, got %T", tree)
	}
	concreteTree.restoreBranchMetadataFromCheckpoints(ordered)

	activeLeafID := ""
	var activeCheckpointSeq uint64
	var latestSeq uint64
	var latestEntryID string
	for _, entry := range ordered {
		if entry.RecordVersion == 1 && entry.Type == EntryTypeCheckpoint && entry.ActiveLeafEntryID != "" && entry.Seq >= activeCheckpointSeq {
			activeLeafID = entry.ActiveLeafEntryID
			activeCheckpointSeq = entry.Seq
		}
		if entry.RecordVersion == 1 && entry.Seq >= latestSeq {
			latestSeq = entry.Seq
			latestEntryID = entry.ID
		}
	}
	if activeCheckpointSeq < latestSeq {
		activeLeafID = latestEntryID
	}
	if activeLeafID != "" {
		if err := tree.MoveTo(ctx, activeLeafID); err != nil {
			return nil, fmt.Errorf("session: restore active leaf %q: %w", activeLeafID, err)
		}
	}
	return tree, nil
}

func strictTopoSortEntries(entryMap map[string]*SessionEntry) ([]*SessionEntry, error) {
	children := make(map[string][]*SessionEntry, len(entryMap))
	indegree := make(map[string]int, len(entryMap))
	for id, entry := range entryMap {
		indegree[id] = 0
		if entry.ParentID != "" {
			children[entry.ParentID] = append(children[entry.ParentID], entry)
			indegree[id] = 1
		}
	}
	less := func(left, right *SessionEntry) bool {
		if left.Seq != right.Seq {
			if left.Seq == 0 {
				return true
			}
			if right.Seq == 0 {
				return false
			}
			return left.Seq < right.Seq
		}
		return left.ID < right.ID
	}
	ready := make([]*SessionEntry, 0, len(entryMap))
	for id, count := range indegree {
		if count == 0 {
			ready = append(ready, entryMap[id])
		}
	}
	var ordered []*SessionEntry
	for len(ready) > 0 {
		sort.Slice(ready, func(i, j int) bool { return less(ready[i], ready[j]) })
		entry := ready[0]
		ready = ready[1:]
		ordered = append(ordered, entry)
		for _, child := range children[entry.ID] {
			indegree[child.ID]--
			if indegree[child.ID] == 0 {
				ready = append(ready, child)
			}
		}
	}
	if len(ordered) != len(entryMap) {
		return nil, fmt.Errorf("session: versioned tree contains a parent cycle")
	}
	return ordered, nil
}

// topoSortEntries returns entries ordered so that every parent appears before
// its children. Within that constraint, entries are ordered by timestamp for
// deterministic output. The function does not recurse more deeply than the
// number of entries (guarded by the visited set).
func topoSortEntries(entryMap map[string]*SessionEntry) []*SessionEntry {
	var result []*SessionEntry
	visited := make(map[string]bool)

	// Collect and sort IDs for deterministic traversal order.
	ids := make([]string, 0, len(entryMap))
	for id := range entryMap {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var visit func(id string)
	visit = func(id string) {
		if visited[id] {
			return
		}
		visited[id] = true
		e := entryMap[id]
		if e.ParentID != "" {
			if _, ok := entryMap[e.ParentID]; ok {
				visit(e.ParentID)
			}
		}
		result = append(result, e)
	}

	for _, id := range ids {
		visit(id)
	}
	return result
}
