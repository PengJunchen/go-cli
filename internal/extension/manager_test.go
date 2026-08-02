package extension

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// --- local stubs (avoid importing internal/mock which creates a cycle) ---

// mgrTestExt records Init/Shutdown calls for lifecycle assertions.
type mgrTestExt struct {
	mu          sync.Mutex
	name        string
	initCalled  bool
	shutdownCnt int
	initErr     error
	shutdownErr error
	registry    ExtensionRegistry
}

var _ Extension = (*mgrTestExt)(nil)

func newMgrTestExt(name string) *mgrTestExt { return &mgrTestExt{name: name} }

func (e *mgrTestExt) Name() string { return e.name }

func (e *mgrTestExt) SetInitError(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.initErr = err
}

func (e *mgrTestExt) SetShutdownError(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.shutdownErr = err
}

func (e *mgrTestExt) Init(_ context.Context, reg ExtensionRegistry) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.initCalled = true
	e.registry = reg
	return e.initErr
}

func (e *mgrTestExt) Shutdown(_ context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.shutdownCnt++
	return e.shutdownErr
}

func (e *mgrTestExt) InitCalled() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.initCalled
}

func (e *mgrTestExt) ShutdownCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.shutdownCnt
}

func (e *mgrTestExt) Registry() ExtensionRegistry {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.registry
}

// mgrTestHook records Handle calls.
type mgrTestHook struct {
	mu     sync.Mutex
	name   string
	calls  int
	result HookResult
}

var _ Hook = (*mgrTestHook)(nil)

func newMgrTestHook(name string) *mgrTestHook {
	return &mgrTestHook{name: name, result: HookResult{Action: HookActionPass}}
}

func (h *mgrTestHook) Name() string { return h.name }

func (h *mgrTestHook) SetResult(r HookResult) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.result = r
}

func (h *mgrTestHook) Handle(_ context.Context, _ HookEvent) HookResult {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls++
	return h.result
}

func (h *mgrTestHook) CallCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

// mgrTestPluginLoader returns predefined extensions for a path.
type mgrTestPluginLoader struct {
	mu         sync.Mutex
	name       string
	results    map[string][]Extension
	loadErr    map[string]error
	loadedPath string
}

var _ PluginLoader = (*mgrTestPluginLoader)(nil)

func newMgrTestPluginLoader(name string) *mgrTestPluginLoader {
	return &mgrTestPluginLoader{
		name:    name,
		results: make(map[string][]Extension),
		loadErr: make(map[string]error),
	}
}

func (l *mgrTestPluginLoader) Name() string { return l.name }

func (l *mgrTestPluginLoader) SetResult(path string, exts []Extension) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.results[path] = exts
}

func (l *mgrTestPluginLoader) SetError(path string, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.loadErr[path] = err
}

func (l *mgrTestPluginLoader) Load(_ context.Context, path string) ([]Extension, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.loadedPath = path
	if err := l.loadErr[path]; err != nil {
		return nil, err
	}
	return l.results[path], nil
}

func (l *mgrTestPluginLoader) LoadedPath() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.loadedPath
}

// captureExporter is a local in-memory tracing.TraceExporter used to verify span
// emission deterministically without importing an unrelated mock package.
type captureExporter struct {
	mu    sync.Mutex
	spans []tracing.SpanData
}

func (e *captureExporter) ExportSpan(_ context.Context, span tracing.TraceSpan) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.spans = append(e.spans, tracing.SpanToData(span))
	return nil
}

func (e *captureExporter) Shutdown(context.Context) error { return nil }

func (e *captureExporter) all() []tracing.SpanData {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]tracing.SpanData(nil), e.spans...)
}

func (e *captureExporter) find(name string) *tracing.SpanData {
	for _, s := range e.all() {
		if s.Name == name {
			return &s
		}
	}
	return nil
}

// waitFind polls until a span with the given name is exported. Span export is
// asynchronous (localSpan.End exports from a goroutine), so tests must wait.
func waitFind(t *testing.T, rec *captureExporter, name string) *tracing.SpanData {
	t.Helper()
	var span *tracing.SpanData
	require.Eventually(t, func() bool {
		span = rec.find(name)
		return span != nil
	}, time.Second, 5*time.Millisecond, "expected span %q to be exported", name)
	return span
}

func attr(m map[string]string, name string) string { return m[name] }

// newTracingCtx returns a context whose Tracer exports to the captureExporter.
func newTracingCtx(rec *captureExporter) context.Context {
	tr := tracing.NewTracer("ext-test", rec)
	_, ctx := tr.Start(context.Background(), "test.root", tracing.SpanKindInternal)
	return ctx
}

// AC-9: extension.init, extension.shutdown and extension.hook spans are emitted
// with the required attributes.
func TestCoordinatorEmitsSpans(t *testing.T) {
	rec := &captureExporter{}
	ctx := newTracingCtx(rec)

	coord := newExtensionCoordinator(nil)
	ext := newMgrTestExt("span-ext")
	require.NoError(t, coord.initExtension(ctx, ext))

	hook := newMgrTestHook("span-hook")
	coord.runHook(ctx, hook, HookEvent{Name: "agent.before_run"})

	require.NoError(t, coord.shutdownExtension(ctx, ext))

	initSpan := waitFind(t, rec, "extension.init")
	got := map[string]string{}
	for _, a := range initSpan.Attributes {
		if s, ok := a.Value.(string); ok {
			got[a.Key] = s
		}
	}
	assert.Equal(t, "span-ext", attr(got, "extension_name"))

	hookSpan := waitFind(t, rec, "extension.hook")
	gotHook := map[string]string{}
	for _, a := range hookSpan.Attributes {
		if s, ok := a.Value.(string); ok {
			gotHook[a.Key] = s
		}
	}
	assert.Equal(t, "span-hook", attr(gotHook, "hook_name"))
	assert.Equal(t, "agent.before_run", attr(gotHook, "event"))
	assert.Equal(t, "pass", attr(gotHook, "action"))

	shutdownSpan := waitFind(t, rec, "extension.shutdown")
	assert.Equal(t, tracing.SpanKindInternal, shutdownSpan.SpanKind)
}

// AC-10: process-wide plugin loader registry provides nil-default accessors.
func TestPluginLoaderRegistry(t *testing.T) {
	registerPluginLoader(nil) // reset to default
	got := getPluginLoader()
	require.NotNil(t, got)
	assert.Equal(t, "default-plugin-loader", got.Name())

	mockLoader := newMgrTestPluginLoader("custom")
	registerPluginLoader(mockLoader)
	assert.Same(t, mockLoader, getPluginLoader())

	// A nil registration resets back to the default loader.
	registerPluginLoader(nil)
	assert.Equal(t, "default-plugin-loader", getPluginLoader().Name())
}

// AC-11: lifecycle transitions Pending -> Running -> Stopped.
func TestCoordinatorLifecycleStates(t *testing.T) {
	coord := newExtensionCoordinator(nil)
	ctx := context.Background()

	ext := newMgrTestExt("lifecycle")
	assert.Equal(t, extensionStatePending, coord.state(ext.Name()))

	require.NoError(t, coord.initExtension(ctx, ext))
	assert.True(t, ext.InitCalled())
	assert.Equal(t, extensionStateRunning, coord.state(ext.Name()))

	require.NoError(t, coord.shutdownExtension(ctx, ext))
	assert.Equal(t, 1, ext.ShutdownCount())
	assert.Equal(t, extensionStateStopped, coord.state(ext.Name()))
}

// AC-11: Init failure surfaces the error and does not mark the extension
// Running; Shutdown failure propagates.
func TestCoordinatorLifecycleErrors(t *testing.T) {
	coord := newExtensionCoordinator(nil)
	ctx := context.Background()

	initExt := newMgrTestExt("fail-init")
	initExt.SetInitError(errors.New("init blew up"))
	err := coord.initExtension(ctx, initExt)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "init blew up")

	shutExt := newMgrTestExt("fail-shutdown")
	require.NoError(t, coord.initExtension(ctx, shutExt))
	shutExt.SetShutdownError(errors.New("shutdown blew up"))
	err = coord.shutdownExtension(ctx, shutExt)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shutdown blew up")
}

// AC-11: the coordinator passes its registry through so extensions can register
// building blocks during Init.
func TestCoordinatorPassesRegistry(t *testing.T) {
	reg := NewExtensionRegistry()
	coord := newExtensionCoordinator(reg)
	ctx := context.Background()

	ext := newMgrTestExt("reg-ext")
	require.NoError(t, coord.initExtension(ctx, ext))
	assert.Same(t, reg, ext.Registry())
}

// Hook block/terminate/replace actions are relayed unchanged by the coordinator.
func TestCoordinatorRunHookActions(t *testing.T) {
	coord := newExtensionCoordinator(nil)
	ctx := context.Background()
	hook := newMgrTestHook("act")
	event := HookEvent{Name: "e", Timestamp: time.Now()}

	hook.SetResult(HookResult{Action: hookActionBlock, Reason: "denied"})
	res := coord.runHook(ctx, hook, event)
	assert.Equal(t, hookActionBlock, res.Action)
	assert.Equal(t, "denied", res.Reason)

	hook.SetResult(HookResult{Action: hookActionReplace, Replacement: "repl"})
	res = coord.runHook(ctx, hook, event)
	assert.Equal(t, hookActionReplace, res.Action)
	assert.Equal(t, "repl", res.Replacement)
}
