package config

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type scopedAuthTestRecord struct {
	Provider string `json:"provider"`
	Protocol string `json:"protocol"`
	Origin   string `json:"origin"`
	APIKey   string `json:"api_key"`
}

func TestUpsertScopedCredentialFilePreservesOtherScopesAndReplacesMatch(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o700))
	path := filepath.Join(dir, "auth.json")
	oldOpenAIKey := authSecretCanary(t)
	newOpenAIKey := authSecretCanary(t)
	preservedKey := authSecretCanary(t)
	openAIScope := AuthScope{ProviderID: "openai", ProtocolID: "openai-compatible", Origin: "https://api.example.test"}
	anthropicScope := AuthScope{ProviderID: "anthropic-proxy", ProtocolID: "openai-compatible", Origin: "https://anthropic.example.test"}
	initial, err := json.Marshal(map[string]any{"credentials": []scopedAuthTestRecord{
		{Provider: openAIScope.ProviderID, Protocol: openAIScope.ProtocolID, Origin: openAIScope.Origin, APIKey: oldOpenAIKey},
		{Provider: anthropicScope.ProviderID, Protocol: anthropicScope.ProtocolID, Origin: anthropicScope.Origin, APIKey: preservedKey},
	}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, initial, 0o600))

	require.NoError(t, UpsertScopedCredentialFile(context.Background(), path, openAIScope, newOpenAIKey))
	gotOpenAI, err := lookupScopedAuthFilePath(path, openAIScope)
	require.NoError(t, err)
	assert.Equal(t, newOpenAIKey, gotOpenAI)
	gotAnthropic, err := lookupScopedAuthFilePath(path, anthropicScope)
	require.NoError(t, err)
	assert.Equal(t, preservedKey, gotAnthropic)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.NotContains(t, string(data), oldOpenAIKey)
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestUpsertScopedCredentialFileDoesNotMigrateLegacyCredential(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o700))
	path := filepath.Join(dir, "auth.json")
	legacySecret := authSecretCanary(t)
	newScopedKey := authSecretCanary(t)
	require.NoError(t, os.WriteFile(path, []byte(`{"api_key":"`+legacySecret+`"}`), 0o600))
	scope := AuthScope{ProviderID: "openai", ProtocolID: "openai-compatible", Origin: "https://api.example.test"}

	require.NoError(t, UpsertScopedCredentialFile(context.Background(), path, scope, newScopedKey))
	key, err := lookupScopedAuthFilePath(path, scope)
	require.NoError(t, err)
	assert.Equal(t, newScopedKey, key)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.NotContains(t, string(data), legacySecret, "the old secret must not be rebound to the new scope")
}

func TestUpsertScopedCredentialFileRejectsInvalidSourceWithoutReplacingIt(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o700))
	path := filepath.Join(dir, "auth.json")
	oldSecret := authSecretCanary(t)
	old := []byte(`{"credentials":[{"provider":"openai","protocol":"openai-compatible","origin":"https://api.example.test","api_key":"` + oldSecret + `"}`)
	require.NoError(t, os.WriteFile(path, old, 0o600))
	scope := AuthScope{ProviderID: "openai", ProtocolID: "openai-compatible", Origin: "https://api.example.test"}

	err := UpsertScopedCredentialFile(context.Background(), path, scope, authSecretCanary(t))
	require.Error(t, err)
	assert.NotContains(t, err.Error(), oldSecret)
	data, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, old, data)
}

func TestUpsertScopedCredentialFileRepairsWideFile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o700))
	path := filepath.Join(dir, "auth.json")
	old := []byte(`{"credentials":[]}`)
	require.NoError(t, os.WriteFile(path, old, 0o644))
	require.NoError(t, os.Chmod(path, 0o644))
	scope := AuthScope{ProviderID: "openai", ProtocolID: "openai-compatible", Origin: "https://api.example.test"}

	newKey := authSecretCanary(t)
	require.NoError(t, UpsertScopedCredentialFile(context.Background(), path, scope, newKey))
	data, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Contains(t, string(data), newKey)
	info, statErr := os.Stat(path)
	require.NoError(t, statErr)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestUpsertScopedCredentialFileConcurrentProcessesPreserveBothScopes(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("cross-process auth-file locking is supported on macOS and Linux")
	}
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o700))
	path := filepath.Join(dir, "auth.json")
	barrier := filepath.Join(dir, "start")

	const existingCount = 256
	initialRecords := make([]scopedAuthTestRecord, 0, existingCount)
	for i := 0; i < existingCount; i++ {
		initialRecords = append(initialRecords, scopedAuthTestRecord{
			Provider: fmt.Sprintf("existing-%03d", i),
			Protocol: "openai-compatible",
			Origin:   "https://api.example.test",
			APIKey:   authSecretCanary(t),
		})
	}
	initial, err := json.Marshal(map[string]any{"credentials": initialRecords})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, initial, 0o600))

	firstScope := AuthScope{ProviderID: "process-one", ProtocolID: "openai-compatible", Origin: "https://api.example.test"}
	secondScope := AuthScope{ProviderID: "process-two", ProtocolID: "openai-compatible", Origin: "https://api.example.test"}
	firstProcessKey := authSecretCanary(t)
	secondProcessKey := authSecretCanary(t)
	firstInput := writeUpsertHelperInput(t, dir, path, barrier, firstScope, firstProcessKey)
	secondInput := writeUpsertHelperInput(t, dir, path, barrier, secondScope, secondProcessKey)
	first := startUpsertHelper(t, firstInput)
	second := startUpsertHelper(t, secondInput)
	require.NoError(t, os.WriteFile(barrier, nil, 0o600))
	waitUpsertHelper(t, first)
	waitUpsertHelper(t, second)

	firstKey, err := lookupScopedAuthFilePath(path, firstScope)
	require.NoError(t, err)
	assert.Equal(t, firstProcessKey, firstKey)
	secondKey, err := lookupScopedAuthFilePath(path, secondScope)
	require.NoError(t, err)
	assert.Equal(t, secondProcessKey, secondKey)
	data, err := readPrivateAuthFile(path)
	require.NoError(t, err)
	var document struct {
		Credentials []scopedAuthTestRecord `json:"credentials"`
	}
	require.NoError(t, json.Unmarshal(data, &document))
	assert.Len(t, document.Credentials, existingCount+2)
}

func TestUpsertScopedCredentialFileProcessHelper(t *testing.T) {
	if os.Getenv("GO_CLI_AUTH_UPSERT_HELPER") != "1" {
		return
	}
	inputData, err := os.ReadFile(os.Getenv("GO_CLI_AUTH_UPSERT_INPUT"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "read helper input failed")
		os.Exit(2)
	}
	var input authUpsertHelperInput
	if err := json.Unmarshal(inputData, &input); err != nil {
		fmt.Fprintln(os.Stderr, "decode helper input failed")
		os.Exit(2)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(input.Barrier); err == nil {
			break
		}
		if time.Now().After(deadline) {
			fmt.Fprintln(os.Stderr, "helper barrier timed out")
			os.Exit(2)
		}
		time.Sleep(time.Millisecond)
	}
	if err := UpsertScopedCredentialFile(context.Background(), input.Path, input.Scope, input.Key); err != nil {
		fmt.Fprintln(os.Stderr, "scoped credential upsert failed")
		os.Exit(2)
	}
	os.Exit(0)
}

type authUpsertHelperInput struct {
	Path    string    `json:"path"`
	Barrier string    `json:"barrier"`
	Scope   AuthScope `json:"scope"`
	Key     string    `json:"key"`
}

type upsertHelper struct {
	cmd  *exec.Cmd
	done chan error
}

func writeUpsertHelperInput(t *testing.T, dir, path, barrier string, scope AuthScope, key string) string {
	t.Helper()
	data, err := json.Marshal(authUpsertHelperInput{Path: path, Barrier: barrier, Scope: scope, Key: key})
	require.NoError(t, err)
	inputPath := filepath.Join(dir, scope.ProviderID+".input.json")
	require.NoError(t, os.WriteFile(inputPath, data, 0o600))
	return inputPath
}

func startUpsertHelper(t *testing.T, inputPath string) upsertHelper {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestUpsertScopedCredentialFileProcessHelper$")
	cmd.Env = append(os.Environ(), "GO_CLI_AUTH_UPSERT_HELPER=1", "GO_CLI_AUTH_UPSERT_INPUT="+inputPath)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	require.NoError(t, cmd.Start())
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	return upsertHelper{cmd: cmd, done: done}
}

func waitUpsertHelper(t *testing.T, helper upsertHelper) {
	t.Helper()
	select {
	case err := <-helper.done:
		if err != nil {
			t.Fatalf("auth upsert helper failed: %v", err)
		}
	case <-time.After(15 * time.Second):
		_ = helper.cmd.Process.Kill()
		t.Fatal("auth upsert helper timed out")
	}
}
