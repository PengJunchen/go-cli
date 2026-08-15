package cli

import (
	"bytes"
	"context"
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
// a token is generated and printed to the output (AC-2).
func TestServeGeneratesAuthToken(t *testing.T) {
	out := &safeBuffer{}
	c := newServeCmd(out)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- c.Run(ctx, NewDefaultConfig(false), []string{
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

	// Auth token should be printed.
	if !strings.Contains(output, "Auth token:") {
		t.Error("expected 'Auth token:' in output when auth is enabled")
	}
	if !strings.Contains(output, "Authorization: Bearer") {
		t.Error("expected 'Authorization: Bearer' hint in output")
	}

	cancel()
	<-done
}

// TestServeCustomTokenUsed verifies that when --token is provided, that
// exact token is printed and used.
func TestServeCustomTokenUsed(t *testing.T) {
	out := &safeBuffer{}
	c := newServeCmd(out)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- c.Run(ctx, NewDefaultConfig(false), []string{
			"--addr", "127.0.0.1:0",
			"--token", "my-custom-token",
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

	if !strings.Contains(output, "my-custom-token") {
		t.Error("expected custom token in output")
	}

	cancel()
	<-done
}
