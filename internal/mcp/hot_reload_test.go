package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tracing"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// recordingMCPClient is a stub MCPClient that records the lifecycle calls made
// by the hot reloader. It can be configured to fail Connect a fixed number of
// times before succeeding, which lets tests drive the exponential-backoff path
// deterministically. It lives here (not in internal/mock) because internal/mock
// imports internal/mcp, which would create an import cycle.
type recordingMCPClient struct {
	name         string
	tools        []MCPTool
	failConnects int // remaining Connect calls that should fail

	mu          sync.Mutex
	connects    int
	disconnects int
	lists       int
}

func newRecordingMCPClient(name string, tools []MCPTool) *recordingMCPClient {
	return &recordingMCPClient{name: name, tools: tools}
}

// Name implements MCPClient.
func (c *recordingMCPClient) Name() string { return c.name }

// Connect implements MCPClient, failing the first failConnects times.
func (c *recordingMCPClient) Connect(_ context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.connects++
	if c.failConnects > 0 {
		c.failConnects--
		return errors.New("connect failed")
	}
	return nil
}

// Disconnect implements MCPClient.
func (c *recordingMCPClient) Disconnect(_ context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.disconnects++
	return nil
}

// ListTools implements MCPClient.
func (c *recordingMCPClient) ListTools(_ context.Context) ([]MCPTool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lists++
	out := make([]MCPTool, len(c.tools))
	copy(out, c.tools)
	return out, nil
}

// CallTool implements MCPClient.
func (c *recordingMCPClient) CallTool(_ context.Context, _ string, _ map[string]any) (*MCPToolResult, error) {
	return nil, errors.New("not implemented")
}

// ProtocolVersion implements MCPClient.
func (c *recordingMCPClient) ProtocolVersion() string { return LatestProtocolVersion }

// counts returns a snapshot of the lifecycle call counters.
func (c *recordingMCPClient) counts() (connects, disconnects, lists int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connects, c.disconnects, c.lists
}

type blockingConnectMCPClient struct {
	*recordingMCPClient
	connectStarted chan struct{}
	connectRelease chan struct{}
}

func (c *blockingConnectMCPClient) Connect(ctx context.Context) error {
	close(c.connectStarted)
	<-c.connectRelease
	return c.recordingMCPClient.Connect(ctx)
}

// reconnected reports whether at least one full cycle has occurred.
func (c *recordingMCPClient) reconnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connects > 0 && c.lists > 0
}

// assertReconnectCycle waits for a full Disconnect -> Connect -> ListTools cycle.
func assertReconnectCycle(t *testing.T, c *recordingMCPClient) {
	t.Helper()
	require.Eventually(t, func() bool {
		connects, disconnects, lists := c.counts()
		return connects >= 1 && disconnects >= 1 && lists >= 1
	}, 2*time.Second, 5*time.Millisecond, "expected a full reconnect cycle")
}

// writeConfig writes content to a temp config file.
func writeConfig(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

func writeConfigAtomically(t *testing.T, path, content string) {
	t.Helper()
	temporary, err := os.CreateTemp(filepath.Dir(path), ".mcp-config-*")
	require.NoError(t, err)
	temporaryPath := temporary.Name()
	t.Cleanup(func() { _ = os.Remove(temporaryPath) })
	defer temporary.Close()

	_, err = temporary.WriteString(content)
	require.NoError(t, err)
	require.NoError(t, temporary.Sync())
	require.NoError(t, temporary.Close())
	require.NoError(t, os.Rename(temporaryPath, path))
}

func TestHotReloaderWatchDetectsChange(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "mcp.json")
	writeConfig(t, cfgPath, `{"version":"1"}`)

	client := newRecordingMCPClient("srv", []MCPTool{{Name: "tool_a", Description: "a"}})
	registered := make(chan []MCPTool, 4)
	hr := NewDefaultHotReloader(client, func(tools []MCPTool) {
		registered <- tools
	}, WithPollInterval(10*time.Millisecond))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, hr.Watch(ctx, cfgPath))

	// Modify the file; the poller should detect it and reconnect.
	writeConfig(t, cfgPath, `{"version":"2"}`)

	assertReconnectCycle(t, client)
	select {
	case tools := <-registered:
		require.Len(t, tools, 1)
		assert.Equal(t, "tool_a", tools[0].Name)
	case <-time.After(2 * time.Second):
		t.Fatal("expected tools to be re-registered after config change")
	}

	require.NoError(t, hr.Stop())
}

func TestHotReloaderExponentialBackoffRecovers(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "mcp.json")
	writeConfig(t, cfgPath, `{"version":"1"}`)

	client := newRecordingMCPClient("srv", []MCPTool{{Name: "tool_a", Description: "a"}})
	client.failConnects = 2 // first two Connect attempts fail, then succeed

	hr := NewDefaultHotReloader(client, func(_ []MCPTool) {},
		WithPollInterval(10*time.Millisecond),
		WithBackoffBase(5*time.Millisecond),
		WithMaxRetries(5),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, hr.Watch(ctx, cfgPath))

	// Trigger a manual reload; Connect fails twice, retries with backoff, then
	// succeeds. Eventually the client must be both connected and listed.
	require.NoError(t, hr.Reload(ctx))

	require.Eventually(t, func() bool {
		connects, _, lists := client.counts()
		return connects >= 3 && lists >= 1
	}, 2*time.Second, 5*time.Millisecond, "expected retries to recover")
	assert.True(t, client.reconnected())

	require.NoError(t, hr.Stop())
}

func TestHotReloaderGivesUpAfterMaxRetries(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "mcp.json")
	writeConfig(t, cfgPath, `{"version":"1"}`)

	client := newRecordingMCPClient("srv", []MCPTool{{Name: "tool_a", Description: "a"}})
	client.failConnects = 4 // initial + 3 retries all fail, the 5th attempt wins

	hr := NewDefaultHotReloader(client, func(_ []MCPTool) {},
		WithPollInterval(5*time.Millisecond),
		WithBackoffBase(1*time.Millisecond),
		WithMaxRetries(3),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, hr.Watch(ctx, cfgPath))

	require.NoError(t, hr.Reload(ctx))

	// The reloader attempts initial + up to maxRetries retries, then gives up.
	require.Eventually(t, func() bool {
		connects, _, lists := client.counts()
		return connects == 4 && lists == 0
	}, 2*time.Second, 5*time.Millisecond, "expected initial + 3 failed retries with no tool list")

	// It stays in a degraded watching state and a later config change recovers,
	// this time succeeding (failConnects exhausted) and listing tools.
	writeConfig(t, cfgPath, `{"version":"2"}`)
	assertReconnectCycle(t, client)

	require.NoError(t, hr.Stop())
}

func TestHotReloaderStopStopsPoller(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "mcp.json")
	writeConfig(t, cfgPath, `{"version":"1"}`)

	client := newRecordingMCPClient("srv", nil)
	hr := NewDefaultHotReloader(client, func(_ []MCPTool) {},
		WithPollInterval(10*time.Millisecond))

	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, hr.Watch(ctx, cfgPath))
	cancel()

	// Stop must return and the poller must have exited cleanly. The deferred
	// goroutine-leak check confirms no poller goroutine survives.
	require.NoError(t, hr.Stop())

	// After Stop the reloader refuses to run a manual reload.
	err := hr.Reload(context.Background())
	require.Error(t, err)

	// Stop is idempotent.
	require.NoError(t, hr.Stop())
}

func TestHotReloaderManualReloadTriggersCycle(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "mcp.json")
	writeConfig(t, cfgPath, `{"version":"1"}`)

	client := newRecordingMCPClient("srv", []MCPTool{{Name: "tool_a", Description: "a"}})
	hr := NewDefaultHotReloader(client, func(_ []MCPTool) {},
		WithPollInterval(50*time.Millisecond))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, hr.Watch(ctx, cfgPath))

	require.NoError(t, hr.Reload(ctx))
	assertReconnectCycle(t, client)

	require.NoError(t, hr.Stop())
}

func TestHotReloaderEmitsSpan(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "mcp.json")
	writeConfig(t, cfgPath, `{"version":"1"}`)

	client := newRecordingMCPClient("srv", []MCPTool{{Name: "tool_a", Description: "a"}})
	hr := NewDefaultHotReloader(client, func(_ []MCPTool) {},
		WithPollInterval(20*time.Millisecond),
		WithTransport(MCPTransportStdio))

	exporter := &recordingExporter{}
	tracer := tracing.NewTracer("trace-hot-reload", exporter)
	tctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	span, sctx := tracer.Start(tctx, "test.hot_reload.root", tracing.SpanKindInternal)
	defer span.End()

	require.NoError(t, hr.Watch(sctx, cfgPath))
	require.NoError(t, hr.Reload(sctx))

	require.Eventually(t, func() bool {
		for _, s := range exporter.Spans() {
			if s.Name == "mcp.hot_reload" {
				return true
			}
		}
		return false
	}, 2*time.Second, 5*time.Millisecond, "expected mcp.hot_reload span")

	found := false
	for _, s := range exporter.Spans() {
		if s.Name != "mcp.hot_reload" {
			continue
		}
		attrs := map[string]any{}
		for _, a := range s.Attributes {
			attrs[a.Key] = a.Value
		}
		assert.Equal(t, cfgPath, attrs["config_path"])
		assert.Equal(t, "stdio", attrs["transport"])
		assert.Equal(t, 1, attrs["reconnect_count"])
		assert.Equal(t, true, attrs["success"])
		found = true
	}
	assert.True(t, found, "mcp.hot_reload span with attributes was exported")

	require.NoError(t, hr.Stop())
}

func TestHotReloaderRegistry(t *testing.T) {
	reg := NewMCPClientRegistry()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "mcp.json")
	writeConfig(t, cfgPath, `{"version":"1"}`)
	client := newRecordingMCPClient("srv", nil)
	hr := NewDefaultHotReloader(client, func(_ []MCPTool) {}, WithPollInterval(time.Hour))

	require.NoError(t, reg.RegisterHotReloader("srv", hr))

	got, ok := reg.HotReloader("srv")
	require.True(t, ok)
	assert.Equal(t, "mcp-hot-reloader", got.Name())

	_, ok = reg.HotReloader("missing")
	assert.False(t, ok)

	require.Error(t, reg.RegisterHotReloader("", hr))
	require.Error(t, reg.RegisterHotReloader("other", nil))
}

func TestHotReloaderWaitDoesNotRequestStop(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "mcp.json")
	writeConfig(t, cfgPath, `{"version":"1"}`)

	client := newRecordingMCPClient("srv", nil)
	hr := NewDefaultHotReloader(client, func(_ []MCPTool) {}, WithPollInterval(time.Hour))
	lifecycle, ok := hr.(HotReloadLifecycle)
	require.True(t, ok, "default reloader exposes the lifecycle capability")
	done := lifecycle.Done()

	waitCtx, cancelWait := context.WithTimeout(context.Background(), 10*time.Millisecond)
	err := lifecycle.Wait(waitCtx)
	cancelWait()
	require.ErrorIs(t, err, ErrHotReloadPending)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	select {
	case <-done:
		t.Fatal("Wait must not implicitly request stop")
	default:
	}

	watchCtx, cancelWatch := context.WithCancel(context.Background())
	defer cancelWatch()
	require.NoError(t, hr.Watch(watchCtx, cfgPath))
	require.NoError(t, hr.Reload(context.Background()), "Wait timeout must not close reload admission")
	assertReconnectCycle(t, client)

	lifecycle.RequestStop()
	stopCtx, cancelStop := context.WithTimeout(context.Background(), time.Second)
	defer cancelStop()
	require.NoError(t, lifecycle.Wait(stopCtx))
}

func TestHotReloaderRequestStopBeforeWatchClosesStableDone(t *testing.T) {
	hr := NewDefaultHotReloader(newRecordingMCPClient("srv", nil), func(_ []MCPTool) {})
	lifecycle, ok := hr.(HotReloadLifecycle)
	require.True(t, ok)
	done := lifecycle.Done()

	lifecycle.RequestStop()
	select {
	case <-done:
	default:
		t.Fatal("stopping an unwatched reloader should complete immediately")
	}
	lifecycle.RequestStop()
	assert.Equal(t, done, lifecycle.Done(), "Done must be stable across repeated stop requests")
	require.NoError(t, lifecycle.Wait(context.Background()))

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "mcp.json")
	writeConfig(t, cfgPath, `{"version":"1"}`)
	require.Error(t, hr.Watch(context.Background(), cfgPath), "Watch after the stop fence must not publish a poller")
}

func TestConfigHotReloaderRecordsFailureAndRetriesOnLaterTrigger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"servers":[]}`), 0o600))
	secret := "candidate rejected: credential=top-secret-value"
	wantFailure := errors.New(secret)
	var calls atomic.Int32
	started := make(chan int32, 2)
	logCapturer := verify.NewLogCapturer()
	logCtx := logCapturer.Attach(context.Background())
	t.Cleanup(logCapturer.Detach)
	traceExporter := &configReloadTraceCapture{spans: make(chan tracing.SpanData, 4)}
	tracer := tracing.NewTracer("config-reload-test", traceExporter)
	parentSpan, reloadCtx := tracer.Start(logCtx, "config-reload-test", tracing.SpanKindInternal)
	t.Cleanup(parentSpan.End)
	reloader := NewConfigHotReloader(func(context.Context, string) error {
		call := calls.Add(1)
		started <- call
		if call == 1 {
			return wantFailure
		}
		return nil
	}, WithPollInterval(5*time.Millisecond))
	require.Equal(t, "mcp-config-hot-reloader", reloader.Name())
	require.NoError(t, reloader.Watch(reloadCtx, path))
	t.Cleanup(func() { assert.NoError(t, reloader.Stop()) })

	require.NoError(t, reloader.Reload(reloadCtx))
	select {
	case call := <-started:
		assert.EqualValues(t, 1, call)
	case <-time.After(time.Second):
		t.Fatal("config reload callback did not run")
	}
	first := waitForConfigReloadAttempt(t, reloader, func(attempt MCPConfigReloadAttempt) bool {
		return attempt.ErrorClass == MCPConfigReloadErrorCandidate
	})
	assert.Equal(t, MCPConfigReloadErrorCandidate, first.ErrorClass)
	assert.NotContains(t, fmt.Sprint(first), secret, "attempt status must not retain raw callback error text")
	failureSpan := waitForConfigReloadTrace(t, traceExporter.spans, "mcp.hot_reload")
	assert.NotContains(t, fmt.Sprint(failureSpan), secret, "trace span must not contain raw callback error text")
	assert.NotContains(t, fmt.Sprint(logCapturer.Entries()), secret, "logs must not contain raw callback error text")
	time.Sleep(20 * time.Millisecond)
	assert.EqualValues(t, 1, calls.Load(), "an ordinary candidate error is recorded once without stopping the watcher or retrying in a loop")
	select {
	case <-reloader.Done():
		t.Fatal("ordinary candidate failure stopped the config watcher")
	default:
	}

	writeConfigAtomically(t, path, `{"servers":[{"name":"changed"}]}`)
	select {
	case call := <-started:
		assert.EqualValues(t, 2, call)
	case <-time.After(time.Second):
		t.Fatal("a later selected-source file change did not retry the config generation")
	}
	second := waitForConfigReloadAttempt(t, reloader, func(attempt MCPConfigReloadAttempt) bool {
		return attempt.ErrorClass == "" && attempt.CompletedAt.After(first.CompletedAt)
	})
	assert.Empty(t, second.ErrorClass)
	assert.EqualValues(t, 2, calls.Load())
}

func TestConfigHotReloaderStopsAfterCallbackPanic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"servers":[]}`), 0o600))
	reloader := NewConfigHotReloader(func(context.Context, string) error {
		panic("sensitive callback detail")
	}, WithPollInterval(time.Hour))
	require.NoError(t, reloader.Watch(context.Background(), path))
	require.NoError(t, reloader.Reload(context.Background()))

	attempt := waitForConfigReloadAttempt(t, reloader, func(attempt MCPConfigReloadAttempt) bool {
		return attempt.ErrorClass == MCPConfigReloadErrorPanic
	})
	assert.Equal(t, MCPConfigReloadErrorPanic, attempt.ErrorClass)
	assert.NotContains(t, fmt.Sprint(attempt), "sensitive callback detail")
	select {
	case <-reloader.Done():
	case <-time.After(time.Second):
		t.Fatal("config reloader did not stop after its callback panicked")
	}
	assert.ErrorIs(t, reloader.Stop(), ErrHotReloadCallbackPanic)
}

type configReloadTraceCapture struct {
	spans chan tracing.SpanData
}

func (e *configReloadTraceCapture) ExportSpan(_ context.Context, span tracing.TraceSpan) error {
	e.spans <- tracing.SpanToData(span)
	return nil
}

func (*configReloadTraceCapture) Shutdown(context.Context) error { return nil }

func waitForConfigReloadTrace(t *testing.T, spans <-chan tracing.SpanData, name string) tracing.SpanData {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		select {
		case span := <-spans:
			if span.Name == name {
				return span
			}
		case <-deadline:
			t.Fatalf("trace span %q was not exported", name)
			return tracing.SpanData{}
		}
	}
}

func waitForConfigReloadAttempt(t *testing.T, reloader *DefaultHotReloader, matches func(MCPConfigReloadAttempt) bool) MCPConfigReloadAttempt {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		attempt, ok := reloader.LastAttempt()
		if ok && matches(attempt) {
			return attempt
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("config reload attempt status did not reach the expected result")
	return MCPConfigReloadAttempt{}
}

func TestHotReloaderRequestStopJoinsReconnectAndRegistrationCallback(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "mcp.json")
	writeConfig(t, cfgPath, `{"version":"1"}`)
	client := newRecordingMCPClient("srv", []MCPTool{{Name: "echo"}})
	callbackStarted := make(chan struct{})
	callbackRelease := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(callbackRelease) }) }
	defer release()

	hr := NewDefaultHotReloader(client, func(_ []MCPTool) {
		close(callbackStarted)
		<-callbackRelease
	}, WithPollInterval(time.Hour))
	lifecycle, ok := hr.(HotReloadLifecycle)
	require.True(t, ok)
	watchCtx, cancelWatch := context.WithCancel(context.Background())
	defer cancelWatch()
	require.NoError(t, hr.Watch(watchCtx, cfgPath))
	require.NoError(t, hr.Reload(context.Background()))
	select {
	case <-callbackStarted:
	case <-time.After(time.Second):
		t.Fatal("registration callback did not start")
	}

	done := lifecycle.Done()
	requestReturned := make(chan struct{})
	go func() {
		lifecycle.RequestStop()
		close(requestReturned)
	}()
	select {
	case <-requestReturned:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("RequestStop waited for the in-flight callback")
	}
	select {
	case <-done:
		t.Fatal("actual Done closed before the registration callback returned")
	default:
	}

	waitCtx, cancelWait := context.WithTimeout(context.Background(), 20*time.Millisecond)
	err := lifecycle.Wait(waitCtx)
	cancelWait()
	require.ErrorIs(t, err, ErrHotReloadPending)
	require.ErrorIs(t, err, context.DeadlineExceeded)

	stopReturned := make(chan error, 1)
	go func() { stopReturned <- hr.Stop() }()
	select {
	case err := <-stopReturned:
		t.Fatalf("Stop returned before callback join: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	release()
	finalWaitCtx, cancelFinalWait := context.WithTimeout(context.Background(), time.Second)
	defer cancelFinalWait()
	require.NoError(t, lifecycle.Wait(finalWaitCtx))
	select {
	case err := <-stopReturned:
		require.NoError(t, err)
	case <-finalWaitCtx.Done():
		t.Fatal("Stop did not return after callback joined")
	}
	select {
	case <-done:
	default:
		t.Fatal("Done did not close after all reconnect work returned")
	}
}

func TestHotReloaderRequestStopJoinsActiveMCPCall(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "mcp.json")
	writeConfig(t, cfgPath, `{"version":"1"}`)
	baseClient := newRecordingMCPClient("srv", []MCPTool{{Name: "echo"}})
	client := &blockingConnectMCPClient{
		recordingMCPClient: baseClient,
		connectStarted:     make(chan struct{}),
		connectRelease:     make(chan struct{}),
	}
	var registered atomic.Int32
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(client.connectRelease) }) }
	defer release()

	hr := NewDefaultHotReloader(client, func(_ []MCPTool) { registered.Add(1) }, WithPollInterval(time.Hour))
	lifecycle, ok := hr.(HotReloadLifecycle)
	require.True(t, ok)
	watchCtx, cancelWatch := context.WithCancel(context.Background())
	defer cancelWatch()
	require.NoError(t, hr.Watch(watchCtx, cfgPath))
	require.NoError(t, hr.Reload(context.Background()))
	select {
	case <-client.connectStarted:
	case <-time.After(time.Second):
		t.Fatal("reconnect Connect call did not start")
	}

	lifecycle.RequestStop()
	waitCtx, cancelWait := context.WithTimeout(context.Background(), 20*time.Millisecond)
	err := lifecycle.Wait(waitCtx)
	cancelWait()
	require.ErrorIs(t, err, ErrHotReloadPending, "Done must include an uncooperative MCP call")
	require.ErrorIs(t, err, context.DeadlineExceeded)

	release()
	finalWaitCtx, cancelFinalWait := context.WithTimeout(context.Background(), time.Second)
	defer cancelFinalWait()
	require.NoError(t, lifecycle.Wait(finalWaitCtx))
	assert.Zero(t, registered.Load(), "cancellation after Connect must not start ListTools or registration")
	connects, disconnects, lists := baseClient.counts()
	assert.Equal(t, 1, connects)
	assert.Equal(t, 1, disconnects)
	assert.Zero(t, lists)
}

func TestHotReloaderPendingChangeDoesNotRescheduleAfterStop(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "mcp.json")
	writeConfig(t, cfgPath, `{"version":"1"}`)
	client := newRecordingMCPClient("srv", []MCPTool{{Name: "echo"}})
	callbackStarted := make(chan struct{})
	callbackRelease := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(callbackRelease) }) }
	defer release()
	var callbacks atomic.Int32
	hr := NewDefaultHotReloader(client, func(_ []MCPTool) {
		callbacks.Add(1)
		if callbacks.Load() == 1 {
			close(callbackStarted)
			<-callbackRelease
		}
	}, WithPollInterval(5*time.Millisecond), WithBackoffBase(time.Millisecond))
	lifecycle, ok := hr.(HotReloadLifecycle)
	require.True(t, ok)
	watchCtx, cancelWatch := context.WithCancel(context.Background())
	defer cancelWatch()
	require.NoError(t, hr.Watch(watchCtx, cfgPath))
	require.NoError(t, hr.Reload(context.Background()))
	select {
	case <-callbackStarted:
	case <-time.After(time.Second):
		t.Fatal("registration callback did not start")
	}

	writeConfig(t, cfgPath, `{"version":"2"}`)
	impl := hr.(*DefaultHotReloader)
	require.Eventually(t, func() bool {
		impl.mu.Lock()
		defer impl.mu.Unlock()
		return impl.pending
	}, time.Second, time.Millisecond, "poller should record a pending change while the callback runs")

	lifecycle.RequestStop()
	release()
	waitCtx, cancelWait := context.WithTimeout(context.Background(), time.Second)
	defer cancelWait()
	require.NoError(t, lifecycle.Wait(waitCtx))
	assert.EqualValues(t, 1, callbacks.Load(), "pending reload must not be scheduled after the stop fence")
	connects, disconnects, lists := client.counts()
	assert.Equal(t, 1, connects)
	assert.Equal(t, 1, disconnects)
	assert.Equal(t, 1, lists)
}

func TestHotReloaderRequestStopRacingWatchHasNoLateOwner(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	for i := 0; i < 25; i++ {
		t.Run(fmt.Sprintf("iteration-%02d", i), func(t *testing.T) {
			dir := t.TempDir()
			cfgPath := filepath.Join(dir, "mcp.json")
			writeConfig(t, cfgPath, `{"version":"1"}`)
			hr := NewDefaultHotReloader(newRecordingMCPClient("srv", nil), func(_ []MCPTool) {}, WithPollInterval(time.Hour))
			lifecycle, ok := hr.(HotReloadLifecycle)
			require.True(t, ok)
			start := make(chan struct{})
			watchResult := make(chan error, 1)
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				watchResult <- hr.Watch(context.Background(), cfgPath)
			}()
			go func() {
				defer wg.Done()
				<-start
				lifecycle.RequestStop()
			}()
			close(start)
			wg.Wait()
			watchErr := <-watchResult
			if watchErr != nil {
				require.EqualError(t, watchErr, "mcp: hot reload: already stopped")
			}
			waitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			require.NoError(t, lifecycle.Wait(waitCtx))
			select {
			case <-lifecycle.Done():
			default:
				t.Fatal("completed Watch/Stop race left Done open")
			}
			require.Error(t, hr.Reload(context.Background()), "a completed stop must keep reload admission closed")
		})
	}
}

func TestHotReloaderInvalidPollIntervalIsRejectedWithoutOwnerOrPanic(t *testing.T) {
	const childModeEnv = "GOCLI_TEST_HOT_RELOAD_INVALID_POLL_INTERVAL"
	if childMode := os.Getenv(childModeEnv); childMode != "" {
		interval := time.Duration(0)
		if childMode == "negative" {
			interval = -time.Second
		}
		t.Run(childMode, func(t *testing.T) {
			dir := t.TempDir()
			configPath := filepath.Join(dir, "mcp.json")
			writeConfig(t, configPath, `{"version":"1"}`)

			invalid := NewDefaultHotReloader(newRecordingMCPClient("invalid", nil), func(_ []MCPTool) {}, WithPollInterval(interval)).(*DefaultHotReloader)
			lifecycle := HotReloadLifecycle(invalid)
			err := invalid.Watch(context.Background(), configPath)
			require.ErrorIs(t, err, ErrHotReloadInvalidPollInterval)
			invalid.mu.Lock()
			assert.False(t, invalid.pollStarted, "invalid Watch must not publish a poll owner")
			assert.Nil(t, invalid.pollCancel, "invalid Watch must not consume watch state")
			assert.Empty(t, invalid.path)
			assert.False(t, invalid.baseline)
			assert.False(t, invalid.stopped)
			invalid.mu.Unlock()

			lifecycle.RequestStop()
			waitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			require.NoError(t, lifecycle.Wait(waitCtx))
			select {
			case <-lifecycle.Done():
			default:
				t.Fatal("RequestStop after a rejected Watch must close Done without an owner")
			}

			reusable := NewDefaultHotReloader(newRecordingMCPClient("reusable", nil), func(_ []MCPTool) {}, WithPollInterval(interval)).(*DefaultHotReloader)
			require.ErrorIs(t, reusable.Watch(context.Background(), configPath), ErrHotReloadInvalidPollInterval)
			reusable.pollInterval = defaultPollInterval
			require.NoError(t, reusable.Watch(context.Background(), configPath), "rejected Watch must leave state reusable")
			reusableLife := HotReloadLifecycle(reusable)
			reusableLife.RequestStop()
			require.NoError(t, reusableLife.Wait(waitCtx))
		})
		return
	}

	defer verify.AssertNoGoroutineLeak(t)()
	for _, childMode := range []string{"zero", "negative"} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHotReloaderInvalidPollIntervalIsRejectedWithoutOwnerOrPanic$")
		cmd.Env = append(os.Environ(), childModeEnv+"="+childMode)
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, "invalid interval child %s failed; output:\n%s", childMode, output)
	}
}

func TestHotReloaderRegistrationPanicIsSanitizedAndJoined(t *testing.T) {
	const helperEnv = "GO_CLI_HOT_RELOAD_PANIC_HELPER"
	if os.Getenv(helperEnv) == "1" {
		testHotReloaderRegistrationPanicHelper(t)
		return
	}

	helperCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(helperCtx, os.Args[0], "-test.run=^TestHotReloaderRegistrationPanicIsSanitizedAndJoined$")
	cmd.Env = append(os.Environ(), helperEnv+"=1")
	output, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "panic helper subprocess failed: %s", output)
}

func testHotReloaderRegistrationPanicHelper(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "mcp.json")
	writeConfig(t, cfgPath, `{"version":"1"}`)
	client := newRecordingMCPClient("srv", []MCPTool{{Name: "echo"}})
	callbackStarted := make(chan struct{})
	panicRelease := make(chan struct{})
	var panicReleaseOnce sync.Once
	releasePanic := func() { panicReleaseOnce.Do(func() { close(panicRelease) }) }
	defer releasePanic()
	register := func(_ []MCPTool) {
		close(callbackStarted)
		<-panicRelease
		panic("credential-panic-payload")
	}
	hr := NewDefaultHotReloader(client, register, WithPollInterval(time.Hour))
	lifecycle, ok := hr.(HotReloadLifecycle)
	require.True(t, ok)
	exporter := &recordingExporter{}
	tracer := tracing.NewTracer("hot-reload-panic", exporter)
	rootSpan, ctx := tracer.Start(context.Background(), "test.root", tracing.SpanKindInternal)
	defer rootSpan.End()
	watchCtx, cancelWatch := context.WithCancel(ctx)
	defer cancelWatch()
	require.NoError(t, hr.Watch(watchCtx, cfgPath))
	require.NoError(t, hr.Reload(context.Background()))
	select {
	case <-callbackStarted:
	case <-time.After(time.Second):
		t.Fatal("registration callback did not start")
	}

	lifecycle.RequestStop()
	releasePanic()
	waitCtx, cancelWait := context.WithTimeout(context.Background(), time.Second)
	defer cancelWait()
	err := lifecycle.Wait(waitCtx)
	var panicErr *HotReloadCallbackPanicError
	require.ErrorAs(t, err, &panicErr)
	require.ErrorIs(t, err, ErrHotReloadCallbackPanic)
	assert.NotContains(t, err.Error(), "credential-panic-payload")
	select {
	case <-lifecycle.Done():
	default:
		t.Fatal("Done must close only after the panicking callback unwinds and reconnect span ends")
	}
	foundHotReloadSpan := false
	for _, span := range exporter.Spans() {
		if span.Name == "mcp.hot_reload" {
			foundHotReloadSpan = true
		}
	}
	assert.True(t, foundHotReloadSpan, "the reconnect span must be exported before Done closes")
	assert.ErrorIs(t, hr.Stop(), ErrHotReloadCallbackPanic)
}
