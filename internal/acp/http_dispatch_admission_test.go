package acp

import (
	"context"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
)

func TestACPDispatchAdmissionBounded(t *testing.T) {
	factory := newDispatchAdmissionFactory()
	fixture := startDispatchAdmissionFixture(t, 2, factory)
	gates := addDispatchAdmissionGates(t, factory, false, false, false)

	for i := 0; i < 2; i++ {
		require.Equal(t, http.StatusOK, fixture.send(t, fixture.sessionID, "held dispatch"))
		awaitDispatchAdmissionWait(t, awaitDispatchAdmissionAgent(t, factory))
	}
	require.EqualValues(t, 2, factory.calls.Load())

	status := fixture.send(t, fixture.sessionID, "must be rejected")
	if status == http.StatusOK {
		awaitDispatchAdmissionAgent(t, factory)
	}
	calls := factory.calls.Load()
	t.Logf("over-capacity send status=%d factory creates=%d", status, calls)
	require.Equal(t, http.StatusServiceUnavailable, status, "over-capacity send must fail before creating another dispatcher worker")
	require.EqualValues(t, 2, calls, "overload must not call the agent factory")

	gates[0].release()
	require.Equal(t, http.StatusOK, sendUntilDispatchAdmission(t, fixture, fixture.sessionID), "completed ProcessMessage must release its dispatch slot")
	awaitDispatchAdmissionWait(t, awaitDispatchAdmissionAgent(t, factory))
	require.EqualValues(t, 3, factory.calls.Load())
}

func TestACPDispatchAdmissionStopJoinsAndRestartsFreshGeneration(t *testing.T) {
	factory := newDispatchAdmissionFactory()
	fixture := startDispatchAdmissionFixture(t, 1, factory)
	gates := addDispatchAdmissionGates(t, factory, false, false)

	require.Equal(t, http.StatusOK, fixture.send(t, fixture.sessionID, "wait for join"))
	awaitDispatchAdmissionWait(t, awaitDispatchAdmissionAgent(t, factory))
	fixture.server.mu.Lock()
	oldGeneration := fixture.server.generation
	fixture.server.mu.Unlock()
	require.NotNil(t, oldGeneration)

	fixture.server.BeginStop()
	stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	stopErr := fixture.server.Stop(stopCtx)
	var pending interface{ Done() <-chan struct{} }
	require.ErrorAs(t, stopErr, &pending)
	require.ErrorIs(t, stopErr, ErrHTTPServerCleanupPending)
	select {
	case <-pending.Done():
		t.Fatal("generation completed while its admitted dispatcher was blocked")
	default:
	}
	require.Error(t, fixture.server.Start(context.Background()), "restart must wait for the prior generation's admitted ProcessMessage")

	gates[0].release()
	select {
	case <-pending.Done():
	case <-time.After(time.Second):
		t.Fatal("old generation did not join its admitted dispatch")
	}
	require.NoError(t, fixture.server.Stop(context.Background()))
	require.NoError(t, fixture.server.Start(context.Background()))
	fixture.server.mu.Lock()
	newGeneration := fixture.server.generation
	fixture.server.mu.Unlock()
	require.NotSame(t, oldGeneration, newGeneration)
	fixture.baseURL = "http://" + fixture.server.Addr()

	newSessionID := fixture.connect(t)
	require.Equal(t, http.StatusOK, fixture.send(t, newSessionID, "new generation"))
	awaitDispatchAdmissionWait(t, awaitDispatchAdmissionAgent(t, factory))
	require.EqualValues(t, 2, factory.calls.Load())
	gates[1].release()
	require.NoError(t, fixture.server.Stop(context.Background()))
}

func TestACPDispatchAdmissionSealRacesWithSendsWithoutOversubscription(t *testing.T) {
	const capacity = 2
	const requestCount = 16
	factory := newDispatchAdmissionFactory()
	fixture := startDispatchAdmissionFixture(t, capacity, factory)
	gates := addDispatchAdmissionGates(t, factory, make([]bool, requestCount)...)

	requests := make([]*http.Request, requestCount)
	for i := range requests {
		requests[i] = profileHTTPRequest(t, http.MethodPost, fixture.baseURL+"/send", fixture.token, testHTTPACPEnvelope{
			SessionID: fixture.sessionID,
			Type:      TypeMessage,
			SenderID:  "admission-race",
			Content:   "racing send",
		}, true)
	}
	start := make(chan struct{})
	results := make(chan admissionRaceResult, requestCount)
	var senders sync.WaitGroup
	for _, request := range requests {
		senders.Add(1)
		go func(request *http.Request) {
			defer senders.Done()
			<-start
			response, err := fixture.client.Do(request)
			if err != nil {
				results <- admissionRaceResult{err: err}
				return
			}
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			results <- admissionRaceResult{status: response.StatusCode}
		}(request)
	}
	sealDone := make(chan struct{})
	go func() {
		<-start
		fixture.server.SealAdmission()
		close(sealDone)
	}()
	close(start)
	<-sealDone
	senders.Wait()

	accepted := 0
	for range requestCount {
		result := <-results
		if result.err != nil {
			continue // The listener may close before a racing request reaches a handler.
		}
		switch result.status {
		case http.StatusOK:
			accepted++
		case http.StatusNotFound, http.StatusServiceUnavailable:
		default:
			t.Fatalf("racing send returned unexpected status %d", result.status)
		}
	}
	require.LessOrEqual(t, accepted, capacity, "only slots reserved before SealAdmission may be acknowledged")

	for _, gate := range gates {
		gate.release()
	}
	require.NoError(t, fixture.server.Stop(context.Background()))
	require.LessOrEqual(t, factory.calls.Load(), int32(capacity), "sealed generation must not create workers beyond its dispatch capacity")
	require.LessOrEqual(t, factory.calls.Load(), int32(accepted), "a worker must belong to an acknowledged admitted send")
	require.ErrorIs(t, fixture.server.Start(context.Background()), ErrHTTPServerAdmissionClosed)
}

func TestACPDispatchAdmissionSlotReleasesAfterWorkerPanic(t *testing.T) {
	factory := newDispatchAdmissionFactory()
	fixture := startDispatchAdmissionFixture(t, 1, factory)
	gates := addDispatchAdmissionGates(t, factory, true, false)

	require.Equal(t, http.StatusOK, fixture.send(t, fixture.sessionID, "panic on release"))
	awaitDispatchAdmissionWait(t, awaitDispatchAdmissionAgent(t, factory))
	gates[0].release()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		fixture.server.mu.Lock()
		generation := fixture.server.generation
		fixture.server.mu.Unlock()
		if generation != nil && generation.workerPanic.Load() {
			break
		}
		time.Sleep(time.Millisecond)
	}
	require.Equal(t, http.StatusOK, sendUntilDispatchAdmission(t, fixture, fixture.sessionID), "panic cleanup must release its admitted slot")
	awaitDispatchAdmissionAgent(t, factory)
	require.EqualValues(t, 2, factory.calls.Load())
	gates[1].release()
	require.ErrorIs(t, fixture.server.Stop(context.Background()), ErrHTTPServerWorkerPanic)
}

type dispatchAdmissionGate struct {
	done           chan struct{}
	panicOnRelease bool
	once           sync.Once
}

func newDispatchAdmissionGate(panicOnRelease bool) *dispatchAdmissionGate {
	return &dispatchAdmissionGate{done: make(chan struct{}), panicOnRelease: panicOnRelease}
}

func (g *dispatchAdmissionGate) release() {
	g.once.Do(func() { close(g.done) })
}

type dispatchAdmissionFactory struct {
	gates   chan *dispatchAdmissionGate
	created chan *dispatchAdmissionAgent
	calls   atomic.Int32
}

func newDispatchAdmissionFactory() *dispatchAdmissionFactory {
	return &dispatchAdmissionFactory{
		gates:   make(chan *dispatchAdmissionGate, 32),
		created: make(chan *dispatchAdmissionAgent, 32),
	}
}

func (f *dispatchAdmissionFactory) Create(_ context.Context, _ string, _ core.SubAgentConfig) (core.SubAgent, error) {
	f.calls.Add(1)
	gate := <-f.gates
	agent := &dispatchAdmissionAgent{gate: gate, events: make(chan core.AgentEvent), waitStarted: make(chan struct{})}
	f.created <- agent
	return agent, nil
}

type dispatchAdmissionAgent struct {
	gate        *dispatchAdmissionGate
	events      chan core.AgentEvent
	closeEvents sync.Once
	waitStarted chan struct{}
	waitOnce    sync.Once
}

func (*dispatchAdmissionAgent) Name() string { return "dispatch-admission-agent" }

func (a *dispatchAdmissionAgent) Run(context.Context, string) (<-chan core.AgentEvent, error) {
	return a.events, nil
}

func (*dispatchAdmissionAgent) Send(context.Context, string) error { return nil }
func (*dispatchAdmissionAgent) Interrupt(context.Context) error    { return nil }

func (a *dispatchAdmissionAgent) Wait(context.Context) (core.AgentMessage, error) {
	defer a.closeEvents.Do(func() { close(a.events) })
	a.waitOnce.Do(func() { close(a.waitStarted) })
	<-a.gate.done // Deliberately ignore cancellation to prove Stop joins admitted work.
	if a.gate.panicOnRelease {
		panic("private admission test panic")
	}
	return core.AgentMessage{Role: "assistant", Content: "finished"}, nil
}

type dispatchAdmissionFixture struct {
	server    *HTTPServer
	client    *http.Client
	baseURL   string
	token     string
	sessionID string
}

func startDispatchAdmissionFixture(t *testing.T, capacity int, factory *dispatchAdmissionFactory) *dispatchAdmissionFixture {
	t.Helper()
	server := NewHTTPServer("test", "127.0.0.1:0", NewCoreHandler(core.NewDefaultSubagentDispatcher(factory), nil))
	server.maxConcurrency = capacity
	token := runtimeTestToken(t)
	server.SetAuth(token, "dispatch-admission-test")
	require.NoError(t, server.Start(context.Background()))
	t.Cleanup(func() { _ = server.Stop(context.Background()) })
	fixture := &dispatchAdmissionFixture{
		server:  server,
		client:  &http.Client{Timeout: 2 * time.Second},
		baseURL: "http://" + server.Addr(),
		token:   token,
	}
	fixture.sessionID = fixture.connect(t)
	return fixture
}

func (f *dispatchAdmissionFixture) connect(t *testing.T) string {
	t.Helper()
	request := profileHTTPRequest(t, http.MethodPost, f.baseURL+"/connect", f.token, testHTTPACPEnvelope{
		Version:  testHTTPACPProfile,
		Type:     TypeConnect,
		SenderID: "admission-race",
	}, true)
	return decodeConnectedSessionID(t, doProfileRequest(t, f.client, request))
}

func (f *dispatchAdmissionFixture) send(t *testing.T, sessionID, content string) int {
	t.Helper()
	request := profileHTTPRequest(t, http.MethodPost, f.baseURL+"/send", f.token, testHTTPACPEnvelope{
		SessionID: sessionID,
		Type:      TypeMessage,
		SenderID:  "admission-race",
		Content:   content,
	}, true)
	response := doProfileRequest(t, f.client, request)
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	return response.StatusCode
}

func addDispatchAdmissionGates(t *testing.T, factory *dispatchAdmissionFactory, panicOnRelease ...bool) []*dispatchAdmissionGate {
	t.Helper()
	gates := make([]*dispatchAdmissionGate, len(panicOnRelease))
	for i, shouldPanic := range panicOnRelease {
		gate := newDispatchAdmissionGate(shouldPanic)
		factory.gates <- gate
		gates[i] = gate
		t.Cleanup(gate.release)
	}
	return gates
}

func awaitDispatchAdmissionAgent(t *testing.T, factory *dispatchAdmissionFactory) *dispatchAdmissionAgent {
	t.Helper()
	select {
	case agent := <-factory.created:
		return agent
	case <-time.After(time.Second):
		t.Fatal("agent factory did not create an admitted dispatch")
		return nil
	}
}

func awaitDispatchAdmissionWait(t *testing.T, agent *dispatchAdmissionAgent) {
	t.Helper()
	select {
	case <-agent.waitStarted:
	case <-time.After(time.Second):
		t.Fatal("admitted agent did not enter its held Wait")
	}
}

func sendUntilDispatchAdmission(t *testing.T, fixture *dispatchAdmissionFixture, sessionID string) int {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		status := fixture.send(t, sessionID, "after a dispatch slot is released")
		if status == http.StatusOK {
			return status
		}
		require.Equal(t, http.StatusServiceUnavailable, status)
		time.Sleep(time.Millisecond)
	}
	t.Fatal("released dispatch capacity was not admitted within one second")
	return 0
}

type admissionRaceResult struct {
	status int
	err    error
}

var _ core.SubAgentFactory = (*dispatchAdmissionFactory)(nil)
var _ core.SubAgent = (*dispatchAdmissionAgent)(nil)
