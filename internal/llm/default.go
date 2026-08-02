package llm

import (
	"context"
	"errors"
)

// These types are the default (compile-time) implementations of the llm
// contracts. Every production interface in a package must
// carry a `var _ Interface = (*Impl)(nil)` default-implementation assertion.
// Real behavior is provided by the mock framework (internal/mock) and later
// production providers; these defaults are minimal placeholders that exist so
// the interfaces are always backed by a conforming type within this package.

// errDefaultModel reports that the default model is not backed by a real
// implementation.
var errDefaultModel = errors.New("llm: default model not implemented")

// defaultChatModel is a minimal BaseChatModel that always returns an error.
type defaultChatModel struct{}

func (defaultChatModel) Generate(_ context.Context, _ []Message, _ ...Option) (*Message, error) {
	return nil, errDefaultModel
}

func (defaultChatModel) Stream(_ context.Context, _ []Message, _ ...Option) (<-chan MessageChunk, error) {
	ch := make(chan MessageChunk)
	close(ch)
	return ch, errDefaultModel
}

var _ BaseChatModel = (*defaultChatModel)(nil)

// defaultProvider is a minimal ModelProvider backed by defaultChatModel.
type defaultProvider struct{}

func (defaultProvider) Name() string { return "default" }

func (defaultProvider) Build(_ context.Context, _ ModelConfig) (BaseChatModel, func(), error) {
	return defaultChatModel{}, func() {}, nil
}

func (defaultProvider) Models() []ModelInfo { return nil }

var _ ModelProvider = (*defaultProvider)(nil)
