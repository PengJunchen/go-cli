package tools

import (
	"context"
	"fmt"
	"log/slog"
)

// mutationToolNames are the names of built-in tools that produce file
// mutations and therefore should route through a FileMutationQueue.
var mutationToolNames = map[string]bool{
	"write": true,
	"edit":  true,
}

// mutationPathFromCall extracts the target file path from a write/edit tool
// call. It returns "" when the call does not carry a usable path.
func mutationPathFromCall(call ToolCall) string {
	if v, ok := call.Args["path"].(string); ok {
		return v
	}
	if v, ok := call.Args["file_path"].(string); ok {
		return v
	}
	return ""
}

// mutationContentFromCall extracts the mutation payload from a write/edit tool
// call. For "write" it is the content string; for "edit" it is an
// {old_string,new_string} map.
func mutationContentFromCall(call ToolCall) any {
	switch call.Name {
	case "write":
		if v, ok := call.Args["content"].(string); ok {
			return v
		}
		return ""
	case "edit":
		content := make(map[string]any)
		if v, ok := call.Args["old_string"]; ok {
			content["old_string"] = v
		}
		if v, ok := call.Args["new_string"]; ok {
			content["new_string"] = v
		}
		return content
	default:
		return nil
	}
}

// WithMutationQueue wraps a tool execution function so that mutation-producing
// tool calls (write/edit) are serialized per file through the given
// FileMutationQueue instead of running inline. Calls for non-mutation tools are
// passed straight through to next. It returns the (possibly queued) execution
// function.
//
// The returned function builds a FileMutation from the call, enqueues it, and
// blocks until the per-file worker reports the result (or the context is done).
func WithMutationQueue(queue FileMutationQueue, next func(ctx context.Context, call ToolCall) (*ToolResult, error)) func(ctx context.Context, call ToolCall) (*ToolResult, error) {
	return func(ctx context.Context, call ToolCall) (*ToolResult, error) {
		if next == nil {
			return nil, fmt.Errorf("tools: WithMutationQueue requires a non-nil next function")
		}
		if !mutationToolNames[call.Name] {
			slog.Debug("tools: passing non-mutation call through", "name", call.Name)
			return next(ctx, call)
		}

		path := mutationPathFromCall(call)
		slog.Debug("tools: queueing mutation call", "name", call.Name, "path", path)
		mutation := FileMutation{
			FilePath:  path,
			Operation: call.Name,
			Content:   mutationContentFromCall(call),
			ToolName:  call.Name,
		}

		resCh, err := queue.Enqueue(ctx, mutation)
		if err != nil {
			return nil, fmt.Errorf("tools: enqueue %s: %w", call.Name, err)
		}

		select {
		case res := <-resCh:
			if res.Error != nil {
				return &ToolResult{Output: "", Metadata: map[string]any{"path": path, "queued": true}}, res.Error
			}
			return &ToolResult{
				Output:   fmt.Sprintf("%s queued and applied for %s", call.Name, path),
				Metadata: map[string]any{"path": path, "queued": true},
			}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}
