package extension

import (
	"context"
	"errors"
)

// defaultConfigProvider is the default (compile-time) implementation of the
// ConfigProvider contract. SCAN-012 requires every production interface in a
// package to carry a `var _ Interface = (*Impl)(nil)` default-implementation
// assertion. Real behavior is provided by the mock framework
// (internal/mock) and later production providers.
type defaultConfigProvider struct{}

// errDefaultConfig reports that the default provider is not backed by a real
// implementation.
var errDefaultConfig = errors.New("extension: default config provider not implemented")

func (defaultConfigProvider) Name() string { return "default" }

func (defaultConfigProvider) Load(_ context.Context, _ string, _ any) error {
	return errDefaultConfig
}

func (defaultConfigProvider) Watch(_ context.Context, _ string) (<-chan ConfigChange, error) {
	return nil, errDefaultConfig
}

var _ ConfigProvider = (*defaultConfigProvider)(nil)
