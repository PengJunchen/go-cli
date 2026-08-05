package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// lspTool wraps an LSPClient as a ToolDefinition, exposing semantic code
// queries (definition, references, hover, diagnostics) to the agent.
type lspTool struct {
	client LSPClient
}

var _ ToolDefinition = (*lspTool)(nil)
var _ Parameterized = (*lspTool)(nil)

// NewLSPTool returns a ToolDefinition backed by the given LSPClient.
func NewLSPTool(client LSPClient) ToolDefinition {
	return &lspTool{client: client}
}

// Name returns the tool name.
func (t *lspTool) Name() string { return "lsp_query" }

// Description returns a brief description of the tool.
func (t *lspTool) Description() string {
	return "lsp_query: Query a Language Server for semantic code information. " +
		"Supports operations: definition, references, hover, diagnostics."
}

// Parameters returns the JSON Schema describing the tool's input parameters.
func (t *lspTool) Parameters() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"operation": map[string]any{
				"type":        "string",
				"enum":        []string{"definition", "references", "hover", "diagnostics"},
				"description": "The LSP operation to perform.",
			},
			"uri": map[string]any{
				"type":        "string",
				"description": "File URI (e.g. file:///path/to/file.go).",
			},
			"line": map[string]any{
				"type":        "integer",
				"description": "0-based line number.",
			},
			"character": map[string]any{
				"type":        "integer",
				"description": "0-based character offset.",
			},
		},
		"required": []string{"operation", "uri"},
	}
}

// Execute runs the LSP query for the given call.
func (t *lspTool) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	span, _ := tracing.SpanFromContext(ctx, "tool.call", tracing.SpanKindClient)
	logger := tracing.NewTraceLogger(span, slog.Default())

	operation, _ := call.Args["operation"].(string)
	uri, _ := call.Args["uri"].(string)
	line, _ := call.Args["line"].(float64)
	character, _ := call.Args["character"].(float64)

	if operation == "" {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		return nil, errors.New("lsp_query: missing 'operation' argument")
	}
	if uri == "" {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		return nil, errors.New("lsp_query: missing 'uri' argument")
	}

	// Validate operation before requiring a client so argument errors are
	// surfaced even when the tool is used without a running server.
	switch operation {
	case "definition", "references", "hover", "diagnostics":
	default:
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		return nil, fmt.Errorf("lsp_query: unknown operation %q", operation)
	}

	if t.client == nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		return nil, errors.New("lsp_query: nil LSP client")
	}

	var output string
	var err error

	switch operation {
	case "definition":
		var locs []Location
		locs, err = t.client.Definition(ctx, uri, int(line), int(character))
		if err == nil {
			output = formatLocations(locs)
		}
	case "references":
		var locs []Location
		locs, err = t.client.References(ctx, uri, int(line), int(character))
		if err == nil {
			output = formatLocations(locs)
		}
	case "hover":
		output, err = t.client.Hover(ctx, uri, int(line), int(character))
	case "diagnostics":
		var diags []Diagnostic
		diags, err = t.client.Diagnostics(ctx, uri)
		if err == nil {
			output = formatDiagnostics(diags)
		}
	}

	if err != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("lsp_query.failed", "operation", operation, "uri", uri, "err", err)
		return nil, err
	}

	span.SetAttributes(tracing.Attribute{Key: "success", Value: true})
	logger.Info("lsp_query.done", "operation", operation, "uri", uri)

	return &ToolResult{
		Output:     output,
		ToolCallID: call.ID,
		Metadata: map[string]any{
			"operation": operation,
			"uri":       uri,
		},
	}, nil
}

// formatLocations renders a slice of Location as one line per location.
func formatLocations(locs []Location) string {
	if len(locs) == 0 {
		return "(no results)"
	}
	var sb strings.Builder
	for i, loc := range locs {
		if i > 0 {
			sb.WriteString("\n")
		}
		fmt.Fprintf(&sb, "%s:%d:%d", loc.URI, loc.Range.Start.Line, loc.Range.Start.Character)
	}
	return sb.String()
}

// formatDiagnostics renders a slice of Diagnostic as one line per diagnostic.
func formatDiagnostics(diags []Diagnostic) string {
	if len(diags) == 0 {
		return "(no diagnostics)"
	}
	var sb strings.Builder
	for i, d := range diags {
		if i > 0 {
			sb.WriteString("\n")
		}
		src := d.Source
		if src == "" {
			src = "lsp"
		}
		fmt.Fprintf(&sb, "[%s] %d:%d %s", src, d.Range.Start.Line, d.Range.Start.Character, d.Message)
	}
	return sb.String()
}
