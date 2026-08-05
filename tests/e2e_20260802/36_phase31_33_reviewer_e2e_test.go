//go:build e2e

// Package e2e_20260802 contains end-to-end integration tests.
// This file covers reviewer E2E tests for Phases 31 (plugin ecosystem),
// 32 (ACP multi-agent communication), and 33 (extension lifecycle).
package e2e_20260802 //nolint:staticcheck

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/acp"
	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/extension"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// =============================================================================
// Mock types
// =============================================================================

// reviewerExt is a mock Extension that records Init/Shutdown calls and can
// register hooks/middleware during Init.
type reviewerExt struct {
	mu            sync.Mutex
	name          string
	initCalled    bool
	shutdownCnt   int
	orderMu       *sync.Mutex
	shutdownOrder *[]string
	hook          extension.Hook
	mw            extension.Middleware
}

var _ extension.Extension = (*reviewerExt)(nil)

func (e *reviewerExt) Name() string { return e.name }

func (e *reviewerExt) Init(ctx context.Context, reg extension.ExtensionRegistry) error {
	e.mu.Lock()
	e.initCalled = true
	e.mu.Unlock()
	if e.hook != nil {
		if err := reg.RegisterHook(ctx, e.hook); err != nil {
			return err
		}
	}
	if e.mw != nil {
		if err := reg.RegisterMiddleware(ctx, e.mw); err != nil {
			return err
		}
	}
	return nil
}

func (e *reviewerExt) Shutdown(_ context.Context) error {
	e.mu.Lock()
	e.shutdownCnt++
	e.mu.Unlock()
	if e.orderMu != nil && e.shutdownOrder != nil {
		e.orderMu.Lock()
		*e.shutdownOrder = append(*e.shutdownOrder, e.name)
		e.orderMu.Unlock()
	}
	return nil
}

func (e *reviewerExt) InitCalled() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.initCalled
}

func (e *reviewerExt) ShutdownCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.shutdownCnt
}

// reviewerHook is a mock Hook that returns HookActionPass.
type reviewerHook struct {
	name string
}

var _ extension.Hook = (*reviewerHook)(nil)

func (h *reviewerHook) Name() string { return h.name }

func (h *reviewerHook) Handle(_ context.Context, _ extension.HookEvent) extension.HookResult {
	return extension.HookResult{Action: extension.HookActionPass}
}

// reviewerMW is a mock Middleware that delegates to the wrapped AgentFunc.
type reviewerMW struct {
	name string
}

var _ extension.Middleware = (*reviewerMW)(nil)

func (m *reviewerMW) Name() string { return m.name }

func (m *reviewerMW) WrapAgent(next extension.AgentFunc) extension.AgentFunc {
	return func(ctx context.Context, input extension.AgentInput) (extension.AgentOutput, error) {
		return next(ctx, input)
	}
}

// reviewerLoader is a mock PluginLoader that returns predefined extensions.
type reviewerLoader struct {
	mu      sync.Mutex
	name    string
	results map[string][]extension.Extension
	errors  map[string]error
}

var _ extension.PluginLoader = (*reviewerLoader)(nil)

func newReviewerLoader(name string) *reviewerLoader {
	return &reviewerLoader{
		name:    name,
		results: make(map[string][]extension.Extension),
		errors:  make(map[string]error),
	}
}

func (l *reviewerLoader) Name() string { return l.name }

func (l *reviewerLoader) SetResult(path string, exts []extension.Extension) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.results[path] = exts
}

func (l *reviewerLoader) SetError(path string, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.errors[path] = err
}

func (l *reviewerLoader) Load(_ context.Context, path string) ([]extension.Extension, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.errors[path]; err != nil {
		return nil, err
	}
	return l.results[path], nil
}

// reviewerACPClient is a mock ACPClient for testing.
type reviewerACPClient struct {
	name string
	recv chan acp.ACPMessage
	mu   sync.Mutex
	sent []acp.ACPMessage
}

var _ acp.ACPClient = (*reviewerACPClient)(nil)

func newReviewerACPClient(name string) *reviewerACPClient {
	return &reviewerACPClient{
		name: name,
		recv: make(chan acp.ACPMessage, 8),
	}
}

func (c *reviewerACPClient) Connect(_ context.Context) error    { return nil }
func (c *reviewerACPClient) Disconnect(_ context.Context) error { return nil }

func (c *reviewerACPClient) SendMessage(_ context.Context, msg acp.ACPMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, msg)
	return nil
}

func (c *reviewerACPClient) ReceiveMessages() <-chan acp.ACPMessage { return c.recv }
func (c *reviewerACPClient) Name() string                           { return c.name }

func (c *reviewerACPClient) SentCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.sent)
}

func (c *reviewerACPClient) LastSent() acp.ACPMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.sent) == 0 {
		return acp.ACPMessage{}
	}
	return c.sent[len(c.sent)-1]
}

// reviewerDispatcher is a mock SubagentDispatcher for testing.
type reviewerDispatcher struct {
	mu     sync.Mutex
	tasks  []core.SubagentTask
	result core.SubagentResult
	err    error
}

var _ core.SubagentDispatcher = (*reviewerDispatcher)(nil)

func (d *reviewerDispatcher) Dispatch(_ context.Context, task core.SubagentTask) (core.SubagentResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.tasks = append(d.tasks, task)
	return d.result, d.err
}

func (d *reviewerDispatcher) ParallelDispatch(_ context.Context, tasks []core.SubagentTask) ([]core.SubagentResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.tasks = append(d.tasks, tasks...)
	results := make([]core.SubagentResult, len(tasks))
	for i := range tasks {
		results[i] = d.result
	}
	return results, d.err
}

func (d *reviewerDispatcher) ListRunning() []core.SubagentTask { return nil }

func (d *reviewerDispatcher) GetTasks() []core.SubagentTask {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]core.SubagentTask, len(d.tasks))
	copy(out, d.tasks)
	return out
}

// reviewerEchoLoop is a minimal AgentLoop that echoes the submission content.
type reviewerEchoLoop struct {
	called bool
}

var _ core.AgentLoop = (*reviewerEchoLoop)(nil)

func (l *reviewerEchoLoop) Run(_ context.Context, submission core.Submission, _ ...core.EventStream) ([]core.AgentEvent, error) {
	l.called = true
	return []core.AgentEvent{{Kind: "message", Content: submission.Content, Timestamp: time.Now()}}, nil
}

// =============================================================================
// Phase 31: Plugin Ecosystem E2E Tests
// =============================================================================

// Test ID:   ET-PHASE31-001
// Task ref:  test(extension): add plugin ecosystem E2E tests
// Feature:   PluginManager Load with empty paths is a no-op
func TestET_Phase31_PluginManagerLoadEmpty(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pm := extension.NewPluginManager(extension.NewDefaultPluginLoader())

	// Load with nil paths.
	require.NoError(t, pm.Load(ctx, nil))
	assert.Empty(t, pm.Extensions())

	// Load with empty slice.
	require.NoError(t, pm.Load(ctx, []string{}))
	assert.Empty(t, pm.Extensions())

	// Registry should still be non-nil.
	assert.NotNil(t, pm.Registry())
}

// Test ID:   ET-PHASE31-002
// Task ref:  test(extension): add plugin ecosystem E2E tests
// Feature:   PluginManager Init/Shutdown lifecycle with reverse-order shutdown
func TestET_Phase31_PluginManagerLifecycle(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	loader := newReviewerLoader("test")
	var orderMu sync.Mutex
	var shutdownOrder []string

	ext1 := &reviewerExt{name: "ext-1", orderMu: &orderMu, shutdownOrder: &shutdownOrder}
	ext2 := &reviewerExt{name: "ext-2", orderMu: &orderMu, shutdownOrder: &shutdownOrder}
	ext3 := &reviewerExt{name: "ext-3", orderMu: &orderMu, shutdownOrder: &shutdownOrder}
	loader.SetResult("plugins", []extension.Extension{ext1, ext2, ext3})

	pm := extension.NewPluginManager(loader)

	// Load extensions.
	require.NoError(t, pm.Load(ctx, []string{"plugins"}))
	require.Len(t, pm.Extensions(), 3)

	// Init all extensions.
	require.NoError(t, pm.Init(ctx))
	assert.True(t, ext1.InitCalled())
	assert.True(t, ext2.InitCalled())
	assert.True(t, ext3.InitCalled())

	// Shutdown in reverse order.
	require.NoError(t, pm.Shutdown(ctx))
	assert.Equal(t, 1, ext1.ShutdownCount())
	assert.Equal(t, 1, ext2.ShutdownCount())
	assert.Equal(t, 1, ext3.ShutdownCount())

	// Verify reverse order: ext-3, ext-2, ext-1.
	assert.Equal(t, []string{"ext-3", "ext-2", "ext-1"}, shutdownOrder)
}

// Test ID:   ET-PHASE31-003
// Task ref:  test(extension): add plugin ecosystem E2E tests
// Feature:   PluginManager graceful error handling for non-existent paths
func TestET_Phase31_PluginManagerGracefulErrorHandling(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pm := extension.NewPluginManager(extension.NewDefaultPluginLoader())

	// Load with a non-existent path should not panic or return an error.
	assert.NotPanics(t, func() {
		err := pm.Load(ctx, []string{"/nonexistent/path/to/plugin.so"})
		assert.NoError(t, err, "Load should swallow individual path errors")
	})

	// No extensions should have been loaded.
	assert.Empty(t, pm.Extensions())
}

// =============================================================================
// Phase 32: ACP Multi-Agent Communication E2E Tests
// =============================================================================

// Test ID:   ET-PHASE32-001
// Task ref:  test(acp): add ACP multi-agent communication E2E tests
// Feature:   ACPMiddlewareAdapter pass-through with nil client
func TestET_Phase32_ACPMiddlewareAdapterPassthrough(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	mw := acp.NewACPMiddleware("acp-bridge", nil)
	adapter := acp.NewACPMiddlewareAdapter(mw, nil, nil)
	defer adapter.Close()

	inner := &reviewerEchoLoop{}
	wrapped := adapter.Wrap(inner)

	// Running the wrapped loop should delegate to the inner loop unchanged.
	events, err := wrapped.Run(ctx, core.Submission{
		Type:    core.SubmissionUserMessage,
		Content: "hello",
	})
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, "hello", events[0].Content)
	assert.True(t, inner.called, "inner loop must be called")
}

// Test ID:   ET-PHASE32-002
// Task ref:  test(acp): add ACP multi-agent communication E2E tests
// Feature:   ACPMiddlewareAdapter message routing to SubagentDispatcher
func TestET_Phase32_ACPMiddlewareAdapterWithClient(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client := newReviewerACPClient("test-client")
	dispatcher := &reviewerDispatcher{
		result: core.SubagentResult{TaskID: "acp-peer", Content: "dispatched-result"},
	}

	mw := acp.NewACPMiddleware("acp-bridge", client)
	adapter := acp.NewACPMiddlewareAdapter(mw, dispatcher, client)
	defer adapter.Close()

	inner := &reviewerEchoLoop{}
	wrapped := adapter.Wrap(inner)

	// Start the router by invoking Run once.
	_, _ = wrapped.Run(ctx, core.Submission{Content: "start"})

	// Simulate an inbound ACP message from the peer.
	client.recv <- acp.ACPMessage{
		Type:       acp.TypeMessage,
		SenderID:   "peer",
		ReceiverID: "me",
		Content:    "do work",
	}

	// The dispatcher must receive a task whose Prompt matches the message.
	require.Eventually(t, func() bool {
		return len(dispatcher.GetTasks()) > 0
	}, 2*time.Second, 10*time.Millisecond, "dispatcher should receive a task")

	tasks := dispatcher.GetTasks()
	require.Len(t, tasks, 1)
	assert.Equal(t, "do work", tasks[0].Prompt)

	// The result must be relayed back as a TypeResponse through the client.
	require.Eventually(t, func() bool {
		return client.SentCount() > 0
	}, 2*time.Second, 10*time.Millisecond, "response should be sent back")

	reply := client.LastSent()
	assert.Equal(t, acp.TypeResponse, reply.Type)
	assert.Equal(t, "me", reply.SenderID)
	assert.Equal(t, "peer", reply.ReceiverID)
	assert.Equal(t, "dispatched-result", reply.Content)
	assert.False(t, reply.Timestamp.IsZero())
}

// Test ID:   ET-PHASE32-003
// Task ref:  test(acp): add ACP multi-agent communication E2E tests
// Feature:   ACPConfig JSON parsing
func TestET_Phase32_ACPConfigParsing(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	_, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	jsonStr := `{
		"transport": "grpc",
		"endpoints": ["http://localhost:8080", "http://localhost:8081"],
		"timeout": 30
	}`

	var cfg config.ACPConfig
	err := json.Unmarshal([]byte(jsonStr), &cfg)
	require.NoError(t, err)

	assert.Equal(t, "grpc", cfg.Transport)
	assert.Equal(t, []string{"http://localhost:8080", "http://localhost:8081"}, cfg.Endpoints)
	assert.Equal(t, 30, cfg.Timeout)
}

// =============================================================================
// Phase 33: Extension Lifecycle E2E Tests
// =============================================================================

// Test ID:   ET-PHASE33-001
// Task ref:  test(extension): add extension lifecycle E2E tests
// Feature:   Extension hooks bridged via PluginManager
func TestET_Phase33_ExtensionHooksBridged(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	loader := newReviewerLoader("test")
	hook := &reviewerHook{name: "ext-hook"}
	ext := &reviewerExt{name: "reg-ext", hook: hook}
	loader.SetResult("p", []extension.Extension{ext})

	pm := extension.NewPluginManager(loader)
	require.NoError(t, pm.Load(ctx, []string{"p"}))

	// Before Init: no hooks available.
	assert.Empty(t, pm.Hooks())

	require.NoError(t, pm.Init(ctx))

	// After Init: hooks should be accessible via PluginManager.Hooks().
	hooks := pm.Hooks()
	require.Len(t, hooks, 1)
	assert.Equal(t, "ext-hook", hooks[0].Name())

	// Also verify via Registry().AllHooks().
	der, ok := pm.Registry().(*extension.DefaultExtensionRegistry)
	require.True(t, ok)
	allHooks := der.AllHooks()
	require.Len(t, allHooks, 1)
	assert.Equal(t, "ext-hook", allHooks[0].Name())

	require.NoError(t, pm.Shutdown(ctx))
}

// Test ID:   ET-PHASE33-002
// Task ref:  test(extension): add extension lifecycle E2E tests
// Feature:   Extension middleware bridged via PluginManager
func TestET_Phase33_ExtensionMiddlewareBridged(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	loader := newReviewerLoader("test")
	mw := &reviewerMW{name: "ext-mw"}
	ext := &reviewerExt{name: "reg-ext", mw: mw}
	loader.SetResult("p", []extension.Extension{ext})

	pm := extension.NewPluginManager(loader)
	require.NoError(t, pm.Load(ctx, []string{"p"}))

	// Before Init: no middleware available.
	assert.Empty(t, pm.Middleware())

	require.NoError(t, pm.Init(ctx))

	// After Init: middleware should be accessible via PluginManager.Middleware().
	mws := pm.Middleware()
	require.Len(t, mws, 1)
	assert.Equal(t, "ext-mw", mws[0].Name())

	// Also verify via Registry().AllMiddleware().
	der, ok := pm.Registry().(*extension.DefaultExtensionRegistry)
	require.True(t, ok)
	allMWs := der.AllMiddleware()
	require.Len(t, allMWs, 1)
	assert.Equal(t, "ext-mw", allMWs[0].Name())

	require.NoError(t, pm.Shutdown(ctx))
}

// Test ID:   ET-PHASE33-003
// Task ref:  test(extension): add extension lifecycle E2E tests
// Feature:   Extension shutdown in reverse registration order
func TestET_Phase33_ExtensionShutdownReverseOrder(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	loader := newReviewerLoader("test")
	var orderMu sync.Mutex
	var shutdownOrder []string

	ext1 := &reviewerExt{name: "alpha", orderMu: &orderMu, shutdownOrder: &shutdownOrder}
	ext2 := &reviewerExt{name: "beta", orderMu: &orderMu, shutdownOrder: &shutdownOrder}
	ext3 := &reviewerExt{name: "gamma", orderMu: &orderMu, shutdownOrder: &shutdownOrder}
	loader.SetResult("p", []extension.Extension{ext1, ext2, ext3})

	pm := extension.NewPluginManager(loader)
	require.NoError(t, pm.Load(ctx, []string{"p"}))
	require.NoError(t, pm.Init(ctx))

	// All extensions should be initialized.
	assert.True(t, ext1.InitCalled())
	assert.True(t, ext2.InitCalled())
	assert.True(t, ext3.InitCalled())

	require.NoError(t, pm.Shutdown(ctx))

	// Shutdown order should be reverse of load order.
	require.Len(t, shutdownOrder, 3)
	assert.Equal(t, []string{"gamma", "beta", "alpha"}, shutdownOrder)
}
