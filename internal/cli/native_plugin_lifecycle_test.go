//go:build !no_plugin

package cli

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/mock"
)

const nativePluginRequiredEnv = "GOCLI_REQUIRE_NATIVE_PLUGIN_TEST"

func TestConfiguredGoPluginLifecycle(t *testing.T) {
	if !nativePluginBuildSupported(runtime.GOOS, runtime.GOARCH) {
		nativePluginUnavailable(t, "Go plugins are unavailable on "+runtime.GOOS+"/"+runtime.GOARCH)
	}
	goTool, err := exec.LookPath("go")
	require.NoError(t, err)
	goEnv := exec.Command(goTool, "env", "CGO_ENABLED")
	goEnv.Env = os.Environ()
	cgoEnabled, err := goEnv.Output()
	require.NoError(t, err)
	if strings.TrimSpace(string(cgoEnabled)) != "1" {
		nativePluginUnavailable(t, "native Go plugin test requires CGO_ENABLED=1")
	}

	workingDir, err := os.Getwd()
	require.NoError(t, err)
	moduleRoot := filepath.Clean(filepath.Join(workingDir, "..", ".."))
	pluginPath := filepath.Join(t.TempDir(), "r7hookplugin.so")
	buildArgs := []string{"build", "-buildmode=plugin"}
	if r7NativePluginRaceEnabled {
		buildArgs = append(buildArgs, "-race")
	}
	buildArgs = append(buildArgs, "-o", pluginPath, "./internal/extension/testdata/r7hookplugin")
	build := exec.Command(goTool, buildArgs...)
	build.Dir = moduleRoot
	build.Env = os.Environ() // GOFLAGS, tags, compiler and cgo settings match this test invocation.
	if output, buildErr := build.CombinedOutput(); buildErr != nil {
		t.Fatalf("buildmode=plugin fixture failed on a supported OS with cgo enabled: %s", strings.TrimSpace(string(output)))
	}

	reportPath := filepath.Join(t.TempDir(), "plugin-report.txt")
	require.NoError(t, os.Setenv("GOCLI_R7_PLUGIN_REPORT", reportPath))
	t.Cleanup(func() { _ = os.Unsetenv("GOCLI_R7_PLUGIN_REPORT") })
	var logs bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(oldLogger) })

	rc := newAssembleTestConfig(t)
	enabled := true
	rc.Extensions.Enabled = &enabled
	rc.Extensions.PluginPaths = []string{pluginPath}
	assembly, err := AssembleAgent(context.Background(), rc, "openai", "r7-fixture-model", io.Discard, WithApproveMode(ApproveAuto))
	require.NoError(t, err)
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = assembly.Close(context.Background())
		}
	})

	model := mock.NewMockLLMServer(mock.NewConversationTemplate("R7", "configured-native-plugin",
		mock.ConversationTurn{AssistantContent: "deterministic fixture response"},
	))
	assembly.LoopAgent.SetModel(model)
	metadata := map[string]any{"nested": map[string]any{"value": "original"}}
	submission := core.Submission{
		Type: core.SubmissionUserMessage, Content: "original fixture request", Metadata: metadata,
		History: []core.AgentMessage{{Role: "user", Content: "previous fixture message"}},
	}
	result, runErr := assembly.TurnRunner.RunTurn(context.Background(), submission)
	require.NoError(t, runErr)
	assert.Equal(t, "original", metadata["nested"].(map[string]any)["value"])
	require.Len(t, model.CallLog(), 1)
	modelSawOriginal := false
	for _, message := range model.CallLog()[0].Messages {
		if strings.Contains(message.Content, "original fixture request") {
			modelSawOriginal = true
		}
	}
	assert.True(t, modelSawOriginal, "the model sees the original prompt after the extension mutates its DTO")
	assert.Equal(t, "deterministic fixture response", result.Message)

	require.NoError(t, assembly.Close(context.Background()))
	closed = true
	report, err := os.ReadFile(reportPath)
	require.NoError(t, err)
	assert.Equal(t, []string{
		"Load",
		"Init",
		"agent.before_run|r7-fixture-owner|mutated-before",
		"agent.after_run|r7-fixture-owner|error-present=false|class=",
		"Shutdown",
	}, strings.Split(strings.TrimSpace(string(report)), "\n"))
	assert.NotContains(t, string(report), "fixture-hook-owner-spoof", "Source must use the registration owner")
	assert.NotContains(t, logs.String(), "fixture-reason-canary-608c")
	assert.NotContains(t, logs.String(), "provider-secret-error-canary")
}

func nativePluginUnavailable(t *testing.T, reason string) {
	t.Helper()
	if os.Getenv(nativePluginRequiredEnv) == "1" {
		t.Fatal(reason)
	}
	t.Skip(reason)
}

func nativePluginBuildSupported(goos, goarch string) bool {
	switch goos + "/" + goarch {
	case "linux/386", "linux/amd64", "linux/arm", "linux/arm64", "linux/loong64", "linux/ppc64le", "linux/riscv64", "linux/s390x",
		"android/386", "android/amd64", "darwin/amd64", "darwin/arm64", "freebsd/amd64":
		return true
	default:
		return false
	}
}
