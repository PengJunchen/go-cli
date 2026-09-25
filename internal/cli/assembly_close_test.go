package cli

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pengjunchen/go-cli/internal/acp"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/extension"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/mcp"
	"github.com/stretchr/testify/require"
)

type assemblyContextCloser interface {
	Close(context.Context) error
}

func TestAssemblyCloseJoinsMemoryProducerBeforeEventBusClose(t *testing.T) {
	bus := core.NewMemoryEventBus()
	eventCtx, cancelEvents := context.WithCancel(context.Background())
	defer cancelEvents()
	events := bus.Subscribe(eventCtx)

	releaseWorker := make(chan struct{})
	var state assembleState
	state.eventBus = bus
	state.memWG.Add(1)
	go func() {
		defer state.memWG.Done()
		<-releaseWorker
		bus.Publish(core.AgentEvent{Kind: "memory-extraction-finished"})
	}()

	idle := make(chan struct{})
	close(idle)
	closeDone := make(chan error, 1)
	go func() { closeDone <- state.closeResources(context.Background(), idle) }()
	select {
	case _, ok := <-events:
		if !ok {
			t.Fatal("event bus closed while the tracked memory producer was still active")
		}
	case <-time.After(20 * time.Millisecond):
	}

	close(releaseWorker)
	select {
	case event, ok := <-events:
		require.True(t, ok, "joined producer event must reach subscribers before bus close")
		require.Equal(t, "memory-extraction-finished", event.Kind)
	case <-time.After(time.Second):
		t.Fatal("tracked producer did not publish its final event")
	}
	require.NoError(t, <-closeDone)
	_, ok := <-events
	require.False(t, ok, "event bus closes after the producer is joined")
}

func TestAssemblyCloseJoinsPluginBeforeDisconnectingMCPClient(t *testing.T) {
	ext := &assemblyBarrierExtension{shutdownStarted: make(chan struct{}), releaseShutdown: make(chan struct{})}
	manager := extension.NewPluginManager(assemblyTestPluginLoader{extension: ext})
	require.NoError(t, manager.Load(context.Background(), []string{"test-plugin"}))
	require.NoError(t, manager.Init(context.Background()))

	client := &assemblyTestMCPClient{
		disconnectStarted: make(chan struct{}),
		done:              make(chan struct{}),
	}
	reloader := newAssemblyBarrierReloader()
	resources := &assemblyMCPResources{}
	resources.addClient(client)
	require.NoError(t, resources.addReloader(reloader))

	state := &assembleState{mcpResources: resources, pluginManagers: []*extension.PluginManager{manager}}
	owner := newAssemblyCloseOwner(
		func() core.RunSlotGuard { return core.NewDefaultRunSlotGuard() },
		nil,
		state.requestStopSources,
		state.closeResources,
	)

	waiterCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	require.ErrorIs(t, owner.Close(waiterCtx), ErrAssemblyClosePending)
	cancel()
	<-reloader.stopRequested
	select {
	case <-ext.shutdownStarted:
		t.Fatal("plugin shutdown began before the MCP reloader joined")
	case <-client.disconnectStarted:
		t.Fatal("MCP client disconnected before plugin shutdown")
	case <-time.After(20 * time.Millisecond):
	}

	close(reloader.releaseDone)
	select {
	case <-ext.shutdownStarted:
	case <-time.After(time.Second):
		t.Fatal("plugin shutdown did not start after the reloader joined")
	}
	select {
	case <-client.disconnectStarted:
		t.Fatal("plugin still owns a live callback that may use the MCP client")
	case <-time.After(20 * time.Millisecond):
	}

	close(ext.releaseShutdown)
	select {
	case <-client.disconnectStarted:
	case <-time.After(time.Second):
		t.Fatal("MCP client was not disconnected after plugin shutdown joined")
	}
	close(client.done)
	require.NoError(t, owner.Close(context.Background()))
}

func TestAssemblyCloseConsumesRealACPPendingCleanupSignal(t *testing.T) {
	streamStarted := make(chan struct{})
	closeStarted := make(chan struct{})
	streamWrapped := make(chan struct{})
	releaseClose := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseClose) }) }
	baseTransport := http.DefaultTransport
	http.DefaultTransport = &assemblyACPBodyCloseTransport{
		base:          baseTransport,
		closeStarted:  closeStarted,
		streamWrapped: streamWrapped,
		releaseClose:  releaseClose,
	}
	t.Cleanup(func() { http.DefaultTransport = baseTransport })

	var disconnectRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/connect":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"version":"go-cli-acp-json/1","session_id":"assembly-owned-session"}`)
		case "/stream":
			close(streamStarted)
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(http.StatusOK)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			<-r.Context().Done()
		case "/disconnect":
			disconnectRequests.Add(1)
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	client := acp.NewGRPCAdapter(server.URL, acp.WithName("assembly-close"))
	t.Cleanup(func() {
		release()
		_ = client.Disconnect(context.Background())
		server.Close()
	})
	require.NoError(t, client.Connect(context.Background()))
	select {
	case <-streamStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("ACP stream request was not dispatched")
	}
	select {
	case <-streamWrapped:
	case <-time.After(time.Second):
		t.Fatal("ACP stream response did not pass through the gated transport")
	}

	observed := &assemblyObservedACPClient{ACPClient: client, results: make(chan error, 2)}
	bus := core.NewMemoryEventBus()
	defer bus.Close()
	events := bus.Subscribe(context.Background())
	state := &assembleState{acpClient: observed, eventBus: bus}
	idle := make(chan struct{})
	close(idle)
	closeCtx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	closeResult := make(chan error, 1)
	go func() { closeResult <- state.closeResources(closeCtx, idle) }()
	var disconnectErr error
	select {
	case disconnectErr = <-observed.results:
	case <-time.After(time.Second):
		t.Fatal("Assembly did not pass its bounded owner context to ACP Disconnect")
	}
	require.ErrorIs(t, disconnectErr, context.DeadlineExceeded)
	var pending interface{ Done() <-chan struct{} }
	require.ErrorAs(t, disconnectErr, &pending, "real ACP cleanup timeout must expose its exact join channel")
	done := pending.Done()
	require.NotNil(t, done)
	select {
	case <-done:
		t.Fatal("ACP completion closed while the real response body Close remained blocked")
	default:
	}
	select {
	case <-closeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("ACP stream response body was not closed by its reader")
	}
	select {
	case <-done:
		t.Fatal("ACP completion closed while the real response body Close remained blocked")
	default:
	}
	select {
	case err := <-closeResult:
		t.Fatalf("Assembly returned before joining ACP body close: %v", err)
	case _, ok := <-events:
		if !ok {
			t.Fatal("Assembly closed the event bus before ACP cleanup joined")
		}
	case <-time.After(20 * time.Millisecond):
	}

	release()
	select {
	case err := <-closeResult:
		require.ErrorIs(t, err, context.DeadlineExceeded, "the joined remote cleanup failure remains visible")
		require.ErrorIs(t, err, ErrAssemblyClientCompletionUnknown,
			"local body cleanup does not prove the remote session was deleted")
	case <-time.After(2 * time.Second):
		t.Fatal("Assembly did not finish after the exact ACP cleanup channel closed")
	}
	select {
	case <-done:
	default:
		t.Fatal("ACP cleanup completion did not close after response body Close returned")
	}
	select {
	case _, ok := <-events:
		require.True(t, ok, "unknown remote cleanup must retain downstream Assembly resources")
	default:
	}
	require.Zero(t, disconnectRequests.Load(), "expired ACP cleanup owner must preserve the adapter fence")
}

func TestAssemblyCloseOwnerKeepsOneSequenceAcrossWaiters(t *testing.T) {
	guard := core.NewDefaultRunSlotGuard()
	blocked := make(chan struct{})
	closeEntered := make(chan struct{})
	var closeCalls atomic.Int32
	var stopCalls atomic.Int32
	owner := newAssemblyCloseOwner(
		func() core.RunSlotGuard { return guard },
		func() {},
		func() { stopCalls.Add(1) },
		func(context.Context, <-chan struct{}) error {
			closeCalls.Add(1)
			close(closeEntered)
			<-blocked
			return nil
		},
	)

	waiterCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	require.ErrorIs(t, owner.Close(waiterCtx), ErrAssemblyClosePending)
	cancel()
	<-closeEntered

	secondWaiterCtx, cancelSecond := context.WithTimeout(context.Background(), 20*time.Millisecond)
	require.ErrorIs(t, owner.Close(secondWaiterCtx), ErrAssemblyClosePending)
	cancelSecond()
	require.EqualValues(t, 1, closeCalls.Load(), "waiters must share the original close sequence")
	require.EqualValues(t, 1, stopCalls.Load(), "close initiation must run exactly once")

	close(blocked)
	require.NoError(t, owner.Close(context.Background()))
	require.EqualValues(t, 1, closeCalls.Load())
}

func TestAssemblyCloseFencesBeforeBlockedStopCallback(t *testing.T) {
	guard := core.NewDefaultRunSlotGuard()
	stopEntered := make(chan struct{})
	releaseStop := make(chan struct{})
	resourcesClosed := make(chan struct{})
	owner := newAssemblyCloseOwner(
		func() core.RunSlotGuard { return guard },
		func() {},
		func() {
			close(stopEntered)
			<-releaseStop
		},
		func(context.Context, <-chan struct{}) error {
			close(resourcesClosed)
			return nil
		},
	)

	waiterCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	closeErr := owner.Close(waiterCtx)
	cancel()
	require.ErrorIs(t, closeErr, ErrAssemblyClosePending)
	<-stopEntered

	_, claimErr := guard.ClaimRun(context.Background())
	require.ErrorIs(t, claimErr, core.ErrRunAdmissionClosed, "the synchronous Assembly fence must reject claims while stop is blocked")

	close(releaseStop)
	require.NoError(t, owner.Close(context.Background()))
	<-resourcesClosed
}

func TestAssemblyClosePanicRetainsPendingOwner(t *testing.T) {
	resourcesStarted := atomic.Bool{}
	owner := newAssemblyCloseOwner(
		func() core.RunSlotGuard { return core.NewDefaultRunSlotGuard() },
		func() {},
		func() { panic("private callback payload") },
		func(context.Context, <-chan struct{}) error {
			resourcesStarted.Store(true)
			return nil
		},
	)

	waiterCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	firstErr := owner.Close(waiterCtx)
	cancel()
	require.ErrorIs(t, firstErr, ErrAssemblyClosePending)
	require.NotContains(t, firstErr.Error(), "private callback payload")
	require.False(t, resourcesStarted.Load(), "a panic before dependency joins must not start teardown")

	secondWaiterCtx, cancelSecond := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelSecond()
	secondErr := owner.Close(secondWaiterCtx)
	require.ErrorIs(t, secondErr, ErrAssemblyClosePending)
	require.ErrorIs(t, secondErr, ErrAssemblyClosePanic)
	require.NotContains(t, secondErr.Error(), "private callback payload")
}

func TestAssemblyCloseCleanupPanicRetainsOwnerAndStopsLaterTeardown(t *testing.T) {
	bus := core.NewMemoryEventBus()
	events := bus.Subscribe(context.Background())
	panicReached := make(chan struct{})
	var laterCleanup atomic.Int32
	state := &assembleState{
		eventBus: bus,
		cleanupList: []func(context.Context) error{
			func(context.Context) error {
				laterCleanup.Add(1)
				return nil
			},
			func(context.Context) error {
				close(panicReached)
				panic("sensitive cleanup detail")
			},
		},
	}
	owner := newAssemblyCloseOwner(
		func() core.RunSlotGuard { return core.NewDefaultRunSlotGuard() },
		nil,
		nil,
		state.closeResources,
	)

	waitCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	closeErr := owner.Close(waitCtx)
	cancel()
	<-panicReached
	require.ErrorIs(t, closeErr, ErrAssemblyClosePending)
	require.ErrorIs(t, closeErr, ErrAssemblyClosePanic)
	require.NotContains(t, closeErr.Error(), "sensitive cleanup detail")
	require.Zero(t, laterCleanup.Load(), "a panicking cleanup must stop later dependency teardown")
	select {
	case _, ok := <-events:
		require.True(t, ok, "the event bus remains available while cleanup completion is unknown")
	default:
	}

	secondCtx, cancelSecond := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelSecond()
	require.ErrorIs(t, owner.Close(secondCtx), ErrAssemblyClosePending)
}

func TestAssemblyCloseModelDisposerFailureRetainsOwner(t *testing.T) {
	cleanupStarted := make(chan struct{})
	selector := llm.NewDefaultModelSelector(nil, nil).WithPrimaryCleanup(func() {
		close(cleanupStarted)
		panic("model cleanup detail")
	})
	state := &assembleState{modelSelector: selector}
	owner := newAssemblyCloseOwner(
		func() core.RunSlotGuard { return core.NewDefaultRunSlotGuard() },
		nil,
		nil,
		state.closeResources,
	)

	waitCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	closeErr := owner.Close(waitCtx)
	cancel()
	<-cleanupStarted
	require.ErrorIs(t, closeErr, ErrAssemblyClosePending)
	require.ErrorIs(t, closeErr, ErrAssemblyResourceCompletionUnknown)
	require.ErrorIs(t, closeErr, llm.ErrModelCleanupPanicked)
	require.NotContains(t, closeErr.Error(), "model cleanup detail")
	require.NotNil(t, state.modelDisposer, "the failed disposer remains retained by the close owner")
}

func TestAssemblyCloseUnknownCompletionRemainsPending(t *testing.T) {
	entered := make(chan struct{})
	owner := newAssemblyCloseOwner(
		func() core.RunSlotGuard { return core.NewDefaultRunSlotGuard() },
		func() {},
		func() {},
		func(context.Context, <-chan struct{}) error {
			close(entered)
			return ErrAssemblyClientCompletionUnknown
		},
	)

	waiterCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	firstErr := owner.Close(waiterCtx)
	cancel()
	<-entered
	require.ErrorIs(t, firstErr, ErrAssemblyClosePending)
	require.ErrorIs(t, firstErr, ErrAssemblyClientCompletionUnknown)

	secondWaiterCtx, cancelSecond := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelSecond()
	secondErr := owner.Close(secondWaiterCtx)
	require.ErrorIs(t, secondErr, ErrAssemblyClosePending)
	require.ErrorIs(t, secondErr, ErrAssemblyClientCompletionUnknown)
}

func TestAssemblyDoesNotTrustCustomClientNilDisconnectWithoutCompletion(t *testing.T) {
	mcpErr := disconnectAssemblyMCPClient(context.Background(), assemblyUnknownMCPClient{})
	require.ErrorIs(t, mcpErr, ErrAssemblyClientCompletionUnknown)

	acpErr := disconnectACPClient(context.Background(), assemblyUnknownACPClient{})
	require.ErrorIs(t, acpErr, ErrAssemblyClientCompletionUnknown)
}

func TestCloseAssemblyForCommandPropagatesCloseError(t *testing.T) {
	closeCause := errors.New("owned cleanup failed")
	assembly := &AgentAssembly{
		closeOwner: newAssemblyCloseOwner(
			func() core.RunSlotGuard { return nil },
			nil,
			nil,
			func(context.Context, <-chan struct{}) error { return closeCause },
		),
	}

	err := closeAssemblyForCommand(context.Background(), assembly, "test-command")
	require.ErrorIs(t, err, closeCause)
	require.Contains(t, err.Error(), "test-command: close assembly")
}

type assemblyTestPluginLoader struct{ extension extension.Extension }

func (assemblyTestPluginLoader) Name() string { return "assembly-test-loader" }
func (l assemblyTestPluginLoader) Load(context.Context, string) ([]extension.Extension, error) {
	return []extension.Extension{l.extension}, nil
}

type assemblyBarrierExtension struct {
	shutdownStarted chan struct{}
	releaseShutdown chan struct{}
}

func (*assemblyBarrierExtension) Name() string { return "assembly-barrier-extension" }
func (*assemblyBarrierExtension) Init(context.Context, extension.ExtensionRegistry) error {
	return nil
}
func (e *assemblyBarrierExtension) Shutdown(context.Context) error {
	close(e.shutdownStarted)
	<-e.releaseShutdown
	return nil
}

type assemblyTestMCPClient struct {
	disconnectStarted chan struct{}
	done              chan struct{}
	disconnectOnce    sync.Once
}

type assemblyUnknownMCPClient struct{}

func (assemblyUnknownMCPClient) Connect(context.Context) error    { return nil }
func (assemblyUnknownMCPClient) Disconnect(context.Context) error { return nil }
func (assemblyUnknownMCPClient) ListTools(context.Context) ([]mcp.MCPTool, error) {
	return nil, nil
}
func (assemblyUnknownMCPClient) CallTool(context.Context, string, map[string]any) (*mcp.MCPToolResult, error) {
	return nil, nil
}
func (assemblyUnknownMCPClient) Name() string            { return "unknown-mcp-client" }
func (assemblyUnknownMCPClient) ProtocolVersion() string { return "test" }

type assemblyUnknownACPClient struct{}

func (assemblyUnknownACPClient) Connect(context.Context) error    { return nil }
func (assemblyUnknownACPClient) Disconnect(context.Context) error { return nil }
func (assemblyUnknownACPClient) SendMessage(context.Context, acp.ACPMessage) error {
	return nil
}
func (assemblyUnknownACPClient) ReceiveMessages() <-chan acp.ACPMessage { return nil }
func (assemblyUnknownACPClient) Name() string                           { return "unknown-acp-client" }

type assemblyObservedACPClient struct {
	acp.ACPClient
	results chan error
}

func (c *assemblyObservedACPClient) Disconnect(ctx context.Context) error {
	err := c.ACPClient.Disconnect(ctx)
	c.results <- err
	return err
}

type assemblyACPBodyCloseTransport struct {
	base          http.RoundTripper
	closeStarted  chan struct{}
	streamWrapped chan struct{}
	releaseClose  <-chan struct{}
	closeOnce     sync.Once
}

func (t *assemblyACPBodyCloseTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(request)
	if err != nil || request.URL.Path != "/stream" {
		return response, err
	}
	close(t.streamWrapped)
	response.Body = &assemblyACPBodyCloseGate{
		ReadCloser:   response.Body,
		closeStarted: t.closeStarted,
		releaseClose: t.releaseClose,
		closeOnce:    &t.closeOnce,
	}
	return response, nil
}

type assemblyACPBodyCloseGate struct {
	io.ReadCloser
	closeStarted chan struct{}
	releaseClose <-chan struct{}
	closeOnce    *sync.Once
}

func (b *assemblyACPBodyCloseGate) Close() error {
	b.closeOnce.Do(func() {
		close(b.closeStarted)
		<-b.releaseClose
	})
	return b.ReadCloser.Close()
}

type assemblyMCPPendingError struct{ done <-chan struct{} }

func (*assemblyMCPPendingError) Error() string           { return "assembly test MCP disconnect pending" }
func (e *assemblyMCPPendingError) Done() <-chan struct{} { return e.done }

func (*assemblyTestMCPClient) Connect(context.Context) error { return nil }
func (c *assemblyTestMCPClient) Disconnect(context.Context) error {
	var firstCall bool
	c.disconnectOnce.Do(func() {
		firstCall = true
		close(c.disconnectStarted)
	})
	if firstCall {
		return &assemblyMCPPendingError{done: c.done}
	}
	select {
	case <-c.done:
		return nil
	default:
		return &assemblyMCPPendingError{done: c.done}
	}
}
func (*assemblyTestMCPClient) ListTools(context.Context) ([]mcp.MCPTool, error) { return nil, nil }
func (*assemblyTestMCPClient) CallTool(context.Context, string, map[string]any) (*mcp.MCPToolResult, error) {
	return nil, nil
}
func (*assemblyTestMCPClient) Name() string            { return "assembly-test-mcp" }
func (*assemblyTestMCPClient) ProtocolVersion() string { return "test" }
func (c *assemblyTestMCPClient) Done() <-chan struct{} { return c.done }

type assemblyBarrierReloader struct {
	stopRequested chan struct{}
	releaseDone   chan struct{}
	stopOnce      sync.Once
}

func newAssemblyBarrierReloader() *assemblyBarrierReloader {
	return &assemblyBarrierReloader{
		stopRequested: make(chan struct{}),
		releaseDone:   make(chan struct{}),
	}
}

func (*assemblyBarrierReloader) Watch(context.Context, string) error { return nil }
func (r *assemblyBarrierReloader) Stop() error {
	r.RequestStop()
	<-r.releaseDone
	return nil
}
func (*assemblyBarrierReloader) Reload(context.Context) error { return nil }
func (*assemblyBarrierReloader) Name() string                 { return "assembly-test-reloader" }
func (r *assemblyBarrierReloader) RequestStop() {
	r.stopOnce.Do(func() { close(r.stopRequested) })
}
func (r *assemblyBarrierReloader) Done() <-chan struct{} { return r.releaseDone }
func (r *assemblyBarrierReloader) Wait(ctx context.Context) error {
	select {
	case <-r.releaseDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestAssemblyCloseFencesAdmissionAndWaitsForRun(t *testing.T) {
	isolatedHome := t.TempDir()
	t.Setenv("HOME", isolatedHome)
	t.Setenv("XDG_CONFIG_HOME", isolatedHome+"/config")
	t.Setenv("XDG_CACHE_HOME", isolatedHome+"/cache")
	t.Setenv("XDG_DATA_HOME", isolatedHome+"/data")

	assembly, err := AssembleAgent(context.Background(), newAssembleTestConfig(), "openai", "test-model", io.Discard)
	require.NoError(t, err)

	closer, ok := any(assembly).(assemblyContextCloser)
	if !ok {
		t.Cleanup(assembly.Cleanup)
		t.Fatalf("AgentAssembly must expose Close(context.Context) error")
	}

	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	closeRelease := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(func() {
		closeRelease()
		assembly.Cleanup()
	})

	assembly.LoopAgent.SetModel(&streamFuncModel{
		streamFn: func(context.Context, []llm.Message, ...llm.Option) (<-chan llm.MessageChunk, error) {
			close(started)
			<-release
			return nil, errors.New("assembly close barrier released")
		},
	})
	stream, err := assembly.Harness.Submit(context.Background(), "hold assembly close")
	require.NoError(t, err)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("run did not reach the controlled model barrier")
	}

	waitCtx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	closeErr := closer.Close(waitCtx)
	cancel()
	require.Error(t, closeErr, "Close must report that the existing run still owns Assembly resources")

	_, submitErr := assembly.Harness.Submit(context.Background(), "must stay fenced")
	require.Error(t, submitErr, "a post-close submission must not start")
	require.True(t,
		errors.Is(submitErr, core.ErrRunAdmissionClosed) || errors.Is(submitErr, context.Canceled),
		"the close fence or linked lifetime must reject the submission",
	)

	closeRelease()
	for range stream.Events() {
	}
	_, _ = stream.Result()
	require.NoError(t, closer.Close(context.Background()))
}
