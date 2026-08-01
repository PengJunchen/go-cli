package skill

import (
	"log/slog"
	"sync"
)

// This file provides a minimal process-wide registry for the skill system. It
// lets callers swap in custom SkillLoader / SkillRegistry implementations while
// keeping a safe nil-default otherwise, mirroring the process registry pattern
// used by the production package.

var (
	registryMu sync.RWMutex

	defaultSkillLoader   SkillLoader
	defaultSkillRegistry SkillRegistry
)

// RegisterSkillLoader sets the active SkillLoader. A nil value resets to a
// fresh YAMLSkillLoader.
func RegisterSkillLoader(l SkillLoader) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if l == nil {
		l = NewYAMLSkillLoader()
	}
	slog.Info("skill.register_loader", "type", loaderTypeName(l))
	defaultSkillLoader = l
}

// GetSkillLoader returns the active SkillLoader, lazily defaulting to a
// YAMLSkillLoader when none has been registered.
func GetSkillLoader() SkillLoader {
	registryMu.RLock()
	defer registryMu.RUnlock()
	if defaultSkillLoader == nil {
		return NewYAMLSkillLoader()
	}
	return defaultSkillLoader
}

// RegisterSkillRegistry sets the active SkillRegistry. A nil value resets to a
// fresh DefaultSkillRegistry.
func RegisterSkillRegistry(r SkillRegistry) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if r == nil {
		r = NewDefaultSkillRegistry()
	}
	slog.Info("skill.register_registry", "type", registryTypeName(r))
	defaultSkillRegistry = r
}

// GetSkillRegistry returns the active SkillRegistry, lazily defaulting to a
// fresh DefaultSkillRegistry when none has been registered.
func GetSkillRegistry() SkillRegistry {
	registryMu.RLock()
	defer registryMu.RUnlock()
	if defaultSkillRegistry == nil {
		return NewDefaultSkillRegistry()
	}
	return defaultSkillRegistry
}

// loaderTypeName returns a stable identifier for a loader, defaulting to the
// standard YAML loader for the concrete default implementation.
func loaderTypeName(l SkillLoader) string {
	if _, ok := l.(*YAMLSkillLoader); ok {
		return "yaml"
	}
	return "custom"
}

// registryTypeName returns a stable identifier for a registry, defaulting to
// the standard in-memory registry for the concrete default implementation.
func registryTypeName(r SkillRegistry) string {
	if _, ok := r.(*DefaultSkillRegistry); ok {
		return "default"
	}
	return "custom"
}
