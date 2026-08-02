package core

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetSubAgentFactoryLazyDefault(t *testing.T) {
	f := GetSubAgentFactory()
	require.NotNil(t, f)

	_, ok := f.(*DefaultSubAgentFactory)
	assert.True(t, ok, "GetSubAgentFactory should return a *DefaultSubAgentFactory when nothing has been registered")
}

func TestRegisterSubAgentFactoryRoundTrip(t *testing.T) {
	orig := GetSubAgentFactory()
	defer RegisterSubAgentFactory(orig)

	custom := subFactoryStub{}
	RegisterSubAgentFactory(custom)

	got := GetSubAgentFactory()
	require.NotNil(t, got)
	assert.Equal(t, custom, got, "GetSubAgentFactory should return the registered custom factory")
}

func TestRegisterSubAgentFactoryNilResets(t *testing.T) {
	orig := GetSubAgentFactory()
	defer RegisterSubAgentFactory(orig)

	RegisterSubAgentFactory(subFactoryStub{})
	assert.NotNil(t, GetSubAgentFactory())

	RegisterSubAgentFactory(nil)

	got := GetSubAgentFactory()
	require.NotNil(t, got)
	_, ok := got.(*DefaultSubAgentFactory)
	assert.True(t, ok, "registering nil should reset the factory to a fresh DefaultSubAgentFactory")
}

type namedFactoryStub struct {
	subFactoryStub
	name string
}

func (n namedFactoryStub) Name() string { return n.name }

func TestFactoryNameWithNamedFactory(t *testing.T) {
	f := namedFactoryStub{name: "custom-name"}
	assert.Equal(t, "custom-name", factoryName(f))
}

func TestFactoryNameWithUnnamedFactory(t *testing.T) {
	f := subFactoryStub{}
	assert.Equal(t, "default", factoryName(f))
}

var _ SubAgentFactory = (*namedFactoryStub)(nil)

func (n namedFactoryStub) Create(_ context.Context, _ string, _ SubAgentConfig) (SubAgent, error) {
	return newTestSubAgent(n.name), nil
}
