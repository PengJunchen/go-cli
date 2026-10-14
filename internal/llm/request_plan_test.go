package llm

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMaterializeGenerationOptionsFreezesFiniteValues(t *testing.T) {
	parameters := map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}}
	definitions := []ToolDefinition{{Name: "search", Parameters: parameters}}
	applications := 0
	option := func(options *GenerationOptions) {
		applications++
		options.MaxTokens = intPointer(128)
		options.Tools = definitions
	}

	frozen, err := MaterializeGenerationOptions(option)
	require.NoError(t, err)
	require.Equal(t, 1, applications)

	parameters["after"] = true
	definitions[0].Name = "mutated"
	require.Equal(t, "search", frozen.Tools[0].Name)
	require.NotContains(t, frozen.Tools[0].Parameters, "after")

	canonical, err := OptionFromGenerationOptions(frozen)
	require.NoError(t, err)
	var dispatched GenerationOptions
	canonical(&dispatched)
	require.Equal(t, frozen, dispatched)
	require.Equal(t, 1, applications, "dispatch must not rerun the original option closure")

	dispatched.Tools[0].Name = "dispatch-mutation"
	require.Equal(t, "search", frozen.Tools[0].Name, "dispatch gets its own schema graph")
}

func TestMaterializeGenerationOptionsRejectsUnsupportedSchema(t *testing.T) {
	type unsupported struct {
		Handle chan int `json:"handle"`
	}
	_, err := MaterializeGenerationOptions(WithTools([]ToolDefinition{{Name: "bad", Parameters: unsupported{Handle: make(chan int)}}}))
	require.Error(t, err)
}

func TestOpenAICompatibleProtocolFramingEstimateSeparatesToolSchemas(t *testing.T) {
	provider := NewEinoProvider()
	withoutTools, err := provider.EstimateProtocolFraming([]Message{{Role: RoleUser, Content: "hello"}}, GenerationOptions{})
	require.NoError(t, err)
	withTools, err := provider.EstimateProtocolFraming([]Message{{Role: RoleUser, Content: "hello"}}, GenerationOptions{
		Tools: []ToolDefinition{{Name: "search", Parameters: map[string]any{"type": "object"}}},
	})
	require.NoError(t, err)
	require.Positive(t, withoutTools)
	require.Positive(t, withTools)
	require.NotEqual(t, withoutTools, withTools, "protocol framing accounts for tool-choice wrapper independently of schema JSON")
}
