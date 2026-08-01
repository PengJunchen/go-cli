package mcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
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

// counts returns a snapshot of the lifecycle call counters.
func (c *recordingMCPClient) counts() (connects, disconnects, lists int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connects, c.disconnects, c.lists
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
