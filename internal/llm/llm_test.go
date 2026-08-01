package llm

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWithTemperatureOption(t *testing.T) {
	opts := &GenerationOptions{}
	WithTemperature(0.7)(opts)
	require.NotNil(t, opts.Temperature)
	assert.Equal(t, 0.7, *opts.Temperature)
}

func TestWithMaxTokensOption(t *testing.T) {
	opts := &GenerationOptions{}
	WithMaxTokens(256)(opts)
	require.NotNil(t, opts.MaxTokens)
	assert.Equal(t, 256, *opts.MaxTokens)
}

func TestWithStopStringsOption(t *testing.T) {
	opts := &GenerationOptions{}
	WithStopStrings("END", "STOP")(opts)
	assert.Equal(t, []string{"END", "STOP"}, opts.StopStrings)
}

func TestOptionsComposeTogether(t *testing.T) {
	opts := &GenerationOptions{}
	WithTemperature(0.1)(opts)
	WithMaxTokens(10)(opts)
	WithStopStrings("x")(opts)

	require.NotNil(t, opts.Temperature)
	assert.Equal(t, 0.1, *opts.Temperature)
	require.NotNil(t, opts.MaxTokens)
	assert.Equal(t, 10, *opts.MaxTokens)
	assert.Equal(t, []string{"x"}, opts.StopStrings)
}

func TestDefaultChatModelGenerateError(t *testing.T) {
	m := defaultChatModel{}
	_, err := m.Generate(context.Background(), nil)
	assert.ErrorIs(t, err, errDefaultModel)
}

func TestDefaultChatModelStreamReturnsClosedChannelAndError(t *testing.T) {
	m := defaultChatModel{}
	ch, err := m.Stream(context.Background(), nil)
	require.ErrorIs(t, err, errDefaultModel)
	require.NotNil(t, ch)
	_, ok := <-ch
	assert.False(t, ok, "stream channel must be closed")
}

func TestDefaultProviderName(t *testing.T) {
	p := defaultProvider{}
	assert.Equal(t, "default", p.Name())
}

func TestDefaultProviderBuildReturnsDefaultModel(t *testing.T) {
	p := defaultProvider{}
	m, cleanup, err := p.Build(context.Background(), ModelConfig{Model: "x"})
	require.NoError(t, err)
	require.NotNil(t, m)
	require.NotNil(t, cleanup)

	_, gerr := m.Generate(context.Background(), nil)
	assert.ErrorIs(t, gerr, errDefaultModel)

	assert.NotPanics(t, func() { cleanup() })
}

func TestDefaultProviderModelsEmpty(t *testing.T) {
	p := defaultProvider{}
	assert.Nil(t, p.Models())
}

func TestRoleConstants(t *testing.T) {
	assert.Equal(t, Role("user"), RoleUser)
	assert.Equal(t, Role("assistant"), RoleAssistant)
	assert.Equal(t, Role("tool"), RoleTool)
	assert.Equal(t, Role("system"), RoleSystem)
}

func TestCompileTimeInterfaceAssertions(t *testing.T) {
	var _ BaseChatModel = (*defaultChatModel)(nil)
	var _ ModelProvider = (*defaultProvider)(nil)
}
