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
	capturedPluginBytes := []byte("captured project extension fixture")
	require.NoError(t, os.WriteFile(pluginPath, capturedPluginBytes, 0o600))

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
	require.NoError(t, os.WriteFile(pluginPath, []byte("post-capture replacement"), 0o600))

	var calls atomic.Int32
	var loads []projectTrustFixtureLoad
	loader := projectTrustFixtureLoader{calls: &calls, loads: &loads}
	projectAssembly := newProjectExtensionTestAssembly(ctx, t, trusted, root, loader)
	require.Len(t, loads, 1)
	assert.NotEqual(t, pluginPath, mustAbsolutePath(t, loads[0].path), "project extension loading must use a private materialization path")
	assert.Equal(t, capturedPluginBytes, loads[0].bytes, "project extension loading must consume the verified captured bytes")
	assert.Equal(t, os.FileMode(0o400), loads[0].mode.Perm(), "materialized project extension bytes must be read-only")
	assert.Equal(t, os.FileMode(0o700), loads[0].directoryMode.Perm(), "materialized project extensions must live in a private directory")
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
	require.Len(t, loads, 2)
	assert.Equal(t, pluginPath, loads[1].path, "user/global extension paths keep their ordinary loader behavior")
	assert.Equal(t, []byte("post-capture replacement"), loads[1].bytes, "user/global extension paths are read from their live configured path")
	globalPrepared := prepareProjectExtensionCall(ctx, t, globalAssembly.registry, "global-after-revoke")
	assert.Equal(t, tools.RegistrationSourceToolRegistry, globalPrepared.ToolIdentity().RegistrationSource())
	globalResult, err := globalPrepared.Dispatch(ctx)
	require.NoError(t, err, "global extension provenance is independent of project trust")
	assert.Equal(t, "project extension tool ran", globalResult.Output)
	assert.EqualValues(t, 2, calls.Load())

	require.NoError(t, os.WriteFile(projectConfigPath, projectConfig, 0o600))
	require.NoError(t, os.WriteFile(pluginPath, capturedPluginBytes, 0o600))
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
	require.Len(t, loads, 3)
	assert.Equal(t, capturedPluginBytes, loads[2].bytes, "fresh project assembly must again use its exact captured bytes")
	assert.Equal(t, os.FileMode(0o400), loads[2].mode.Perm())
	assert.Equal(t, os.FileMode(0o700), loads[2].directoryMode.Perm())
	freshResult, err := freshPrepared.Dispatch(ctx)
	require.NoError(t, err, "a fresh assembly must bind the newly granted generation")
	assert.Equal(t, "project extension tool ran", freshResult.Output)
	assert.EqualValues(t, 3, calls.Load())

	_, err = stalePrepared.Dispatch(ctx)
	require.Error(t, err, "same-source regrant must not revive an invocation bound to the old generation")
	assert.False(t, stalePrepared.DispatchCommitted())
	assert.EqualValues(t, 3, calls.Load(), "a stale prepared invocation must not reach the extension tool")
}

func TestProjectExtensionCaptureLimitsFailBeforeLoaderOrInit(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		extension int64
		command   int64
	}{
		{name: "module exceeds per-file budget", extension: 16<<20 + 1},
		{name: "module plus command exceed aggregate budget", extension: 15 << 20, command: 1 << 20},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			t.Chdir(root)
			home := t.TempDir()
			t.Setenv("HOME", home)
			pluginPath := filepath.Join(root, "extensions", "native.so")
			require.NoError(t, os.MkdirAll(filepath.Dir(pluginPath), 0o700))
			require.NoError(t, os.WriteFile(pluginPath, nil, 0o600))
			require.NoError(t, os.Truncate(pluginPath, testCase.extension))
			projectConfig := "extensions:\n  enabled: true\n  plugin_paths:\n    - extensions/native.so\n"
			if testCase.command > 0 {
				commandPath := filepath.Join(root, "bin", "helper")
				require.NoError(t, os.MkdirAll(filepath.Dir(commandPath), 0o700))
				require.NoError(t, os.WriteFile(commandPath, nil, 0o700))
				require.NoError(t, os.Truncate(commandPath, testCase.command))
				projectConfig += "tools:\n  custom_tools:\n    - name: helper\n      command:\n        - ./bin/helper\n      working_dir: " + filepath.ToSlash(root) + "\n"
			}
			require.NoError(t, os.WriteFile(filepath.Join(root, ".go-cli.yaml"), []byte(projectConfig), 0o600))

			counts := &projectExtensionLoadInitCounts{}
			manager := approval.NewDefaultTrustManager(approval.NewFileTrustStore(filepath.Join(home, ".config", "go-cli", "trust.json")))
			_, captureErr := config.CaptureProjectSources(ctx, root, nil)
			require.ErrorIs(t, captureErr, config.ErrProjectSourceLimit)
			loaded, loadErr := config.LoadTrustedWithManager(ctx, manager)
			require.NoError(t, loadErr)
			require.Equal(t, config.ProjectSourceTrustCaptureError, loaded.ProjectSourceTrustStatus)
			state, _ := newAssembleState(ctx, loaded, "openai", "trust-test", io.Discard, WithApproveMode(ApproveAuto))
			state.pluginLoader = projectExtensionLoadInitCounter{counts: counts}
			state.gitCwd = root
			require.NoError(t, state.assembleExtensions())
			assert.Zero(t, counts.load.Load(), "source budget rejection must precede plugin loading")
			assert.Zero(t, counts.init.Load(), "source budget rejection must precede extension Init")
		})
	}
}

type projectExtensionLoadInitCounts struct {
	load atomic.Int32
	init atomic.Int32
}

type projectExtensionLoadInitCounter struct {
	counts *projectExtensionLoadInitCounts
}

func (projectExtensionLoadInitCounter) Name() string { return "project-extension-limit-counter" }

func (loader projectExtensionLoadInitCounter) Load(context.Context, string) ([]extension.Extension, error) {
	loader.counts.load.Add(1)
	return []extension.Extension{projectExtensionInitCounter{counts: loader.counts}}, nil
}

type projectExtensionInitCounter struct {
	counts *projectExtensionLoadInitCounts
}

func (projectExtensionInitCounter) Name() string { return "project-extension-limit-counter" }

func (extensionValue projectExtensionInitCounter) Init(context.Context, extension.ExtensionRegistry) error {
	extensionValue.counts.init.Add(1)
	return nil
}

func (projectExtensionInitCounter) Shutdown(context.Context) error { return nil }

func mustAbsolutePath(t *testing.T, path string) string {
	t.Helper()
	absolute, err := filepath.Abs(path)
	require.NoError(t, err)
	return absolute
}

type projectExtensionTestAssembly struct {
	registry *tools.MiddlewareToolRegistry
	state    *assembleState
	closed   bool
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
	assembly := &projectExtensionTestAssembly{registry: registry, state: state}
	t.Cleanup(func() { closeProjectExtensionTestAssembly(t, assembly) })
	return assembly
}

func closeProjectExtensionTestAssembly(t *testing.T, assembly *projectExtensionTestAssembly) {
	t.Helper()
	if assembly == nil || assembly.closed {
		return
	}
	assembly.closed = true
	state := assembly.state
	for _, manager := range state.pluginManagers {
		if err := manager.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown extension fixture: %v", err)
		}
		if completed, err := manager.WaitShutdown(context.Background()); !completed || err != nil {
			t.Errorf("wait for extension fixture shutdown: completed=%v err=%v", completed, err)
		}
	}
	for i := len(state.cleanupList) - 1; i >= 0; i-- {
		if err := state.cleanupList[i](context.Background()); err != nil {
			t.Errorf("cleanup assembly fixture: %v", err)
		}
	}
	state.lifetimeCancel()
}

func prepareProjectExtensionCall(ctx context.Context, t *testing.T, registry *tools.MiddlewareToolRegistry, id string) tools.PreparedInvocation {
	t.Helper()
	prepared, err := registry.PrepareToolCall(ctx, tools.ToolCall{ID: id, Name: "project_extension_tool"})
	require.NoError(t, err)
	return prepared
}

type projectTrustFixtureLoader struct {
	calls *atomic.Int32
	loads *[]projectTrustFixtureLoad
}

type projectTrustFixtureLoad struct {
	path          string
	bytes         []byte
	mode          os.FileMode
	directoryMode os.FileMode
}

func (l projectTrustFixtureLoader) Name() string { return "project-trust-fixture" }

func (l projectTrustFixtureLoader) Load(ctx context.Context, path string) ([]extension.Extension, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	bytes, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	directoryInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	if l.loads != nil {
		*l.loads = append(*l.loads, projectTrustFixtureLoad{
			path: path, bytes: bytes, mode: info.Mode(), directoryMode: directoryInfo.Mode(),
		})
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
