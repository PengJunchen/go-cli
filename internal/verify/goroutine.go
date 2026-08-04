// Package verify — goroutine leak detection and Go-specific safety checks.
package verify

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"
)

// AssertNoGoroutineLeak returns a cleanup function that should be deferred
// at the start of each test. It records the goroutine count at call time
// and checks for leaks when the deferred function runs.
//
// Uses polling with a 2-second timeout to avoid flaky failures in CI.
//
// Usage:
//
//	func TestFoo(t *testing.T) {
//	    defer verify.AssertNoGoroutineLeak(t)()
//	    // test code...
//	}
func AssertNoGoroutineLeak(t TestingT) func() {
	t.Helper()
	startCount := countFilteredGoroutines()
	return func() {
		// Poll for goroutines to settle, up to 2 seconds.
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if countFilteredGoroutines() <= startCount {
				return // no leak
			}
			time.Sleep(50 * time.Millisecond)
		}
		endCount := countFilteredGoroutines()
		if endCount > startCount {
			buf := make([]byte, 4096*4)
			n := runtime.Stack(buf, true)
			t.Fatalf("goroutine leak detected: started with %d, ended with %d\nLeaked goroutine stacks:\n%s",
				startCount, endCount, string(buf[:n]))
		}
	}
}

// knownLeakPatterns lists goroutine stack patterns that are false positives
// from the Go standard library and should be excluded from leak detection.
var knownLeakPatterns = []string{
	"net/http.(*persistConn).readLoop",
	"net/http.(*persistConn).writeLoop",
	// os/signal.loop is the runtime signal-watcher goroutine started by
	// signal.Notify. It is created once via sync.Once and never exits, even
	// after signal.Stop. It is a known Go runtime goroutine, not a leak.
	"os/signal.loop",
}

// countFilteredGoroutines returns the number of goroutines excluding known
// false-positive patterns (e.g., HTTP connection pool goroutines).
func countFilteredGoroutines() int {
	buf := make([]byte, 4096*4)
	n := runtime.Stack(buf, true)
	stackStr := string(buf[:n])

	// Each goroutine stack block is separated by "\n\n".
	blocks := strings.Split(stackStr, "\n\n")
	count := 0
	for _, block := range blocks {
		if strings.HasPrefix(block, "goroutine ") {
			isKnown := false
			for _, pattern := range knownLeakPatterns {
				if strings.Contains(block, pattern) {
					isKnown = true
					break
				}
			}
			if !isKnown {
				count++
			}
		}
	}
	return count
}

// AssertContextCanceled verifies that the given function returns with
// context.Canceled when the context is already canceled.
func AssertContextCanceled(t TestingT, fn func(ctx context.Context) error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := fn(ctx)
	if err == nil {
		t.Fatal("expected error from canceled context, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}
}

// AssertContextTimeout verifies that the given function returns with
// context.DeadlineExceeded when the context has a very short timeout.
func AssertContextTimeout(t TestingT, fn func(ctx context.Context) error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	defer cancel()

	// Drain the timer.
	time.Sleep(10 * time.Millisecond)

	err := fn(ctx)
	if err == nil {
		t.Fatal("expected error from expired context, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded, got: %v", err)
	}
}

// AssertErrorIs wraps errors.Is with a test failure.
func AssertErrorIs(t TestingT, err error, target error) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error to be %v, got nil", target)
	}
	if !errors.Is(err, target) {
		t.Fatalf("expected error to be %v, got: %v", target, err)
	}
}

// AssertNoPanic runs fn and fails the test if it panics.
func AssertNoPanic(t TestingT, fn func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			buf := make([]byte, 4096)
			n := runtime.Stack(buf, false)
			t.Fatalf("unexpected panic: %v\nStack:\n%s", r, string(buf[:n]))
		}
	}()
	fn()
}

// AssertPanic runs fn and fails the test if it does NOT panic.
func AssertPanic(t TestingT, fn func()) {
	t.Helper()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic, but function did not panic")
		}
	}()
	fn()
}

// GoLeakChecker provides goroutine leak detection across multiple test phases.
type GoLeakChecker struct {
	mu          sync.Mutex
	checkpoints []leakCheckpoint
}

type leakCheckpoint struct {
	name  string
	count int
}

// NewGoLeakChecker creates a new goroutine leak checker.
func NewGoLeakChecker() *GoLeakChecker {
	return &GoLeakChecker{}
}

// Checkpoint records the current goroutine count at a named checkpoint.
func (g *GoLeakChecker) Checkpoint(name string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.checkpoints = append(g.checkpoints, leakCheckpoint{
		name:  name,
		count: countFilteredGoroutines(),
	})
}

// Assert checks for goroutine leaks between the first checkpoint and now.
// Uses polling with a 2-second timeout to avoid flaky failures.
// At least two checkpoints must have been recorded before calling Assert.
func (g *GoLeakChecker) Assert(t TestingT) {
	t.Helper()
	g.mu.Lock()
	if len(g.checkpoints) < 2 {
		g.mu.Unlock()
		t.Fatal("need at least 2 checkpoints to check for leaks")
		return
	}
	first := g.checkpoints[0].count
	checkpointsStr := g.formatCheckpoints()
	g.mu.Unlock()

	// Poll outside lock to avoid blocking Checkpoint calls.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if countFilteredGoroutines() <= first {
			return // no leak
		}
		time.Sleep(50 * time.Millisecond)
	}

	last := countFilteredGoroutines()
	if last > first {
		buf := make([]byte, 4096*4)
		n := runtime.Stack(buf, true)
		t.Fatalf("goroutine leak: started=%d, current=%d (checkpoints: %s)\nStacks:\n%s",
			first, last, checkpointsStr, string(buf[:n]))
	}
}

func (g *GoLeakChecker) formatCheckpoints() string {
	var sb strings.Builder
	for i, cp := range g.checkpoints {
		if i > 0 {
			sb.WriteString(" -> ")
		}
		sb.WriteString(fmt.Sprintf("%s:%d", cp.name, cp.count))
	}
	return sb.String()
}
