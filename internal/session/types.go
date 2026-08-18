// Package session provides the conversational session storage layer: an
// append-only entry store, a branching session tree, and JSONL persistence. It
// implements the richer Phase 2 session interfaces (SessionStore and
// SessionTree) that build on top of the core stubs.
package session

import (
	"context"
	"log/slog"
	"time"

	"github.com/pengjunchen/go-cli/internal/llm"
)

// EntryType categorizes a single immutable session entry.
type EntryType string

func init() {
	slog.Debug("session types initialized", "entry_types", []string{"user", "assistant", "tool", "compaction", "system"})
}

const (
	// EntryTypeUser is a message authored by the human user.
	EntryTypeUser EntryType = "user"
	// EntryTypeAssistant is a message produced by the assistant.
	EntryTypeAssistant EntryType = "assistant"
	// EntryTypeTool is a tool invocation or its result.
	EntryTypeTool EntryType = "tool"
	// EntryTypeCompaction is a summarized replacement for older history.
	EntryTypeCompaction EntryType = "compaction"
	// EntryTypeSystem is a system-level informational entry.
	EntryTypeSystem EntryType = "system"
)

// SurfaceOp controls how an entry is projected into the model-visible message
// stream by DeriveMessages. It separates the storage format from the
// model-visible format.
type SurfaceOp string

const (
	// SurfaceOpVisible marks an entry as included in the model context. This
	// is the default behavior when SurfaceOp is empty.
	SurfaceOpVisible SurfaceOp = "visible"
	// SurfaceOpCompacted marks an entry as replaced by a compaction summary.
	// It is not included individually in the model context.
	SurfaceOpCompacted SurfaceOp = "compacted"
	// SurfaceOpHidden marks an entry as excluded from the model context.
	SurfaceOpHidden SurfaceOp = "hidden"
)

// SessionEntry is a single immutable record in a session. Entries are linked
// into a tree via ParentID and are never mutated once appended.
type SessionEntry struct {
	ID            string             `json:"id"`
	ParentID      string             `json:"parent_id,omitempty"`
	Type          EntryType          `json:"type"`
	Content       string             `json:"content"`
	ContentBlocks []llm.ContentBlock `json:"content_blocks,omitempty"`
	Timestamp     time.Time          `json:"timestamp"`
	Summary       string             `json:"summary,omitempty"`
	// IsSummary marks an entry as an auto-generated branch summary appended to
	// a departed branch by SessionTree.MoveTo. It does not affect replay.
	IsSummary bool `json:"is_summary,omitempty"`
	// ToolCalls holds tool invocations requested by the assistant. Populated
	// for assistant entries that request tool execution.
	ToolCalls []llm.ToolCall `json:"tool_calls,omitempty"`
	// ToolCallID associates a tool-result entry with the originating tool
	// call. Populated for tool entries.
	ToolCallID string `json:"tool_call_id,omitempty"`
	// ToolName is the name of the tool that produced this entry. Populated
	// for tool entries.
	ToolName string `json:"tool_name,omitempty"`
	// SurfaceOp controls how DeriveMessages projects this entry into the
	// model-visible message stream. An empty value is treated as
	// SurfaceOpVisible.
	SurfaceOp SurfaceOp `json:"surface_op,omitempty"`
	// Seq is a monotonically increasing sequence number assigned by the
	// session tree when the entry is appended. It is zero when unset.
	Seq uint64 `json:"seq"`
}

// clone returns a defensive copy of the entry so callers cannot mutate the
// stored immutable record through the returned pointer.
func (e *SessionEntry) clone() *SessionEntry {
	if e == nil {
		return nil
	}
	cp := *e
	if e.ContentBlocks != nil {
		cp.ContentBlocks = make([]llm.ContentBlock, len(e.ContentBlocks))
		copy(cp.ContentBlocks, e.ContentBlocks)
		for i := range cp.ContentBlocks {
			if cp.ContentBlocks[i].ImageURL != nil {
				cp.ContentBlocks[i].ImageURL = &llm.ImageURL{
					URL:    cp.ContentBlocks[i].ImageURL.URL,
					Detail: cp.ContentBlocks[i].ImageURL.Detail,
				}
			}
		}
	}
	if e.ToolCalls != nil {
		cp.ToolCalls = make([]llm.ToolCall, len(e.ToolCalls))
		copy(cp.ToolCalls, e.ToolCalls)
	}
	return &cp
}

// SurfaceVisible reports whether the entry should be included in the
// model-visible message stream. It returns true when SurfaceOp is empty
// (the default) or explicitly set to SurfaceOpVisible.
func (e SessionEntry) SurfaceVisible() bool {
	return e.SurfaceOp == "" || e.SurfaceOp == SurfaceOpVisible
}

// SessionContext is the effective, replayable context for a session branch.
type SessionContext struct {
	// LeafID is the entry the context was reconstructed for.
	LeafID string `json:"leaf_id"`
	// RootID is the root entry of the reconstructed branch.
	RootID string `json:"root_id,omitempty"`
	// Messages are the ordered (root to leaf) effective messages for the
	// context. Compaction entries are folded into a single summary message.
	Messages []SessionEntry `json:"messages"`
	// Traversed are the raw entries visited while walking from the leaf back to
	// the root, in walk (leaf to root) order.
	Traversed []SessionEntry `json:"traversed,omitempty"`
	// EntryCount is the number of effective messages in Messages.
	EntryCount int `json:"entry_count"`
	// EstimatedTokens is a heuristic estimate of the token count for the
	// reconstructed context.
	EstimatedTokens int `json:"estimated_tokens,omitempty"`
	// LastUpdate is the most recent timestamp among the traversed entries.
	LastUpdate time.Time `json:"last_update,omitempty"`
}

// SessionStore persists and loads immutable session entries.
type SessionStore interface {
	// Append inserts an immutable entry without overwriting an existing id.
	Append(ctx context.Context, entry *SessionEntry) error
	// Get returns a defensive copy of the entry with the given id.
	Get(ctx context.Context, id string) (*SessionEntry, error)
	// Save flushes any buffered state (no-op for in-memory stores).
	Save(ctx context.Context) error
}

// SessionTree manages the append-only branching structure of a session.
type SessionTree interface {
	// Append adds an immutable entry to the tree.
	Append(ctx context.Context, entry *SessionEntry) error
	// MoveTo sets the current leaf pointer to the given entry id.
	MoveTo(ctx context.Context, leafID string) error
	// GetBranch returns the ordered entries from root to leafID.
	GetBranch(ctx context.Context, leafID string) ([]*SessionEntry, error)
	// BuildContext reconstructs the effective context for a branch.
	BuildContext(ctx context.Context, leafID string) (*SessionContext, error)
	// CurrentLeaf returns the id of the current leaf, or "" when empty.
	CurrentLeaf() string
	// Branch zero-copy points the current leaf at the existing entry fromID,
	// establishing a new branch without copying any entries.
	Branch(ctx context.Context, fromID string, opts ...BranchOption) error
	// ListBranches returns metadata for all branches in the tree.
	ListBranches() []BranchMeta
	// Clone deep-copies entries from the source branch into a new branch ID.
	Clone(ctx context.Context, fromBranchID, newBranchID string) error
}
