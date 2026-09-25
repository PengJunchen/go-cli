package compaction

import (
	"errors"
	"fmt"
)

var errMalformedToolGroup = errors.New("compaction: malformed tool-call group")

type itemGroup struct {
	start  int
	end    int
	system bool
}

// groupTurnItems validates tool-call closure in the current assistant-message
// scope and returns a copy with uniquely inferable receipt names filled in.
// ToolCallIDs are intentionally not tracked across closed groups.
func groupTurnItems(items []TurnItem) ([]TurnItem, []itemGroup, error) {
	normalized := append([]TurnItem(nil), items...)
	groups := make([]itemGroup, 0, len(normalized))
	for i := range normalized {
		if normalized[i].Role == RoleSystem && normalized[i].IsCompaction {
			// Older persisted summaries may still carry system role metadata.
			// They are derived context, never mandatory instructions; normalize
			// only the defensive copy returned by this helper.
			normalized[i].Role = RoleUser
		}
	}

	for i := 0; i < len(normalized); {
		item := normalized[i]
		if item.Role == RoleTool {
			return nil, nil, malformedToolGroup(i, "receipt has no open assistant call group")
		}

		if item.Role != RoleAssistant || len(item.ToolCalls) == 0 {
			groups = append(groups, itemGroup{
				start:  i,
				end:    i + 1,
				system: item.Role == RoleSystem && !item.IsCompaction,
			})
			i++
			continue
		}

		callNames := make(map[string]string, len(item.ToolCalls))
		for _, call := range item.ToolCalls {
			if call.ID == "" {
				return nil, nil, malformedToolGroup(i, "assistant call has an empty ID")
			}
			if _, exists := callNames[call.ID]; exists {
				return nil, nil, malformedToolGroup(i, "assistant call group repeats an ID")
			}
			if call.Name == "" {
				return nil, nil, malformedToolGroup(i, "assistant call has an empty tool name")
			}
			callNames[call.ID] = call.Name
		}

		seenReceipts := make(map[string]struct{}, len(callNames))
		end := i + 1
		for len(seenReceipts) < len(callNames) {
			if end >= len(normalized) || normalized[end].Role != RoleTool {
				return nil, nil, malformedToolGroup(i, "assistant call group is missing a terminal receipt")
			}

			receipt := &normalized[end]
			if receipt.ToolCallID == "" {
				return nil, nil, malformedToolGroup(end, "receipt has an empty ToolCallID")
			}
			callName, exists := callNames[receipt.ToolCallID]
			if !exists {
				return nil, nil, malformedToolGroup(end, "receipt ID does not belong to the current assistant group")
			}
			if _, duplicate := seenReceipts[receipt.ToolCallID]; duplicate {
				return nil, nil, malformedToolGroup(end, "assistant call group has duplicate receipts")
			}
			if receipt.ToolName == "" {
				receipt.ToolName = callName
			} else if receipt.ToolName != callName {
				return nil, nil, malformedToolGroup(end, "receipt tool name conflicts with its assistant call")
			}
			seenReceipts[receipt.ToolCallID] = struct{}{}
			end++
		}

		groups = append(groups, itemGroup{start: i, end: end})
		i = end
	}

	return normalized, groups, nil
}

func malformedToolGroup(itemIndex int, reason string) error {
	return fmt.Errorf("%w at item %d: %s", errMalformedToolGroup, itemIndex, reason)
}
