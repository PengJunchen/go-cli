package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAuthPersistenceRealCLIOnboardingThenRestartedPrompt(t *testing.T) {
	apiKey := runtimeAuthPersistenceCanary(t)
	requestAuth := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestAuth <- r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"restart-ok"}}]}`)
	}))
	defer server.Close()

	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	binDir := t.TempDir()
	binary := filepath.Join(binDir, "go-cli")
	build := exec.Command("go", "build", "-o", binary, "./cmd/cli")
	build.Dir = root
	buildOutput, err := build.CombinedOutput()
	require.NoError(t, err, string(buildOutput))

	home := filepath.Join(t.TempDir(), "home")
	workDir := filepath.Join(t.TempDir(), "work")
	configDir := filepath.Join(home, ".go-cli")
	xdgDir := filepath.Join(home, "xdg")
	cacheDir := filepath.Join(home, "cache")
	for _, dir := range []string{home, workDir, configDir, xdgDir, cacheDir} {
		require.NoError(t, os.MkdirAll(dir, 0o700))
	}
	configYAML := fmt.Sprintf("provider:\n  name: openai\n  base_url: %s/v1\n  model: gpt-4o-mini\nmodel:\n  name: gpt-4o-mini\ntracing:\n  enabled: false\n", server.URL)
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(configYAML), 0o600))

	securityDir := filepath.Join(t.TempDir(), "security-bin")
	require.NoError(t, os.Mkdir(securityDir, 0o700))
	statePath := filepath.Join(t.TempDir(), "fake-keychain-state")
	logPath := filepath.Join(t.TempDir(), "fake-keychain-operations.log")
	securityScript := `#!/bin/sh
operation=$1
shift
service=
account=
value=
while [ "$#" -gt 0 ]; do
  case "$1" in
    -s) service=$2; shift 2 ;;
    -a) account=$2; shift 2 ;;
    -w) if [ "$operation" = "add-generic-password" ]; then value=$2; shift 2; else shift; fi ;;
    *) shift ;;
  esac
done
printf '%s|%s|%s\n' "$operation" "$service" "$account" >> "$FAKE_SECURITY_LOG"
if [ "$operation" = "find-generic-password" ]; then
  if [ -f "$FAKE_SECURITY_STATE" ]; then
    IFS='|' read -r saved_account saved_value < "$FAKE_SECURITY_STATE"
    if [ "$saved_account" = "$account" ]; then
      printf '%s\n' "$saved_value"
      exit 0
    fi
  fi
  printf '%s\n' 'security: SecKeychainSearchCopyNext: The specified item could not be found in the keychain.' >&2
  exit 44
fi
if [ "$operation" = "add-generic-password" ]; then
  printf '%s|%s\n' "$account" "$value" > "$FAKE_SECURITY_STATE"
  exit 0
fi
exit 0
`
	securityPath := filepath.Join(securityDir, "security")
	require.NoError(t, os.WriteFile(securityPath, []byte(securityScript), 0o700))
	env := authPersistenceTestEnv(os.Environ(), map[string]string{
		"HOME":                 home,
		"XDG_CONFIG_HOME":      xdgDir,
		"XDG_CACHE_HOME":       cacheDir,
		"FAKE_SECURITY_STATE":  statePath,
		"FAKE_SECURITY_LOG":    logPath,
		"PATH":                 securityDir,
		"NO_COLOR":             "1",
		"TERM":                 "dumb",
		"GO_CLI_NO_ONBOARDING": "",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	first := exec.CommandContext(ctx, binary, "interactive", "-provider=openai")
	first.Dir = workDir
	first.Env = env
	first.Stdin = strings.NewReader(apiKey + "\n")
	var firstStdout, firstStderr bytes.Buffer
	first.Stdout = &firstStdout
	first.Stderr = &firstStderr
	firstErr := first.Run()
	if firstErr != nil {
		state, _ := os.ReadFile(statePath)
		operations, _ := os.ReadFile(logPath)
		t.Fatalf("interactive onboarding failed: err=%v stdout=%s stderr=%s fake_state=%q fake_operations=%q", firstErr, firstStdout.String(), firstStderr.String(), state, operations)
	}
	require.Contains(t, firstStdout.String(), "API key saved for this provider")
	require.NotContains(t, firstStdout.String(), apiKey)
	require.NotContains(t, firstStderr.String(), apiKey)

	second := exec.CommandContext(ctx, binary, "prompt", "-provider=openai", "-output=json", "restart authentication check")
	second.Dir = workDir
	second.Env = env
	var secondStdout, secondStderr bytes.Buffer
	second.Stdout = &secondStdout
	second.Stderr = &secondStderr
	require.NoError(t, second.Run(), "restarted prompt failed: stdout=%s stderr=%s", secondStdout.String(), secondStderr.String())
	require.NotContains(t, secondStdout.String(), "Welcome to go-cli")
	require.NotContains(t, secondStdout.String(), "API key")
	require.NotContains(t, secondStdout.String(), apiKey)
	require.NotContains(t, secondStderr.String(), apiKey)
	require.Contains(t, secondStdout.String(), "restart-ok")

	select {
	case authorization := <-requestAuth:
		require.Equal(t, "Bearer "+apiKey, authorization)
	case <-ctx.Done():
		t.Fatal("local HTTP server did not receive restarted prompt request")
	}

	if runtime.GOOS == "darwin" {
		operations, readErr := os.ReadFile(logPath)
		require.NoError(t, readErr)
		require.Contains(t, string(operations), "find-generic-password|go-cli|")
		require.Contains(t, string(operations), "add-generic-password|go-cli|")
		require.NotContains(t, string(operations), apiKey, "fake security log must not record secret arguments")
	}
}

func runtimeAuthPersistenceCanary(t *testing.T) string {
	t.Helper()
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		t.Fatalf("generate auth persistence test canary: %v", err)
	}
	return "auth-runtime-" + hex.EncodeToString(value[:])
}

func authPersistenceTestEnv(existing []string, updates map[string]string) []string {
	filtered := make([]string, 0, len(existing)+len(updates))
	for _, item := range existing {
		key, _, _ := strings.Cut(item, "=")
		if _, replace := updates[key]; !replace {
			filtered = append(filtered, item)
		}
	}
	for key, value := range updates {
		filtered = append(filtered, key+"="+value)
	}
	return filtered
}
