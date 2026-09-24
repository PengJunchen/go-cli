package session

import (
	"errors"
	"fmt"
	"sort"
)

type durableBranch struct {
	parentBranch string
}

// validateDurableBranchJournal checks version-one branch metadata in file
// order. Branch checkpoints are the authority that introduces a branch; an
// arbitrary BranchID string on another row cannot create one implicitly.
func validateDurableBranchJournal(entries []SessionEntry) error {
	branches := map[string]durableBranch{"main": {}}
	entryBranches := make(map[string]string, len(entries))
	knownEntries := make(map[string]struct{}, len(entries))

	for _, entry := range entries {
		branchID := entry.BranchID
		if branchID == "" {
			branchID = "main"
		}
		if entry.RecordVersion != 1 {
			if entry.ID != "" {
				knownEntries[entry.ID] = struct{}{}
				entryBranches[entry.ID] = branchID
			}
			continue
		}

		introducesBranch := entry.Type == EntryTypeCheckpoint && entry.CheckpointKind == CheckpointBranch
		if introducesBranch {
			if branchID == "" || branchID == "main" || entry.ParentBranchID == "" {
				return fmt.Errorf("session: branch checkpoint %q has invalid branch ancestry", entry.ID)
			}
			if _, exists := branches[branchID]; exists {
				return fmt.Errorf("session: branch checkpoint %q reuses branch ID %q", entry.ID, branchID)
			}
			if _, exists := branches[entry.ParentBranchID]; !exists {
				return fmt.Errorf("session: branch checkpoint %q names unknown parent branch %q", entry.ID, entry.ParentBranchID)
			}
			baseBranch, exists := entryBranches[entry.BaseLeafEntryID]
			if !exists {
				return fmt.Errorf("session: branch checkpoint %q names unknown base leaf %q", entry.ID, entry.BaseLeafEntryID)
			}
			if !durableBranchContains(branches, entry.ParentBranchID, baseBranch) {
				return fmt.Errorf("session: branch checkpoint %q base leaf is outside parent branch %q", entry.ID, entry.ParentBranchID)
			}
			branches[branchID] = durableBranch{parentBranch: entry.ParentBranchID}
		} else if _, exists := branches[branchID]; !exists {
			return fmt.Errorf("session: entry %q refers to unknown branch %q", entry.ID, branchID)
		}

		if entry.ParentID != "" {
			if _, exists := knownEntries[entry.ParentID]; !exists {
				return fmt.Errorf("session: entry %q has unknown parent %q in branch validation", entry.ID, entry.ParentID)
			}
			parentBranch := entryBranches[entry.ParentID]
			if !durableBranchContains(branches, branchID, parentBranch) {
				return fmt.Errorf("session: entry %q branches from unrelated branch %q", entry.ID, parentBranch)
			}
		}

		if entry.Type == EntryTypeCheckpoint && entry.CheckpointKind == CheckpointActiveLeaf && entry.ActiveLeafEntryID != entry.ID {
			leafBranch, exists := entryBranches[entry.ActiveLeafEntryID]
			if !exists || !durableBranchContains(branches, branchID, leafBranch) {
				return fmt.Errorf("session: active-leaf checkpoint %q points outside known branch %q", entry.ID, branchID)
			}
		}

		knownEntries[entry.ID] = struct{}{}
		entryBranches[entry.ID] = branchID
	}
	return nil
}

func validateDurableTranscriptCoordinates(entries []SessionEntry) error {
	transcriptIndexes := make(map[string]map[uint64]string)
	assistantOrdinals := make(map[string]map[uint64]string)
	for _, entry := range entries {
		if entry.RecordVersion != 1 {
			continue
		}
		if entry.TranscriptIndex != nil && entry.Type != EntryTypeCompaction && entry.Type != EntryTypeCheckpoint && entry.ActionState == "" {
			byIndex := transcriptIndexes[entry.RunID]
			if byIndex == nil {
				byIndex = make(map[uint64]string)
				transcriptIndexes[entry.RunID] = byIndex
			}
			if previous, exists := byIndex[*entry.TranscriptIndex]; exists {
				return fmt.Errorf("session: run %q repeats transcript index %d at entries %q and %q", entry.RunID, *entry.TranscriptIndex, previous, entry.ID)
			}
			byIndex[*entry.TranscriptIndex] = entry.ID
		}
		if entry.Type != EntryTypeAssistant || len(entry.ToolCalls) == 0 || entry.AssistantTranscriptOrdinal == nil {
			continue
		}
		byOrdinal := assistantOrdinals[entry.RunID]
		if byOrdinal == nil {
			byOrdinal = make(map[uint64]string)
			assistantOrdinals[entry.RunID] = byOrdinal
		}
		ordinal := *entry.AssistantTranscriptOrdinal
		if previous, exists := byOrdinal[ordinal]; exists {
			return fmt.Errorf("session: run %q repeats assistant ordinal %d at entries %q and %q", entry.RunID, ordinal, previous, entry.ID)
		}
		byOrdinal[ordinal] = entry.ID
	}
	return nil
}

func durableBranchContains(branches map[string]durableBranch, branchID, candidate string) bool {
	if branchID == "" || candidate == "" {
		return false
	}
	seen := make(map[string]struct{}, len(branches))
	for current := branchID; current != ""; {
		if current == candidate {
			return true
		}
		if _, exists := seen[current]; exists {
			return false
		}
		seen[current] = struct{}{}
		branch, exists := branches[current]
		if !exists {
			return false
		}
		current = branch.parentBranch
	}
	return false
}

func entriesInSequence(entries map[string]*SessionEntry) ([]SessionEntry, error) {
	ordered := make([]SessionEntry, 0, len(entries))
	for _, entry := range entries {
		if entry == nil {
			return nil, errors.New("session: nil entry in durable store")
		}
		ordered = append(ordered, *entry.clone())
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Seq < ordered[j].Seq })
	return ordered, nil
}
