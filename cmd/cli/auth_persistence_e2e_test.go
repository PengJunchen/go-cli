package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/config"
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
	configYAML := fmt.Sprintf("provider:\n  name: openai\n  base_url: %s/v1\n  model: gpt-4o-mini\nmodel:\n  name: gpt-4o-mini\ntracing:\n  enabled: false\nmemory:\n  enabled: false\n", server.URL)
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

func TestInteractiveWizardModelSelectionAndCLIOverride(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	binDir := t.TempDir()
	binary := filepath.Join(binDir, "go-cli")
	build := exec.Command("go", "build", "-o", binary, "./cmd/cli")
	build.Dir = root
	buildOutput, err := build.CombinedOutput()
	require.NoError(t, err, string(buildOutput))

	models := make(chan string, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		models <- request.Model
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"model-ok"}}]}`)
	}))
	defer server.Close()
	deadProxy := authPersistenceDeadProxy(t)

	for _, testCase := range []struct {
		name        string
		modelFlag   string
		wantSession string
		wantOutput  []string
	}{
		{name: "wizard selection is effective without flag", wantSession: "gpt-4o", wantOutput: []string{"Default model selected for this session: gpt-4o"}},
		{name: "explicit flag stays session override", modelFlag: "gpt-4o-mini", wantSession: "gpt-4o-mini", wantOutput: []string{"Default model selected: gpt-4o", "Current session uses --model gpt-4o-mini"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			apiKey := runtimeAuthPersistenceCanary(t)
			home := filepath.Join(t.TempDir(), "home")
			workDir := filepath.Join(t.TempDir(), "work")
			xdgConfig := filepath.Join(home, "xdg-config")
			xdgCache := filepath.Join(home, "cache")
			for _, dir := range []string{home, workDir, xdgConfig, xdgCache} {
				require.NoError(t, os.MkdirAll(dir, 0o700))
			}
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

			args := []string{"interactive", "-provider=openai"}
			if testCase.modelFlag != "" {
				args = append(args, "-model="+testCase.modelFlag)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, binary, args...)
			command.Dir = workDir
			command.Env = authPersistenceTestEnv(os.Environ(), map[string]string{
				"HOME":                     home,
				"XDG_CONFIG_HOME":          xdgConfig,
				"XDG_CACHE_HOME":           xdgCache,
				"FAKE_SECURITY_STATE":      statePath,
				"FAKE_SECURITY_LOG":        logPath,
				"PATH":                     securityDir,
				"GO_CLI_MEMORY_ENABLED":    "false",
				"GO_CLI_PROVIDER_NAME":     "openai",
				"GO_CLI_PROVIDER_MODEL":    "gpt-4o-mini",
				"GO_CLI_PROVIDER_BASE_URL": server.URL + "/v1",
				"GO_CLI_NO_ONBOARDING":     "",
				"HTTPS_PROXY":              deadProxy,
				"HTTP_PROXY":               deadProxy,
				"ALL_PROXY":                deadProxy,
				"https_proxy":              deadProxy,
				"http_proxy":               deadProxy,
				"all_proxy":                deadProxy,
				"NO_PROXY":                 "127.0.0.1,localhost",
				"no_proxy":                 "127.0.0.1,localhost",
				"NO_COLOR":                 "1",
				"TERM":                     "dumb",
			})
			stdin, err := command.StdinPipe()
			require.NoError(t, err)
			stdout := newAuthPersistenceOutput()
			var stderr bytes.Buffer
			command.Stdout = stdout
			command.Stderr = &stderr
			require.NoError(t, command.Start())
			t.Cleanup(func() {
				if command.Process != nil {
					_ = command.Process.Kill()
				}
			})
			authPersistenceAnswer(t, stdout, stdin, "API Key:", apiKey)
			authPersistenceAnswer(t, stdout, stdin, "Choose a model", "2")
			authPersistenceAnswer(t, stdout, stdin, "Choose a theme", "1")
			authPersistenceAnswer(t, stdout, stdin, "Interactive session started", "inspect chosen model")

			select {
			case actualModel := <-models:
				require.Equal(t, testCase.wantSession, actualModel)
			case <-ctx.Done():
				t.Fatalf("local provider was not called; stdout=%s stderr=%s", stdout.String(), stderr.String())
			}
			_, err = io.WriteString(stdin, "exit\n")
			require.NoError(t, err)
			require.NoError(t, stdin.Close())
			if err := command.Wait(); err != nil {
				t.Fatalf("interactive failed: err=%v stdout=%s stderr=%s", err, stdout.String(), stderr.String())
			}
			require.NotContains(t, stdout.String(), apiKey)
			require.NotContains(t, stderr.String(), apiKey)
			for _, expected := range testCase.wantOutput {
				if !strings.Contains(stdout.String(), expected) {
					t.Errorf("expected CLI output to contain %q; output=%s", expected, stdout.String())
				}
			}

			configData, err := os.ReadFile(filepath.Join(home, ".go-cli", "config.yaml"))
			require.NoError(t, err)
			var saved config.Config
			require.NoError(t, config.UnmarshalConfig(configData, config.ConfigFormatYAML, &saved))
			require.Equal(t, "gpt-4o", saved.Provider.Model)
			require.Equal(t, "gpt-4o", saved.Model.Name)
			require.NotContains(t, string(configData), apiKey)
		})
	}
}

func authPersistenceDeadProxy(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := listener.Addr().String()
	require.NoError(t, listener.Close())
	return "http://" + address
}

type authPersistenceOutput struct {
	mu      sync.Mutex
	buffer  bytes.Buffer
	changed chan struct{}
}

func newAuthPersistenceOutput() *authPersistenceOutput {
	return &authPersistenceOutput{changed: make(chan struct{})}
}

func (output *authPersistenceOutput) Write(data []byte) (int, error) {
	output.mu.Lock()
	defer output.mu.Unlock()
	n, err := output.buffer.Write(data)
	close(output.changed)
	output.changed = make(chan struct{})
	return n, err
}

func (output *authPersistenceOutput) String() string {
	output.mu.Lock()
	defer output.mu.Unlock()
	return output.buffer.String()
}

func authPersistenceAnswer(t *testing.T, output *authPersistenceOutput, stdin io.Writer, prompt, answer string) {
	t.Helper()
	deadline := time.NewTimer(20 * time.Second)
	defer deadline.Stop()
	for {
		output.mu.Lock()
		if strings.Contains(output.buffer.String(), prompt) {
			output.mu.Unlock()
			_, err := io.WriteString(stdin, answer+"\n")
			require.NoError(t, err)
			return
		}
		changed := output.changed
		output.mu.Unlock()
		select {
		case <-changed:
		case <-deadline.C:
			t.Fatalf("CLI did not display %q; output=%s", prompt, output.String())
		}
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
