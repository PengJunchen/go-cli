package extension

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- local stubs (avoid importing internal/mock which creates a cycle) ---

// typesTestExt records Init/Shutdown calls for lifecycle assertions.
type typesTestExt struct {
	mu          sync.Mutex
	name        string
	initCalled  bool
	shutdownCnt int
	initErr     error
	shutdownErr error
	registry    ExtensionRegistry
}

var _ Extension = (*typesTestExt)(nil)

func newTypesTestExt(name string) *typesTestExt { return &typesTestExt{name: name} }

func (e *typesTestExt) Name() string { return e.name }

func (e *typesTestExt) SetInitError(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.initErr = err
}

func (e *typesTestExt) Init(_ context.Context, reg ExtensionRegistry) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.initCalled = true
	e.registry = reg
	return e.initErr
}

func (e *typesTestExt) Shutdown(_ context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.shutdownCnt++
	return e.shutdownErr
}

func (e *typesTestExt) InitCalled() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.initCalled
}

func (e *typesTestExt) ShutdownCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.shutdownCnt
}

func (e *typesTestExt) Registry() ExtensionRegistry {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.registry
}

// typesTestHook records Handle calls.
type typesTestHook struct {
	mu     sync.Mutex
	name   string
	calls  int
	result HookResult
}

var _ Hook = (*typesTestHook)(nil)

func newTypesTestHook(name string) *typesTestHook {
	return &typesTestHook{name: name, result: HookResult{Action: HookActionPass}}
}

func (h *typesTestHook) Name() string { return h.name }

func (h *typesTestHook) SetResult(r HookResult) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.result = r
}

func (h *typesTestHook) Handle(_ context.Context, _ HookEvent) HookResult {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls++
	return h.result
}

func (h *typesTestHook) CallCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

// typesTestMiddleware wraps an AgentFunc, recording wrap counts.
type typesTestMiddleware struct {
	mu        sync.Mutex
	name      string
	wrapCount int
}

var _ Middleware = (*typesTestMiddleware)(nil)

func newTypesTestMiddleware(name string) *typesTestMiddleware {
	return &typesTestMiddleware{name: name}
}

func (m *typesTestMiddleware) Name() string { return m.name }

func (m *typesTestMiddleware) WrapAgent(next AgentFunc) AgentFunc {
	m.mu.Lock()
	m.wrapCount++
	m.mu.Unlock()
	return func(ctx context.Context, input AgentInput) (AgentOutput, error) {
		return next(ctx, input)
	}
}

func (m *typesTestMiddleware) WrapCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.wrapCount
}

// typesTestModelMiddleware wraps a ModelFunc, recording wrap counts.
type typesTestModelMiddleware struct {
	mu        sync.Mutex
	name      string
	wrapCount int
}

var _ ModelMiddleware = (*typesTestModelMiddleware)(nil)

func newTypesTestModelMiddleware(name string) *typesTestModelMiddleware {
	return &typesTestModelMiddleware{name: name}
}

func (m *typesTestModelMiddleware) Name() string { return m.name }

func (m *typesTestModelMiddleware) WrapModel(next ModelFunc) ModelFunc {
	m.mu.Lock()
	m.wrapCount++
	m.mu.Unlock()
	return func(ctx context.Context, req ModelRequest) (ModelResponse, error) {
		return next(ctx, req)
	}
}

func (m *typesTestModelMiddleware) WrapCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.wrapCount
}

// typesTestToolMiddleware wraps a ToolFunc, recording wrap counts.
type typesTestToolMiddleware struct {
	mu        sync.Mutex
	name      string
	wrapCount int
}

var _ ToolMiddleware = (*typesTestToolMiddleware)(nil)

func newTypesTestToolMiddleware(name string) *typesTestToolMiddleware {
	return &typesTestToolMiddleware{name: name}
}

func (m *typesTestToolMiddleware) Name() string { return m.name }

func (m *typesTestToolMiddleware) WrapTool(next ToolFunc) ToolFunc {
	m.mu.Lock()
	m.wrapCount++
	m.mu.Unlock()
	return func(ctx context.Context, name string, input any) (any, error) {
		return next(ctx, name, input)
	}
}

func (m *typesTestToolMiddleware) WrapCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.wrapCount
}

// --- tests ---

// AC-1: the Extension interface exposes Name/Init/Shutdown, implemented by the
// default stub and recorded by the mock.
func TestExtensionInterface(t *testing.T) {
	ctx := context.Background()
	var def Extension = &defaultExtension{}

	assert.Equal(t, "default-extension", def.Name())
	require.NoError(t, def.Init(ctx, NewExtensionRegistry()))
	require.NoError(t, def.Shutdown(ctx))

	// The stub satisfies the same contract and records invocations.
	ext := newTypesTestExt("recorded")
	reg := NewExtensionRegistry()
	require.NoError(t, ext.Init(ctx, reg))
	assert.True(t, ext.InitCalled())
	require.NoError(t, ext.Shutdown(ctx))
	assert.Equal(t, 1, ext.ShutdownCount())
}

// AC-2/AC-3: the Hook interface (Name/Handle) and HookEvent/HookResult model
// with the four actions.
func TestHookAndHookActions(t *testing.T) {
	ctx := context.Background()

	hook := newTypesTestHook("h1")
	event := HookEvent{
		Name:      "agent.before_run",
		Data:      "payload",
		Source:    "test-extension",
		Timestamp: time.Now(),
	}
	result := hook.Handle(ctx, event)
	assert.Equal(t, HookActionPass, result.Action)
	assert.Equal(t, 1, hook.CallCount())

	// All four actions are representable.
	actions := []HookAction{
		HookActionPass,
		hookActionBlock,
		hookActionTerminate,
		hookActionReplace,
	}
	expected := []string{"pass", "block", "terminate", "replace"}
	for i, a := range actions {
		assert.Equal(t, expected[i], string(a))
	}

	// Replace carries a substitution value.
	hook.SetResult(HookResult{Action: hookActionReplace, Replacement: "new-payload"})
	res := hook.Handle(ctx, event)
	assert.Equal(t, hookActionReplace, res.Action)
	assert.Equal(t, "new-payload", res.Replacement)

	// Default hook is a pass-through.
	defHook := &defaultHook{}
	assert.Equal(t, HookActionPass, defHook.Handle(ctx, event).Action)
}

// AC-4: Middleware, ModelMiddleware and ToolMiddleware interfaces and their
// pass-through default implementations actually wrap (unwrap) correctly.
func TestMiddlewareInterfaces(t *testing.T) {
	ctx := context.Background()

	// Agent-level middleware chain.
	mw := newTypesTestMiddleware("mw")
	var innerCalled bool
	base := func(_ context.Context, input AgentInput) (AgentOutput, error) {
		innerCalled = true
		return AgentOutput{Text: "echo:" + input.Message}, nil
	}
	wrapped := mw.WrapAgent(base)
	out, err := wrapped(ctx, AgentInput{Message: "hi"})
	require.NoError(t, err)
	assert.True(t, innerCalled)
	assert.Equal(t, "echo:hi", out.Text)
	assert.Equal(t, 1, mw.WrapCount())

	// Model middleware chain.
	mmw := newTypesTestModelMiddleware("mmw")
	modelBase := func(_ context.Context, req ModelRequest) (ModelResponse, error) {
		return ModelResponse{Text: req.Prompt + "!"}, nil
	}
	mOut, err := mmw.WrapModel(modelBase)(ctx, ModelRequest{Prompt: "p", Model: "m", Temperature: 0.5})
	require.NoError(t, err)
	assert.Equal(t, "p!", mOut.Text)
	assert.Equal(t, 1, mmw.WrapCount())

	// Tool middleware chain.
	tmw := newTypesTestToolMiddleware("tmw")
	toolBase := func(_ context.Context, name string, input any) (any, error) {
		s, ok := input.(string)
		if !ok {
			s = ""
		}
		return name + ":" + s, nil
	}
	tOut, err := tmw.WrapTool(toolBase)(ctx, "read", "file")
	require.NoError(t, err)
	assert.Equal(t, "read:file", tOut)
	assert.Equal(t, 1, tmw.WrapCount())

	// Default middlewares are pass-through.
	defMw := &defaultMiddleware{}
	_, outErr := defMw.WrapAgent(func(_ context.Context, i AgentInput) (AgentOutput, error) {
		return AgentOutput{}, nil
	})(ctx, AgentInput{Message: "x"})
	require.NoError(t, outErr)
}
