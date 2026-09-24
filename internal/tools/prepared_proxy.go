package tools

import (
	"context"
	"errors"
	"fmt"
)

type preparedToolDefinition struct {
	registry *MiddlewareToolRegistry
	def      ToolDefinition
	fallback ToolDefinition
}

func newProtectedToolDefinition(registry *MiddlewareToolRegistry, def ToolDefinition, wrappers []ToolExecutorWrapper) (ToolDefinition, error) {
	if def == nil {
		return nil, errors.New("tools: cannot protect a nil tool definition")
	}
	var fallback ToolDefinition = def
	if len(wrappers) > 0 {
		fallback = &wrappedToolDef{def: def, wrappers: append([]ToolExecutorWrapper(nil), wrappers...)}
	}
	core := &preparedToolDefinition{registry: registry, def: def, fallback: fallback}

	var capabilities uint8
	var parameterized parameterizedCapability
	if definition, ok := def.(Parameterized); ok {
		schema, err := freezeToolSchema(definition.Parameters())
		if err != nil {
			return nil, fmt.Errorf("tools: snapshot exposed schema for %q: %w", def.Name(), err)
		}
		parameterized.schema = schema
		capabilities |= capabilityParameterized
	}
	var guidelines promptGuidelineCapability
	if definition, ok := def.(PromptGuideliner); ok {
		guidelines.guidelines = append([]string(nil), definition.PromptGuidelines()...)
		capabilities |= capabilityPromptGuidelines
	}
	var streaming streamingCapability
	if definition, ok := def.(StreamingBashTool); ok {
		streaming = streamingCapability{core: core, definition: definition}
		capabilities |= capabilityStreaming
	}
	var topTools topToolsCapability
	if definition, ok := def.(TopToolsProvider); ok {
		topTools = topToolsCapability{registry: registry, provider: definition}
		capabilities |= capabilityTopTools
	}

	switch capabilities {
	case 0:
		return core, nil
	case capabilityParameterized:
		return &protectedParameterized{preparedToolDefinition: core, parameterizedCapability: parameterized}, nil
	case capabilityPromptGuidelines:
		return &protectedPromptGuideline{preparedToolDefinition: core, promptGuidelineCapability: guidelines}, nil
	case capabilityParameterized | capabilityPromptGuidelines:
		return &protectedParameterizedPromptGuideline{core, parameterized, guidelines}, nil
	case capabilityStreaming:
		return &protectedStreaming{preparedToolDefinition: core, streamingCapability: streaming}, nil
	case capabilityParameterized | capabilityStreaming:
		return &protectedParameterizedStreaming{core, parameterized, streaming}, nil
	case capabilityPromptGuidelines | capabilityStreaming:
		return &protectedPromptGuidelineStreaming{core, guidelines, streaming}, nil
	case capabilityParameterized | capabilityPromptGuidelines | capabilityStreaming:
		return &protectedParameterizedPromptGuidelineStreaming{core, parameterized, guidelines, streaming}, nil
	case capabilityTopTools:
		return &protectedTopTools{preparedToolDefinition: core, topToolsCapability: topTools}, nil
	case capabilityParameterized | capabilityTopTools:
		return &protectedParameterizedTopTools{core, parameterized, topTools}, nil
	case capabilityPromptGuidelines | capabilityTopTools:
		return &protectedPromptGuidelineTopTools{core, guidelines, topTools}, nil
	case capabilityParameterized | capabilityPromptGuidelines | capabilityTopTools:
		return &protectedParameterizedPromptGuidelineTopTools{core, parameterized, guidelines, topTools}, nil
	case capabilityStreaming | capabilityTopTools:
		return &protectedStreamingTopTools{core, streaming, topTools}, nil
	case capabilityParameterized | capabilityStreaming | capabilityTopTools:
		return &protectedParameterizedStreamingTopTools{core, parameterized, streaming, topTools}, nil
	case capabilityPromptGuidelines | capabilityStreaming | capabilityTopTools:
		return &protectedPromptGuidelineStreamingTopTools{core, guidelines, streaming, topTools}, nil
	case capabilityParameterized | capabilityPromptGuidelines | capabilityStreaming | capabilityTopTools:
		return &protectedAllCapabilities{core, parameterized, guidelines, streaming, topTools}, nil
	default:
		return nil, errors.New("tools: unsupported proxy capability set")
	}
}

const (
	capabilityParameterized uint8 = 1 << iota
	capabilityPromptGuidelines
	capabilityStreaming
	capabilityTopTools
)

func (d *preparedToolDefinition) Name() string        { return d.def.Name() }
func (d *preparedToolDefinition) Description() string { return d.def.Description() }

func (d *preparedToolDefinition) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	if call.Name != d.def.Name() {
		return nil, ErrPreparedProxyNameMismatch
	}
	enabled, generations, fence := d.registry.preparedExecutionState()
	if enabled {
		invocation, err := d.registry.PrepareToolCall(ctx, call)
		if err != nil {
			return nil, err
		}
		return invocation.Dispatch(ctx)
	}
	if fence != nil {
		if err := fence.commit(generations, func() {}); err != nil {
			return nil, err
		}
	}
	return d.fallback.Execute(ctx, call)
}

type parameterizedCapability struct{ schema ToolSchemaSnapshot }

func (c parameterizedCapability) Parameters() any { return c.schema.Schema() }

type promptGuidelineCapability struct{ guidelines []string }

func (c promptGuidelineCapability) PromptGuidelines() []string {
	return append([]string(nil), c.guidelines...)
}

type streamingCapability struct {
	core       *preparedToolDefinition
	definition StreamingBashTool
}

func (c streamingCapability) ExecuteStreaming(ctx context.Context, call ToolCall, sink StreamSink) (*ToolResult, error) {
	if c.core == nil || c.definition == nil {
		return nil, ErrPreparedInvocationUnavailable
	}
	if call.Name != c.core.def.Name() {
		return nil, ErrPreparedProxyNameMismatch
	}
	enabled, generations, fence := c.core.registry.preparedExecutionState()
	if !enabled {
		if fence != nil {
			if err := fence.commit(generations, func() {}); err != nil {
				return nil, err
			}
		}
		// Legacy execution wrappers only wrap Execute. Keep that path instead of
		// calling a raw streaming executor outside those wrappers.
		return c.core.Execute(ctx, call)
	}
	invocation, err := c.core.registry.PrepareToolCall(ctx, call)
	if err != nil {
		return nil, err
	}
	streaming, ok := invocation.(PreparedStreamingInvocation)
	if !ok {
		return nil, ErrPreparedInvocationUnavailable
	}
	return streaming.DispatchStreaming(ctx, sink)
}

type topToolsCapability struct {
	registry *MiddlewareToolRegistry
	provider TopToolsProvider
}

func (c topToolsCapability) TopTools(ctx context.Context, query string, k int) ([]ToolDefinition, error) {
	if c.registry == nil || c.provider == nil {
		return nil, ErrPreparedInvocationUnavailable
	}
	defs, err := c.provider.TopTools(ctx, query, k)
	if err != nil {
		return nil, err
	}
	protected := make([]ToolDefinition, 0, len(defs))
	for _, def := range defs {
		if def == nil {
			return nil, errors.New("tools: top-tools provider returned a nil definition")
		}
		wrapped, err := c.registry.Get(ctx, def.Name())
		if err != nil {
			return nil, fmt.Errorf("tools: protect top tool %q: %w", def.Name(), err)
		}
		protected = append(protected, wrapped)
	}
	return protected, nil
}

type protectedParameterized struct {
	*preparedToolDefinition
	parameterizedCapability
}
type protectedPromptGuideline struct {
	*preparedToolDefinition
	promptGuidelineCapability
}
type protectedParameterizedPromptGuideline struct {
	*preparedToolDefinition
	parameterizedCapability
	promptGuidelineCapability
}
type protectedStreaming struct {
	*preparedToolDefinition
	streamingCapability
}
type protectedParameterizedStreaming struct {
	*preparedToolDefinition
	parameterizedCapability
	streamingCapability
}
type protectedPromptGuidelineStreaming struct {
	*preparedToolDefinition
	promptGuidelineCapability
	streamingCapability
}
type protectedParameterizedPromptGuidelineStreaming struct {
	*preparedToolDefinition
	parameterizedCapability
	promptGuidelineCapability
	streamingCapability
}
type protectedTopTools struct {
	*preparedToolDefinition
	topToolsCapability
}
type protectedParameterizedTopTools struct {
	*preparedToolDefinition
	parameterizedCapability
	topToolsCapability
}
type protectedPromptGuidelineTopTools struct {
	*preparedToolDefinition
	promptGuidelineCapability
	topToolsCapability
}
type protectedParameterizedPromptGuidelineTopTools struct {
	*preparedToolDefinition
	parameterizedCapability
	promptGuidelineCapability
	topToolsCapability
}
type protectedStreamingTopTools struct {
	*preparedToolDefinition
	streamingCapability
	topToolsCapability
}
type protectedParameterizedStreamingTopTools struct {
	*preparedToolDefinition
	parameterizedCapability
	streamingCapability
	topToolsCapability
}
type protectedPromptGuidelineStreamingTopTools struct {
	*preparedToolDefinition
	promptGuidelineCapability
	streamingCapability
	topToolsCapability
}
type protectedAllCapabilities struct {
	*preparedToolDefinition
	parameterizedCapability
	promptGuidelineCapability
	streamingCapability
	topToolsCapability
}

func (r *MiddlewareToolRegistry) preparedExecutionState() (bool, DispatchFenceSnapshot, *DispatchFence) {
	state, fence := r.preparedToolPipeline()
	if state == nil || fence == nil {
		return false, DispatchFenceSnapshot{}, nil
	}
	var enabled bool
	var generations DispatchFenceSnapshot
	if err := fence.read(func(current DispatchFenceSnapshot) {
		generations = current
		pipeline := state.snapshot()
		enabled = pipeline.policyConfigured || pipeline.stagesConfigured
	}); err != nil {
		return true, DispatchFenceSnapshot{}, fence
	}
	return enabled, generations, fence
}
