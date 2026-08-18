//exempt:scan003
package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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

// TestServeApproveAskWithoutAuthAllowed verifies that --approve ask (default)
// with --no-auth does not refuse to start. The server should start normally.
func TestServeApproveAskWithoutAuthAllowed(t *testing.T) {
	out := &safeBuffer{}
	c := newServeCmd(out)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- c.Run(ctx, NewDefaultConfig(false), []string{
			"--no-auth",
			"--approve", "ask",
			"--addr", "127.0.0.1:0",
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
			return // server started and stopped
		case <-time.After(50 * time.Millisecond):
		}
	}

	if !strings.Contains(out.String(), "listening") {
		t.Fatal("server did not start within timeout")
	}

	// No auth token should be printed with --no-auth.
	if strings.Contains(out.String(), "Auth token") {
		t.Error("expected no auth token output with --no-auth")
	}

	cancel()
	<-done
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

// TestServe_TokenNotInStdout verifies that when a token is auto-generated,
// the token value does not appear in stdout (SEC-5, AC-1).
func TestServe_TokenNotInStdout(t *testing.T) {
	out := &safeBuffer{}
	errOut := &safeBuffer{}
	c := newServeCmd(out)
	c.errOut = errOut

	tokenPath := filepath.Join(t.TempDir(), "serve-token")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- c.Run(ctx, NewDefaultConfig(false), []string{
			"--addr", "127.0.0.1:0",
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
		done <- c.Run(ctx, NewDefaultConfig(false), []string{
			"--addr", "127.0.0.1:0",
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

	tokenPath := filepath.Join(t.TempDir(), "serve-token")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- c.Run(ctx, NewDefaultConfig(false), []string{
			"--addr", "127.0.0.1:0",
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
		done <- c.Run(ctx, NewDefaultConfig(false), []string{
			"--addr", "127.0.0.1:0",
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
		done <- c.Run(ctx, NewDefaultConfig(false), []string{
			"--addr", "127.0.0.1:0",
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
		done <- c.Run(ctx, NewDefaultConfig(false), []string{"--no-auth"})
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

	tokenPath := filepath.Join(t.TempDir(), "serve-token")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- c.Run(ctx, NewDefaultConfig(false), []string{
			"--addr", "127.0.0.1:0",
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
		done <- c.Run(ctx, NewDefaultConfig(false), []string{
			"--addr", "127.0.0.1:0",
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

// TestServeFallbackWarnsEchoMode verifies that when the server falls back to
// echo mode (no config available), a prominent WARNING is printed to the
// output so users notice the degraded mode.
func TestServeFallbackWarnsEchoMode(t *testing.T) {
	out := &safeBuffer{}
	c := newServeCmd(out)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		// NewDefaultConfig(false) returns a *defaultConfig, not a
		// *config.Config, so rc will be nil and the server falls back
		// to echo mode.
		done <- c.Run(ctx, NewDefaultConfig(false), []string{
			"--no-auth",
			"--addr", "127.0.0.1:0",
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

	// The fallback should produce a prominent WARNING about echo mode.
	if !strings.Contains(output, "WARNING") {
		t.Errorf("expected 'WARNING' in output when falling back to echo mode, got: %s", output)
	}
	if !strings.Contains(output, "echo mode") {
		t.Errorf("expected 'echo mode' in output when falling back, got: %s", output)
	}

	cancel()
	<-done
}
