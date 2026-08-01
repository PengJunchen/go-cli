package core

import (
	"log/slog"
	"sync"
)

// This file provides a minimal process-wide registry for the Phase 4 sub-agent
// factory, mirroring the internal/production registry pattern. It lets callers
// swap in a custom SubAgentFactory while keeping a safe nil-default otherwise.

var (
	subAgentRegistryMu     sync.RWMutex
	defaultSubAgentFactory SubAgentFactory
)

// RegisterSubAgentFactory sets the active SubAgentFactory. A nil value resets
// to a fresh DefaultSubAgentFactory.
func RegisterSubAgentFactory(f SubAgentFactory) {
	subAgentRegistryMu.Lock()
	defer subAgentRegistryMu.Unlock()
	if f == nil {
		f = NewSubAgentFactory()
	}
	slog.Info("core.register.subagent_factory", "name", factoryName(f))
	defaultSubAgentFactory = f
}

// GetSubAgentFactory returns the active SubAgentFactory, lazily defaulting to
// a fresh DefaultSubAgentFactory when none has been registered.
func GetSubAgentFactory() SubAgentFactory {
	subAgentRegistryMu.RLock()
	defer subAgentRegistryMu.RUnlock()
	if defaultSubAgentFactory == nil {
		return NewSubAgentFactory()
	}
	return defaultSubAgentFactory
}

// factoryName returns a stable identifier for a SubAgentFactory for logging
// purposes.
func factoryName(f SubAgentFactory) string {
	if n, ok := f.(interface{ Name() string }); ok {
		return n.Name()
	}
	return "default"
}
