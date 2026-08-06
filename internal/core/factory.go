package core

import (
	"context"
	"log/slog"
)

// SubAgentFactory creates named SubAgent instances from a SubAgentConfig. It is
// the creation seam the runtime uses to spawn delegated sub-tasks.
type SubAgentFactory interface {
	// Create builds a SubAgent for the given name (falling back to the config
	// name) and configuration. It may validate the config and return an error.
	Create(ctx context.Context, name string, config SubAgentConfig) (SubAgent, error)
}

// subAgentFactoryConfig holds the configurable knobs of a DefaultSubAgentFactory.
type subAgentFactoryConfig struct {
	runnerFactory SubAgentRunnerFactory
}

// SubAgentFactoryOption configures a DefaultSubAgentFactory at construction time.
type SubAgentFactoryOption func(*subAgentFactoryConfig)

// WithSubAgentRunnerFactory sets the pluggable harness (runner) constructor
// used by the factory when building sub-agents.
func WithSubAgentRunnerFactory(factory SubAgentRunnerFactory) SubAgentFactoryOption {
	return func(c *subAgentFactoryConfig) { c.runnerFactory = factory }
}

// DefaultSubAgentFactory is the default SubAgentFactory. It builds
// DefaultSubAgent instances, forwarding the pluggable runner factory when one
// was configured.
type DefaultSubAgentFactory struct {
	runnerFactory SubAgentRunnerFactory
}

var _ SubAgentFactory = (*DefaultSubAgentFactory)(nil)

// NewSubAgentFactory builds a DefaultSubAgentFactory from functional options.
func NewSubAgentFactory(opts ...SubAgentFactoryOption) SubAgentFactory {
	cfg := subAgentFactoryConfig{}
	for _, o := range opts {
		o(&cfg)
	}
	f := &DefaultSubAgentFactory{runnerFactory: cfg.runnerFactory}
	slog.Info("core.subagent.factory.new", "runner_set", cfg.runnerFactory != nil)
	return f
}

// Create builds a SubAgent for name/config, forwarding the configured runner
// factory so sub-agents share the same pluggable harness seam.
func (f *DefaultSubAgentFactory) Create(_ context.Context, name string, config SubAgentConfig) (SubAgent, error) {
	if name != "" {
		config.Name = name
	}
	opts := []SubAgentOption{}
	if f.runnerFactory != nil {
		opts = append(opts, WithSubAgentRunner(f.runnerFactory))
	}
	sub := NewDefaultSubAgent(config, opts...)
	slog.Info("core.subagent.factory.create", "name", sub.Name())
	return sub, nil
}
