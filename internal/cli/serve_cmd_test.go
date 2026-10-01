//exempt:scan003
package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pengjunchen/go-cli/internal/config"
)

// safeBuffer is a thread-safe bytes.Buffer for concurrent read/write.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestServeApproveAutoWithoutAuthRefuses verifies that --approve auto with
// --no-auth refuses to start (AC-3).
func TestServeApproveAutoWithoutAuthRefuses(t *testing.T) {
	out := &safeBuffer{}
	c := newServeCmd(out)

	err := c.Run(context.Background(), NewDefaultConfig(false), []string{
		"--no-auth",
		"--approve", "auto",
		"--addr", "127.0.0.1:0",
	})

	if err == nil {
		t.Fatal("expected error when --approve auto is used with --no-auth")
	}

	ue, ok := err.(*UsageError)
	if !ok {
		t.Fatalf("expected UsageError, got %T: %v", err, err)
	}

	if !strings.Contains(ue.Error(), "approve auto") {
		t.Errorf("error should mention --approve auto, got: %v", err)
	}
	if !strings.Contains(ue.Error(), "auth") {
		t.Errorf("error should mention auth, got: %v", err)
	}
}

func TestServeApproveAskRejectedBeforeTokenWriteOrListener(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "default ask"},
		{name: "explicit ask", args: []string{"--approve", "ask"}},
		{name: "no-auth ask", args: []string{"--no-auth", "--approve", "ask"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := &safeBuffer{}
			c := newServeCmd(out)
			tokenPath := privateServeTokenPath(t)
			const oldToken = "preserve-existing-token"
			if err := os.WriteFile(tokenPath, []byte(oldToken), 0o600); err != nil {
				t.Fatal(err)
			}
			assemblyCalls := 0
			c.assemblyFactory = func(context.Context, *config.Config, string, string, io.Writer, ...AssembleOption) (*AgentAssembly, error) {
				assemblyCalls++
				return nil, errors.New("assembly must not run for ask mode")
			}

			args := append([]string{"--addr", "127.0.0.1:0", "--token-file", tokenPath}, tc.args...)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			err := c.Run(ctx, newAssembleTestConfig(t), args)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), "unsupported") {
				t.Fatalf("expected unsupported remote approval error, got %v", err)
			}
			var unsupported *UnsupportedServeApprovalError
			if !errors.As(err, &unsupported) || !errors.Is(err, ErrServeRemoteApprovalUnsupported) {
				t.Fatalf("expected typed unsupported approval error, got %T: %v", err, err)
			}
			if assemblyCalls != 0 {
				t.Fatalf("agent assembly ran before rejecting ask mode %d time(s)", assemblyCalls)
			}
			if !strings.Contains(err.Error(), "--approve auto") || !strings.Contains(err.Error(), "--approve deny") {
				t.Errorf("unsupported error should give supported alternatives, got %v", err)
			}
			if strings.Contains(out.String(), "listening") {
				t.Fatalf("serve listened despite unsupported ask mode: %s", out.String())
			}
			data, readErr := os.ReadFile(tokenPath)
			if readErr != nil {
				t.Fatalf("existing token file was removed: %v", readErr)
			}
			if string(data) != oldToken {
				t.Fatalf("token file changed before rejecting ask mode: %q", data)
			}
		})
	}
}

func TestServeRemoteTUIRejectedBeforeAssemblyOrTokenWrite(t *testing.T) {
	out := &safeBuffer{}
	c := newServeCmd(out)
	tokenPath := privateServeTokenPath(t)
	const oldToken = "preserve-existing-token"
	if err := os.WriteFile(tokenPath, []byte(oldToken), 0o600); err != nil {
		t.Fatal(err)
	}
	assemblyCalls := 0
	c.assemblyFactory = func(context.Context, *config.Config, string, string, io.Writer, ...AssembleOption) (*AgentAssembly, error) {
		assemblyCalls++
		return nil, errors.New("assembly must not run for unsupported remote TUI")
	}

	err := c.Run(context.Background(), newAssembleTestConfig(t), []string{
		"--tui",
		"--addr", "127.0.0.1:0",
		"--token-file", tokenPath,
	})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "flag provided but not defined") {
		t.Fatalf("expected unsupported remote TUI flag error, got %v", err)
	}
	if assemblyCalls != 0 {
		t.Fatalf("agent assembly ran before rejecting remote TUI %d time(s)", assemblyCalls)
	}
	if strings.Contains(out.String(), "listening") {
		t.Fatalf("serve listened despite unsupported remote TUI: %s", out.String())
	}
	data, readErr := os.ReadFile(tokenPath)
	if readErr != nil || string(data) != oldToken {
		t.Fatalf("unsupported remote TUI changed the token file: data=%q err=%v", data, readErr)
	}
}

// waitForServeStart polls out for the "listening" message and returns when
// the server has started or done receives an error. It fatals on error.
func waitForServeStart(t *testing.T, out *safeBuffer, done <-chan error) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(out.String(), "listening") {
			return
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("server failed to start: %v", err)
			}
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
	if !strings.Contains(out.String(), "listening") {
		t.Fatal("server did not start within timeout")
	}
}

func TestServeStartsAssembledRuntimeWithExplicitDenyOnLoopback(t *testing.T) {
	out := &safeBuffer{}
	c := newServeCmd(out)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- c.Run(ctx, newAssembleTestConfig(t), []string{
			"--no-auth",
			"--approve", "deny",
			"--addr", "127.0.0.1:0",
		})
	}()

	waitForServeStart(t, out, done)
	if strings.Contains(out.String(), "echo mode") || strings.Contains(out.String(), "fallback") {
		t.Fatalf("serve started without the configured agent runtime: %s", out.String())
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("serve shutdown failed: %v", err)
	}
}

func privateServeTokenPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "serve-token")
}

// TestServe_TokenNotInStdout verifies that when a token is auto-generated,
// the token value does not appear in stdout (SEC-5, AC-1).
func TestServe_TokenNotInStdout(t *testing.T) {
	out := &safeBuffer{}
	errOut := &safeBuffer{}
	c := newServeCmd(out)
	c.errOut = errOut

	tokenPath := privateServeTokenPath(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- c.Run(ctx, newAssembleTestConfig(t), []string{
			"--addr", "127.0.0.1:0",
			"--approve", "deny",
			"--token-file", tokenPath,
		})
	}()

	waitForServeStart(t, out, done)

	// Read the token from the file so we can check it is not in stdout.
	tokenBytes, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatalf("failed to read token file: %v", err)
	}
	token := string(tokenBytes)

	stdout := out.String()
	if strings.Contains(stdout, token) {
		t.Errorf("auto-generated token must not appear in stdout; token=%s", token)
	}

	// Token confirmation should be on stderr, not stdout.
	if strings.Contains(stdout, "Auth token:") {
		t.Error("'Auth token:' should not appear in stdout")
	}

	cancel()
	<-done
}

// TestServe_TokenFilePermission verifies that the auto-generated token file
// has 0600 permissions (SEC-5, AC-2).
func TestServe_TokenFilePermission(t *testing.T) {
	out := &safeBuffer{}
	c := newServeCmd(out)

	tokenPath := filepath.Join(t.TempDir(), "sub", "serve-token")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- c.Run(ctx, newAssembleTestConfig(t), []string{
			"--addr", "127.0.0.1:0",
			"--approve", "deny",
			"--token-file", tokenPath,
		})
	}()

	waitForServeStart(t, out, done)

	info, err := os.Stat(tokenPath)
	if err != nil {
		t.Fatalf("token file not created: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("expected token file permission 0600, got %o", info.Mode().Perm())
	}

	cancel()
	<-done
}

// TestServe_TokenFileReadable verifies that the token written to the file
// can be read back and is a valid bearer token (SEC-5, AC-3).
func TestServe_TokenFileReadable(t *testing.T) {
	out := &safeBuffer{}
	c := newServeCmd(out)

	tokenPath := privateServeTokenPath(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- c.Run(ctx, newAssembleTestConfig(t), []string{
			"--addr", "127.0.0.1:0",
			"--approve", "deny",
			"--token-file", tokenPath,
		})
	}()

	waitForServeStart(t, out, done)

	tokenBytes, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatalf("failed to read token file: %v", err)
	}
	token := string(tokenBytes)

	// Token should be a non-empty 64-char hex string (32 bytes hex-encoded).
	if len(token) != 64 {
		t.Errorf("expected 64-char hex token, got %d chars: %q", len(token), token)
	}
	for _, ch := range token {
		isHex := (ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f')
		if !isHex {
			t.Errorf("token contains non-hex character %q in %q", ch, token)
			break
		}
	}

	// The token is usable as a Bearer token (format check).
	bearerHeader := "Bearer " + token
	if !strings.HasPrefix(bearerHeader, "Bearer ") {
		t.Error("token cannot form a valid Bearer header")
	}

	cancel()
	<-done
}

// TestServe_ExplicitTokenToStderr verifies that a token provided via --token
// is not written to stdout; confirmation goes to stderr (SEC-5).
func TestServe_ExplicitTokenToStderr(t *testing.T) {
	out := &safeBuffer{}
	errOut := &safeBuffer{}
	c := newServeCmd(out)
	c.errOut = errOut

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- c.Run(ctx, newAssembleTestConfig(t), []string{
			"--addr", "127.0.0.1:0",
			"--approve", "deny",
			"--token", "explicit-secret-token",
		})
	}()

	waitForServeStart(t, out, done)

	// Token must NOT appear in stdout.
	if strings.Contains(out.String(), "explicit-secret-token") {
		t.Error("explicit token must not appear in stdout")
	}

	// Confirmation should appear on stderr.
	if !strings.Contains(errOut.String(), "Auth token configured via --token") {
		t.Error("expected 'Auth token configured via --token' in stderr")
	}
	// Without --show-token, the token itself should not be on stderr.
	if strings.Contains(errOut.String(), "explicit-secret-token") {
		t.Error("token should not appear on stderr without --show-token")
	}

	cancel()
	<-done
}

// TestServe_TokenFileCustomPath verifies that --token-file writes the token
// to the specified path with 0600 permissions (SEC-5).
func TestServe_TokenFileCustomPath(t *testing.T) {
	out := &safeBuffer{}
	errOut := &safeBuffer{}
	c := newServeCmd(out)
	c.errOut = errOut

	customPath := filepath.Join(t.TempDir(), "custom-dir", "my-token")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- c.Run(ctx, newAssembleTestConfig(t), []string{
			"--addr", "127.0.0.1:0",
			"--approve", "deny",
			"--token-file", customPath,
		})
	}()

	waitForServeStart(t, out, done)

	// Token should be at the custom path.
	info, err := os.Stat(customPath)
	if err != nil {
		t.Fatalf("token file not created at custom path: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("expected token file permission 0600, got %o", info.Mode().Perm())
	}

	// stderr should mention the custom path.
	if !strings.Contains(errOut.String(), customPath) {
		t.Errorf("expected stderr to mention custom path %q, got: %s", customPath, errOut.String())
	}

	// File should contain a non-empty token.
	tokenBytes, err := os.ReadFile(customPath)
	if err != nil {
		t.Fatalf("failed to read token file: %v", err)
	}
	if len(tokenBytes) == 0 {
		t.Error("token file is empty")
	}

	cancel()
	<-done
}

// TestServeDefaultAddrIsLocalhost verifies that the default --addr flag
// binds to 127.0.0.1, not 0.0.0.0 or an empty host (AC-1).
func TestServeDefaultAddrIsLocalhost(t *testing.T) {
	out := &safeBuffer{}
	c := newServeCmd(out)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		// Use --no-auth so we don't need to deal with tokens.
		// Don't pass --addr to test the default.
		done <- c.Run(ctx, newAssembleTestConfig(t), []string{"--no-auth", "--approve", "deny"})
	}()

	// Poll for output or error. The server may fail if port 9090 is
	// already in use — either way the output/error should reference
	// 127.0.0.1, confirming the default binds to localhost.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		output := out.String()
		if strings.Contains(output, "listening") {
			// Server started — verify it bound to 127.0.0.1.
			if !strings.Contains(output, "127.0.0.1") {
				t.Errorf("expected output to contain 127.0.0.1, got: %s", output)
			}
			cancel()
			<-done
			return
		}
		select {
		case err := <-done:
			// Server failed to start — verify error mentions 127.0.0.1
			// (the default addr), confirming it's not 0.0.0.0 or empty.
			if err == nil {
				return
			}
			if !strings.Contains(err.Error(), "127.0.0.1") {
				t.Errorf("expected error to contain 127.0.0.1 (default addr), got: %v", err)
			}
			return
		case <-time.After(50 * time.Millisecond):
		}
	}

	t.Fatal("server did not start or fail within timeout")
	cancel()
	<-done
}

// TestServeGeneratesAuthToken verifies that when auth is enabled (default),
// a token is auto-generated and written to a file (0600). The token must
// NOT appear in stdout; confirmation goes to stderr.
func TestServeGeneratesAuthToken(t *testing.T) {
	out := &safeBuffer{}
	errOut := &safeBuffer{}
	c := newServeCmd(out)
	c.errOut = errOut

	tokenPath := privateServeTokenPath(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- c.Run(ctx, newAssembleTestConfig(t), []string{
			"--addr", "127.0.0.1:0",
			"--approve", "deny",
			"--token-file", tokenPath,
		})
	}()

	// Wait for the server to start.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(out.String(), "listening") {
			break
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("server failed to start: %v", err)
			}
			return
		case <-time.After(50 * time.Millisecond):
		}
	}

	output := out.String()
	if !strings.Contains(output, "listening") {
		t.Fatal("server did not start within timeout")
	}

	// Token must NOT appear in stdout.
	if strings.Contains(output, "Auth token:") {
		t.Error("token info should not appear in stdout")
	}

	// Confirmation should go to stderr.
	if !strings.Contains(errOut.String(), "Auth token written to") {
		t.Error("expected 'Auth token written to' in stderr")
	}

	// Token file should exist.
	info, err := os.Stat(tokenPath)
	if err != nil {
		t.Fatalf("token file not created: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("expected token file permission 0600, got %o", info.Mode().Perm())
	}

	cancel()
	<-done
}

// TestServeCustomTokenUsed verifies that when --token is provided, that
// exact token is used. The token must NOT appear in stdout; with
// --show-token it is printed to stderr.
func TestServeCustomTokenUsed(t *testing.T) {
	out := &safeBuffer{}
	errOut := &safeBuffer{}
	c := newServeCmd(out)
	c.errOut = errOut

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- c.Run(ctx, newAssembleTestConfig(t), []string{
			"--addr", "127.0.0.1:0",
			"--approve", "deny",
			"--token", "my-custom-token",
			"--show-token",
		})
	}()

	// Wait for the server to start.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(out.String(), "listening") {
			break
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("server failed to start: %v", err)
			}
			return
		case <-time.After(50 * time.Millisecond):
		}
	}

	output := out.String()
	if !strings.Contains(output, "listening") {
		t.Fatal("server did not start within timeout")
	}

	// Token must NOT appear in stdout.
	if strings.Contains(output, "my-custom-token") {
		t.Error("custom token should not appear in stdout")
	}

	// With --show-token, the token should appear on stderr.
	if !strings.Contains(errOut.String(), "my-custom-token") {
		t.Error("expected custom token in stderr with --show-token")
	}

	cancel()
	<-done
}

func TestServeMissingRuntimeFailsBeforeTokenOrListener(t *testing.T) {
	out := &safeBuffer{}
	tokenPath := privateServeTokenPath(t)
	c := newServeCmd(out)
	assemblyCalls := 0
	c.assemblyFactory = func(context.Context, *config.Config, string, string, io.Writer, ...AssembleOption) (*AgentAssembly, error) {
		assemblyCalls++
		return nil, errors.New("assembly must not run without a config")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := c.Run(ctx, NewDefaultConfig(false), []string{
		"--addr", "127.0.0.1:0",
		"--approve", "deny",
		"--token-file", tokenPath,
	})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "runtime") {
		t.Fatalf("expected missing-runtime error, got %v", err)
	}
	if strings.Contains(out.String(), "listening") {
		t.Fatalf("server listened without a configured runtime: %s", out.String())
	}
	if _, statErr := os.Stat(tokenPath); !os.IsNotExist(statErr) {
		t.Fatalf("token file should not be written without a runtime, stat error: %v", statErr)
	}
	if assemblyCalls != 0 {
		t.Fatalf("agent assembly ran without a loaded config %d time(s)", assemblyCalls)
	}
}

func TestWriteTokenFileReplacesWideFilePrivately(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "serve-token")
	if err := os.WriteFile(path, []byte("old-token"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := writeTokenFile(path, "new-token-secret"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new-token-secret" {
		t.Fatalf("unexpected token file contents: %q", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("expected token file permission 0600, got %o", info.Mode().Perm())
	}
}

func TestServeTokenFileFailureDoesNotStartListener(t *testing.T) {
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := reservation.Addr().String()
	if err := reservation.Close(); err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	out := &safeBuffer{}
	errOut := &safeBuffer{}
	c := newServeCmd(out)
	c.errOut = errOut
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- c.Run(ctx, newAssembleTestConfig(t), []string{
			"--addr", addr,
			"--approve", "deny",
			"--token-file", filepath.Join(dir, "serve-token"),
		})
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected token file permission failure")
		}
		if strings.Contains(out.String(), "listening") {
			t.Fatalf("listener started despite token file failure: %s", out.String())
		}
		probe, listenErr := net.Listen("tcp", addr)
		if listenErr != nil {
			t.Fatalf("token file failure left a listener on %s: %v", addr, listenErr)
		}
		if err := probe.Close(); err != nil {
			t.Fatal(err)
		}
	case <-time.After(300 * time.Millisecond):
		cancel()
		<-done
		t.Fatal("serve did not fail before listener startup")
	}
}

func TestServeBindFailureDoesNotPublishTokenOrLeaveListener(t *testing.T) {
	for _, tc := range []struct {
		name          string
		existingToken string
	}{
		{name: "new token"},
		{name: "preserve existing token", existingToken: "previous-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			occupied, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			addr := occupied.Addr().String()
			defer occupied.Close()

			tokenPath := privateServeTokenPath(t)
			if tc.existingToken != "" {
				if err := os.WriteFile(tokenPath, []byte(tc.existingToken), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			out := &safeBuffer{}
			errOut := &safeBuffer{}
			c := newServeCmd(out)
			c.errOut = errOut
			err = c.Run(context.Background(), newAssembleTestConfig(t), []string{
				"--addr", addr,
				"--approve", "deny",
				"--token-file", tokenPath,
			})
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), "listen") {
				t.Fatalf("expected bind failure, got %v", err)
			}
			if strings.Contains(out.String(), "listening") {
				t.Fatalf("serve reported success after bind failure: %s", out.String())
			}
			if strings.Contains(errOut.String(), "Auth token:") {
				t.Fatalf("serve disclosed a token after bind failure: %s", errOut.String())
			}

			data, readErr := os.ReadFile(tokenPath)
			if tc.existingToken == "" {
				if !os.IsNotExist(readErr) {
					t.Fatalf("failed serve published a token file: data=%q err=%v", data, readErr)
				}
			} else if readErr != nil || string(data) != tc.existingToken {
				t.Fatalf("failed serve replaced the existing token: data=%q err=%v", data, readErr)
			}

			if err := occupied.Close(); err != nil {
				t.Fatal(err)
			}
			probe, err := net.Listen("tcp", addr)
			if err != nil {
				t.Fatalf("failed serve left a listener on %s: %v", addr, err)
			}
			if err := probe.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestServeAssemblyFailureDoesNotWriteTokenOrStartListener(t *testing.T) {
	workDir := t.TempDir()
	hookDir := filepath.Join(workDir, ".go-cli")
	if err := os.MkdirAll(hookDir, 0o700); err != nil {
		t.Fatal(err)
	}
	const commandCanary = "PRIVATE_HOOK_COMMAND_CANARY"
	hooks := "hooks:\n  - event: post_tool_use\n    command: echo " + commandCanary + "\n    timeout: 1s\n"
	configPath := filepath.Join(hookDir, "hooks.yaml")
	if err := os.WriteFile(configPath, []byte(hooks), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := newTrustedProjectHookAssembleTestConfig(t, workDir)
	out := &safeBuffer{}
	c := newServeCmd(out)
	tokenPath := privateServeTokenPath(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := c.Run(ctx, cfg, []string{
		"--approve", "deny",
		"--addr", "127.0.0.1:0",
		"--token-file", tokenPath,
	})
	if err == nil {
		t.Fatal("expected unsupported assembly error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "posttooluse") {
		t.Errorf("expected error to identify the unsupported hook, got: %v", err)
	}
	if !strings.Contains(err.Error(), configPath) || !strings.Contains(err.Error(), "hook 0") {
		t.Errorf("expected error to identify the configured path and hook index, got: %v", err)
	}
	if strings.Contains(err.Error(), commandCanary) {
		t.Errorf("assembly error must not disclose hook command contents, got: %v", err)
	}
	if strings.Contains(out.String(), "listening") {
		t.Fatalf("server listened after assembly failure: %s", out.String())
	}
	if _, statErr := os.Stat(tokenPath); !os.IsNotExist(statErr) {
		t.Fatalf("token file should not be written after assembly failure, stat error: %v", statErr)
	}
}

func TestServeAssemblyErrorClosesReturnedAssembly(t *testing.T) {
	assembly, err := AssembleAgent(context.Background(), newAssembleTestConfig(t), "openai", "test-model", io.Discard,
		WithApproveMode(ApproveDeny),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = assembly.Close(context.Background()) })
	if assembly.closeOwner == nil {
		t.Fatal("assembled runtime has no close owner")
	}
	closeDone := assembly.closeOwner.done

	fixtureErr := errors.New("fixture failure after partial assembly")
	out := &safeBuffer{}
	c := newServeCmd(out)
	c.assemblyFactory = func(context.Context, *config.Config, string, string, io.Writer, ...AssembleOption) (*AgentAssembly, error) {
		return assembly, fixtureErr
	}
	tokenPath := privateServeTokenPath(t)
	err = c.Run(context.Background(), newAssembleTestConfig(t), []string{
		"--approve", "deny",
		"--addr", "127.0.0.1:0",
		"--token-file", tokenPath,
	})
	if !errors.Is(err, fixtureErr) {
		t.Fatalf("expected the assembly error to be preserved, got %v", err)
	}
	select {
	case <-closeDone:
	default:
		t.Fatal("serve returned without closing the assembly owner")
	}
	if strings.Contains(out.String(), "listening") {
		t.Fatalf("server listened after assembly failure: %s", out.String())
	}
	if _, statErr := os.Stat(tokenPath); !os.IsNotExist(statErr) {
		t.Fatalf("token file should not be written after assembly failure, stat error: %v", statErr)
	}
}
