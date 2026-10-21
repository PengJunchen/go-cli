package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

const maxToolListPages = 128
const maxDiscoveredTools = 4096
const maxToolCatalogBytes = 8 << 20

// listToolPages applies one bounded pagination contract to both transports.
// Cursor loops fail explicitly instead of hanging during lazy activation.
func listToolPages(ctx context.Context, request func(context.Context, map[string]any) (map[string]any, error)) ([]MCPTool, error) {
	var tools []MCPTool
	cursor := ""
	seen := map[string]bool{}
	totalBytes := 0
	for page := 0; page < maxToolListPages; page++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		result, err := request(ctx, params)
		if err != nil {
			return nil, err
		}
		raw, ok := result["tools"].([]any)
		if !ok {
			return nil, errors.New("mcp: unexpected tools/list result")
		}
		for _, item := range raw {
			object, ok := item.(map[string]any)
			if !ok {
				continue
			}
			encoded, err := json.Marshal(object)
			if err != nil {
				return nil, fmt.Errorf("mcp: tool catalog: %w", err)
			}
			totalBytes += len(encoded)
			if totalBytes > maxToolCatalogBytes || len(tools) >= maxDiscoveredTools {
				return nil, errors.New("mcp: tool catalog budget exhausted")
			}
			tools = append(tools, mcpToolFromProtocolObject(object))
		}
		next, present := result["nextCursor"]
		if !present || next == nil {
			return tools, nil
		}
		value, ok := next.(string)
		if !ok {
			return nil, errors.New("mcp: invalid tools/list cursor")
		}
		if value == "" {
			return tools, nil
		}
		if seen[value] {
			return nil, errors.New("mcp: repeated tools/list cursor")
		}
		seen[value] = true
		cursor = value
	}
	return nil, errors.New("mcp: tools/list page budget exhausted")
}
