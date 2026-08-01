package mock

import (
	"context"
	"sync"

	"github.com/pengjunchen/go-cli/internal/extension"
)

// This file provides mock implementations of the Phase 4 Extension system
// contracts for tests: MockExtension, MockHook, MockPluginLoader and the
// middleware mocks. They record invocations so tests can assert on lifecycle
// and wrapping behavior.

// MockExtension records Init/Shutdown calls for lifecycle assertions.
type MockExtension struct {
	mu          sync.Mutex
	name        string
	initCalled  bool
	shutdownCnt int
	initErr     error
	shutdownErr error
	registry    extension.ExtensionRegistry
}

var _ extension.Extension = (*MockExtension)(nil)

// NewMockExtension creates a MockExtension with the given name.
func NewMockExtension(name string) *MockExtension {
	return &MockExtension{name: name}
}

// Name returns the mock extension name.
func (m *MockExtension) Name() string { return m.name }

// SetInitError forces Init to fail.
func (m *MockExtension) SetInitError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.initErr = err
}

// SetShutdownError forces Shutdown to fail.
func (m *MockExtension) SetShutdownError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.shutdownErr = err
}

// Init records the call and stores the registry it received.
func (m *MockExtension) Init(_ context.Context, reg extension.ExtensionRegistry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.initCalled = true
	m.registry = reg
	return m.initErr
}

// Shutdown records the call.
func (m *MockExtension) Shutdown(_ context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.shutdownCnt++
	return m.shutdownErr
}

// InitCalled reports whether Init ran.
func (m *MockExtension) InitCalled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.initCalled
}

// ShutdownCount returns how many times Shutdown ran.
func (m *MockExtension) ShutdownCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.shutdownCnt
}

// Registry returns the registry captured at Init time.
func (m *MockExtension) Registry() extension.ExtensionRegistry {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.registry
}

// MockHook records Handle calls.
type MockHook struct {
	mu     sync.Mutex
	name   string
	calls  int
	result extension.HookResult
}

var _ extension.Hook = (*MockHook)(nil)

// NewMockHook creates a MockHook that always returns HookActionPass by default.
func NewMockHook(name string) *MockHook {
	return &MockHook{name: name, result: extension.HookResult{Action: extension.HookActionPass}}
}

// Name returns the mock hook name.
func (m *MockHook) Name() string { return m.name }

// SetResult sets the result returned on each Handle call.
func (m *MockHook) SetResult(r extension.HookResult) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.result = r
}

// Handle records the call and returns the configured result.
func (m *MockHook) Handle(_ context.Context, _ extension.HookEvent) extension.HookResult {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	return m.result
}

// CallCount returns how many times Handle ran.
func (m *MockHook) CallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// MockPluginLoader returns predefined extensions for a path.
type MockPluginLoader struct {
	mu         sync.Mutex
	name       string
	results    map[string][]extension.Extension
	loadErr    map[string]error
	loadedPath string
}

var _ extension.PluginLoader = (*MockPluginLoader)(nil)

// NewMockPluginLoader creates a MockPluginLoader with the given name.
func NewMockPluginLoader(name string) *MockPluginLoader {
	return &MockPluginLoader{
		name:    name,
		results: make(map[string][]extension.Extension),
		loadErr: make(map[string]error),
	}
}

// Name returns the mock loader name.
func (m *MockPluginLoader) Name() string { return m.name }

// SetResult presets the extensions returned for the given path.
func (m *MockPluginLoader) SetResult(path string, exts []extension.Extension) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.results[path] = exts
}

// SetError presets the error returned for the given path.
func (m *MockPluginLoader) SetError(path string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loadErr[path] = err
}

// Load returns the preset extensions/error for path.
func (m *MockPluginLoader) Load(_ context.Context, path string) ([]extension.Extension, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loadedPath = path
	if err := m.loadErr[path]; err != nil {
		return nil, err
	}
	return m.results[path], nil
}

// LoadedPath returns the most recent path passed to Load.
func (m *MockPluginLoader) LoadedPath() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.loadedPath
}

// MockMiddleware wraps an AgentFunc, recording wrap counts.
type MockMiddleware struct {
	mu        sync.Mutex
	name      string
	wrapCount int
}

var _ extension.Middleware = (*MockMiddleware)(nil)

// NewMockMiddleware creates a MockMiddleware.
func NewMockMiddleware(name string) *MockMiddleware {
	return &MockMiddleware{name: name}
}

// Name returns the mock middleware name.
func (m *MockMiddleware) Name() string { return m.name }

// WrapAgent wraps next, recording the wrap and delegating to next.
func (m *MockMiddleware) WrapAgent(next extension.AgentFunc) extension.AgentFunc {
	m.mu.Lock()
	m.wrapCount++
	m.mu.Unlock()
	return func(ctx context.Context, input extension.AgentInput) (extension.AgentOutput, error) {
		return next(ctx, input)
	}
}

// WrapCount returns how many times WrapAgent ran.
func (m *MockMiddleware) WrapCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.wrapCount
}

// MockModelMiddleware wraps a ModelFunc, recording wrap counts.
type MockModelMiddleware struct {
	mu        sync.Mutex
	name      string
	wrapCount int
}

var _ extension.ModelMiddleware = (*MockModelMiddleware)(nil)

// NewMockModelMiddleware creates a MockModelMiddleware.
func NewMockModelMiddleware(name string) *MockModelMiddleware {
	return &MockModelMiddleware{name: name}
}

// Name returns the mock model middleware name.
func (m *MockModelMiddleware) Name() string { return m.name }

// WrapModel wraps next, recording the wrap and delegating to next.
func (m *MockModelMiddleware) WrapModel(next extension.ModelFunc) extension.ModelFunc {
	m.mu.Lock()
	m.wrapCount++
	m.mu.Unlock()
	return func(ctx context.Context, req extension.ModelRequest) (extension.ModelResponse, error) {
		return next(ctx, req)
	}
}

// WrapCount returns how many times WrapModel ran.
func (m *MockModelMiddleware) WrapCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.wrapCount
}

// MockToolMiddleware wraps a ToolFunc, recording wrap counts.
type MockToolMiddleware struct {
	mu        sync.Mutex
	name      string
	wrapCount int
}

var _ extension.ToolMiddleware = (*MockToolMiddleware)(nil)

// NewMockToolMiddleware creates a MockToolMiddleware.
func NewMockToolMiddleware(name string) *MockToolMiddleware {
	return &MockToolMiddleware{name: name}
}

// Name returns the mock tool middleware name.
func (m *MockToolMiddleware) Name() string { return m.name }

// WrapTool wraps next, recording the wrap and delegating to next.
func (m *MockToolMiddleware) WrapTool(next extension.ToolFunc) extension.ToolFunc {
	m.mu.Lock()
	m.wrapCount++
	m.mu.Unlock()
	return func(ctx context.Context, name string, input any) (any, error) {
		return next(ctx, name, input)
	}
}

// WrapCount returns how many times WrapTool ran.
func (m *MockToolMiddleware) WrapCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.wrapCount
}
