package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// toolCategory is a coarse classification used by tool_search's category filter.
type toolCategory string

const (
	// toolCategoryFile covers tools that read/write/edit/list/search files.
	toolCategoryFile toolCategory = "file"
	// toolCategoryNetwork covers tools that make network requests.
	toolCategoryNetwork toolCategory = "network"
	// toolCategoryShell covers tools that run external shell commands.
	toolCategoryShell toolCategory = "shell"
	// toolCategoryOther is the fallback for tools that fit no known category.
	toolCategoryOther toolCategory = "other"
)

// categorizeTool assigns a tool to a category based on heuristic keyword
// matching against its name and description.
func categorizeTool(def ToolDefinition) toolCategory {
	text := strings.ToLower(def.Name() + " " + def.Description())

	fileKeywords := []string{"read", "write", "edit", "grep", "find", "ls", "file", "glob", "path", "directory"}
	for _, kw := range fileKeywords {
		if strings.Contains(text, kw) {
			return toolCategoryFile
		}
	}
	networkKeywords := []string{"http", "network", "fetch", "url", "request", "rest", "web"}
	for _, kw := range networkKeywords {
		if strings.Contains(text, kw) {
			return toolCategoryNetwork
		}
	}
	shellKeywords := []string{"bash", "shell", "sh ", "exec", "command", "terminal"}
	for _, kw := range shellKeywords {
		if strings.Contains(text, kw) {
			return toolCategoryShell
		}
	}
	return toolCategoryOther
}

// ToolSearchTool searches an underlying registry's tools by name and
// description keyword, optionally filtered by category. It implements the
// ToolDefinition interface.
type ToolSearchTool struct {
	reg ToolRegistry // SCAN-013: dependency held by interface.
}

var _ ToolDefinition = (*ToolSearchTool)(nil)

// NewToolSearchTool returns a ToolSearchTool backed by the given registry,
// which must be non-nil.
func NewToolSearchTool(reg ToolRegistry) *ToolSearchTool {
	return &ToolSearchTool{reg: reg}
}

// Name returns the tool name.
func (t *ToolSearchTool) Name() string { return "tool_search" }

// Description returns a brief description of the tool.
func (t *ToolSearchTool) Description() string {
	return "tool_search: lists available tools matching a keyword query, optionally filtered by category. Args: query (string, optional), category (string, optional: file|network|shell|other)."
}

// searchResult is a single matched tool rendered deterministically.
type searchResult struct {
	Name        string
	Description string
}

// Execute searches the underlying registry for tools whose name or
// description contains the query keyword, optionally restrained to a category.
// An empty query returns all tools. Results are sorted by name for
// determinism.
func (t *ToolSearchTool) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	span, _ := tracing.SpanFromContext(ctx, "tools.search", tracing.SpanKindInternal)
	logger := tracing.NewTraceLogger(span, slog.Default())
	defer span.End()

	start := time.Now()

	if t.reg == nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		return nil, errors.New("tool_search: nil registry")
	}

	query := strings.ToLower(strings.TrimSpace(getStringArg(call, "query")))

	var category toolCategory
	explicitCategory := false
	if raw := strings.ToLower(strings.TrimSpace(getStringArg(call, "category"))); raw != "" {
		switch raw {
		case "file", "network", "shell", "other":
			category = toolCategory(raw)
			explicitCategory = true
		default:
			span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
			logger.Error("tool_search.invalid_category", "tool", "tool_search", "category", raw)
			return nil, fmt.Errorf("tool_search: invalid category %q (expected file, network, shell or other)", raw)
		}
	}

	defs, err := t.reg.List(ctx)
	if err != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("tool_search.list_failed", "tool", "tool_search", "err", err)
		return nil, fmt.Errorf("tool_search: %w", err)
	}

	var results []searchResult
	for _, def := range defs {
		if def == nil {
			continue
		}
		lowerName := strings.ToLower(def.Name())
		lowerDesc := strings.ToLower(def.Description())
		matched := query == "" || strings.Contains(lowerName, query) || strings.Contains(lowerDesc, query)
		if !matched {
			continue
		}
		if explicitCategory && categorizeTool(def) != category {
			continue
		}
		results = append(results, searchResult{Name: def.Name(), Description: def.Description()})
	}

	// Deterministic ordering: sort by name, then by description.
	sort.Slice(results, func(i, j int) bool {
		if results[i].Name != results[j].Name {
			return results[i].Name < results[j].Name
		}
		return results[i].Description < results[j].Description
	})

	var sb strings.Builder
	for _, r := range results {
		sb.WriteString(r.Name)
		sb.WriteString("\t")
		sb.WriteString(r.Description)
		sb.WriteString("\n")
	}

	span.SetAttributes(
		tracing.Attribute{Key: "query", Value: query},
		tracing.Attribute{Key: "category", Value: string(category)},
		tracing.Attribute{Key: "matches", Value: len(results)},
		tracing.Attribute{Key: "success", Value: true},
	)
	logger.Info("tool_search.done",
		"tool", "tool_search",
		"query", query,
		"category", string(category),
		"matches", len(results),
		"duration_ms", time.Since(start).Milliseconds())

	return &ToolResult{
		Output:   strings.TrimSuffix(sb.String(), "\n"),
		Metadata: map[string]any{"query": query, "category": string(category), "matches": len(results)},
	}, nil
}

// getStringArg returns the string value of Args[key], or "" when absent or not
// a string.
func getStringArg(call ToolCall, key string) string {
	if v, ok := call.Args[key].(string); ok {
		return v
	}
	return ""
}
