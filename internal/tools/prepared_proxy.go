package tools

import (
	"context"
	"errors"
	"fmt"
)

type preparedToolDefinition struct {
	registry    *MiddlewareToolRegistry
	def         ToolDefinition
	description string
	pin         protectedRegistrationPin
}

type protectedRegistrationPin struct {
	identity           TrustedToolIdentity
	registryGeneration uint64
}

func newProtectedToolDefinition(registry *MiddlewareToolRegistry, snapshot registeredToolSnapshot) (ToolDefinition, error) {
	def := snapshot.definition
	if def == nil {
		return nil, errors.New("tools: cannot protect a nil tool definition")
	}
	core := &preparedToolDefinition{
		registry:    registry,
		def:         def,
		description: snapshot.description,
		pin: protectedRegistrationPin{
			identity:           snapshot.identity,
			registryGeneration: snapshot.generations.Registry,
		},
	}

	var capabilities uint8
	var parameterized parameterizedCapability
	if snapshot.hasParameters {
		parameterized.schema = snapshot.schema
		capabilities |= capabilityParameterized
	}
	var guidelines promptGuidelineCapability
	if snapshot.hasPromptGuidelines {
		guidelines.guidelines = append([]string(nil), snapshot.promptGuidelines...)
		capabilities |= capabilityPromptGuidelines
	}
	var streaming streamingCapability
	if snapshot.hasStreaming {
		definition, ok := def.(StreamingBashTool)
		if !ok {
			return nil, ErrPreparedRegistrationUnavailable
		}
		streaming = streamingCapability{core: core, definition: definition}
		capabilities |= capabilityStreaming
	}
	var topTools topToolsCapability
	if snapshot.hasTopTools {
		definition, ok := def.(TopToolsProvider)
		if !ok {
			return nil, ErrPreparedRegistrationUnavailable
		}
		topTools = topToolsCapability{registry: registry, provider: definition, pin: core.pin}
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

func (d *preparedToolDefinition) Name() string        { return d.pin.identity.ToolName() }
func (d *preparedToolDefinition) Description() string { return d.description }

func (d *preparedToolDefinition) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	if call.Name != d.pin.identity.ToolName() {
		return nil, ErrPreparedProxyNameMismatch
	}
	if d.registry == nil {
		return nil, ErrPreparedInvocationUnavailable
	}
	invocation, err := d.registry.prepareToolCall(ctx, call, &d.pin)
	if err != nil {
		return nil, err
	}
	return invocation.Dispatch(ctx)
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
	if call.Name != c.core.pin.identity.ToolName() {
		return nil, ErrPreparedProxyNameMismatch
	}
	invocation, err := c.core.registry.prepareToolCall(ctx, call, &c.core.pin)
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
	pin      protectedRegistrationPin
}

func (c topToolsCapability) TopTools(ctx context.Context, query string, k int) ([]ToolDefinition, error) {
	if c.registry == nil || c.provider == nil {
		return nil, ErrPreparedInvocationUnavailable
	}
	if err := c.registry.validateProxyPin(ctx, c.pin); err != nil {
		return nil, err
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
