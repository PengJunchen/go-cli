//go:build !no_plugin

package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/approval"
	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/extension"
	"github.com/pengjunchen/go-cli/internal/tools"
)

const r6NativePluginRequiredEnv = "GOCLI_REQUIRE_NATIVE_PLUGIN_TEST"

func TestProjectExtensionNativeTrustRegrantUsesStableModulePath(t *testing.T) {
	if !r6NativePluginBuildSupported(runtime.GOOS, runtime.GOARCH) {
		r6NativePluginUnavailable(t, "Go plugins are unavailable on "+runtime.GOOS+"/"+runtime.GOARCH)
	}
	goTool, err := exec.LookPath("go")
	require.NoError(t, err)
	workingDir, err := os.Getwd()
	require.NoError(t, err)
	moduleRoot := filepath.Clean(filepath.Join(workingDir, "..", ".."))
	goEnv := exec.Command(goTool, "env", "CGO_ENABLED")
	goEnv.Dir = moduleRoot
	goEnv.Env = os.Environ()
	cgoEnabled, err := goEnv.Output()
	require.NoError(t, err)
	if strings.TrimSpace(string(cgoEnabled)) != "1" {
		r6NativePluginUnavailable(t, "native Go plugin test requires CGO_ENABLED=1")
	}

	buildPluginPath := filepath.Join(t.TempDir(), "r6-project-plugin.so")
	buildArgs := []string{"build", "-buildmode=plugin"}
	if r6NativePluginRaceEnabled {
		buildArgs = append(buildArgs, "-race")
	}
	buildArgs = append(buildArgs, "-o", buildPluginPath, "./internal/cli/testdata/r6_project_extension_plugin")
	build := exec.Command(goTool, buildArgs...)
	build.Dir = moduleRoot
	build.Env = os.Environ()
	if output, buildErr := build.CombinedOutput(); buildErr != nil {
		t.Fatalf("buildmode=plugin fixture failed on a supported OS with cgo enabled: %s", strings.TrimSpace(string(output)))
	}
	originalPluginBytes, err := os.ReadFile(buildPluginPath)
	require.NoError(t, err)
	require.Greater(t, len(originalPluginBytes), 1<<20, "the native module fixture must exercise the extension-specific capture limit")

	root := t.TempDir()
	t.Chdir(root)
	home := t.TempDir()
	t.Setenv("HOME", home)
	pluginPath := filepath.Join(root, ".go-cli", "extensions", "native.so")
	require.NoError(t, os.MkdirAll(filepath.Dir(pluginPath), 0o700))
	require.NoError(t, os.WriteFile(pluginPath, originalPluginBytes, 0o600))

	globalConfigDir := filepath.Join(home, ".go-cli")
	require.NoError(t, os.MkdirAll(globalConfigDir, 0o700))
	globalConfig := "provider:\n  name: openai\n  api_key: test-key\n  base_url: http://127.0.0.1:1/v1\n  model: r6-native-trust-test\nmodel:\n  name: r6-native-trust-test\ntracing:\n  enabled: false\n"
	require.NoError(t, os.WriteFile(filepath.Join(globalConfigDir, "config.yaml"), []byte(globalConfig), 0o600))
	projectConfig := []byte("extensions:\n  enabled: true\n  plugin_paths:\n    - .go-cli/extensions/native.so\n")
	require.NoError(t, os.WriteFile(filepath.Join(root, ".go-cli.yaml"), projectConfig, 0o600))
	trustStorePath := filepath.Join(home, ".config", "go-cli", "trust.json")
	manager := approval.NewDefaultTrustManager(approval.NewFileTrustStore(trustStorePath))
	uncurated, err := config.LoadTrustedWithManager(context.Background(), manager)
	require.NoError(t, err)
	snapshot := uncurated.CapturedProjectSourcesForGrant()
	require.NotNil(t, snapshot)
	capturedPluginBytes, present := snapshot.SourceBytes("extensions/.go-cli/extensions/native.so")
	require.True(t, present)
	assert.Equal(t, originalPluginBytes, capturedPluginBytes)
	frozen, ok := manager.(approval.FrozenProjectSourceTrust)
	require.True(t, ok)
	require.NoError(t, frozen.TrustProjectSourceSnapshot(context.Background(), snapshot))
	trusted, err := config.LoadTrustedWithManager(context.Background(), manager)
	require.NoError(t, err)
	require.Equal(t, config.ProjectSourceTrustVerified, trusted.ProjectSourceTrustStatus)
	firstGeneration := trusted.VerifiedProjectSources().TrustGeneration()
	require.NoError(t, os.WriteFile(pluginPath, []byte("post-capture replacement is not a Go module"), 0o600))

	reportPath := filepath.Join(t.TempDir(), "native-plugin-report.txt")
	t.Setenv("GOCLI_R6_PROJECT_PLUGIN_REPORT", reportPath)
	var loadedPaths []string
	loader := r6RecordingDefaultPluginLoader{delegate: extension.NewDefaultPluginLoader(), paths: &loadedPaths}
	firstAssembly := newProjectExtensionTestAssembly(context.Background(), t, trusted, root, loader)
	require.Len(t, loadedPaths, 1)
	stagePath := r6ExpectedProjectExtensionPath(t, capturedPluginBytes)
	assert.Equal(t, stagePath, loadedPaths[0], "the default native loader must receive the content-addressed captured bytes")
	assert.NotEqual(t, pluginPath, loadedPaths[0], "project plugin loading must not reopen the changed source path")
	assert.Equal(t, os.FileMode(0o400), r6MustStat(t, stagePath).Mode().Perm())
	assert.Equal(t, os.FileMode(0o700), r6MustStat(t, filepath.Dir(stagePath)).Mode().Perm())
	firstPrepared := prepareR6NativeProjectExtensionCall(t, firstAssembly.registry, "native-first")
	assert.Equal(t, tools.RegistrationSourceProjectSourced, firstPrepared.ToolIdentity().RegistrationSource())
	firstResult, err := firstPrepared.Dispatch(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "r6 native project extension ran", firstResult.Output)
	stalePrepared := prepareR6NativeProjectExtensionCall(t, firstAssembly.registry, "native-stale")

	revoker := approval.NewDefaultTrustManager(approval.NewFileTrustStore(trustStorePath))
	require.NoError(t, revoker.RevokeTrust(context.Background(), snapshot.CanonicalRoot()))
	_, err = stalePrepared.Dispatch(context.Background())
	require.Error(t, err)
	assert.False(t, stalePrepared.DispatchCommitted())
	r6AssertNativeProjectExtensionExecutions(t, reportPath, 1)

	// Restore the exact captured module and grant the same manifest at a new
	// trust generation. The prior invocation remains bound to its old epoch.
	require.NoError(t, os.WriteFile(pluginPath, originalPluginBytes, 0o600))
	regrant, ok := revoker.(approval.FrozenProjectSourceTrust)
	require.True(t, ok)
	require.NoError(t, regrant.TrustProjectSourceSnapshot(context.Background(), snapshot))
	freshConfig, err := config.LoadTrustedWithManager(context.Background(), revoker)
	require.NoError(t, err)
	require.Equal(t, config.ProjectSourceTrustVerified, freshConfig.ProjectSourceTrustStatus)
	assert.Equal(t, firstGeneration+2, freshConfig.VerifiedProjectSources().TrustGeneration(), "revoke and identical regrant advance the root epoch twice")
	freshAssembly := newProjectExtensionTestAssembly(context.Background(), t, freshConfig, root, loader)
	require.Len(t, loadedPaths, 2)
	assert.Equal(t, stagePath, loadedPaths[1], "same bytes in a fresh assembly must use the exact same plugin path")
	assert.Equal(t, loadedPaths[0], loadedPaths[1])
	assert.FileExists(t, stagePath)
	_, err = stalePrepared.Dispatch(context.Background())
	require.Error(t, err, "same-source regrant must not revive the old invocation")
	assert.False(t, stalePrepared.DispatchCommitted())
	r6AssertNativeProjectExtensionExecutions(t, reportPath, 1)

	freshPrepared := prepareR6NativeProjectExtensionCall(t, freshAssembly.registry, "native-fresh")
	assert.Equal(t, tools.RegistrationSourceProjectSourced, freshPrepared.ToolIdentity().RegistrationSource())
	closeProjectExtensionTestAssembly(t, firstAssembly)
	assert.FileExists(t, stagePath, "the fresh assembly lease keeps the native module staged after the first closes")
	freshResult, err := freshPrepared.Dispatch(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "r6 native project extension ran", freshResult.Output)
	r6AssertNativeProjectExtensionExecutions(t, reportPath, 2)
	closeProjectExtensionTestAssembly(t, freshAssembly)
	assert.NoFileExists(t, stagePath, "the final assembly close releases the staged module")
}

func r6NativePluginUnavailable(t *testing.T, reason string) {
	t.Helper()
	if os.Getenv(r6NativePluginRequiredEnv) == "1" {
		t.Fatal(reason)
	}
	t.Skip(reason)
}

func r6NativePluginBuildSupported(goos, goarch string) bool {
	switch goos + "/" + goarch {
	case "linux/386", "linux/amd64", "linux/arm", "linux/arm64", "linux/loong64", "linux/ppc64le", "linux/riscv64", "linux/s390x",
		"darwin/amd64", "darwin/arm64", "freebsd/amd64":
		return true
	default:
		return false
	}
}

type r6RecordingDefaultPluginLoader struct {
	delegate extension.PluginLoader
	paths    *[]string
}

func (l r6RecordingDefaultPluginLoader) Name() string { return l.delegate.Name() }

func (l r6RecordingDefaultPluginLoader) Load(ctx context.Context, path string) ([]extension.Extension, error) {
	*l.paths = append(*l.paths, path)
	return l.delegate.Load(ctx, path)
}

func r6ExpectedProjectExtensionPath(t *testing.T, data []byte) string {
	t.Helper()
	digest := sha256.Sum256(data)
	projectExtensionStageCache.Lock()
	root := projectExtensionStageCache.root
	projectExtensionStageCache.Unlock()
	require.NotEmpty(t, root)
	return filepath.Join(root, hex.EncodeToString(digest[:])+".so")
}

func r6MustStat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	return info
}

func prepareR6NativeProjectExtensionCall(t *testing.T, registry *tools.MiddlewareToolRegistry, id string) tools.PreparedInvocation {
	t.Helper()
	prepared, err := registry.PrepareToolCall(context.Background(), tools.ToolCall{ID: id, Name: "r6_native_project_extension_tool"})
	require.NoError(t, err)
	return prepared
}

func r6AssertNativeProjectExtensionExecutions(t *testing.T, path string, expected int) {
	t.Helper()
	data, err := os.ReadFile(path)
	if expected == 0 {
		require.True(t, os.IsNotExist(err))
		return
	}
	require.NoError(t, err)
	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	require.Len(t, lines, expected)
	for _, line := range lines {
		assert.Equal(t, []byte("executed"), line)
	}
}
