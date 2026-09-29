//go:build e2e

package e2e_20260802 //nolint:staticcheck

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/cli"
	"github.com/pengjunchen/go-cli/internal/tracing"
)

// requireRealCLIInvocationTrace builds and runs the user-facing CLI version
// command with isolated user config, then reads the invocation-owned JSONL file.
func requireRealCLIInvocationTrace(t *testing.T) []tracing.SpanData {
	t.Helper()

	root := moduleRoot(t)
	binary := filepath.Join(t.TempDir(), "go-cli")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	build := exec.Command("go", "build", "-o", binary, "./cmd/cli")
	build.Dir = root
	buildOutput, err := build.CombinedOutput()
	require.NoError(t, err, "build real CLI: %s", buildOutput)

	testRoot := t.TempDir()
	home := filepath.Join(testRoot, "home")
	workDir := filepath.Join(testRoot, "work")
	configHome := filepath.Join(testRoot, "xdg-config")
	cacheHome := filepath.Join(testRoot, "xdg-cache")
	tmpDir := filepath.Join(testRoot, "tmp")
	traceDir := filepath.Join(testRoot, "traces")
	for _, dir := range []string{home, workDir, configHome, cacheHome, tmpDir, traceDir} {
		require.NoError(t, os.MkdirAll(dir, 0o700))
	}

	configDir := filepath.Join(home, ".go-cli")
	require.NoError(t, os.MkdirAll(configDir, 0o700))
	configYAML := fmt.Sprintf(
		"provider:\n  name: openai\n  base_url: %q\n  api_key: test-key\n  model: trace-fixture\ntracing:\n  enabled: true\n  exporter: jsonl\n  file_path: %q\n",
		"http://127.0.0.1:12345/v1", filepath.ToSlash(traceDir),
	)
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(configYAML), 0o600))

	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + configHome,
		"XDG_CACHE_HOME=" + cacheHome,
		"TMPDIR=" + tmpDir,
		"GO_CLI_NO_ONBOARDING=1",
		"NO_COLOR=1",
		"TERM=dumb",
		"TZ=UTC",
	}
	if runtime.GOOS == "windows" {
		env = append(env,
			"SYSTEMROOT="+os.Getenv("SYSTEMROOT"),
			"USERPROFILE="+home,
		)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, "version")
	command.Dir = workDir
	command.Env = env
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	require.NoError(t, command.Run(), "CLI version command: stdout=%q stderr=%q", stdout.String(), stderr.String())
	require.Contains(t, stdout.String(), cli.Version)
	require.Empty(t, stderr.String())

	entries, err := os.ReadDir(traceDir)
	require.NoError(t, err)
	var traceFiles []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".jsonl") {
			traceFiles = append(traceFiles, filepath.Join(traceDir, entry.Name()))
		}
	}
	require.Len(t, traceFiles, 1, "the CLI invocation should own one JSONL trace file")

	traceData, err := os.ReadFile(traceFiles[0])
	require.NoError(t, err)
	require.NotEmpty(t, traceData, "the CLI should flush invocation spans before exit")
	scanner := bufio.NewScanner(bytes.NewReader(traceData))
	var spans []tracing.SpanData
	for scanner.Scan() {
		var span tracing.SpanData
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &span), "each JSONL row must be valid span JSON")
		spans = append(spans, span)
	}
	require.NoError(t, scanner.Err())
	require.NotEmpty(t, spans)
	return spans
}
