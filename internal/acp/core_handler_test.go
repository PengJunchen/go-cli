package acp

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/pengjunchen/go-cli/internal/core"
)

// --- Session tests ---

func TestSession_EnqueueAndDrain(t *testing.T) {
	s := NewSession("client-1")

	msgs := s.Drain()
	if msgs != nil {
		t.Fatalf("expected nil from empty drain, got %v", msgs)
	}

	s.Enqueue(ACPMessage{Type: TypeResponse, Content: "hello"})
	s.Enqueue(ACPMessage{Type: TypeResponse, Content: "world"})

	drained := s.Drain()
	if len(drained) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(drained))
	}
	if drained[0].Content != "hello" || drained[1].Content != "world" {
		t.Errorf("unexpected content: %q, %q", drained[0].Content, drained[1].Content)
	}

	// Drain should clear pending.
	if drained2 := s.Drain(); drained2 != nil {
		t.Fatalf("expected nil after drain, got %v", drained2)
	}
}

func TestSession_CloseDropsEnqueue(t *testing.T) {
	s := NewSession("client-1")
	s.Enqueue(ACPMessage{Type: TypeResponse, Content: "before-close"})

	s.Close()

	if !s.IsClosed() {
		t.Fatal("expected closed=true")
	}

	s.Enqueue(ACPMessage{Type: TypeResponse, Content: "after-close"})

	drained := s.Drain()
	// After close, Drain returns nil even if messages were pending before close.
	if drained != nil {
		t.Fatalf("expected nil from closed session drain, got %v", drained)
	}
}

func TestSession_ID(t *testing.T) {
	s := NewSession("my-session")
	if s.ID() != "my-session" {
		t.Errorf("expected ID 'my-session', got %q", s.ID())
	}
}

func TestSession_ConcurrentEnqueueDrain(t *testing.T) {
	s := NewSession("concurrent")
	var wg sync.WaitGroup

	// Concurrent enqueuers.
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			s.Enqueue(ACPMessage{Content: "msg"})
		}(i)
	}

	// Concurrent drainer.
	var total int
	var drainerWg sync.WaitGroup
	drainerWg.Add(1)
	go func() {
		defer drainerWg.Done()
		for {
			d := s.Drain()
			total += len(d)
			if total >= 10 {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	wg.Wait()
	drainerWg.Wait()
	if total != 10 {
		t.Errorf("expected 10 total drained, got %d", total)
	}
}

// --- mockSubagentDispatcher ---

type mockSubagentDispatcher struct {
	mu       sync.Mutex
	tasks    []core.SubagentTask
	result   core.SubagentResult
	err      error
	delay    time.Duration
	dispatched chan struct{}
}

func (m *mockSubagentDispatcher) Dispatch(ctx context.Context, task core.SubagentTask) (core.SubagentResult, error) {
	m.mu.Lock()
	m.tasks = append(m.tasks, task)
	delay := m.delay
	m.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return core.SubagentResult{}, ctx.Err()
		}
	}

	if m.dispatched != nil {
		m.dispatched <- struct{}{}
	}
	return m.result, m.err
}

func (m *mockSubagentDispatcher) ParallelDispatch(ctx context.Context, tasks []core.SubagentTask) ([]core.SubagentResult, error) {
	results := make([]core.SubagentResult, len(tasks))
	for i, task := range tasks {
		r, err := m.Dispatch(ctx, task)
		if err != nil {
			return nil, err
		}
		results[i] = r
	}
	return results, nil
}

func (m *mockSubagentDispatcher) ListRunning() []core.SubagentTask { return nil }

// --- CoreHandler tests ---

func TestCoreHandler_NilDispatcher_MessageGetsError(t *testing.T) {
	h := NewCoreHandler(nil, nil)
	sess := NewSession("client-1")

	h.ProcessMessage(context.Background(), ACPMessage{
		Type:       TypeMessage,
		SenderID:   "client-1",
		ReceiverID: "server",
		Content:    "hello",
	}, sess)

	drained := sess.Drain()
	if len(drained) != 1 {
		t.Fatalf("expected 1 message, got %d", len(drained))
	}
	if drained[0].Type != TypeError {
		t.Errorf("expected TypeError, got %s", drained[0].Type)
	}
	if drained[0].Content != "no sub-agent dispatcher configured" {
		t.Errorf("unexpected error content: %q", drained[0].Content)
	}
}

func TestCoreHandler_MessageDispatchSuccess(t *testing.T) {
	dispatched := make(chan struct{}, 1)
	d := &mockSubagentDispatcher{
		result:     core.SubagentResult{TaskID: "acp-client-1", Content: "result from agent"},
		dispatched: dispatched,
	}
	h := NewCoreHandler(d, nil)
	sess := NewSession("client-1")

	h.ProcessMessage(context.Background(), ACPMessage{
		Type:       TypeMessage,
		SenderID:   "client-1",
		ReceiverID: "server",
		Content:    "do something",
		Metadata:   map[string]string{"role": "implementer"},
	}, sess)

	<-dispatched // wait for dispatch

	drained := sess.Drain()
	if len(drained) != 1 {
		t.Fatalf("expected 1 message, got %d", len(drained))
	}
	if drained[0].Type != TypeResponse {
		t.Errorf("expected TypeResponse, got %s", drained[0].Type)
	}
	if drained[0].Content != "result from agent" {
		t.Errorf("unexpected content: %q", drained[0].Content)
	}
	if drained[0].ReceiverID != "client-1" {
		t.Errorf("expected ReceiverID client-1, got %s", drained[0].ReceiverID)
	}

	// Verify the task was dispatched with correct fields.
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.tasks) != 1 {
		t.Fatalf("expected 1 task dispatched, got %d", len(d.tasks))
	}
	if d.tasks[0].Prompt != "do something" {
		t.Errorf("expected prompt 'do something', got %q", d.tasks[0].Prompt)
	}
	if d.tasks[0].Role != "implementer" {
		t.Errorf("expected role 'implementer', got %q", d.tasks[0].Role)
	}
}

func TestCoreHandler_MessageDispatchError(t *testing.T) {
	dispatched := make(chan struct{}, 1)
	d := &mockSubagentDispatcher{
		err:        errors.New("dispatch failed"),
		dispatched: dispatched,
	}
	h := NewCoreHandler(d, nil)
	sess := NewSession("client-1")

	h.ProcessMessage(context.Background(), ACPMessage{
		Type:       TypeMessage,
		SenderID:   "client-1",
		ReceiverID: "server",
		Content:    "hello",
	}, sess)

	<-dispatched

	drained := sess.Drain()
	if len(drained) != 1 {
		t.Fatalf("expected 1 message, got %d", len(drained))
	}
	if drained[0].Type != TypeError {
		t.Errorf("expected TypeError, got %s", drained[0].Type)
	}
	if drained[0].Content != "dispatch failed" {
		t.Errorf("unexpected content: %q", drained[0].Content)
	}
}

func TestCoreHandler_MessageDispatchResultError(t *testing.T) {
	dispatched := make(chan struct{}, 1)
	d := &mockSubagentDispatcher{
		result: core.SubagentResult{
			TaskID: "acp-client-1",
			Error:  errors.New("agent failed"),
		},
		dispatched: dispatched,
	}
	h := NewCoreHandler(d, nil)
	sess := NewSession("client-1")

	h.ProcessMessage(context.Background(), ACPMessage{
		Type:       TypeMessage,
		SenderID:   "client-1",
		ReceiverID: "server",
		Content:    "hello",
	}, sess)

	<-dispatched

	drained := sess.Drain()
	if len(drained) != 1 {
		t.Fatalf("expected 1 message, got %d", len(drained))
	}
	if drained[0].Type != TypeError {
		t.Errorf("expected TypeError, got %s", drained[0].Type)
	}
	if drained[0].Content != "agent failed" {
		t.Errorf("unexpected content: %q", drained[0].Content)
	}
}

func TestCoreHandler_RPC_NilDispatcher(t *testing.T) {
	h := NewCoreHandler(nil, nil)
	sess := NewSession("client-1")

	rpcMsg, _ := json.Marshal(RPCMessage{JSONRPC: "2.0", Method: "test", ID: 1})

	h.ProcessMessage(context.Background(), ACPMessage{
		Type:       TypeRPC,
		SenderID:   "client-1",
		ReceiverID: "server",
		Content:    string(rpcMsg),
	}, sess)

	drained := sess.Drain()
	if len(drained) != 1 {
		t.Fatalf("expected 1 message, got %d", len(drained))
	}

	var resp RPCResponse
	if err := json.Unmarshal([]byte(drained[0].Content), &resp); err != nil {
		t.Fatalf("failed to unmarshal RPC response: %v", err)
	}
	if resp.Error == nil {
		t.Fatal("expected error in RPC response")
	}
	if resp.Error.Code != RPCCodeInternalError {
		t.Errorf("expected error code %d, got %d", RPCCodeInternalError, resp.Error.Code)
	}
}

func TestCoreHandler_RPC_Success(t *testing.T) {
	rpc := NewRPCDispatcher()
	rpc.Register("echo", func(_ context.Context, params json.RawMessage) (any, error) {
		return map[string]any{"echoed": string(params)}, nil
	})

	h := NewCoreHandler(nil, rpc)
	sess := NewSession("client-1")

	rpcMsg, _ := json.Marshal(RPCMessage{JSONRPC: "2.0", Method: "echo", Params: json.RawMessage(`"hello"`), ID: 42})

	h.ProcessMessage(context.Background(), ACPMessage{
		Type:       TypeRPC,
		SenderID:   "client-1",
		ReceiverID: "server",
		Content:    string(rpcMsg),
	}, sess)

	drained := sess.Drain()
	if len(drained) != 1 {
		t.Fatalf("expected 1 message, got %d", len(drained))
	}

	var resp RPCResponse
	if err := json.Unmarshal([]byte(drained[0].Content), &resp); err != nil {
		t.Fatalf("failed to unmarshal RPC response: %v", err)
	}
	if resp.ID != 42 {
		t.Errorf("expected ID 42, got %d", resp.ID)
	}
	if resp.Error != nil {
		t.Errorf("expected no error, got %v", resp.Error)
	}
}

func TestCoreHandler_RPC_NotificationNoResponse(t *testing.T) {
	rpc := NewRPCDispatcher()
	called := make(chan struct{}, 1)
	rpc.Register("notify", func(_ context.Context, _ json.RawMessage) (any, error) {
		called <- struct{}{}
		return nil, nil
	})

	h := NewCoreHandler(nil, rpc)
	sess := NewSession("client-1")

	// ID=0 means notification — no response should be enqueued.
	rpcMsg, _ := json.Marshal(RPCMessage{JSONRPC: "2.0", Method: "notify", ID: 0})

	h.ProcessMessage(context.Background(), ACPMessage{
		Type:       TypeRPC,
		SenderID:   "client-1",
		ReceiverID: "server",
		Content:    string(rpcMsg),
	}, sess)

	<-called

	drained := sess.Drain()
	if drained != nil {
		t.Fatalf("expected no response for notification, got %v", drained)
	}
}

func TestCoreHandler_RPC_InvalidJSON(t *testing.T) {
	h := NewCoreHandler(nil, NewRPCDispatcher())
	sess := NewSession("client-1")

	h.ProcessMessage(context.Background(), ACPMessage{
		Type:       TypeRPC,
		SenderID:   "client-1",
		ReceiverID: "server",
		Content:    "not-valid-json",
	}, sess)

	drained := sess.Drain()
	if drained != nil {
		t.Fatalf("expected no response for invalid JSON, got %v", drained)
	}
}

func TestCoreHandler_OtherMessageTypesIgnored(t *testing.T) {
	h := NewCoreHandler(nil, nil)
	sess := NewSession("client-1")

	for _, msgType := range []string{TypeConnect, TypeDisconnect, TypeAck, TypeResponse} {
		h.ProcessMessage(context.Background(), ACPMessage{
			Type:    msgType,
			Content: "ignored",
		}, sess)
	}

	drained := sess.Drain()
	if drained != nil {
		t.Fatalf("expected no messages for lifecycle types, got %v", drained)
	}
}
