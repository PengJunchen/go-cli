package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/approval"
	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/extension"
	"github.com/pengjunchen/go-cli/internal/tools"
)

const (
	r7ProjectionSchemaErrorCanary = "schema-secret-canary-r7-27d1"
	r7ProjectionSourceErrorCanary = "source-path-canary-r7-61fa"
)

func TestAssembleAgentRejectsProjectExtensionToolProjectionFailure(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	t.Chdir(root)
	home := t.TempDir()
	t.Setenv("HOME", home)

	pluginPath := filepath.Join(root, ".go-cli", "extensions", r7ProjectionSourceErrorCanary+".so")
	require.NoError(t, os.MkdirAll(filepath.Dir(pluginPath), 0o700))
	capturedBytes := []byte("captured project plugin bytes")
	require.NoError(t, os.WriteFile(pluginPath, capturedBytes, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".go-cli.yaml"), []byte(
		"extensions:\n  enabled: true\n  plugin_paths:\n    - .go-cli/extensions/"+r7ProjectionSourceErrorCanary+".so\n"), 0o600))
	globalConfigDir := filepath.Join(home, ".go-cli")
	require.NoError(t, os.MkdirAll(globalConfigDir, 0o700))
	globalConfig := "provider:\n  name: openai\n  api_key: test-key\n  base_url: http://127.0.0.1:1/v1\n  model: projection-test\nmodel:\n  name: projection-test\ntracing:\n  enabled: false\n"
	require.NoError(t, os.WriteFile(filepath.Join(globalConfigDir, "config.yaml"), []byte(globalConfig), 0o600))

	trustManager := approval.NewDefaultTrustManager(approval.NewFileTrustStore(filepath.Join(home, ".config", "go-cli", "trust.json")))
	uncurated, err := config.LoadTrustedWithManager(ctx, trustManager)
	require.NoError(t, err)
	snapshot := uncurated.CapturedProjectSourcesForGrant()
	require.NotNil(t, snapshot)
	frozenTrust, ok := trustManager.(approval.FrozenProjectSourceTrust)
	require.True(t, ok)
	require.NoError(t, frozenTrust.TrustProjectSourceSnapshot(ctx, snapshot))
	trustedConfig, err := config.LoadTrustedWithManager(ctx, trustManager)
	require.NoError(t, err)
	require.Equal(t, config.ProjectSourceTrustVerified, trustedConfig.ProjectSourceTrustStatus)
	require.NotNil(t, trustedConfig.ProjectOverlayForVerifiedSources())
	// A post-capture change must not influence the injected loader. Assembly must
	// materialize the already-authorized bytes before invoking this private seam.
	replacementBytes := []byte("replacement bytes must never reach the plugin loader")
	require.NoError(t, os.WriteFile(pluginPath, replacementBytes, 0o600))

	var prefixCalls atomic.Int32
	fixture := &r7ProjectionFailureExtension{prefixCalls: &prefixCalls}
	loader := &r7ProjectionCaptureLoader{extension: fixture}
	var logs bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(oldLogger) })

	assembly, err := AssembleAgent(ctx, trustedConfig, "openai", "projection-test", io.Discard,
		WithApproveMode(ApproveAuto), withPluginLoaderForTest(loader))
	require.Nil(t, assembly, "partial tool projection must not return a usable Assembly")
	require.Error(t, err)
	var registrationErr *ExtensionToolRegistrationError
	require.ErrorAs(t, err, &registrationErr)
	assert.Equal(t, "r7-projection-owner", registrationErr.Owner)
	assert.Equal(t, "r7-invalid-schema-tool", registrationErr.Tool)
	assert.Equal(t, "non_json_schema", registrationErr.Class)
	assert.ErrorIs(t, err, tools.ErrPreparedValueNotJSON)
	assert.Zero(t, prefixCalls.Load(), "a private successfully registered prefix must never be dispatched")
	assert.Equal(t, int32(1), fixture.shutdownCalls.Load(), "Assembly's common cleanup owner shuts down the initialized extension exactly once")

	require.Len(t, loader.loads, 1)
	assert.NotEqual(t, pluginPath, loader.loads[0].path, "the injected loader must receive the private materialization, never the source path")
	assert.Equal(t, capturedBytes, loader.loads[0].bytes, "the injected loader consumes the verified frozen source bytes")
	assert.NotEqual(t, replacementBytes, loader.loads[0].bytes)

	logText := logs.String()
	assert.Contains(t, logText, "assemble_extension_tool_register_failed")
	assert.Contains(t, logText, "owner=r7-projection-owner")
	assert.Contains(t, logText, "tool=r7-invalid-schema-tool")
	assert.Contains(t, logText, "class=non_json_schema")
	assert.NotContains(t, logText, "assemble_extensions_ready", "a failed projection cannot announce readiness")
	assert.NotContains(t, logText, r7ProjectionSourceErrorCanary)
	assert.NotContains(t, logText, r7ProjectionSchemaErrorCanary)
	assert.NotContains(t, logText, string(replacementBytes))
	assert.NotContains(t, err.Error(), r7ProjectionSourceErrorCanary)
	assert.NotContains(t, registrationErr.Error(), r7ProjectionSchemaErrorCanary)
}

func TestExtensionToolRegistrationErrorClasses(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		cause error
		class string
	}{
		{name: "non-json schema", cause: tools.ErrPreparedValueNotJSON, class: "non_json_schema"},
		{name: "reserved name", cause: fmt.Errorf("private mcp details: %w", tools.ErrMCPNameReserved), class: "name_reserved"},
		{name: "other registration error", cause: errors.New("private registration details"), class: "registration_rejected"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			registrationErr := newExtensionToolRegistrationError("owner", "tool", testCase.cause)
			assert.Equal(t, testCase.class, registrationErr.Class)
			assert.ErrorIs(t, registrationErr, testCase.cause)
			assert.NotContains(t, registrationErr.Error(), testCase.cause.Error())
		})
	}
}

func TestExtensionToolRegistrationErrorRedactsUntrustedLabels(t *testing.T) {
	cause := errors.New("schema payload must not be printed")
	registrationErr := newExtensionToolRegistrationError("/tmp/plugin-secret", "bad/name", cause)
	assert.Equal(t, "<redacted>", registrationErr.Owner)
	assert.Equal(t, "<redacted>", registrationErr.Tool)
	assert.NotContains(t, registrationErr.Error(), "/tmp/plugin-secret")
	assert.NotContains(t, registrationErr.Error(), cause.Error())
	assert.ErrorIs(t, registrationErr, cause)
}

func TestExtensionToolProjectionPreservesRegistryRegistrationContracts(t *testing.T) {
	ctx := context.Background()
	registry := tools.NewDefaultToolRegistry()
	first := &r7ProjectionTestTool{name: "shared-tool", schema: map[string]any{"type": "object", "properties": map[string]any{}}}
	second := &r7ProjectionTestTool{name: "shared-tool", schema: map[string]any{"type": "object", "properties": map[string]any{}}}
	require.NoError(t, registry.Register(ctx, first))
	require.NoError(t, registry.Register(ctx, second))

	registered, err := registry.Get(ctx, "shared-tool")
	require.NoError(t, err)
	assert.Same(t, second, registered, "DefaultToolRegistry keeps its last-writer-wins contract")

	unsupportedSchema := &r7ProjectionTestTool{
		name: "unsupported-semantic-schema-tool",
		schema: map[string]any{
			"type":              "object",
			"properties":        map[string]any{},
			"patternProperties": map[string]any{},
		},
	}
	require.NoError(t, registry.Register(ctx, unsupportedSchema), "semantic schema validation remains a dispatch-time check")
	registered, err = registry.Get(ctx, unsupportedSchema.Name())
	require.NoError(t, err)
	assert.Same(t, unsupportedSchema, registered)
}

type r7ProjectionCaptureLoader struct {
	extension extension.Extension
	loads     []r7ProjectionLoadedSource
}

type r7ProjectionLoadedSource struct {
	path  string
	bytes []byte
}

func (*r7ProjectionCaptureLoader) Name() string { return "r7-projection-capture-loader" }

func (loader *r7ProjectionCaptureLoader) Load(_ context.Context, path string) ([]extension.Extension, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	loader.loads = append(loader.loads, r7ProjectionLoadedSource{path: path, bytes: data})
	return []extension.Extension{loader.extension}, nil
}

type r7ProjectionFailureExtension struct {
	shutdownCalls atomic.Int32
	prefixCalls   *atomic.Int32
}

func (*r7ProjectionFailureExtension) Name() string { return "r7-projection-owner" }

func (fixture *r7ProjectionFailureExtension) Init(ctx context.Context, registry extension.ExtensionRegistry) error {
	if err := registry.RegisterTool(ctx, &r7ProjectionTestTool{name: "r7-prefix-valid-tool", calls: fixture.prefixCalls}); err != nil {
		return err
	}
	return registry.RegisterTool(ctx, &r7ProjectionTestTool{
		name: "r7-invalid-schema-tool",
		schema: map[string]any{
			"type":        "object",
			"description": r7ProjectionSchemaErrorCanary,
			"properties":  map[string]any{"invalid": make(chan int)},
		},
	})
}

func (fixture *r7ProjectionFailureExtension) Shutdown(context.Context) error {
	fixture.shutdownCalls.Add(1)
	return nil
}

type r7ProjectionTestTool struct {
	name   string
	schema any
	calls  *atomic.Int32
}

func (tool *r7ProjectionTestTool) Name() string    { return tool.name }
func (*r7ProjectionTestTool) Description() string  { return "R7 tool projection fixture" }
func (tool *r7ProjectionTestTool) Parameters() any { return tool.schema }
func (tool *r7ProjectionTestTool) Execute(context.Context, tools.ToolCall) (*tools.ToolResult, error) {
	if tool.calls != nil {
		tool.calls.Add(1)
	}
	return &tools.ToolResult{Output: "fixture executed"}, nil
}

func withPluginLoaderForTest(loader extension.PluginLoader) AssembleOption {
	return func(options *assembleConfig) { options.pluginLoader = loader }
}
