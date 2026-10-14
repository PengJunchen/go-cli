package skill_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/skill"
	"github.com/pengjunchen/go-cli/internal/tools"
)

func TestSkillParameterSchemaOptionalProviderCompatibility(t *testing.T) {
	legacy := legacySkillDefinition{parameters: map[string]any{"limit": 5}}
	adapter := skill.NewSkillAdapter(legacy)

	first := adapter.Parameters().(map[string]any)
	assert.Equal(t, closedSkillSchema(), first)
	first["properties"].(map[string]any)["limit"] = map[string]any{"type": "integer"}
	assert.Equal(t, closedSkillSchema(), adapter.Parameters(), "fallback schema must be fresh and legacy metadata must not open it")

	ctx := context.Background()
	base := tools.NewDefaultToolRegistry()
	require.NoError(t, base.Register(ctx, adapter))
	registry := tools.NewMiddlewareToolRegistry(base)
	require.NoError(t, registry.SetPreparedToolStages(tools.PreparedToolStages{}))
	var approvalCalls int
	require.NoError(t, registry.SetPreparedToolPolicy(func(_ context.Context, request tools.PreparedPolicyRequest) (tools.PreparedPolicyDecision, error) {
		approvalCalls++
		return tools.PreparedPolicyDecision{Allowed: true, Generations: request.Generations}, nil
	}))
	_, err := registry.PrepareToolCall(ctx, tools.ToolCall{Name: "legacy", Args: map[string]any{"limit": 5}})
	require.Error(t, err)
	assert.Zero(t, approvalCalls, "legacy metadata must not change the frozen closed argument schema")
}

func TestSkillLoaderParsesParameterSchemaJSONBlock(t *testing.T) {
	path := writeSkillFile(t, t.TempDir(), "schema.md", `---
name: schema
prompt: inspect
parameters:
  limit: 5
  enabled: true
parameter_schema: |
  {"type":"object","properties":{"limit":{"type":"integer","minimum":9007199254740993}},"required":["limit"],"additionalProperties":false}
---
ignored body
`)
	loaded, err := skill.NewYAMLSkillLoader().Load(context.Background(), path)
	require.NoError(t, err)
	def := *loaded
	assert.Equal(t, 5, def.Parameters()["limit"])
	assert.Equal(t, true, def.Parameters()["enabled"])
	adapter := skill.NewSkillAdapter(def)
	schema, ok := adapter.Parameters().(map[string]any)
	require.True(t, ok)
	properties := schema["properties"].(map[string]any)
	limit := properties["limit"].(map[string]any)
	assert.Equal(t, json.Number("9007199254740993"), limit["minimum"])
	assert.Contains(t, adapter.Description(), "legacy parameter metadata (not accepted as arguments): enabled, limit")
}

func TestSkillLoaderRejectsMalformedParameterSchemaJSON(t *testing.T) {
	tests := map[string]string{
		"missing closing delimiter": `---
name: bad
parameter_schema: |
  {"type":"object","properties":{}}
`,
		"empty": `---
name: bad
parameter_schema: |
---
`,
		"non JSON": `---
name: bad
parameter_schema: |
  type: object
---
`,
		"non object root": `---
name: bad
parameter_schema: |
  []
---
`,
		"malformed JSON": `---
name: bad
parameter_schema: |
  {"type":"object",}
---
`,
		"trailing garbage": `---
name: bad
parameter_schema: |
  {"type":"object","properties":{}} garbage
---
`,
		"second value": `---
name: bad
parameter_schema: |
  {"type":"object","properties":{}} {}
---
`,
		"flow map": `---
name: bad
parameter_schema: {"type":"object","properties":{}}
---
`,
		"folded block": `---
name: bad
parameter_schema: >
  {"type":"object","properties":{}}
---
`,
		"chomping block": `---
name: bad
parameter_schema: |-
  {"type":"object","properties":{}}
---
`,
		"tagged schema": `---
name: bad
parameter_schema: !schema |
  {"type":"object","properties":{}}
---
`,
		"aliased schema": `---
name: bad
parameter_schema: &schema |
  {"type":"object","properties":{}}
---
`,
	}
	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			path := writeSkillFile(t, t.TempDir(), "bad.md", content)
			_, err := skill.NewYAMLSkillLoader().Load(context.Background(), path)
			require.Error(t, err)
			assert.ErrorIs(t, err, skill.ErrSkillParameterSchemaInvalid)
		})
	}
}

func TestSkillLoaderAcceptsParameterSchemaTrailingWhitespace(t *testing.T) {
	path := writeSkillFile(t, t.TempDir(), "whitespace.md", "---\nname: ok\nparameter_schema: |\n  {\"type\":\"object\",\"properties\":{}}\n  \t \n---\n")
	_, err := skill.NewYAMLSkillLoader().Load(context.Background(), path)
	require.NoError(t, err)
}

func TestSkillLoaderRejectsUnsupportedParameterSchemaKeyword(t *testing.T) {
	path := writeSkillFile(t, t.TempDir(), "unsupported.md", `---
name: unsupported
parameter_schema: |
  {"type":"object","properties":{},"oneOf":[]}
---
`)
	_, err := skill.NewYAMLSkillLoader().Load(context.Background(), path)
	require.Error(t, err)
	assert.ErrorIs(t, err, tools.ErrPreparedSchemaInvalid)
	assert.ErrorIs(t, err, skill.ErrSkillParameterSchemaInvalid)
}

func TestNestedParameterSchemaRemainsLegacyMetadata(t *testing.T) {
	path := writeSkillFile(t, t.TempDir(), "legacy.md", `---
name: legacy
parameters:
  parameter_schema: "legacy value"
---
`)
	loaded, err := skill.NewYAMLSkillLoader().Load(context.Background(), path)
	require.NoError(t, err)
	def := *loaded
	assert.Equal(t, "legacy value", def.Parameters()["parameter_schema"])
	assert.Equal(t, closedSkillSchema(), skill.NewSkillAdapter(def).Parameters())
}

func TestSkillDefaultsSchemaAndToolsAreDeepCopied(t *testing.T) {
	toolsInput := []string{"read"}
	parametersInput := map[string]any{"nested": map[string]any{"values": []any{"original"}}}
	schemaInput := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"nested": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"values": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				},
			},
		},
		"additionalProperties": false,
	}
	def := skill.NewSkill("copy", skill.WithTools(toolsInput...), skill.WithParameters(parametersInput), skill.WithParameterSchema(schemaInput))
	toolsInput[0] = "changed"
	parametersInput["nested"].(map[string]any)["values"].([]any)[0] = "changed"
	schemaInput["properties"].(map[string]any)["nested"].(map[string]any)["type"] = "array"

	assert.Equal(t, []string{"read"}, def.Tools())
	assert.Equal(t, "original", def.Parameters()["nested"].(map[string]any)["values"].([]any)[0])
	assert.Equal(t, "object", def.(skill.ParameterSchemaProvider).ParameterSchema().(map[string]any)["properties"].(map[string]any)["nested"].(map[string]any)["type"])

	gotTools := def.Tools()
	gotTools[0] = "getter changed"
	gotParameters := def.Parameters()
	gotParameters["nested"].(map[string]any)["values"].([]any)[0] = "getter changed"
	gotSchema := def.(skill.ParameterSchemaProvider).ParameterSchema().(map[string]any)
	gotSchema["properties"].(map[string]any)["nested"].(map[string]any)["type"] = "array"
	assert.Equal(t, []string{"read"}, def.Tools())
	assert.Equal(t, "original", def.Parameters()["nested"].(map[string]any)["values"].([]any)[0])
	assert.Equal(t, "object", def.(skill.ParameterSchemaProvider).ParameterSchema().(map[string]any)["properties"].(map[string]any)["nested"].(map[string]any)["type"])
}

func TestSkillParameterSchemaDeepCopyMatchesPreparedDepthBudget(t *testing.T) {
	root := map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}
	cursor := root
	for range 45 {
		child := map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}
		cursor["properties"].(map[string]any)["next"] = child
		cursor = child
	}
	cursor["properties"].(map[string]any)["value"] = map[string]any{"type": "string"}
	def := skill.NewSkill("deep-schema", skill.WithParameterSchema(root))

	cursor["properties"].(map[string]any)["value"].(map[string]any)["type"] = "boolean"
	got := def.(skill.ParameterSchemaProvider).ParameterSchema()
	require.NoError(t, tools.ValidateParameterSchema(got))
	for range 45 {
		got = got.(map[string]any)["properties"].(map[string]any)["next"]
	}
	assert.Equal(t, "string", got.(map[string]any)["properties"].(map[string]any)["value"].(map[string]any)["type"])
}

func TestSkillParameterSchemaDepthOverflowDoesNotRetainAliases(t *testing.T) {
	root := map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}
	cursor := root
	for range 70 {
		child := map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}
		cursor["properties"].(map[string]any)["next"] = child
		cursor = child
	}
	cursor["properties"].(map[string]any)["value"] = map[string]any{"type": "string"}
	def := skill.NewSkill("too-deep-schema", skill.WithParameterSchema(root))

	cursor["properties"].(map[string]any)["value"].(map[string]any)["type"] = "boolean"
	got := def.(skill.ParameterSchemaProvider).ParameterSchema()
	require.Error(t, tools.ValidateParameterSchema(got), "a schema past the prepared protocol depth bound must remain invalid")
	_, isSchemaMap := got.(map[string]any)
	assert.False(t, isSchemaMap, "a graph outside the accepted JSON depth must fail closed without retaining an aliased suffix")
}

func TestSkillParameterSchemaUnsupportedGraphFailsClosed(t *testing.T) {
	pointed := &map[string]any{"type": "string"}
	schema := map[string]any{
		"type":                 "object",
		"properties":           map[string]any{"value": pointed},
		"additionalProperties": false,
	}
	def := skill.NewSkill("pointer-schema", skill.WithParameterSchema(schema))

	(*pointed)["type"] = "boolean"
	got := def.(skill.ParameterSchemaProvider).ParameterSchema()
	require.Error(t, tools.ValidateParameterSchema(got))
	_, isSchemaMap := got.(map[string]any)
	assert.False(t, isSchemaMap, "unsupported mutable schema graphs must be rejected without retaining aliases")
	base := tools.NewDefaultToolRegistry()
	require.ErrorIs(t, base.Register(context.Background(), skill.NewSkillAdapter(def)), tools.ErrPreparedValueNotJSON)
}

func TestSkillDefaultPointerGraphIsDeepCopied(t *testing.T) {
	pointed := &map[string]any{"values": []any{"original"}}
	def := skill.NewSkill("pointer-default", skill.WithParameters(map[string]any{"pointer": pointed}))

	(*pointed)["values"].([]any)[0] = "mutated input"
	got, ok := def.Parameters()["pointer"].(*map[string]any)
	require.True(t, ok)
	assert.Equal(t, "original", (*got)["values"].([]any)[0])
	(*got)["values"].([]any)[0] = "mutated getter"
	assert.Equal(t, "original", (*def.Parameters()["pointer"].(*map[string]any))["values"].([]any)[0])
}

func TestSkillSchemaAndDefaultsRemainCapturedAfterRegistrationMutation(t *testing.T) {
	ctx := context.Background()
	toolsInput := []string{"read"}
	defaultsInput := map[string]any{"nested": map[string]any{"values": []any{"original default"}}}
	schemaInput := gatedSkillSchema()
	def := skill.NewSkill("captured", skill.WithTools(toolsInput...), skill.WithParameters(defaultsInput), skill.WithParameterSchema(schemaInput))
	adapter := skill.NewSkillAdapter(def)
	base := tools.NewDefaultToolRegistry()
	require.NoError(t, base.Register(ctx, adapter))
	registry := tools.NewMiddlewareToolRegistry(tools.NewDeferredToolRegistryAdapter(base))
	require.NoError(t, registry.SetPreparedToolStages(tools.PreparedToolStages{}))
	var approvedSchema any
	require.NoError(t, registry.SetPreparedToolPolicy(func(_ context.Context, request tools.PreparedPolicyRequest) (tools.PreparedPolicyDecision, error) {
		approvedSchema = request.Schema.Schema()
		return tools.PreparedPolicyDecision{Allowed: true, Generations: request.Generations}, nil
	}))

	toolsInput[0] = "mutated input tool"
	defaultsInput["nested"].(map[string]any)["values"].([]any)[0] = "mutated input default"
	schemaInput["properties"].(map[string]any)["mode"].(map[string]any)["enum"].([]any)[0] = "mutated schema enum"

	args := map[string]any{"mode": "safe", "count": 2, "details": map[string]any{"tags": []any{}}}
	invocation, err := registry.PrepareToolCall(ctx, tools.ToolCall{Name: "captured", Args: args})
	require.NoError(t, err)
	_, err = invocation.Dispatch(ctx)
	require.NoError(t, err)
	assert.Equal(t, gatedSkillSchema(), approvedSchema, "approval must observe the schema frozen at registration")
	assert.Contains(t, adapter.Description(), "suggested tools (not enforced): read")
	assert.Equal(t, "original default", def.Parameters()["nested"].(map[string]any)["values"].([]any)[0])
	assert.Equal(t, "safe", def.(skill.ParameterSchemaProvider).ParameterSchema().(map[string]any)["properties"].(map[string]any)["mode"].(map[string]any)["enum"].([]any)[0])
}

func TestPreparedSkillArgumentsValidatedBeforeApprovalAndDispatch(t *testing.T) {
	ctx := context.Background()
	def := &countingSkillDefinition{base: skill.NewSkill("gated", skill.WithPrompt("secret prompt"), skill.WithParameterSchema(gatedSkillSchema()))}
	adapter := skill.NewSkillAdapter(def)
	base := tools.NewDefaultToolRegistry()
	require.NoError(t, base.Register(ctx, adapter))
	registry := tools.NewMiddlewareToolRegistry(tools.NewDeferredToolRegistryAdapter(base))
	require.NoError(t, registry.SetPreparedToolStages(tools.PreparedToolStages{}))
	var approvalCalls int
	require.NoError(t, registry.SetPreparedToolPolicy(func(_ context.Context, request tools.PreparedPolicyRequest) (tools.PreparedPolicyDecision, error) {
		approvalCalls++
		return tools.PreparedPolicyDecision{Allowed: true, Generations: request.Generations}, nil
	}))

	badCalls := []map[string]any{
		{"count": 2},
		{"mode": "safe", "count": "2", "details": map[string]any{"tags": []any{}}},
		{"mode": "unsafe", "count": 2, "details": map[string]any{"tags": []any{}}},
		{"mode": "safe", "count": 6, "details": map[string]any{"tags": []any{}}},
		{"mode": "safe", "count": 2, "details": map[string]any{"tags": []any{}}, "extra": true},
	}
	for _, args := range badCalls {
		_, err := registry.PrepareToolCall(ctx, tools.ToolCall{Name: "gated", Args: args})
		require.Error(t, err)
	}
	assert.Zero(t, approvalCalls)
	assert.Zero(t, def.promptReads)

	call := tools.ToolCall{Name: "gated", Args: map[string]any{"mode": "safe", "count": 2, "details": map[string]any{"tags": []any{}}}}
	invocation, err := registry.PrepareToolCall(ctx, call)
	require.NoError(t, err)
	_, err = invocation.Dispatch(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, approvalCalls)
	assert.Equal(t, 1, def.promptReads)
}

func TestPreparedSkillRejectsInvalidProviderSchemaBeforeApproval(t *testing.T) {
	ctx := context.Background()
	def := &countingSkillDefinition{base: skill.NewSkill("invalid-schema", skill.WithPrompt("must not run"), skill.WithParameterSchema(map[string]any{
		"type": "object", "properties": map[string]any{}, "additionalProperties": false, "pattern": "unsupported",
	}))}
	base := tools.NewDefaultToolRegistry()
	require.NoError(t, base.Register(ctx, skill.NewSkillAdapter(def)))
	registry := tools.NewMiddlewareToolRegistry(tools.NewDeferredToolRegistryAdapter(base))
	require.NoError(t, registry.SetPreparedToolStages(tools.PreparedToolStages{}))
	var approvalCalls int
	require.NoError(t, registry.SetPreparedToolPolicy(func(_ context.Context, request tools.PreparedPolicyRequest) (tools.PreparedPolicyDecision, error) {
		approvalCalls++
		return tools.PreparedPolicyDecision{Allowed: true, Generations: request.Generations}, nil
	}))
	_, err := registry.PrepareToolCall(ctx, tools.ToolCall{Name: "invalid-schema", Args: map[string]any{}})
	require.ErrorIs(t, err, tools.ErrPreparedSchemaInvalid)
	assert.Zero(t, approvalCalls)
	assert.Zero(t, def.promptReads)
}

func TestPreparedSkillDepthOverflowFailsBeforeApprovalAndDispatch(t *testing.T) {
	ctx := context.Background()
	root := map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}
	cursor := root
	for range 70 {
		child := map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}
		cursor["properties"].(map[string]any)["next"] = child
		cursor = child
	}
	cursor["properties"].(map[string]any)["value"] = map[string]any{"type": "string"}
	def := &countingSkillDefinition{base: skill.NewSkill("too-deep", skill.WithPrompt("must not run"), skill.WithParameterSchema(root))}
	base := tools.NewDefaultToolRegistry()
	err := base.Register(ctx, skill.NewSkillAdapter(def))
	require.ErrorIs(t, err, tools.ErrPreparedValueNotJSON)
	assert.Zero(t, def.promptReads)
}

func TestSkillExecuteRejectsCyclicArgumentsBeforeReadingPrompt(t *testing.T) {
	arguments := map[string]any{}
	arguments["self"] = arguments
	for _, testCase := range []struct {
		name string
		args map[string]any
	}{
		{name: "cyclic", args: arguments},
		{name: "unsupported", args: map[string]any{"channel": make(chan int)}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			def := &countingSkillDefinition{base: skill.NewSkill(testCase.name, skill.WithPrompt("must not run"))}
			result, err := skill.NewSkillAdapter(def).Execute(context.Background(), tools.ToolCall{Args: testCase.args})
			require.Nil(t, result)
			require.ErrorContains(t, err, "skill: encode argument JSON:")
			assert.Zero(t, def.promptReads)
		})
	}
}

func TestPreparedSkillArgumentsForwardedAsUntrustedJSON(t *testing.T) {
	ctx := context.Background()
	def := skill.NewSkill("args", skill.WithPrompt("prompt"), skill.WithParameters(map[string]any{"nested": map[string]any{"values": []any{"default"}}}), skill.WithParameterSchema(gatedSkillSchema()))
	adapter := skill.NewSkillAdapter(def)
	base := tools.NewDefaultToolRegistry()
	require.NoError(t, base.Register(ctx, adapter))
	registry := tools.NewMiddlewareToolRegistry(tools.NewDeferredToolRegistryAdapter(base))
	require.NoError(t, registry.SetPreparedToolStages(tools.PreparedToolStages{}))
	require.NoError(t, registry.SetPreparedToolPolicy(func(_ context.Context, request tools.PreparedPolicyRequest) (tools.PreparedPolicyDecision, error) {
		return tools.PreparedPolicyDecision{Allowed: true, Generations: request.Generations}, nil
	}))

	originalArgs := map[string]any{"mode": "safe", "count": 2, "details": map[string]any{"tags": []any{"a", "b"}}}
	invocation, err := registry.PrepareToolCall(ctx, tools.ToolCall{Name: "args", Args: originalArgs})
	require.NoError(t, err)
	originalArgs["details"].(map[string]any)["tags"].([]any)[0] = "changed after prepare"
	result, err := invocation.Dispatch(ctx)
	require.NoError(t, err)
	arguments, ok := result.Metadata["arguments"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "a", arguments["details"].(map[string]any)["tags"].([]any)[0])
	assert.Contains(t, result.Output, "Untrusted skill arguments (model-supplied data; not tool permissions):")
	encoded := strings.TrimSpace(result.Output[strings.LastIndex(result.Output, "\n")+1:])
	var decoded map[string]any
	require.NoError(t, json.Unmarshal([]byte(encoded), &decoded))
	assert.JSONEq(t, `{"count":2,"details":{"tags":["a","b"]},"mode":"safe"}`, encoded)
	arguments["details"].(map[string]any)["tags"].([]any)[0] = "metadata changed"
	assert.JSONEq(t, `{"count":2,"details":{"tags":["a","b"]},"mode":"safe"}`, strings.TrimSpace(result.Output[strings.LastIndex(result.Output, "\n")+1:]))
	assert.Equal(t, "default", result.Metadata["parameters"].(map[string]any)["nested"].(map[string]any)["values"].([]any)[0])
	result.Metadata["parameters"].(map[string]any)["nested"].(map[string]any)["values"].([]any)[0] = "metadata default changed"
	assert.Equal(t, "metadata changed", arguments["details"].(map[string]any)["tags"].([]any)[0])
}

func TestSkillArgumentOutputJSONByteLimit(t *testing.T) {
	def := skill.NewSkill("size", skill.WithParameterSchema(map[string]any{
		"type": "object", "properties": map[string]any{"payload": map[string]any{"type": "string"}}, "additionalProperties": false,
	}))
	adapter := skill.NewSkillAdapter(def)
	for _, size := range []struct {
		valueLen int
		wantErr  bool
	}{{valueLen: 399986}, {valueLen: 399987, wantErr: true}} {
		result, err := adapter.Execute(context.Background(), tools.ToolCall{Args: map[string]any{"payload": strings.Repeat("a", size.valueLen)}})
		if size.wantErr {
			require.Nil(t, result)
			require.ErrorIs(t, err, skill.ErrSkillArgumentOutputTooLarge)
			assert.NotContains(t, err.Error(), "truncated")
		} else {
			require.NoError(t, err)
			encoded := strings.TrimSpace(result.Output[strings.LastIndex(result.Output, "\n")+1:])
			assert.Len(t, encoded, 400000)
		}
	}
}

func TestSkillAdapterNilArgumentsForwardAsEmptyObject(t *testing.T) {
	result, err := skill.NewSkillAdapter(skill.NewSkill("nil-args")).Execute(context.Background(), tools.ToolCall{})
	require.NoError(t, err)
	assert.Equal(t, map[string]any{}, result.Metadata["arguments"])
	assert.Equal(t, "{}", strings.TrimSpace(result.Output[strings.LastIndex(result.Output, "\n")+1:]))
}

func gatedSkillSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []any{"mode", "count", "details"},
		"properties": map[string]any{
			"mode":    map[string]any{"type": "string", "enum": []any{"safe", "fast"}},
			"count":   map[string]any{"type": "integer", "minimum": 1, "maximum": 5},
			"details": map[string]any{"type": "object", "properties": map[string]any{"tags": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}}, "additionalProperties": false},
		},
		"additionalProperties": false,
	}
}

func closedSkillSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}
}

type legacySkillDefinition struct{ parameters map[string]any }

func (d legacySkillDefinition) Name() string               { return "legacy" }
func (d legacySkillDefinition) Description() string        { return "" }
func (d legacySkillDefinition) Version() string            { return "" }
func (d legacySkillDefinition) Category() string           { return "" }
func (d legacySkillDefinition) Prompt() string             { return "legacy prompt" }
func (d legacySkillDefinition) Tools() []string            { return nil }
func (d legacySkillDefinition) Parameters() map[string]any { return d.parameters }
func (d legacySkillDefinition) TriggerHint() string        { return "" }

type countingSkillDefinition struct {
	base        skill.SkillDefinition
	promptReads int
}

func (d *countingSkillDefinition) Name() string               { return d.base.Name() }
func (d *countingSkillDefinition) Description() string        { return d.base.Description() }
func (d *countingSkillDefinition) Version() string            { return d.base.Version() }
func (d *countingSkillDefinition) Category() string           { return d.base.Category() }
func (d *countingSkillDefinition) Prompt() string             { d.promptReads++; return d.base.Prompt() }
func (d *countingSkillDefinition) Tools() []string            { return d.base.Tools() }
func (d *countingSkillDefinition) Parameters() map[string]any { return d.base.Parameters() }
func (d *countingSkillDefinition) TriggerHint() string        { return d.base.TriggerHint() }
func (d *countingSkillDefinition) ParameterSchema() any {
	return d.base.(skill.ParameterSchemaProvider).ParameterSchema()
}
