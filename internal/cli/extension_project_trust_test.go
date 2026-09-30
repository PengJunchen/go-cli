package cli

import (
	"context"
	"io"
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

func TestProjectExtensionToolTrustFenceAndGlobalProvenance(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	t.Chdir(root)
	home := t.TempDir()
	t.Setenv("HOME", home)

	pluginPath := filepath.Join(root, ".go-cli", "extensions", "fixture.so")
	require.NoError(t, os.MkdirAll(filepath.Dir(pluginPath), 0o700))
	// The injected loader below lets this test exercise the real assembly
	// provenance and dispatch path without requiring a host-native .so build.
	require.NoError(t, os.WriteFile(pluginPath, []byte("captured project extension fixture"), 0o600))

	globalConfigDir := filepath.Join(home, ".go-cli")
	require.NoError(t, os.MkdirAll(globalConfigDir, 0o700))
	globalConfig := "provider:\n  name: openai\n  api_key: test-key\n  base_url: http://127.0.0.1:1/v1\n  model: trust-test\nmodel:\n  name: trust-test\ntracing:\n  enabled: false\n"
	globalConfig += "extensions:\n  enabled: true\n  plugin_paths:\n    - " + filepath.ToSlash(pluginPath) + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(globalConfigDir, "config.yaml"), []byte(globalConfig), 0o600))
	projectConfig := []byte("extensions:\n  enabled: true\n  plugin_paths:\n    - .go-cli/extensions/fixture.so\n")
	projectConfigPath := filepath.Join(root, ".go-cli.yaml")
	require.NoError(t, os.WriteFile(projectConfigPath, projectConfig, 0o600))

	trustDir := filepath.Join(home, ".config", "go-cli")
	require.NoError(t, os.MkdirAll(trustDir, 0o700))
	trustStorePath := filepath.Join(trustDir, "trust.json")
	manager := approval.NewDefaultTrustManager(approval.NewFileTrustStore(trustStorePath))
	uncurated, err := config.LoadTrustedWithManager(ctx, manager)
	require.NoError(t, err)
	snapshot := uncurated.CapturedProjectSourcesForGrant()
	require.NotNil(t, snapshot)
	frozen, ok := manager.(approval.FrozenProjectSourceTrust)
	require.True(t, ok)
	require.NoError(t, frozen.TrustProjectSourceSnapshot(ctx, snapshot))
	trusted, err := config.LoadTrustedWithManager(ctx, manager)
	require.NoError(t, err)
	require.Equal(t, config.ProjectSourceTrustVerified, trusted.ProjectSourceTrustStatus)
	projectOverlay := trusted.ProjectOverlayForVerifiedSources()
	require.NotNil(t, projectOverlay)
	require.Equal(t, projectOverlay.Extensions.PluginPaths, trusted.Extensions.PluginPaths)

	var calls atomic.Int32
	loader := projectTrustFixtureLoader{calls: &calls}
	projectAssembly := newProjectExtensionTestAssembly(ctx, t, trusted, root, loader)
	projectPrepared := prepareProjectExtensionCall(ctx, t, projectAssembly.registry, "project-before-revoke")
	assert.Equal(t, tools.RegistrationSourceProjectSourced, projectPrepared.ToolIdentity().RegistrationSource())
	result, err := projectPrepared.Dispatch(ctx)
	require.NoError(t, err)
	assert.Equal(t, "project extension tool ran", result.Output)
	assert.EqualValues(t, 1, calls.Load())

	stalePrepared := prepareProjectExtensionCall(ctx, t, projectAssembly.registry, "project-after-revoke")
	revoker := approval.NewDefaultTrustManager(approval.NewFileTrustStore(trustStorePath))
	require.NoError(t, revoker.RevokeTrust(ctx, snapshot.CanonicalRoot()))
	_, err = stalePrepared.Dispatch(ctx)
	require.Error(t, err, "a project extension invocation prepared before revocation must not dispatch")
	assert.False(t, stalePrepared.DispatchCommitted())
	assert.EqualValues(t, 1, calls.Load(), "the revoked project tool executor must not run")

	// With the project layer absent, the same path supplied only by the
	// user-global config remains an ordinary unbound registration.
	require.NoError(t, os.Remove(projectConfigPath))
	globalOnly, err := config.LoadTrustedWithManager(ctx, manager)
	require.NoError(t, err)
	assert.Nil(t, globalOnly.ProjectOverlayForVerifiedSources())
	globalAssembly := newProjectExtensionTestAssembly(ctx, t, globalOnly, root, loader)
	globalPrepared := prepareProjectExtensionCall(ctx, t, globalAssembly.registry, "global-after-revoke")
	assert.Equal(t, tools.RegistrationSourceToolRegistry, globalPrepared.ToolIdentity().RegistrationSource())
	globalResult, err := globalPrepared.Dispatch(ctx)
	require.NoError(t, err, "global extension provenance is independent of project trust")
	assert.Equal(t, "project extension tool ran", globalResult.Output)
	assert.EqualValues(t, 2, calls.Load())

	require.NoError(t, os.WriteFile(projectConfigPath, projectConfig, 0o600))
	grantor := approval.NewDefaultTrustManager(approval.NewFileTrustStore(trustStorePath))
	grant, ok := grantor.(approval.FrozenProjectSourceTrust)
	require.True(t, ok)
	require.NoError(t, grant.TrustProjectSourceSnapshot(ctx, snapshot))
	freshConfig, err := config.LoadTrustedWithManager(ctx, grantor)
	require.NoError(t, err)
	require.Equal(t, config.ProjectSourceTrustVerified, freshConfig.ProjectSourceTrustStatus)
	freshAssembly := newProjectExtensionTestAssembly(ctx, t, freshConfig, root, loader)
	freshPrepared := prepareProjectExtensionCall(ctx, t, freshAssembly.registry, "project-after-regrant")
	assert.Equal(t, tools.RegistrationSourceProjectSourced, freshPrepared.ToolIdentity().RegistrationSource())
	freshResult, err := freshPrepared.Dispatch(ctx)
	require.NoError(t, err, "a fresh assembly must bind the newly granted generation")
	assert.Equal(t, "project extension tool ran", freshResult.Output)
	assert.EqualValues(t, 3, calls.Load())

	_, err = stalePrepared.Dispatch(ctx)
	require.Error(t, err, "same-source regrant must not revive an invocation bound to the old generation")
	assert.False(t, stalePrepared.DispatchCommitted())
	assert.EqualValues(t, 3, calls.Load(), "a stale prepared invocation must not reach the extension tool")
}

type projectExtensionTestAssembly struct {
	registry *tools.MiddlewareToolRegistry
	state    *assembleState
}

func newProjectExtensionTestAssembly(ctx context.Context, t *testing.T, cfg *config.Config, root string, loader extension.PluginLoader) *projectExtensionTestAssembly {
	t.Helper()
	base := tools.NewDefaultToolRegistry()
	deferred := tools.NewDeferredToolRegistryAdapter(base)
	registry := tools.NewMiddlewareToolRegistry(deferred)
	require.NoError(t, registry.SetPreparedToolStages(tools.PreparedToolStages{}))
	require.NoError(t, registry.SetPreparedToolPolicy(func(_ context.Context, request tools.PreparedPolicyRequest) (tools.PreparedPolicyDecision, error) {
		return tools.PreparedPolicyDecision{Allowed: true, Generations: request.Generations}, nil
	}))
	state, _ := newAssembleState(ctx, cfg, "openai", "trust-test", io.Discard, WithApproveMode(ApproveAuto))
	state.gitCwd = root
	state.tr = registry
	state.pluginLoader = loader
	require.NoError(t, state.assembleExtensions())
	t.Cleanup(func() {
		for _, manager := range state.pluginManagers {
			_ = manager.Shutdown(context.Background())
		}
		state.lifetimeCancel()
	})
	return &projectExtensionTestAssembly{registry: registry, state: state}
}

func prepareProjectExtensionCall(ctx context.Context, t *testing.T, registry *tools.MiddlewareToolRegistry, id string) tools.PreparedInvocation {
	t.Helper()
	prepared, err := registry.PrepareToolCall(ctx, tools.ToolCall{ID: id, Name: "project_extension_tool"})
	require.NoError(t, err)
	return prepared
}

type projectTrustFixtureLoader struct {
	calls *atomic.Int32
}

func (l projectTrustFixtureLoader) Name() string { return "project-trust-fixture" }

func (l projectTrustFixtureLoader) Load(ctx context.Context, _ string) ([]extension.Extension, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return []extension.Extension{projectTrustFixtureExtension{calls: l.calls}}, nil
}

type projectTrustFixtureExtension struct {
	calls *atomic.Int32
}

func (projectTrustFixtureExtension) Name() string { return "project-trust-fixture-extension" }

func (e projectTrustFixtureExtension) Init(ctx context.Context, registry extension.ExtensionRegistry) error {
	return registry.RegisterTool(ctx, projectTrustFixtureTool{calls: e.calls})
}

func (projectTrustFixtureExtension) Shutdown(ctx context.Context) error { return ctx.Err() }

type projectTrustFixtureTool struct {
	calls *atomic.Int32
}

func (projectTrustFixtureTool) Name() string        { return "project_extension_tool" }
func (projectTrustFixtureTool) Description() string { return "project extension trust fixture" }

func (t projectTrustFixtureTool) Execute(ctx context.Context, _ tools.ToolCall) (*tools.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	t.calls.Add(1)
	return &tools.ToolResult{Output: "project extension tool ran"}, nil
}
