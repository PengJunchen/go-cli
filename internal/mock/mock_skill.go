//go:build mock

package mock

import (
	"context"
	"sync"

	"github.com/pengjunchen/go-cli/internal/skill"
)

// MockSkillLoaderOption configures a MockSkillLoader.
type MockSkillLoaderOption func(*MockSkillLoader)

// MockSkillLoader is a test-only skill.SkillLoader that returns configured
// definitions or errors and records every Load / LoadDir invocation.
type MockSkillLoader struct {
	mu sync.Mutex

	load         func(ctx context.Context, path string) (*skill.SkillDefinition, error)
	loadDir      func(ctx context.Context, dir string) ([]*skill.SkillDefinition, error)
	loadCount    int
	loadDirCalls []string
}

// Compile-time assertion that the mock satisfies the loader contract.
var _ skill.SkillLoader = (*MockSkillLoader)(nil)

// NewMockSkillLoader creates an empty mock loader. By default it returns no
// definitions and no error.
func NewMockSkillLoader(opts ...MockSkillLoaderOption) *MockSkillLoader {
	l := &MockSkillLoader{
		load: func(_ context.Context, _ string) (*skill.SkillDefinition, error) {
			return nil, nil
		},
		loadDir: func(_ context.Context, _ string) ([]*skill.SkillDefinition, error) {
			return nil, nil
		},
	}
	for _, opt := range opts {
		opt(l)
	}
	return l
}

// WithLoad sets the behavior of Load.
func WithLoad(f func(ctx context.Context, path string) (*skill.SkillDefinition, error)) MockSkillLoaderOption {
	return func(l *MockSkillLoader) { l.load = f }
}

// WithLoadDir sets the behavior of LoadDir.
func WithLoadDir(f func(ctx context.Context, dir string) ([]*skill.SkillDefinition, error)) MockSkillLoaderOption {
	return func(l *MockSkillLoader) { l.loadDir = f }
}

// Load records the call, then delegates to the configured handler.
func (l *MockSkillLoader) Load(ctx context.Context, path string) (*skill.SkillDefinition, error) {
	l.mu.Lock()
	l.loadCount++
	l.mu.Unlock()
	return l.load(ctx, path)
}

// LoadDir records the call, then delegates to the configured handler.
func (l *MockSkillLoader) LoadDir(ctx context.Context, dir string) ([]*skill.SkillDefinition, error) {
	l.mu.Lock()
	l.loadDirCalls = append(l.loadDirCalls, dir)
	l.mu.Unlock()
	return l.loadDir(ctx, dir)
}

// LoadCount returns the number of Load invocations.
func (l *MockSkillLoader) LoadCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.loadCount
}

// LoadDirCalls returns a copy of the directories passed to LoadDir.
func (l *MockSkillLoader) LoadDirCalls() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.loadDirCalls))
	copy(out, l.loadDirCalls)
	return out
}

// MockSkillRegistry is a test-only skill.SkillRegistry with an in-memory,
// concurrency-safe store.
type MockSkillRegistry struct {
	mu   sync.Mutex
	by   map[string]skill.SkillDefinition
	regs int
	dels int
}

// Compile-time assertion that the mock satisfies the registry contract.
var _ skill.SkillRegistry = (*MockSkillRegistry)(nil)

// NewMockSkillRegistry creates an empty mock registry.
func NewMockSkillRegistry() *MockSkillRegistry {
	return &MockSkillRegistry{by: map[string]skill.SkillDefinition{}}
}

// Register stores def, replacing any existing entry with the same name.
func (m *MockSkillRegistry) Register(_ context.Context, def skill.SkillDefinition) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.regs++
	m.by[def.Name()] = def
	return nil
}

// Get returns the skill with the given name, if present.
func (m *MockSkillRegistry) Get(_ context.Context, name string) (skill.SkillDefinition, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	def, ok := m.by[name]
	return def, ok
}

// List returns all registered skills, optionally filtered by category.
func (m *MockSkillRegistry) List(_ context.Context, category ...string) []skill.SkillDefinition {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []skill.SkillDefinition
	for _, def := range m.by {
		if len(category) > 0 && !containsCategory(def.Category(), category) {
			continue
		}
		out = append(out, def)
	}
	return out
}

// Match returns skills whose name contains hint. It is a simplified matcher for
// test use.
func (m *MockSkillRegistry) Match(_ context.Context, hint string) []skill.SkillDefinition {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []skill.SkillDefinition
	for _, def := range m.by {
		if def.Name() == hint {
			out = append(out, def)
		}
	}
	return out
}

// Unregister removes the named skill.
func (m *MockSkillRegistry) Unregister(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.by, name)
	m.dels++
	return nil
}

// RegisterCount returns the number of Register invocations.
func (m *MockSkillRegistry) RegisterCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.regs
}

// UnregisterCount returns the number of Unregister invocations.
func (m *MockSkillRegistry) UnregisterCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.dels
}

func containsCategory(cat string, cats []string) bool {
	for _, c := range cats {
		if cat == c {
			return true
		}
	}
	return false
}
