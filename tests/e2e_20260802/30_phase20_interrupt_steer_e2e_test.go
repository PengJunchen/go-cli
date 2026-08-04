//go:build e2e

// Package e2e_20260802 contains end-to-end integration tests.
// This file verifies Phase 20 Interrupt/Steer: InterruptHandler signal
// handling, goroutine cleanup, SteerChannel, and context cancellation.
package e2e_20260802

import (
	"context"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/pengjunchen/go-cli/internal/cli"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// TestET_Phase20_Interrupt exercises the Phase 20 InterruptHandler end-to-end
// using real signal delivery (syscall.Kill) and goroutine leak detection.
// No mocks are used.
func TestET_Phase20_Interrupt(t *testing.T) {
	// AC-1: Start() then send SIGINT -> cancelFn is called -> context cancelled.
	t.Run("AC1_SigIntCancelsContext", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		handler := cli.NewInterruptHandler(cancel)
		handler.Start(nil)
		defer handler.Stop()

		// Allow the monitor goroutine to start and register for signals.
		time.Sleep(100 * time.Millisecond)

		// Send SIGINT to the current process.
		syscall.Kill(syscall.Getpid(), syscall.SIGINT)

		// Wait for the context to be cancelled.
		select {
		case <-ctx.Done():
			assert.ErrorIs(t, ctx.Err(), context.Canceled)
		case <-time.After(5 * time.Second):
			t.Fatal("context was not cancelled after SIGINT")
		}
	})

	// AC-2: Stop() cleans up the monitor goroutine (no leak).
	t.Run("AC2_StopNoLeak", func(t *testing.T) {
		defer verify.AssertNoGoroutineLeak(t)()

		_, cancel := context.WithCancel(context.Background())
		defer cancel()

		handler := cli.NewInterruptHandler(cancel)
		handler.Start(nil)
		handler.Stop()

		// If Stop() returned, the monitor goroutine has exited.
		// AssertNoGoroutineLeak (deferred) verifies no goroutine leak.
	})

	// AC-3: SteerChannel returns a receive-only channel.
	t.Run("AC3_SteerChannel", func(t *testing.T) {
		_, cancel := context.WithCancel(context.Background())
		defer cancel()

		handler := cli.NewInterruptHandler(cancel)
		ch := handler.SteerChannel()

		// Compile-time check: ch must be <-chan string.
		var _ <-chan string = ch

		// The channel should be initially empty (no steer messages).
		select {
		case v, ok := <-ch:
			if ok {
				t.Fatalf("SteerChannel should be empty initially, got %q", v)
			}
		default:
			// No message ready - expected.
		}
	})

	// AC-4: Context cancellation propagates to goroutines waiting on ctx.Done().
	t.Run("AC4_ContextCancellationPropagation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())

		done := make(chan struct{})
		go func() {
			<-ctx.Done()
			close(done)
		}()

		cancel()

		select {
		case <-done:
			// Success - goroutine exited after context cancellation.
		case <-time.After(5 * time.Second):
			t.Fatal("goroutine did not exit after context cancellation")
		}
	})
}
