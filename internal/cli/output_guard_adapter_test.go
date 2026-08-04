package cli

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/production"
)

// stubModel is a test double for llm.BaseChatModel.
type stubModel struct {
	resp *llm.Message
	err  error
}

func (m *stubModel) Generate(_ context.Context, _ []llm.Message, _ ...llm.Option) (*llm.Message, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.resp, nil
}

func (m *stubModel) Stream(_ context.Context, _ []llm.Message, _ ...llm.Option) (<-chan llm.MessageChunk, error) {
	return nil, nil
}

func TestOutputGuardModel_SanitizesPII(t *testing.T) {
	inner := &stubModel{
		resp: &llm.Message{Role: llm.RoleAssistant, Content: "Contact me at john@example.com"},
	}
	m := &outputGuardModel{inner: inner, guard: production.NewPIIOutputGuard()}

	resp, err := m.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.NotContains(t, resp.Content, "john@example.com")
}

func TestOutputGuardModel_PassthroughSafe(t *testing.T) {
	inner := &stubModel{
		resp: &llm.Message{Role: llm.RoleAssistant, Content: "Hello, world!"},
	}
	m := &outputGuardModel{inner: inner, guard: production.NewPIIOutputGuard()}

	resp, err := m.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "Hello, world!", resp.Content)
}

func TestOutputGuardModel_LengthTruncation(t *testing.T) {
	long := string(make([]byte, 200))
	inner := &stubModel{
		resp: &llm.Message{Role: llm.RoleAssistant, Content: long},
	}
	m := &outputGuardModel{inner: inner, guard: production.NewLengthGuard(100)}

	resp, err := m.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(resp.Content), 100)
}

func TestOutputGuardModel_NilGuard(t *testing.T) {
	inner := &stubModel{
		resp: &llm.Message{Role: llm.RoleAssistant, Content: "untouched"},
	}
	m := &outputGuardModel{inner: inner, guard: nil}

	resp, err := m.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "untouched", resp.Content)
}

func TestNewModelWrapper_AppliesBothWrappers(t *testing.T) {
	inner := &stubModel{
		resp: &llm.Message{Role: llm.RoleAssistant, Content: "Contact john@example.com"},
	}
	pw := production.NewProductionModelWrapper()
	guard := production.NewPIIOutputGuard()
	wrapper := newModelWrapper(pw, nil, guard)

	wrapped := wrapper(inner)
	require.NotNil(t, wrapped)

	baseModel, ok := wrapped.(llm.BaseChatModel)
	require.True(t, ok)

	resp, err := baseModel.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.NotContains(t, resp.Content, "john@example.com")
}

func TestNewModelWrapper_NilGuardReturnsProductionWrapped(t *testing.T) {
	inner := &stubModel{
		resp: &llm.Message{Role: llm.RoleAssistant, Content: "safe content"},
	}
	pw := production.NewProductionModelWrapper()
	wrapper := newModelWrapper(pw, nil, nil)

	wrapped := wrapper(inner)
	require.NotNil(t, wrapped)

	_, ok := wrapped.(llm.BaseChatModel)
	require.True(t, ok)
}

func TestNewModelWrapper_NonBaseChatModelReturnsUnchanged(t *testing.T) {
	pw := production.NewProductionModelWrapper()
	wrapper := newModelWrapper(pw, nil, nil)

	result := wrapper("not a model")
	assert.Equal(t, "not a model", result)
}
