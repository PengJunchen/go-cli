//go:build linux || darwin

package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

func environmentBashVariants(opts ...BashToolOption) map[string]func(context.Context, ToolCall) (*ToolResult, error) {
	return map[string]func(context.Context, ToolCall) (*ToolResult, error){
		"normal": NewBashTool(opts...).Execute,
		"streaming": func(ctx context.Context, call ToolCall) (*ToolResult, error) {
			return NewStreamingBashTool(opts...).ExecuteStreaming(ctx, call, &mockStreamSink{})
		},
	}
}

func TestBashHostEnvironmentNonLogin(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	for name, execute := range environmentBashVariants(WithNoSandbox()) {
		t.Run(name, func(t *testing.T) {
			result, err := execute(context.Background(), ToolCall{Args: map[string]any{"command": `if shopt -q login_shell; then printf login; else printf nonlogin; fi`}})
			require.NoError(t, err)
			require.Equal(t, "nonlogin", result.Output)
		})
	}
}

func TestBashHostEnvironmentPATHAndCache(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	bin := filepath.Join(t.TempDir(), "host bin")
	require.NoError(t, os.Mkdir(bin, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(bin, "host-cache-probe"), []byte("#!/bin/sh\nprintf host-path-nonce\n"), 0o700))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	paths := []string{filepath.Join(t.TempDir(), "go path"), filepath.Join(t.TempDir(), "go cache"), filepath.Join(t.TempDir(), "go mod cache")}
	for index, key := range []string{"GOPATH", "GOCACHE", "GOMODCACHE"} {
		t.Setenv(key, paths[index])
	}
	for name, execute := range environmentBashVariants(WithNoSandbox()) {
		t.Run(name, func(t *testing.T) {
			result, err := execute(context.Background(), ToolCall{Args: map[string]any{"command": `printf '%s\n' "$GOPATH" "$GOCACHE" "$GOMODCACHE"; host-cache-probe`}})
			require.NoError(t, err)
			require.Equal(t, strings.Join(paths, "\n")+"\nhost-path-nonce", result.Output)
		})
	}
}

func TestFilteredEnvInheritedCachePresence(t *testing.T) {
	for _, key := range []string{"GOPATH", "GOCACHE", "GOMODCACHE"} {
		t.Run(key, func(t *testing.T) {
			for _, state := range []string{"unset", "empty", "value"} {
				t.Run(state, func(t *testing.T) {
					value := ""
					if state == "value" {
						value = filepath.Join(t.TempDir(), "host cache")
					}
					t.Setenv(key, value)
					if state == "unset" {
						require.NoError(t, os.Unsetenv(key))
					}
					found := false
					actual := ""
					for _, entry := range filteredEnv(nil) {
						name, v, ok := strings.Cut(entry, "=")
						if ok && name == key {
							found = true
							actual = v
						}
					}
					require.Equal(t, state != "unset", found, "preserve explicit presence")
					require.Equal(t, value, actual)
				})
			}
		})
	}
}

func TestBashHostEnvironmentRejectsAmbientUnsafeAndRetainsExplicitOverride(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	for _, key := range []string{"GOPROXY", "GOFLAGS", "BASH_ENV", "ENV", "LD_PRELOAD", "UNLISTED_AMBIENT", "CUSTOM_API_KEY"} {
		t.Setenv(key, "ambient-test-sentinel")
	}
	t.Setenv("GOPATH", "parent-cache-path")
	for name, execute := range environmentBashVariants(WithNoSandbox(), WithEnv(map[string]string{"GOPATH": "explicit-cache-path", "SAFE_EXTRA": "explicit-safe", "CUSTOM_API_KEY": "extra-test-sentinel"})) {
		t.Run(name, func(t *testing.T) {
			command := `printf '%s\n' "${GOPROXY+x}" "${GOFLAGS+x}" "${BASH_ENV+x}" "${ENV+x}" "${LD_PRELOAD+x}" "${UNLISTED_AMBIENT+x}" "${CUSTOM_API_KEY+x}" "$GOPATH" "$SAFE_EXTRA"`
			result, err := execute(context.Background(), ToolCall{Args: map[string]any{"command": command}})
			require.NoError(t, err)
			require.Equal(t, strings.Repeat("\n", 7)+"explicit-cache-path\nexplicit-safe\n", result.Output)
		})
	}
}
