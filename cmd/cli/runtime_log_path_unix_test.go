//go:build darwin || linux

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/pengjunchen/go-cli/internal/config"
)

func TestRuntimeLogOverrideCapturesConfigStartup(t *testing.T) {
	work := t.TempDir()
	t.Chdir(work)
	target := filepath.Join(t.TempDir(), "runtime", "go-cli.log")
	t.Setenv("GO_CLI_LOG_FILE_PATH", target)
	before := slog.Default()
	var stdout, stderr bytes.Buffer
	code := run([]string{"version"}, &stdout, &stderr, func() (*config.Config, error) {
		slog.Info("selected.startup", "prompt", "PRIVATE-PROMPT-CANARY")
		return disabledCfg(), nil
	})
	require.Equal(t, 0, code)
	require.FileExists(t, target)
	body, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Contains(t, string(body), "selected.startup")
	require.NotContains(t, string(body), "PRIVATE-PROMPT-CANARY")
	require.NoDirExists(t, filepath.Join(work, ".go-cli", "logs"))
	require.Same(t, before, slog.Default())
	require.Empty(t, stderr.String())
}

func TestRuntimeLogExplicitAppendModes(t *testing.T) {
	for _, mask := range []int{0o022, 0o077} {
		t.Run(fmt.Sprintf("umask-%03o", mask), func(t *testing.T) {
			previous := unix.Umask(mask)
			defer unix.Umask(previous)
			parent := filepath.Join(t.TempDir(), "new-private")
			path := filepath.Join(parent, "$LITERAL.log")
			f, err := openExplicitRuntimeLogFile(path)
			require.NoError(t, err)
			_, err = f.WriteString("first\n")
			require.NoError(t, err)
			require.NoError(t, f.Close())
			info, err := os.Stat(parent)
			require.NoError(t, err)
			require.Equal(t, os.FileMode(0o700), info.Mode().Perm())
			info, err = os.Stat(path)
			require.NoError(t, err)
			require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
			f, err = openExplicitRuntimeLogFile(path)
			require.NoError(t, err)
			_, err = f.WriteString("second\n")
			require.NoError(t, err)
			require.NoError(t, f.Close())
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, "first\nsecond\n", string(data))
		})
	}
}

func TestRuntimeLogExplicitFailureIsRedactedStderrOnly(t *testing.T) {
	for _, kind := range []string{"empty", "relative", "directory", "symlink", "parent-symlink", "wide-file", "wide-parent", "parent-file"} {
		t.Run(kind, func(t *testing.T) {
			work := t.TempDir()
			t.Chdir(work)
			parent := filepath.Join(t.TempDir(), "private")
			require.NoError(t, os.Mkdir(parent, 0o700))
			target := filepath.Join(parent, "PRIVATE-PATH-CANARY.log")
			var original string
			switch kind {
			case "empty":
				target = ""
			case "relative":
				target = "PRIVATE-PATH-CANARY.log"
			case "directory":
				require.NoError(t, os.Mkdir(target, 0o700))
			case "symlink":
				original = filepath.Join(parent, "untouched")
				require.NoError(t, os.WriteFile(original, []byte("keep"), 0o600))
				require.NoError(t, os.Symlink(original, target))
			case "parent-symlink":
				alias := parent + "-alias"
				require.NoError(t, os.Symlink(parent, alias))
				target = filepath.Join(alias, filepath.Base(target))
			case "wide-file":
				require.NoError(t, os.WriteFile(target, []byte("keep"), 0o600))
				require.NoError(t, os.Chmod(target, 0o644))
				original = target
			case "wide-parent":
				require.NoError(t, os.Chmod(parent, 0o755))
			case "parent-file":
				require.NoError(t, os.Remove(parent))
				require.NoError(t, os.WriteFile(parent, []byte("keep"), 0o600))
				original = parent
			}
			t.Setenv(runtimeLogFileEnv, target)
			var stdout, stderr bytes.Buffer
			previous := slog.Default()
			code := run([]string{"version"}, &stdout, &stderr, func() (*config.Config, error) {
				slog.Info("fallback.startup", "prompt", "RAW-PROMPT-CANARY")
				return disabledCfg(), nil
			})
			require.Equal(t, 0, code)
			require.Same(t, previous, slog.Default())
			require.Contains(t, stderr.String(), "runtime log file unavailable; continuing on stderr")
			require.Contains(t, stderr.String(), "fallback.startup")
			require.NotContains(t, stderr.String(), "PRIVATE-PATH-CANARY")
			require.NotContains(t, stderr.String(), "RAW-PROMPT-CANARY")
			require.NotContains(t, stdout.String(), "warning")
			require.NoDirExists(t, filepath.Join(work, ".go-cli", "logs"))
			if original != "" {
				data, err := os.ReadFile(original)
				require.NoError(t, err)
				require.Equal(t, "keep", string(data))
			}
			if kind == "wide-parent" {
				info, err := os.Stat(parent)
				require.NoError(t, err)
				require.Equal(t, os.FileMode(0o755), info.Mode().Perm())
			}
		})
	}
}

func TestRuntimeLogDefaultAndExplicitTraceRemainSeparate(t *testing.T) {
	t.Run("unset-default", func(t *testing.T) {
		t.Setenv(runtimeLogFileEnv, "")
		require.NoError(t, os.Unsetenv(runtimeLogFileEnv))
		work := t.TempDir()
		t.Chdir(work)
		t.Setenv("HOME", t.TempDir())
		t.Setenv("USERPROFILE", t.TempDir())
		var stdout, stderr bytes.Buffer
		cfg := disabledCfg()
		cfg.Git.WorkDir = t.TempDir()
		require.Equal(t, 0, run([]string{"version"}, &stdout, &stderr, func() (*config.Config, error) { slog.Info("old.default"); return cfg, nil }))
		require.FileExists(t, filepath.Join(work, ".go-cli", "logs", "go-cli.log"))
	})
	t.Run("explicit-trace", func(t *testing.T) {
		work := t.TempDir()
		t.Chdir(work)
		target := filepath.Join(t.TempDir(), "private", "log")
		t.Setenv(runtimeLogFileEnv, target)
		cfg := disabledCfg()
		enabled := true
		cfg.Tracing.Enabled = &enabled
		cfg.Tracing.Exporter = "jsonl"
		cfg.Tracing.FilePath = t.TempDir()
		var stdout, stderr bytes.Buffer
		require.Equal(t, 0, run([]string{"version"}, &stdout, &stderr, func() (*config.Config, error) { return cfg, nil }))
		require.FileExists(t, target)
		files, err := filepath.Glob(filepath.Join(cfg.Tracing.FilePath, "*.jsonl"))
		require.NoError(t, err)
		require.NotEmpty(t, files)
		require.NoDirExists(t, filepath.Join(work, ".go-cli"))
	})
}

func TestRuntimeLogRejectsLeafReplacementAfterParentOpen(t *testing.T) {
	parent := t.TempDir()
	victim := filepath.Join(t.TempDir(), "untouched")
	require.NoError(t, os.WriteFile(victim, []byte("keep"), 0o600))
	fd, err := unix.Open(parent, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	require.NoError(t, err)
	defer func() { require.NoError(t, unix.Close(fd)) }()
	require.NoError(t, os.Symlink(victim, filepath.Join(parent, "log")))
	f, err := openRuntimeLogAt(fd, "log", filepath.Join(parent, "log"))
	require.Error(t, err)
	require.Nil(t, f)
	data, err := os.ReadFile(victim)
	require.NoError(t, err)
	require.Equal(t, "keep", string(data))
}

func TestRuntimeLogFIFOIsNonblocking(t *testing.T) {
	const childEnv = "GOCLI_RUNTIME_LOG_FIFO_CHILD"
	if os.Getenv(childEnv) == "1" {
		os.Exit(run([]string{"--version"}, os.Stdout, os.Stderr, func() (*config.Config, error) { return disabledCfg(), nil }))
	}
	work := t.TempDir()
	parent := t.TempDir()
	fifo := filepath.Join(parent, "pipe")
	require.NoError(t, unix.Mkfifo(fifo, 0o600))
	binary, err := os.Executable()
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, "-test.run=^TestRuntimeLogFIFOIsNonblocking$")
	command.Dir = work
	command.Env = append(os.Environ(), childEnv+"=1", runtimeLogFileEnv+"="+fifo)
	output, err := command.CombinedOutput()
	require.NoError(t, ctx.Err(), "FIFO must not block: %s", output)
	require.NoError(t, err, "%s", output)
	require.Contains(t, string(output), "runtime log file unavailable; continuing on stderr")
	require.NoDirExists(t, filepath.Join(work, ".go-cli", "logs"))
	require.False(t, strings.Contains(string(output), fifo))
}

func TestRuntimeLogExplicitEarlyReturnsRestoreLogger(t *testing.T) {
	for _, mode := range []string{"--help", "--version", "config-failure"} {
		t.Run(mode, func(t *testing.T) {
			work := t.TempDir()
			t.Chdir(work)
			target := filepath.Join(t.TempDir(), "private", "log")
			t.Setenv(runtimeLogFileEnv, target)
			before := slog.Default()
			var stdout, stderr bytes.Buffer
			called := false
			args := []string{mode}
			if mode == "config-failure" {
				args = []string{"version"}
			}
			code := run(args, &stdout, &stderr, func() (*config.Config, error) {
				called = true
				slog.Info("config.before.error", "prompt", "RAW-CONFIG-PROMPT")
				return nil, errors.New("synthetic config failure")
			})
			require.Same(t, before, slog.Default())
			require.FileExists(t, target)
			require.NoDirExists(t, filepath.Join(work, ".go-cli", "logs"))
			if mode == "config-failure" {
				require.Equal(t, 1, code)
				require.True(t, called)
				body, err := os.ReadFile(target)
				require.NoError(t, err)
				require.Contains(t, string(body), "config.before.error")
				require.NotContains(t, string(body), "RAW-CONFIG-PROMPT")
			} else {
				require.Equal(t, 0, code)
				require.False(t, called)
			}
		})
	}
}
