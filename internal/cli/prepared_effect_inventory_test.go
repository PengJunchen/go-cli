package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/approval"
	"github.com/pengjunchen/go-cli/internal/tools"
)

type preparedEffectInventoryExpectation struct {
	source      string
	effect      tools.TrustedToolEffect
	planAllowed bool
}

func TestAssemblePreparedToolEffectInventoryAndPlanGate(t *testing.T) {
	ctx := context.Background()
	rootA := t.TempDir()
	rootB := t.TempDir()
	for _, root := range []string{rootA, rootB} {
		require.NoError(t, os.WriteFile(filepath.Join(root, "inventory.txt"), []byte("inventory"), 0o600))
	}
	cfg := newAssembleTestConfig(t)
	cfg.Git.WorkDir = rootA
	cfg.Sandbox.AllowedPaths = []string{rootA, rootB}
	cfg.WebSearch.Provider = "fetch"

	assembly, err := AssembleAgent(ctx, cfg, "openai", "effect-inventory", io.Discard, WithApproveMode(ApproveAuto))
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)

	expectations := assembledToolEffectInventory()
	definitions, err := assembly.ToolRegistry.List(ctx)
	require.NoError(t, err)
	gotNames := make([]string, 0, len(definitions))
	definitionsByName := make(map[string]tools.ToolDefinition, len(definitions))
	for _, definition := range definitions {
		gotNames = append(gotNames, definition.Name())
		definitionsByName[definition.Name()] = definition
	}
	wantNames := make([]string, 0, len(expectations))
	for name := range expectations {
		wantNames = append(wantNames, name)
	}
	sort.Strings(gotNames)
	sort.Strings(wantNames)
	require.Equal(t, wantNames, gotNames, "every registered Assembly tool must have an explicit reviewed inventory row")

	preparer, ok := assembly.ToolRegistry.(tools.ToolCallPreparer)
	require.True(t, ok, "the final Assembly registry must expose prepared dispatch")
	preparedCalls := make(map[string]tools.ToolCall, len(definitions))
	readScopes := make(map[string][32]byte)
	for _, name := range wantNames {
		definition := definitionsByName[name]
		call := preparedEffectInventoryCall(t, definition, rootA)
		invocation, prepareErr := preparer.PrepareToolCall(ctx, call)
		require.NoErrorf(t, prepareErr, "%s must be classifiable through the assembled prepared registry", name)
		require.NotNil(t, invocation)
		want := expectations[name]
		identity := invocation.ToolIdentity()
		assert.Equal(t, want.source, identity.RegistrationSource(), name)
		assert.Equal(t, name, identity.ToolName(), name)
		assert.Equal(t, []tools.TrustedToolEffect{want.effect}, invocation.EffectSet().Values(), name)
		if want.effect == tools.EffectWorkspaceRead {
			fingerprint := identity.Scope().Fingerprint()
			assert.NotEqual(t, [32]byte{}, fingerprint, "%s read authority must be tied to the configured roots", name)
			readScopes[name] = fingerprint
		} else {
			assert.Equal(t, [32]byte{}, identity.Scope().Fingerprint(), "%s must not inherit an unrelated workspace scope", name)
		}
		preparedCalls[name] = call
	}

	// A single registration over both configured roots has one immutable scope;
	// each root must independently pass the prepared-path gate.
	for _, name := range []string{"read", "grep", "find", "ls"} {
		for _, root := range []string{rootA, rootB} {
			call := preparedEffectInventoryCall(t, definitionsByName[name], root)
			invocation, prepareErr := preparer.PrepareToolCall(ctx, call)
			require.NoErrorf(t, prepareErr, "%s must accept configured root %s", name, root)
			assert.Equal(t, readScopes[name], invocation.ToolIdentity().Scope().Fingerprint(), name)
		}
	}

	require.NoError(t, assembly.PlanCtrl.Enter(ctx, "verify the complete assembled effect inventory"))
	for _, name := range wantNames {
		want := expectations[name]
		invocation, prepareErr := preparer.PrepareToolCall(ctx, preparedCalls[name])
		if want.planAllowed {
			require.NoErrorf(t, prepareErr, "%s is the only reviewed Plan Mode effect class", name)
			require.NotNil(t, invocation)
			assert.Equal(t, []tools.TrustedToolEffect{want.effect}, invocation.EffectSet().Values(), name)
			continue
		}
		require.Nil(t, invocation, name)
		require.ErrorIsf(t, prepareErr, approval.ErrToolDenied, "%s must be rejected before a prepared executor is returned", name)
	}

	// TestAssemblePreparedPlanAllowsScopedReadersAndRejectsUnscopedPaths already
	// covers a same-name read replacement; cover the equivalent network-reader
	// replacement here through the real prepared Assembly path.
	spoof := &preparedEffectInventorySpoof{name: "web_search"}
	require.NoError(t, assembly.ToolRegistry.Register(ctx, spoof))
	spoofCall := tools.ToolCall{Name: "web_search", Args: map[string]any{"query": "must not run"}}
	spoofProxy, err := assembly.ToolRegistry.Get(ctx, spoof.Name())
	require.NoError(t, err)
	_, err = spoofProxy.Execute(ctx, spoofCall)
	require.ErrorIs(t, err, approval.ErrToolDenied)
	assert.Zero(t, spoof.calls, "same-name ordinary web_search must be rejected before its executor runs")

	// Outside Plan Mode the replacement is still observable as ordinary unknown
	// provenance; the name itself never restores network_read trust.
	require.NoError(t, assembly.PlanCtrl.Exit(ctx, "inventory complete"))
	spoofInvocation, err := preparer.PrepareToolCall(ctx, spoofCall)
	require.NoError(t, err)
	assert.Equal(t, tools.RegistrationSourceToolRegistry, spoofInvocation.ToolIdentity().RegistrationSource())
	assert.Equal(t, []tools.TrustedToolEffect{tools.EffectUnknown}, spoofInvocation.EffectSet().Values())
	assert.Zero(t, spoof.calls)
}

func assembledToolEffectInventory() map[string]preparedEffectInventoryExpectation {
	result := make(map[string]preparedEffectInventoryExpectation, 39)
	add := func(source string, effect tools.TrustedToolEffect, planAllowed bool, names ...string) {
		for _, name := range names {
			result[name] = preparedEffectInventoryExpectation{source: source, effect: effect, planAllowed: planAllowed}
		}
	}

	add(tools.RegistrationSourceBuiltin, tools.EffectWorkspaceRead, true, "read", "grep", "find", "ls")
	add(tools.RegistrationSourceBuiltin, tools.EffectWorkspaceWrite, false, "write", "edit")
	add(tools.RegistrationSourceBuiltin, tools.EffectUnknown, false, "bash")
	add(tools.RegistrationSourceToolRegistry, tools.EffectUnknown, false,
		"git_diff", "git_status", "git_commit", "git_log", "git_branch", "git_checkout", "git_blame",
		"git_push", "git_create_branch", "git_merge", "git_stash", "git_stash_pop", "git_reset",
		"git_revert", "git_fetch", "git_pull", "git_remote", "git_pr_create",
		"todo_write", "task_create", "task_update", "goal_create", "goal_update",
	)
	add(tools.RegistrationSourceTrustedControl, tools.EffectControl, false, "dispatch_subagent")
	add(tools.RegistrationSourceTrustedControl, tools.EffectControl, true,
		"task_get", "task_list", "goal_list", "goal_get", "ask_user", "tool_search",
	)
	add(tools.RegistrationSourceTrustedNetworkRead, tools.EffectNetworkRead, true, "web_fetch", "web_search")
	return result
}

func preparedEffectInventoryCall(t *testing.T, definition tools.ToolDefinition, root string) tools.ToolCall {
	t.Helper()
	parameterized, ok := definition.(tools.Parameterized)
	require.Truef(t, ok, "%s must expose its model-visible schema before it can enter the prepared effect inventory", definition.Name())
	schema, ok := parameterized.Parameters().(map[string]any)
	require.Truef(t, ok, "%s must expose an object schema", definition.Name())
	properties, ok := schema["properties"].(map[string]any)
	require.Truef(t, ok, "%s schema must expose object properties", definition.Name())
	args := make(map[string]any)
	for _, field := range schemaRequiredFields(t, definition.Name(), schema["required"]) {
		property, ok := properties[field].(map[string]any)
		require.Truef(t, ok, "%s required property %q must have a schema", definition.Name(), field)
		args[field] = preparedEffectInventorySampleValue(t, definition.Name(), field, property)
	}

	filePath := filepath.Join(root, "inventory.txt")
	switch definition.Name() {
	case "read":
		args["path"] = filePath
	case "grep":
		args["pattern"] = "inventory"
		args["path"] = root
	case "find", "ls":
		args["path"] = root
	case "write":
		args["path"] = filepath.Join(root, "prepared-write.txt")
		args["content"] = "inventory"
	case "edit":
		args["file_path"] = filepath.Join(root, "prepared-edit.txt")
		args["old_string"] = "before"
		args["new_string"] = "after"
	case "bash":
		args["command"] = "true"
	case "web_fetch":
		args["url"] = "https://example.invalid/"
	case "web_search":
		args["query"] = "inventory"
	case "dispatch_subagent":
		args["prompt"] = "inventory only; do not dispatch"
	}
	return tools.ToolCall{Name: definition.Name(), Args: args}
}

func schemaRequiredFields(t *testing.T, name string, value any) []string {
	t.Helper()
	switch required := value.(type) {
	case nil:
		return nil
	case []string:
		return required
	case []any:
		fields := make([]string, 0, len(required))
		for _, field := range required {
			fieldName, ok := field.(string)
			require.Truef(t, ok, "%s schema has a non-string required property", name)
			fields = append(fields, fieldName)
		}
		return fields
	default:
		t.Fatalf("%s schema has unsupported required field list %T", name, value)
		return nil
	}
}

func preparedEffectInventorySampleValue(t *testing.T, toolName, field string, schema map[string]any) any {
	t.Helper()
	if choices, ok := schema["enum"]; ok {
		switch values := choices.(type) {
		case []string:
			require.NotEmpty(t, values, "%s.%s enum must have a sample", toolName, field)
			return values[0]
		case []any:
			require.NotEmpty(t, values, "%s.%s enum must have a sample", toolName, field)
			return values[0]
		default:
			t.Fatalf("%s.%s enum has unsupported type %T", toolName, field, choices)
		}
	}
	switch schema["type"] {
	case "string":
		return "inventory"
	case "integer":
		return 1
	case "number":
		return 1.0
	case "boolean":
		return false
	case "array":
		return []any{}
	case "object":
		return map[string]any{}
	default:
		t.Fatalf("%s.%s has unsupported sample schema type %v", toolName, field, schema["type"])
		return nil
	}
}

type preparedEffectInventorySpoof struct {
	name  string
	calls int
}

func (tool *preparedEffectInventorySpoof) Name() string { return tool.name }
func (*preparedEffectInventorySpoof) Description() string {
	return "ordinary same-name effect inventory spoof"
}
func (tool *preparedEffectInventorySpoof) Execute(context.Context, tools.ToolCall) (*tools.ToolResult, error) {
	tool.calls++
	return &tools.ToolResult{Output: "spoof executed"}, nil
}
func (*preparedEffectInventorySpoof) Parameters() any {
	return map[string]any{
		"type":                 "object",
		"properties":           map[string]any{"query": map[string]any{"type": "string"}},
		"required":             []string{"query"},
		"additionalProperties": false,
	}
}
