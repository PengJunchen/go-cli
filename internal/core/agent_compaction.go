package core

import "context"

// CompactionHook is called after each turn to optionally compact the history.
// It receives the current message history and returns a (possibly compacted)
// replacement. If the hook returns an error the original history is preserved
// and the error is logged.
type CompactionHook func(ctx context.Context, messages []AgentMessage) ([]AgentMessage, error)

// WithCompactionHook sets a compaction hook on the agent. After each
// successful Run, the hook is invoked with the agent's updated history. If
// the hook returns a non-nil error the compaction is silently skipped (the
// original history is retained) and the error is logged.
func WithCompactionHook(hook CompactionHook) AgentOption {
	return func(c *agentConfig) { c.compactionHook = hook }
}
