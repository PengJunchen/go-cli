package session

import (
	"github.com/pengjunchen/go-cli/internal/core"
)

// DeriveMessages projects a slice of SessionEntry values into the
// model-visible message stream. It applies the same compaction-point logic as
// DefaultContextManager.BuildContext—entries before the last compaction entry
// are replaced by the compaction summary—and additionally filters entries based
// on their SurfaceOp field. Hidden and compacted entries are excluded; visible
// (or unmarked) entries are included.
func DeriveMessages(entries []SessionEntry) []SessionEntry {
	// Find the last compaction point; entries before it are replaced by the
	// compaction summary.
	startIdx := findLastCompactionPoint(entries)

	result := make([]SessionEntry, 0, len(entries)-startIdx)
	for _, e := range entries[startIdx:] {
		if e.Type == EntryTypeCompaction {
			result = append(result, SessionEntry{
				ID:        e.ID,
				ParentID:  e.ParentID,
				Type:      EntryTypeCompaction,
				Content:   e.Summary,
				Timestamp: e.Timestamp,
			})
			continue
		}
		if !e.SurfaceVisible() {
			continue
		}
		result = append(result, e)
	}
	return result
}

// DeriveAgentMessages projects a slice of SessionEntry values into
// core.AgentMessage values suitable for restoring agent history. It first
// applies DeriveMessages to obtain the model-visible entries, then converts
// them via EntriesToAgentMessages.
func DeriveAgentMessages(entries []SessionEntry) []core.AgentMessage {
	return EntriesToAgentMessages(DeriveMessages(entries))
}

// findLastCompactionPoint returns the index of the last EntryTypeCompaction
// entry in entries, or 0 if none exists. Both DeriveMessages and
// DefaultContextManager.BuildContext use this to determine where to start
// the model-visible message stream.
func findLastCompactionPoint(entries []SessionEntry) int {
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Type == EntryTypeCompaction {
			return i
		}
	}
	return 0
}
