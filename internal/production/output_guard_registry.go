package production

import (
	"log/slog"
	"sync"
)

// This file provides a process-wide registry for the active OutputGuard,
// mirroring internal/production/registry.go. Registering nil (or doing nothing)
// lazily selects a default chain of the built-in guards.

// defaultOutputGuardChain builds the default chain of built-in guards.
// The chain runs in order: code-injection and PII guards block outright;
// prompt-injection guard wraps flagged content as untrusted; length guard
// truncates last so any wrapping/tagging is accounted for before truncation.
func defaultOutputGuardChain() OutputGuard {
	return NewOutputGuardChain([]OutputGuard{
		NewCodeInjectionGuard(),
		NewPIIOutputGuard(),
		NewPromptInjectionGuard(),
		NewLengthGuard(8192),
	})
}

var (
	outputGuardMu      sync.RWMutex
	defaultOutputGuard OutputGuard
)

// RegisterOutputGuard sets the active OutputGuard. A nil value resets to a
// fresh default guard chain.
func RegisterOutputGuard(g OutputGuard) {
	outputGuardMu.Lock()
	defer outputGuardMu.Unlock()
	if g == nil {
		g = defaultOutputGuardChain()
	}
	slog.Info("production.register.output_guard", "name", g.Name())
	defaultOutputGuard = g
}

// GetOutputGuard returns the active OutputGuard, lazily defaulting to a fresh
// chain of built-in guards when none has been registered.
func GetOutputGuard() OutputGuard {
	outputGuardMu.RLock()
	defer outputGuardMu.RUnlock()
	if defaultOutputGuard == nil {
		return defaultOutputGuardChain()
	}
	return defaultOutputGuard
}
