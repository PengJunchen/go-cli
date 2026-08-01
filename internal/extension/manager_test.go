package extension_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/extension"
	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/tracing"
)

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

	coord := extension.NewExtensionCoordinator(nil)
	ext := mock.NewMockExtension("span-ext")
	require.NoError(t, coord.InitExtension(ctx, ext))

	hook := mock.NewMockHook("span-hook")
	coord.RunHook(ctx, hook, extension.HookEvent{Name: "agent.before_run"})

	require.NoError(t, coord.ShutdownExtension(ctx, ext))

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
	extension.RegisterPluginLoader(nil) // reset to default
	got := extension.GetPluginLoader()
	require.NotNil(t, got)
	assert.Equal(t, "default-plugin-loader", got.Name())

	mockLoader := mock.NewMockPluginLoader("custom")
	extension.RegisterPluginLoader(mockLoader)
	assert.Same(t, mockLoader, extension.GetPluginLoader())

	// A nil registration resets back to the default loader.
	extension.RegisterPluginLoader(nil)
	assert.Equal(t, "default-plugin-loader", extension.GetPluginLoader().Name())
}

// AC-11: lifecycle transitions Pending -> Running -> Stopped.
func TestCoordinatorLifecycleStates(t *testing.T) {
	coord := extension.NewExtensionCoordinator(nil)
	ctx := context.Background()

	ext := mock.NewMockExtension("lifecycle")
	assert.Equal(t, extension.ExtensionStatePending, coord.State(ext.Name()))

	require.NoError(t, coord.InitExtension(ctx, ext))
	assert.True(t, ext.InitCalled())
	assert.Equal(t, extension.ExtensionStateRunning, coord.State(ext.Name()))

	require.NoError(t, coord.ShutdownExtension(ctx, ext))
	assert.Equal(t, 1, ext.ShutdownCount())
	assert.Equal(t, extension.ExtensionStateStopped, coord.State(ext.Name()))
}

// AC-11: Init failure surfaces the error and does not mark the extension
// Running; Shutdown failure propagates.
func TestCoordinatorLifecycleErrors(t *testing.T) {
	coord := extension.NewExtensionCoordinator(nil)
	ctx := context.Background()

	initExt := mock.NewMockExtension("fail-init")
	initExt.SetInitError(errors.New("init blew up"))
	err := coord.InitExtension(ctx, initExt)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "init blew up")

	shutExt := mock.NewMockExtension("fail-shutdown")
	require.NoError(t, coord.InitExtension(ctx, shutExt))
	shutExt.SetShutdownError(errors.New("shutdown blew up"))
	err = coord.ShutdownExtension(ctx, shutExt)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shutdown blew up")
}

// AC-11: the coordinator passes its registry through so extensions can register
// building blocks during Init.
func TestCoordinatorPassesRegistry(t *testing.T) {
	reg := extension.NewExtensionRegistry()
	coord := extension.NewExtensionCoordinator(reg)
	ctx := context.Background()

	ext := mock.NewMockExtension("reg-ext")
	require.NoError(t, coord.InitExtension(ctx, ext))
	assert.Same(t, reg, ext.Registry())
}

// Hook block/terminate/replace actions are relayed unchanged by the coordinator.
func TestCoordinatorRunHookActions(t *testing.T) {
	coord := extension.NewExtensionCoordinator(nil)
	ctx := context.Background()
	hook := mock.NewMockHook("act")
	event := extension.HookEvent{Name: "e", Timestamp: time.Now()}

	hook.SetResult(extension.HookResult{Action: extension.HookActionBlock, Reason: "denied"})
	res := coord.RunHook(ctx, hook, event)
	assert.Equal(t, extension.HookActionBlock, res.Action)
	assert.Equal(t, "denied", res.Reason)

	hook.SetResult(extension.HookResult{Action: extension.HookActionReplace, Replacement: "repl"})
	res = coord.RunHook(ctx, hook, event)
	assert.Equal(t, extension.HookActionReplace, res.Action)
	assert.Equal(t, "repl", res.Replacement)
}
