package acp

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/pengjunchen/go-cli/internal/core"
)

// ACPMiddlewareAdapter bridges the extension.Middleware-based ACPMiddleware
// into the core.Middleware model used by the LoopAgent middleware chain. It
// also routes inbound ACP messages received from the ACPClient to a
// SubagentDispatcher: each TypeMessage is converted to a SubagentTask,
// dispatched, and the result is relayed back to the peer as a TypeResponse (or
// TypeError on failure).
//
// When the client or dispatcher is nil the adapter acts as a pure pass-through
// — Wrap returns a loop that delegates to the inner loop unchanged — so ACP
// can be silently skipped when it is not configured.
type ACPMiddlewareAdapter struct {
	acpMiddleware *ACPMiddleware
	dispatcher    core.SubagentDispatcher
	client        ACPClient

	mu      sync.Mutex
	started bool
	cancel  context.CancelFunc
	done    chan struct{}
}

var _ core.Middleware = (*ACPMiddlewareAdapter)(nil)

// NewACPMiddlewareAdapter creates a bridge that wraps the given ACPMiddleware
// for use in a core.MiddlewareChain. dispatcher and client may be nil to
// disable message routing (pass-through mode).
func NewACPMiddlewareAdapter(mw *ACPMiddleware, dispatcher core.SubagentDispatcher, client ACPClient) *ACPMiddlewareAdapter {
	return &ACPMiddlewareAdapter{
		acpMiddleware: mw,
		dispatcher:    dispatcher,
		client:        client,
	}
}

// Name returns the middleware identifier, delegating to the wrapped
// ACPMiddleware.
func (a *ACPMiddlewareAdapter) Name() string {
	return a.acpMiddleware.Name()
}

// Wrap returns a wrapped AgentLoop that lazily starts the ACP message router
// on first invocation and delegates Run calls to the inner loop. The router
// runs in the background using a detached context so it outlives any single
// Run call; it is stopped by Close.
func (a *ACPMiddlewareAdapter) Wrap(loop core.AgentLoop) core.AgentLoop {
	return &acpBridgeLoop{adapter: a, next: loop}
}

// acpBridgeLoop is the concrete wrapped loop produced by
// ACPMiddlewareAdapter.
type acpBridgeLoop struct {
	adapter *ACPMiddlewareAdapter
	next    core.AgentLoop
}

// Run starts the ACP message router (idempotent) and delegates to the inner
// loop.
func (l *acpBridgeLoop) Run(ctx context.Context, submission core.Submission, stream ...core.EventStream) ([]core.AgentEvent, error) {
	l.adapter.startRouter()
	return l.next.Run(ctx, submission, stream...)
}

// startRouter starts the background goroutine that reads inbound ACP messages
// and dispatches them to the SubagentDispatcher. It is idempotent and safe to
// call from multiple goroutines.
func (a *ACPMiddlewareAdapter) startRouter() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.started || a.client == nil || a.dispatcher == nil {
		return
	}
	a.started = true
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	done := make(chan struct{})
	a.done = done
	go a.routeMessages(ctx, done)
}

// routeMessages reads inbound ACP messages from the client, dispatches each
// TypeMessage to the SubagentDispatcher, and sends the result back as an ACP
// response. It exits when the context is canceled or the client's receive
// channel is closed. The done channel is closed on exit so Close can wait.
func (a *ACPMiddlewareAdapter) routeMessages(ctx context.Context, done chan struct{}) {
	defer close(done)
	recv := a.client.ReceiveMessages()
	if recv == nil {
		return
	}
	for {
		select {
		case msg, ok := <-recv:
			if !ok {
				return
			}
			a.handleMessage(ctx, msg)
		case <-ctx.Done():
			return
		}
	}
}

// handleMessage converts an ACP message to a SubagentTask, dispatches it, and
// relays the result back to the peer. Non-TypeMessage messages are ignored.
func (a *ACPMiddlewareAdapter) handleMessage(ctx context.Context, msg ACPMessage) {
	if msg.Type != TypeMessage {
		return
	}

	task := core.SubagentTask{
		ID:     fmt.Sprintf("acp-%s", msg.SenderID),
		Prompt: msg.Content,
	}
	if role, ok := msg.Metadata["role"]; ok {
		task.Role = role
	}

	result, err := a.dispatcher.Dispatch(ctx, task)

	content := result.Content
	msgType := TypeResponse
	if err != nil {
		content = err.Error()
		msgType = TypeError
	} else if result.Error != nil {
		content = result.Error.Error()
		msgType = TypeError
	}

	reply := ACPMessage{
		Type:       msgType,
		SenderID:   msg.ReceiverID,
		ReceiverID: msg.SenderID,
		Content:    content,
		Timestamp:  time.Now(),
	}
	if sendErr := a.client.SendMessage(ctx, reply); sendErr != nil {
		slog.Warn("acp.bridge.reply_failed", "err", sendErr, "receiver", msg.SenderID)
	}
}

// Close stops the background message router and waits for it to exit. It is
// safe to call multiple times and when the router was never started.
func (a *ACPMiddlewareAdapter) Close() {
	a.mu.Lock()
	if !a.started {
		a.mu.Unlock()
		return
	}
	a.started = false
	cancel := a.cancel
	done := a.done
	a.cancel = nil
	a.done = nil
	a.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}
