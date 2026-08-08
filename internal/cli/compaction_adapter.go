package cli

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/core"
)

// newCompactionHook builds a core.CompactionHook that bridges the compaction
// package's TurnItem-based API and the core package's AgentMessage-based
// history. After each successful Run, AgentImpl calls this hook with its full
// history; the hook converts to TurnItems, runs the compactor, and converts
// back. If compaction is not triggered (below threshold), the original
// history is returned unchanged.
//
// This adapter is the wiring layer between two existing subsystems — it does
// not create new logic, only connects them.
func newCompactionHook(
	compactor compaction.Compactor,
	estimator compaction.TokenEstimator,
	maxTokens int,
) core.CompactionHook {
	return func(ctx context.Context, messages []core.AgentMessage) ([]core.AgentMessage, error) {
		if len(messages) == 0 {
			return messages, nil
		}

		// Convert AgentMessage -> TurnItem
		items := make([]compaction.TurnItem, len(messages))
		for i, msg := range messages {
			items[i] = compaction.TurnItem{
				ID:            fmt.Sprintf("msg-%d", i),
				Role:          msg.Role,
				Content:       msg.Content,
				ContentBlocks: msg.ContentBlocks,
				ToolCalls:     msg.ToolCalls,
				ToolCallID:    msg.ToolCallID,
				ToolName:      msg.ToolName,
			}
		}

		// Run compaction
		compacted, err := compactor.Compact(ctx, items, maxTokens, estimator)
		if err != nil {
			return nil, fmt.Errorf("compaction hook: %w", err)
		}

		// Convert TurnItem -> AgentMessage
		result := make([]core.AgentMessage, len(compacted))
		for i, item := range compacted {
			result[i] = core.AgentMessage{
				Role:          item.Role,
				Content:       item.Content,
				ContentBlocks: item.ContentBlocks,
				ToolCalls:     item.ToolCalls,
				ToolCallID:    item.ToolCallID,
				ToolName:      item.ToolName,
			}
		}

		slog.Info("cli_compaction_hook",
			"op", "cli.compaction.hook",
			"before", len(messages),
			"after", len(result),
		)

		return result, nil
	}
}
