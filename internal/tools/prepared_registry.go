package tools

import (
	"context"
	"errors"
	"fmt"
)

var _ ToolCallPreparer = (*MiddlewareToolRegistry)(nil)

func (*MiddlewareToolRegistry) toolCallPreparerCapability() {}

// PrepareToolCall pins one registered definition, its schema, trusted metadata,
// and the configured execution stages before asking policy to authorize it.
func (r *MiddlewareToolRegistry) PrepareToolCall(ctx context.Context, call ToolCall) (PreparedInvocation, error) {
	if err := validatePreparedContext(ctx); err != nil {
		return nil, err
	}
	if call.Name == "" {
		return nil, errors.New("tools: prepared call has no tool name")
	}

	snapshot, err := r.toolRegistrationSnapshot(ctx, call.Name)
	if err != nil {
		return nil, err
	}
	if !snapshot.pipeline.policyConfigured || !snapshot.pipeline.stagesConfigured || snapshot.pipeline.policy == nil {
		return nil, ErrPreparedToolPolicyUnavailable
	}
	if registryHasOpaqueLegacyWrapper(r) {
		return nil, ErrPreparedOpaqueWrapperUnsupported
	}
	if snapshot.definition == nil || snapshot.identity.ToolName() != call.Name {
		return nil, ErrPreparedRegistrationUnavailable
	}
	if mutationToolNames[call.Name] {
		return nil, ErrPreparedMutationQueueUnavailable
	}

	normalized := call
	for _, preparer := range snapshot.pipeline.stages.ArgumentPreparers {
		if preparer == nil {
			continue
		}
		if _, ok := preparer.(*PathNormalizer); !ok {
			return nil, ErrPreparedNormalizerUnsupported
		}
		normalized, err = preparer.PrepareArguments(ctx, normalized)
		if err != nil {
			return nil, fmt.Errorf("tools: normalize prepared call %q: %w", call.Name, err)
		}
		if normalized.Name != call.Name || normalized.ID != call.ID {
			return nil, ErrPreparedCallMutation
		}
	}
	if snapshot.schema.Present() {
		schema := snapshot.schema.Schema()
		if err := validateCapturedSchema(normalized.Args, schema); err != nil {
			return nil, fmt.Errorf("tools: validate prepared call %q: %w", call.Name, err)
		}
	}
	frozenCall, err := freezeToolCall(normalized)
	if err != nil {
		return nil, err
	}
	fence := r.DispatchFence()
	if fence == nil {
		return nil, ErrDispatchFenceUnavailable
	}
	decision, err := snapshot.pipeline.policy(ctx, PreparedPolicyRequest{
		Call:        frozenCall,
		Identity:    snapshot.identity,
		Effects:     newTrustedEffectSet(snapshot.effects.Values()...),
		Schema:      snapshot.schema,
		Generations: snapshot.generations,
		Fence:       fence,
	})
	if err != nil {
		return nil, err
	}
	if !decision.Allowed {
		return nil, ErrPreparedToolPolicyDenied
	}
	if decision.Generations != snapshot.generations {
		return nil, ErrDispatchFenceChanged
	}

	executor := preparedExecutor(snapshot.definition, snapshot.pipeline.stages.ExecutionWrappers, frozenCall)
	config := preparedInvocationConfig{
		fence:       fence,
		generations: decision.Generations,
		identity:    snapshot.identity,
		effects:     snapshot.effects,
		schema:      snapshot.schema,
		call:        frozenCall,
		execute:     executor,
	}
	if streaming, ok := snapshot.definition.(StreamingBashTool); ok {
		streamExecutor := preparedStreamingExecutor(streaming, snapshot.pipeline.stages.ExecutionWrappers, frozenCall)
		return newPreparedStreamingInvocation(config, streamExecutor), nil
	}
	return newPreparedInvocation(config), nil
}

func registryHasOpaqueLegacyWrapper(reg ToolRegistry) bool {
	switch current := reg.(type) {
	case *MiddlewareToolRegistry:
		return len(current.wrappers) > 0 || registryHasOpaqueLegacyWrapper(current.inner)
	case *DeferredToolRegistryAdapter:
		return registryHasOpaqueLegacyWrapper(current.underlying)
	default:
		return false
	}
}

func validateCapturedSchema(args map[string]any, schema any) error {
	schemaMap, ok := schema.(map[string]any)
	if !ok {
		return nil
	}
	if required, ok := schemaMap["required"].([]string); ok {
		for _, name := range required {
			if _, exists := args[name]; !exists {
				return fmt.Errorf("missing required parameter: %s", name)
			}
		}
	}
	if err := validateBasicSchema(args, schema); err != nil {
		return err
	}
	return nil
}

func preparedExecutor(def ToolDefinition, wrappers []ToolExecutorWrapper, frozen FrozenToolCall) func(context.Context, ToolCall) (*ToolResult, error) {
	canonical := frozen.ToolCall()
	executor := func(ctx context.Context, call ToolCall) (*ToolResult, error) {
		if !matchesFrozenCall(call, frozen) {
			return nil, ErrPreparedCallMutation
		}
		return def.Execute(ctx, canonical)
	}
	for i := len(wrappers) - 1; i >= 0; i-- {
		if wrappers[i] != nil {
			executor = wrappers[i](executor)
		}
	}
	return func(ctx context.Context, _ ToolCall) (*ToolResult, error) {
		return executor(ctx, frozen.ToolCall())
	}
}

func preparedStreamingExecutor(def StreamingBashTool, wrappers []ToolExecutorWrapper, frozen FrozenToolCall) func(context.Context, ToolCall, StreamSink) (*ToolResult, error) {
	return func(ctx context.Context, _ ToolCall, sink StreamSink) (*ToolResult, error) {
		canonical := frozen.ToolCall()
		executor := func(ctx context.Context, call ToolCall) (*ToolResult, error) {
			if !matchesFrozenCall(call, frozen) {
				return nil, ErrPreparedCallMutation
			}
			return def.ExecuteStreaming(ctx, canonical, sink)
		}
		for i := len(wrappers) - 1; i >= 0; i-- {
			if wrappers[i] != nil {
				executor = wrappers[i](executor)
			}
		}
		return executor(ctx, frozen.ToolCall())
	}
}

func matchesFrozenCall(call ToolCall, frozen FrozenToolCall) bool {
	if call.ID != frozen.id || call.Name != frozen.name {
		return false
	}
	candidate, err := freezeToolCall(call)
	return err == nil && candidate.digest == frozen.digest
}
