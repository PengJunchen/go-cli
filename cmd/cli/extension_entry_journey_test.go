package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/approval"
	"github.com/pengjunchen/go-cli/internal/config"
	api "github.com/pengjunchen/go-cli/pkg/extensionapi/v1"
)

func TestPublicSDKExtensionCommandAndProviderThroughCLIEntry(t *testing.T) {
	packageDir, err := os.Getwd()
	require.NoError(t, err)
	worker := filepath.Join(t.TempDir(), "worker")
	if runtime.GOOS == "windows" {
		worker += ".exe"
	}
	buildCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Compile from a separate module, so the fixture can only consume the
	// installed public contract rather than this module's internal packages.
	externalRoot := t.TempDir()
	source, err := os.ReadFile(filepath.Join(packageDir, "testdata", "extension-worker", "main.go"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(externalRoot, "main.go"), source, 0600))
	module := "module example.com/cli-entry-fixture\n\ngo 1.24.2\n\nrequire github.com/pengjunchen/go-cli v0.0.0\n\nreplace github.com/pengjunchen/go-cli => " + strconv.Quote(filepath.Clean(filepath.Join(packageDir, "..", ".."))) + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(externalRoot, "go.mod"), []byte(module), 0600))
	build := exec.CommandContext(buildCtx, "go", "build", "-mod=readonly", "-o", worker, ".")
	build.Dir = externalRoot
	output, err := build.CombinedOutput()
	require.NoError(t, err, string(output))
	artifact, err := os.ReadFile(worker)
	require.NoError(t, err)
	digest := sha256.Sum256(artifact)

	project := t.TempDir()
	t.Chdir(project)
	project, err = filepath.EvalSymlinks(project)
	require.NoError(t, err)
	for key, value := range map[string]string{
		"GO_CLI_PROVIDER_NAME": "entrysdk", "GO_CLI_MODEL_NAME": "entrymodel",
		"GO_CLI_PROVIDER_API_KEY": "", "GO_CLI_PROVIDER_BASE_URL": "",
		"GO_CLI_TRACING_ENABLED": "false", "GO_CLI_GIT_ENABLED": "false", "GO_CLI_EXTENSIONS_ENABLED": "true",
	} {
		t.Setenv(key, value)
	}
	eventsPath := filepath.Join(project, "worker-events.jsonl")
	descriptor := map[string]any{"command": []string{worker}, "artifact_sha256": hex.EncodeToString(digest[:]), "env": map[string]string{"ENTRY_EVENTS": eventsPath}, "timeout_millis": 10000}
	descriptorBytes, err := json.Marshal(descriptor)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(project, "entry.rpc.json"), descriptorBytes, 0600))
	settings := map[string]any{
		"runtime":    map[string]any{"profile": "edge", "max_prompt_bytes": 131072},
		"extensions": map[string]any{"enabled": true, "plugin_paths": []string{"entry.rpc.json"}},
	}
	isolateEntryFixtureStorage(t, project, settings)
	settingsBytes, err := json.Marshal(settings)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(project, ".go-cli.json"), settingsBytes, 0600))
	manager := approval.NewDefaultTrustManager(approval.NewFileTrustStore(filepath.Join(t.TempDir(), "trust", "trust.json")))
	frozen, ok := manager.(approval.FrozenProjectSourceTrust)
	require.True(t, ok)
	snapshot, err := config.CaptureProjectSources(context.Background(), project, nil)
	require.NoError(t, err)
	require.NoError(t, frozen.TrustProjectSourceSnapshot(context.Background(), snapshot))
	loaded, err := config.LoadTrustedWithManager(context.Background(), manager)
	require.NoError(t, err)
	require.Equal(t, config.ProjectSourceTrustVerified, loaded.ProjectSourceTrustStatus)
	require.Empty(t, loaded.Provider.APIKey)
	require.Empty(t, loaded.Provider.BaseURL)

	// Legacy command callbacks write to process stdout. Use the same actual
	// stream for the entry writer so both routes are observed without a shell.
	commandOutput, commandError, code := captureExtensionEntry(t, manager, []string{"extension-hello", "entry", "literal $HOME"})
	require.Zero(t, code, commandError)
	require.Contains(t, commandOutput, "PUBLIC_SDK_COMMAND_OK")
	assertExtensionEntryWorkersClosed(t, eventsPath, "command.run", 1)

	var stdout, stderr bytes.Buffer
	code = runWithTrustManager([]string{"prompt", "--provider", "entrysdk", "--model", "entrymodel", "--approve", "auto", "ENTRY_PROVIDER_REQUEST"}, &stdout, &stderr, manager)
	require.Zero(t, code, stderr.String())
	require.Contains(t, stdout.String(), "PUBLIC_SDK_PROVIDER_OK")
	assertExtensionEntryWorkersClosed(t, eventsPath, "provider.generate", 2)
}

func captureExtensionEntry(t *testing.T, manager approval.TrustManager, argv []string) (string, string, int) {
	t.Helper()
	reader, writer, err := os.Pipe()
	require.NoError(t, err)
	previous := os.Stdout
	os.Stdout = writer
	defer func() { os.Stdout = previous }()
	var stderr bytes.Buffer
	code := runWithTrustManager(argv, writer, &stderr, manager)
	require.NoError(t, writer.Close())
	output, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	return string(output), stderr.String(), code
}

func assertExtensionEntryWorkersClosed(t *testing.T, path, method string, wantWorkers int) {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	type event struct {
		Kind       string          `json:"kind"`
		PID        int             `json:"pid"`
		Executable string          `json:"executable"`
		Request    json.RawMessage `json:"request"`
	}
	started, initialized := map[int]bool{}, map[int]bool{}
	methods := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var observed event
		require.NoError(t, json.Unmarshal([]byte(line), &observed))
		require.Positive(t, observed.PID)
		switch observed.Kind {
		case "started":
			started[observed.PID] = true
			_, statErr := os.Stat(observed.Executable)
			require.ErrorIs(t, statErr, os.ErrNotExist, "captured artifact is removed only after the worker's Wait completes")
		case "initialized":
			initialized[observed.PID] = true
		case "command.run":
			var command api.CommandRequest
			require.NoError(t, json.Unmarshal(observed.Request, &command))
			require.Equal(t, []string{"entry", "literal $HOME"}, command.Args)
			methods[observed.Kind]++
		case "provider.generate":
			var request api.ModelRequest
			require.NoError(t, json.Unmarshal(observed.Request, &request))
			require.Equal(t, "entrysdk", request.Provider)
			require.Equal(t, "entrymodel", request.Model)
			methods[observed.Kind]++
		}
	}
	require.Len(t, started, wantWorkers)
	require.Equal(t, started, initialized, "every owned worker must complete the public handshake")
	require.Equal(t, 1, methods[method], "the actual CLI route must invoke the public callback once")
}
