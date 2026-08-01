package verify

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestAssertNoGoroutineLeak_Clean(t *testing.T) {
	defer AssertNoGoroutineLeak(t)()

	// Do nothing — should not leak.
}

func TestAssertNoGoroutineLeak_WithGoroutines(t *testing.T) {
	defer AssertNoGoroutineLeak(t)()

	var wg sync.WaitGroup
	wg.Add(5)
	for i := 0; i < 5; i++ {
		go func() {
			defer wg.Done()
			time.Sleep(10 * time.Millisecond)
		}()
	}
	wg.Wait()
}

func TestAssertContextCanceled(t *testing.T) {
	fn := func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}
	AssertContextCanceled(t, fn)
}

func TestAssertContextTimeout(t *testing.T) {
	fn := func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}
	AssertContextTimeout(t, fn)
}

func TestAssertErrorIs(t *testing.T) {
	err := fmt.Errorf("wrapped: %w", context.Canceled)
	AssertErrorIs(t, err, context.Canceled)
}

func TestAssertNoPanic(t *testing.T) {
	AssertNoPanic(t, func() {
		// normal code
	})
}

func TestAssertPanic(t *testing.T) {
	AssertPanic(t, func() {
		panic("expected panic")
	})
}

func TestGoLeakChecker_NoLeak(t *testing.T) {
	checker := NewGoLeakChecker()
	checker.Checkpoint("start")

	var wg sync.WaitGroup
	wg.Add(3)
	for i := 0; i < 3; i++ {
		go func() {
			defer wg.Done()
			time.Sleep(10 * time.Millisecond)
		}()
	}
	wg.Wait()

	checker.Checkpoint("after_goroutines")
	checker.Assert(t)
}

func TestGoLeakChecker_TooFewCheckpoints(t *testing.T) {
	// Assert with fewer than 2 checkpoints should fail the fake TestingT.
	checker := NewGoLeakChecker()
	checker.Checkpoint("only_one")

	ft := &fakeT{}
	checker.Assert(ft)
	if !ft.failed {
		t.Error("expected Assert to fail with fewer than 2 checkpoints")
	}
}
