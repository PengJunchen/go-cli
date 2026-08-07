package tools

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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

// WithArgumentPreparation wraps a tool executor so that the given preparers
// are applied to the ToolCall before it reaches the underlying tool. Preparers
// run in order; if any preparer returns an error, execution is aborted.
func WithArgumentPreparation(preparers ...ArgumentPreparer) ToolExecutorWrapper {
	return func(next func(ctx context.Context, call ToolCall) (*ToolResult, error)) func(ctx context.Context, call ToolCall) (*ToolResult, error) {
		return func(ctx context.Context, call ToolCall) (*ToolResult, error) {
			prepared := call
			for _, p := range preparers {
				if p == nil {
					continue
				}
				var err error
				prepared, err = p.PrepareArguments(ctx, prepared)
				if err != nil {
					return nil, fmt.Errorf("tools: prepare arguments for %q: %w", call.Name, err)
				}
			}
			return next(ctx, prepared)
		}
	}
}

// PathNormalizer converts relative path arguments to absolute paths. It
// inspects common path-like argument keys (path, file_path, dir, directory,
// cwd, workspace) and resolves them against the base directory.
type PathNormalizer struct {
	baseDir string
}

// NewPathNormalizer creates a PathNormalizer that resolves relative paths
// against baseDir. When baseDir is empty, the process working directory is used.
func NewPathNormalizer(baseDir string) *PathNormalizer {
	if baseDir == "" {
		if wd, err := os.Getwd(); err == nil {
			baseDir = wd
		}
	}
	return &PathNormalizer{baseDir: baseDir}
}

// pathArgKeys are the argument keys that PathNormalizer inspects.
var pathArgKeys = []string{"path", "file_path", "dir", "directory", "cwd", "workspace"}

// PrepareArguments resolves relative path-like argument values against baseDir.
// The original call's Args map is not mutated; a copy is made when any value
// changes.
func (n *PathNormalizer) PrepareArguments(_ context.Context, call ToolCall) (ToolCall, error) {
	if len(call.Args) == 0 {
		return call, nil
	}
	modified := false
	newArgs := make(map[string]any, len(call.Args))
	for k, v := range call.Args {
		newArgs[k] = v
	}
	for _, key := range pathArgKeys {
		if p, ok := newArgs[key].(string); ok && p != "" {
			if !filepath.IsAbs(p) {
				abs := filepath.Join(n.baseDir, p)
				newArgs[key] = abs
				modified = true
			}
		}
	}
	if modified {
		call.Args = newArgs
	}
	return call, nil
}

// SchemaValidator validates tool call arguments against the tool's JSON Schema.
// It looks up the tool definition via the optional ToolRegistry to access the
// Parameterized interface. When no registry is set, validation is skipped.
type SchemaValidator struct {
	registry ToolRegistry
}

// NewSchemaValidator creates a SchemaValidator that uses the given registry
// to look up tool schemas. A nil registry disables validation.
func NewSchemaValidator(registry ToolRegistry) *SchemaValidator {
	return &SchemaValidator{registry: registry}
}

// PrepareArguments looks up the tool's schema and validates the call's
// arguments against it. When the tool is unknown or has no schema, validation
// is skipped so the executor can surface the real error.
func (v *SchemaValidator) PrepareArguments(ctx context.Context, call ToolCall) (ToolCall, error) {
	if v.registry == nil {
		return call, nil
	}
	def, err := v.registry.Get(ctx, call.Name)
	if err != nil {
		// Tool not found - skip validation, let the executor handle the error
		return call, nil
	}
	param, ok := def.(Parameterized)
	if !ok {
		// Tool doesn't have a schema - skip validation
		return call, nil
	}
	schema := param.Parameters()
	if schema == nil {
		return call, nil
	}
	// Basic schema validation: check required fields and types
	if err := validateBasicSchema(call.Args, schema); err != nil {
		return call, fmt.Errorf("schema validation: %w", err)
	}
	return call, nil
}

// validateBasicSchema performs lightweight validation of args against a JSON
// Schema. It checks required fields and basic type matching.
func validateBasicSchema(args map[string]any, schema any) error {
	schemaMap, ok := schema.(map[string]any)
	if !ok {
		return nil
	}
	props, ok := schemaMap["properties"].(map[string]any)
	if !ok {
		return nil
	}
	// Check required fields
	if required, ok := schemaMap["required"].([]any); ok {
		for _, r := range required {
			if name, ok := r.(string); ok {
				if _, exists := args[name]; !exists {
					return fmt.Errorf("missing required parameter: %s", name)
				}
			}
		}
	}
	// Check types
	for name, val := range args {
		propDef, ok := props[name].(map[string]any)
		if !ok {
			continue
		}
		expectedType, _ := propDef["type"].(string)
		if err := checkType(name, val, expectedType); err != nil {
			return err
		}
	}
	return nil
}

// checkType verifies that val matches the JSON Schema type string. JSON numbers
// decoded by encoding/json arrive as float64.
func checkType(name string, val any, expectedType string) error {
	switch expectedType {
	case "string":
		if _, ok := val.(string); !ok {
			return fmt.Errorf("parameter %q must be a string", name)
		}
	case "number", "integer":
		switch val.(type) {
		case float64, int, int64:
			// ok
		default:
			return fmt.Errorf("parameter %q must be a number", name)
		}
	case "boolean":
		if _, ok := val.(bool); !ok {
			return fmt.Errorf("parameter %q must be a boolean", name)
		}
	case "array":
		if _, ok := val.([]any); !ok {
			return fmt.Errorf("parameter %q must be an array", name)
		}
	case "object":
		if _, ok := val.(map[string]any); !ok {
			return fmt.Errorf("parameter %q must be an object", name)
		}
	}
	return nil
}
