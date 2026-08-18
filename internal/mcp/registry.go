package mcp

import (
	"context"
	"errors"
	"sync"
)

// ErrMCPClientNotFound is returned by GetClient when no client has been
// registered under the requested server name.
var ErrMCPClientNotFound = errors.New("mcp: client not found")

// MCPClientRegistry stores MCPClient instances by their logical server name,
// plus their associated HotReloaders. It is concurrency-safe: Register/Get and
// the hot-reloader methods may be called concurrently.
type MCPClientRegistry struct {
	mu           sync.RWMutex
	clients      map[string]MCPClient
	order        []string
	hotReloaders map[string]HotReloader
}

// NewMCPClientRegistry returns an empty, ready-to-use registry.
func NewMCPClientRegistry() *MCPClientRegistry {
	return &MCPClientRegistry{
		clients:      map[string]MCPClient{},
		hotReloaders: map[string]HotReloader{},
	}
}

// RegisterHotReloader stores the hot reloader under the given name. Registering
// a name a second time overwrites the previous reloader.
func (r *MCPClientRegistry) RegisterHotReloader(name string, hr HotReloader) error {
	if hr == nil {
		return errors.New("mcp: cannot register a nil hot reloader")
	}
	if name == "" {
		return errors.New("mcp: cannot register a hot reloader with an empty name")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.hotReloaders[name] = hr
	return nil
}

// HotReloader returns the hot reloader registered under name, or ok=false when
// none is registered.
func (r *MCPClientRegistry) HotReloader(name string) (HotReloader, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	hr, ok := r.hotReloaders[name]
	return hr, ok
}

// Register stores the client under the given server name. Registering a name a
// second time overwrites the previous client.
func (r *MCPClientRegistry) Register(name string, client MCPClient) error {
	if client == nil {
		return errors.New("mcp: cannot register a nil client")
	}
	if name == "" {
		return errors.New("mcp: cannot register a client with an empty name")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.clients[name]; !ok {
		r.order = append(r.order, name)
	}
	r.clients[name] = client
	return nil
}

// Get returns the client registered under name, or ErrMCPClientNotFound.
func (r *MCPClientRegistry) Get(name string) (MCPClient, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	client, ok := r.clients[name]
	if !ok {
		return nil, ErrMCPClientNotFound
	}
	return client, nil
}

// List returns all registered clients in registration order.
func (r *MCPClientRegistry) List(_ context.Context) []MCPClient {
	r.mu.RLock()
	defer r.mu.RUnlock()

	clients := make([]MCPClient, 0, len(r.order))
	for _, name := range r.order {
		clients = append(clients, r.clients[name])
	}
	return clients
}

// RegisterMCPClient is a convenience function that registers a client into a
// new registry, defaulting the name to the client's own Name() when name is
// empty. It exists to mirror the design's minimal registration API.
func RegisterMCPClient(reg *MCPClientRegistry, name string, client MCPClient) error {
	if reg == nil {
		return errors.New("mcp: cannot register a client into a nil registry")
	}
	if name == "" && client != nil {
		name = client.Name()
	}
	return reg.Register(name, client)
}
