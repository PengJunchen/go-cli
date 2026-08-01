package core

import "errors"

// Sentinel errors reported by the default stub implementations. They indicate
// that a capability is not yet backed by a real implementation.
var (
	// errPluginsUnsupported reports that the default plugin loader cannot
	// load plugins yet.
	errPluginsUnsupported = errors.New("core: plugin loading not implemented")
	// errToolUnknown reports that a tool is not registered in the default
	// tool registry.
	errToolUnknown = errors.New("core: tool not found")
	// errModelUnsupported reports that the default model provider cannot
	// build a model yet.
	errModelUnsupported = errors.New("core: model provider not implemented")
	// errConfigUnsupported reports that the default config provider cannot
	// load configuration yet.
	errConfigUnsupported = errors.New("core: config provider not implemented")
)
