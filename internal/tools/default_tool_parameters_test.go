package tools

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type defaultToolSchemaExpectation struct {
	propertyTypes map[string]string
	required      []string
	closed        bool
	enums         map[string][]string
}

func TestRegisteredDefaultToolsExposeAccurateParameterSchemas(t *testing.T) {
	ctx := context.Background()
	base := NewDefaultToolRegistry()
	require.NoError(t, RegisterDefaults(ctx, base))
	registry := NewMiddlewareToolRegistry(base)
	configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{})

	wants := map[string]defaultToolSchemaExpectation{
		"read": {
			propertyTypes: map[string]string{"path": "string"},
			required:      []string{"path"},
			closed:        true,
		},
		"bash": {
			propertyTypes: map[string]string{"command": "string", "timeout_ms": "integer"},
			required:      []string{"command"},
			closed:        true,
		},
		"write": {
			propertyTypes: map[string]string{"path": "string", "content": "string", "append": "boolean"},
			required:      []string{"path", "content"},
		},
		"edit": {
			propertyTypes: map[string]string{"file_path": "string", "old_string": "string", "new_string": "string"},
			required:      []string{"file_path", "old_string", "new_string"},
		},
		"grep": {
			propertyTypes: map[string]string{"pattern": "string", "glob": "string", "path": "string"},
			required:      []string{"pattern"},
			closed:        true,
		},
		"find": {
			propertyTypes: map[string]string{"path": "string", "pattern": "string", "type": "string", "max_depth": "integer"},
			enums:         map[string][]string{"type": {"", "f", "d"}},
			closed:        true,
		},
		"ls": {
			propertyTypes: map[string]string{"path": "string", "all": "boolean", "long": "boolean", "sort": "string"},
			enums:         map[string][]string{"sort": {"", "name", "time", "size"}},
			closed:        true,
		},
	}

	definitions, err := registry.List(ctx)
	require.NoError(t, err)
	require.Len(t, definitions, len(wants))
	seen := make(map[string]bool, len(definitions))
	for _, definition := range definitions {
		name := definition.Name()
		seen[name] = true
		want, ok := wants[name]
		require.Truef(t, ok, "unexpected registered default %q", name)
		parameterized, ok := definition.(Parameterized)
		require.Truef(t, ok, "%s must retain its Parameterized capability through the prepared proxy", name)
		schema := requireSchemaObject(t, parameterized.Parameters())
		assert.Equal(t, "object", schema["type"], "%s root schema", name)
		properties := requireSchemaObject(t, schema["properties"])
		assert.Len(t, properties, len(want.propertyTypes), "%s properties", name)
		for propertyName, expectedType := range want.propertyTypes {
			property := requireSchemaObject(t, properties[propertyName])
			assert.Equal(t, expectedType, property["type"], "%s.%s type", name, propertyName)
			if expectedEnum, exists := want.enums[propertyName]; exists {
				assert.Equal(t, expectedEnum, schemaStringArray(t, property["enum"]))
			}
		}
		assert.ElementsMatch(t, want.required, schemaStringArray(t, schema["required"]))
		if want.closed {
			assert.Equal(t, false, schema["additionalProperties"], "%s must reject undeclared fields", name)
		} else {
			assert.NotContains(t, schema, "additionalProperties", "%s keeps its existing schema unchanged", name)
		}
	}
	for name := range wants {
		assert.True(t, seen[name], "missing registered default %q", name)
	}

	// Parameters and the protected proxy must return independent schema values:
	// callers cannot mutate a later model definition or a captured snapshot.
	definition, err := registry.Get(ctx, "read")
	require.NoError(t, err)
	parameterized := definition.(Parameterized)
	returned := requireSchemaObject(t, parameterized.Parameters())
	returnedProperties := requireSchemaObject(t, returned["properties"])
	returnedProperties["path"] = map[string]any{"type": "integer"}
	returnedProperties["injected"] = map[string]any{"type": "string"}
	second := requireSchemaObject(t, parameterized.Parameters())
	secondProperties := requireSchemaObject(t, second["properties"])
	secondPath := requireSchemaObject(t, secondProperties["path"])
	assert.Equal(t, "string", secondPath["type"])
	assert.NotContains(t, secondProperties, "injected")
}

func TestDefaultToolPreparedProxyValidCallsAndOptionalDefaults(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	fixture := filepath.Join(root, "fixture.txt")
	require.NoError(t, os.WriteFile(fixture, []byte("needle\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".hidden"), []byte("hidden\n"), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(root, "nested"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "nested", "other.txt"), []byte("other\n"), 0o600))

	base := NewDefaultToolRegistry()
	require.NoError(t, RegisterDefaults(ctx, base, WithRegisteredBashSandbox(AllowAllSandbox{})))
	registry := NewMiddlewareToolRegistry(base)
	var policyCalls atomic.Int32
	var dispatchCalls atomic.Int32
	wrapper := ToolExecutorWrapper(func(next func(context.Context, ToolCall) (*ToolResult, error)) func(context.Context, ToolCall) (*ToolResult, error) {
		return func(ctx context.Context, call ToolCall) (*ToolResult, error) {
			dispatchCalls.Add(1)
			return next(ctx, call)
		}
	})
	require.NoError(t, registry.SetPreparedToolStages(PreparedToolStages{
		ArgumentPreparers:            []ArgumentPreparer{NewPathNormalizer(root)},
		ExecutionWrappers:            []ToolExecutorWrapper{wrapper},
		SynchronousExecutionWrappers: true,
	}))
	require.NoError(t, registry.SetPreparedToolPolicy(func(_ context.Context, request PreparedPolicyRequest) (PreparedPolicyDecision, error) {
		policyCalls.Add(1)
		return PreparedPolicyDecision{Allowed: true, Generations: request.Generations}, nil
	}))

	getProxy := func(name string) ToolDefinition {
		t.Helper()
		definition, err := registry.Get(ctx, name)
		require.NoError(t, err)
		return definition
	}

	// Reusing the same call map must not let path preparation leak mutation
	// between invocations or back into the caller-owned arguments.
	readArgs := map[string]any{"path": "fixture.txt"}
	for i := 0; i < 2; i++ {
		result, err := getProxy("read").Execute(ctx, ToolCall{Name: "read", Args: readArgs})
		require.NoError(t, err)
		assert.Equal(t, "needle\n", result.Output)
		assert.Equal(t, "fixture.txt", readArgs["path"])
	}

	result, err := getProxy("bash").Execute(ctx, ToolCall{Name: "bash", Args: map[string]any{"command": "printf bash-schema-ok"}})
	require.NoError(t, err)
	assert.Equal(t, "bash-schema-ok", result.Output)

	result, err = getProxy("grep").Execute(ctx, ToolCall{Name: "grep", Args: map[string]any{"pattern": "needle", "path": root}})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "fixture.txt:1:needle", "omitted glob means no filename filter")

	result, err = getProxy("find").Execute(ctx, ToolCall{Name: "find", Args: map[string]any{"path": root}})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "fixture.txt", "omitted pattern/type/depth preserve unfiltered search")
	assert.Contains(t, result.Output, "nested")

	result, err = getProxy("ls").Execute(ctx, ToolCall{Name: "ls", Args: map[string]any{"path": root}})
	require.NoError(t, err)
	assert.NotContains(t, result.Output, ".hidden", "all defaults false")
	assert.Equal(t, "name", result.Metadata["sort"], "sort defaults to name")

	result, err = getProxy("ls").Execute(ctx, ToolCall{Name: "ls", Args: map[string]any{
		"path": root, "all": true, "long": true, "sort": "size",
	}})
	require.NoError(t, err)
	assert.Contains(t, result.Output, ".hidden", "all=true includes dotfiles")
	assert.Equal(t, "size", result.Metadata["sort"])

	result, err = getProxy("ls").Execute(ctx, ToolCall{Name: "ls", Args: map[string]any{
		"path": root, "sort": "",
	}})
	require.NoError(t, err)
	assert.Equal(t, "name", result.Metadata["sort"], "an explicit empty sort preserves the default")

	result, err = getProxy("find").Execute(ctx, ToolCall{Name: "find", Args: map[string]any{
		"path": root, "pattern": "*.txt", "type": "f", "max_depth": 1,
	}})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "fixture.txt")
	assert.NotContains(t, result.Output, "nested/other.txt", "type/depth filters are applied")

	result, err = getProxy("find").Execute(ctx, ToolCall{Name: "find", Args: map[string]any{
		"path": root, "pattern": "", "type": "", "max_depth": 0,
	}})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "fixture.txt", "empty filters preserve the default unfiltered search")
	assert.Contains(t, result.Output, "nested")
}

func TestDefaultToolPreparedProxyRejectsInvalidArgumentsBeforePolicyOrDispatch(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	fixture := filepath.Join(root, "fixture.txt")
	require.NoError(t, os.WriteFile(fixture, []byte("needle\n"), 0o600))

	base := NewDefaultToolRegistry()
	require.NoError(t, RegisterDefaults(ctx, base, WithRegisteredBashSandbox(AllowAllSandbox{})))
	registry := NewMiddlewareToolRegistry(base)
	var policyCalls atomic.Int32
	var dispatchCalls atomic.Int32
	wrapper := ToolExecutorWrapper(func(next func(context.Context, ToolCall) (*ToolResult, error)) func(context.Context, ToolCall) (*ToolResult, error) {
		return func(ctx context.Context, call ToolCall) (*ToolResult, error) {
			dispatchCalls.Add(1)
			return next(ctx, call)
		}
	})
	require.NoError(t, registry.SetPreparedToolStages(PreparedToolStages{
		ExecutionWrappers:            []ToolExecutorWrapper{wrapper},
		SynchronousExecutionWrappers: true,
	}))
	require.NoError(t, registry.SetPreparedToolPolicy(func(_ context.Context, request PreparedPolicyRequest) (PreparedPolicyDecision, error) {
		policyCalls.Add(1)
		return PreparedPolicyDecision{Allowed: true, Generations: request.Generations}, nil
	}))

	invalidCalls := []struct {
		name string
		args map[string]any
	}{
		{name: "read", args: map[string]any{"path": fixture, "unexpected": true}},
		{name: "bash", args: map[string]any{"command": "printf ignored", "unexpected": true}},
		{name: "grep", args: map[string]any{"pattern": "needle", "path": root, "unexpected": true}},
		{name: "find", args: map[string]any{"path": root, "unexpected": true}},
		{name: "find", args: map[string]any{"path": root, "type": "x"}},
		{name: "find", args: map[string]any{"path": root, "max_depth": 1.5}},
		{name: "ls", args: map[string]any{"path": root, "unexpected": true}},
		{name: "ls", args: map[string]any{"path": root, "sort": "modified"}},
		{name: "read", args: map[string]any{}},
		{name: "bash", args: map[string]any{"command": 7}},
		{name: "grep", args: map[string]any{"pattern": 7}},
	}
	for _, test := range invalidCalls {
		t.Run(test.name+"/"+invalidArgumentLabel(test.args), func(t *testing.T) {
			beforePolicy, beforeDispatch := policyCalls.Load(), dispatchCalls.Load()
			_, err := func() (*ToolResult, error) {
				definition, getErr := registry.Get(ctx, test.name)
				if getErr != nil {
					return nil, getErr
				}
				return definition.Execute(ctx, ToolCall{Name: test.name, Args: test.args})
			}()
			require.Error(t, err)
			assert.Equal(t, beforePolicy, policyCalls.Load(), "invalid schema input must fail before approval policy")
			assert.Equal(t, beforeDispatch, dispatchCalls.Load(), "invalid schema input must not reach the executor wrapper")
		})
	}
}

func invalidArgumentLabel(args map[string]any) string {
	for key := range args {
		if key == "unexpected" {
			return "unknown-field"
		}
	}
	if value, exists := args["type"]; exists && value == "x" {
		return "invalid-type-enum"
	}
	if value, exists := args["sort"]; exists && value == "modified" {
		return "invalid-sort-enum"
	}
	if value, exists := args["max_depth"]; exists && value == 1.5 {
		return "fractional-max-depth"
	}
	return "invalid-shape"
}

func requireSchemaObject(t *testing.T, value any) map[string]any {
	t.Helper()
	object, ok := value.(map[string]any)
	require.True(t, ok, "expected schema object, got %T", value)
	return object
}

func schemaStringArray(t *testing.T, value any) []string {
	t.Helper()
	if value == nil {
		return nil
	}
	switch values := value.(type) {
	case []string:
		return values
	case []any:
		result := make([]string, 0, len(values))
		for _, value := range values {
			item, ok := value.(string)
			require.True(t, ok, "expected string array item, got %T", value)
			result = append(result, item)
		}
		return result
	default:
		require.FailNow(t, "expected string array, got %T", value)
		return nil
	}
}
