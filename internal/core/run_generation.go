package core

import (
	"context"
	"errors"

	"github.com/pengjunchen/go-cli/internal/tools"
)

// RunGenerationProvider pins a complete runtime after run-slot admission.
// Implementations return a derived context carrying the immutable selected plan.
type RunGenerationProvider interface {
	AcquireRun(context.Context) (context.Context, RunGenerationLease, error)
}

// RunGenerationLease retains resources until the run owner is quiescent.
type RunGenerationLease interface{ Release() }

// RunGenerationMetadata records runtime identity without retaining runtime resources.
type RunGenerationMetadata struct {
	GenerationID     string `json:"generation_id"`
	PromptDigest     string `json:"prompt_digest"`
	CapabilityDigest string `json:"capability_digest"`
	PolicyEpoch      uint64 `json:"policy_epoch"`
	TrustEpoch       uint64 `json:"trust_epoch"`
}

type runGenerationMetadataKey struct{}
type runHookChainKey struct{}
type runToolRegistryKey struct{}
type runCapabilityGuardKey struct{}
type runContextToolInterceptorKey struct{}
type runContextToolInterceptorValue struct{ interceptor ContextToolInterceptor }

// WithRunContextToolInterceptor pins the generation pre-tool hook, including explicit disable.
func WithRunContextToolInterceptor(ctx context.Context, interceptor ContextToolInterceptor) context.Context {
	return context.WithValue(ctx, runContextToolInterceptorKey{}, runContextToolInterceptorValue{interceptor})
}

// RunCapabilityGuard checks current policy/trust at callback admission and completion.
type RunCapabilityGuard interface {
	BeginCall(context.Context, string) (RunCapabilityInvocation, error)
	Check() error
}

// RunCapabilityInvocation owns an independent resource reference for one callback.
type RunCapabilityInvocation interface {
	Context() context.Context
	Complete() error
	Release()
}

// WithRunCapabilityGuard installs the latest-policy guard for a pinned run.
func WithRunCapabilityGuard(ctx context.Context, guard RunCapabilityGuard) context.Context {
	return context.WithValue(ctx, runCapabilityGuardKey{}, guard)
}

// CheckRunGeneration rechecks authorization without comparing against the current generation.
func CheckRunGeneration(ctx context.Context) error {
	if ctx != nil {
		if guard, ok := ctx.Value(runCapabilityGuardKey{}).(RunCapabilityGuard); ok {
			return guard.Check()
		}
	}
	return nil
}

func beginRunCapability(ctx context.Context, id string) (context.Context, func() error, error) {
	if guard, ok := ctx.Value(runCapabilityGuardKey{}).(RunCapabilityGuard); ok {
		invocation, err := guard.BeginCall(ctx, id)
		if err != nil {
			return ctx, nil, err
		}
		return invocation.Context(), func() error { defer invocation.Release(); return invocation.Complete() }, nil
	}
	return ctx, func() error { return nil }, nil
}

// WithRunToolRegistry pins the concrete prepared-dispatch registry for this run.
func WithRunToolRegistry(ctx context.Context, registry tools.ToolRegistry) context.Context {
	return context.WithValue(ctx, runToolRegistryKey{}, registry)
}

func runToolRegistry(ctx context.Context, fallback tools.ToolRegistry) tools.ToolRegistry {
	if ctx != nil {
		if registry, ok := ctx.Value(runToolRegistryKey{}).(tools.ToolRegistry); ok {
			return registry
		}
	}
	return fallback
}

// WithRunGenerationMetadata attaches the selected generation's audit identity.
func WithRunGenerationMetadata(ctx context.Context, metadata RunGenerationMetadata) context.Context {
	return context.WithValue(ctx, runGenerationMetadataKey{}, metadata)
}

// RunGenerationMetadataFromContext returns the selected generation, if enabled.
func RunGenerationMetadataFromContext(ctx context.Context) (RunGenerationMetadata, bool) {
	if ctx == nil {
		return RunGenerationMetadata{}, false
	}
	metadata, ok := ctx.Value(runGenerationMetadataKey{}).(RunGenerationMetadata)
	return metadata, ok
}

// WithRunHookChain pins the hook plan used by middleware and the turn runner.
func WithRunHookChain(ctx context.Context, chain *HookChain) context.Context {
	if chain == nil {
		chain = NewHookChain()
	}
	return context.WithValue(ctx, runHookChainKey{}, chain)
}

func runHookChain(ctx context.Context, fallback *HookChain) *HookChain {
	if ctx != nil {
		if chain, ok := ctx.Value(runHookChainKey{}).(*HookChain); ok {
			return chain
		}
	}
	return fallback
}

// WithRunGenerationProvider enables generation pinning on the harness.
func WithRunGenerationProvider(provider RunGenerationProvider) HarnessOption {
	return func(c *harnessConfig) { c.generationProvider = provider }
}

// SetRunGenerationProvider enables the same pinning contract for interactive turns.
func (r *EinoTurnRunner) SetRunGenerationProvider(provider RunGenerationProvider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.generationProvider = provider
}

func releaseGeneration(lease RunGenerationLease) (err error) {
	if lease == nil {
		return nil
	}
	defer func() {
		if recover() != nil {
			err = errors.New("core: generation lease release panicked")
		}
	}()
	lease.Release()
	return nil
}

func runGenerationMetadataPointer(ctx context.Context) *RunGenerationMetadata {
	metadata, ok := RunGenerationMetadataFromContext(ctx)
	if !ok {
		return nil
	}
	return &metadata
}
