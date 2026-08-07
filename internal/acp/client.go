// Package acp implements Agent Communication Protocol (ACP) support for
// go-cli. It defines the ACPMessage wire format, the ACPClient/ACPServer
// contracts, and a transport enum used to select how two agents talk to each
// other.
//
// Two client adapters are provided:
//
//   - gRPCAdapter: a stdlib JSON-over-HTTP interpretation of the ACP gRPC
//     contract. No external gRPC dependency exists, so the adapter dials an
//     HTTP endpoint and exchanges JSON messages over well-known routes
//     (/connect, /send, /disconnect, /stream) that mirror the ACP service and
//     method naming convention.
//   - StdioAdapter: exchanges newline-delimited JSON (JSON-RPC style) over an
//     io.Reader/io.Writer pair (typically stdin/stdout). This is what makes the
//     protocol testable in-process.
//
// ACPMiddleware (see middleware.go) bridges ACP messages into the extension
// middleware model by converting an ACP message carried in AgentInput.Data
// into an agent event.
package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// ACPMessage is the wire format exchanged between agents. Every field is
// serialized to JSON so the same structure works over both HTTP and stdio.
type ACPMessage struct {
	// Type identifies the message kind (see the Type* constants).
	Type string `json:"type"`
	// SenderID is the identity of the agent that produced the message.
	SenderID string `json:"sender_id"`
	// ReceiverID is the identity of the agent the message targets.
	ReceiverID string `json:"receiver_id"`
	// Content is the human-readable payload of the message.
	Content string `json:"content"`
	// Metadata carries optional structured key/value context.
	Metadata map[string]string `json:"metadata,omitempty"`
	// Timestamp records when the message was created.
	Timestamp time.Time `json:"timestamp"`
}

// ACP message types. They follow the ACP message vocabulary and are emitted
// both over the wire and as span/log attributes.
const (
	TypeConnect    = "connect"
	TypeDisconnect = "disconnect"
	TypeMessage    = "message"
	TypeResponse   = "response"
	TypeAck        = "ack"
	TypeError      = "error"
	TypeRPC        = "rpc"
)

// ACPTransport identifies the transport an ACP client is wired to.
type ACPTransport string

// Supported ACP transports.
const (
	ACPTransportGRPC  ACPTransport = "gRPC"
	ACPTransportStdio ACPTransport = "Stdio"
)

// String returns the canonical transport name.
func (t ACPTransport) String() string { return string(t) }

// RPCMessage is a JSON-RPC 2.0 request embedded in an ACPMessage.
// The Method identifies the procedure to invoke, Params carries the
// arguments as raw JSON, and ID is the request identifier (0 means
// notification - no response expected).
type RPCMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
	ID      int64           `json:"id"`
}

// RPCResponse is a JSON-RPC 2.0 response.
type RPCResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	Result  any       `json:"result,omitempty"`
	Error   *RPCError `json:"error,omitempty"`
	ID      int64     `json:"id"`
}

// RPCError is a JSON-RPC 2.0 error object.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Error implements the error interface.
func (e *RPCError) Error() string {
	return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message)
}

// RPC error codes (JSON-RPC 2.0 spec).
const (
	RPCCodeParseError     = -32700
	RPCCodeMethodNotFound = -32601
	RPCCodeInvalidParams  = -32602
	RPCCodeInternalError  = -32603
)

// ACPClient is the client side of an ACP connection. Implementations connect
// to a peer, send ACP messages, and expose received messages as a channel.
type ACPClient interface {
	// Connect establishes the session. It emits an acp.connect span carrying
	// transport/endpoint attributes.
	Connect(ctx context.Context) error
	// Disconnect tears down the session and releases resources.
	Disconnect(ctx context.Context) error
	// SendMessage delivers an ACP message to the peer. It emits an acp.send
	// span carrying message_type/receiver_id attributes.
	SendMessage(ctx context.Context, msg ACPMessage) error
	// ReceiveMessages returns a channel that yields messages received from the
	// peer. The channel is closed when the connection is torn down.
	ReceiveMessages() <-chan ACPMessage
	// Name returns the logical client identity.
	Name() string
}

// ACPServer is the server side of an ACP connection: it accepts a peer and
// serves it until stopped.
type ACPServer interface {
	// Start brings the server up and begins serving.
	Start(ctx context.Context) error
	// Stop shuts the server down and releases resources.
	Stop(ctx context.Context) error
	// Name returns the logical server identity.
	Name() string
}

// DefaultACPServer is a minimal ACPServer that only tracks its running state.
// It is a useful base for server-side integrations and serves as the default
// implementation backing the ACPServer contract.
type DefaultACPServer struct {
	name string

	mu      sync.Mutex
	running bool
}

// Compile-time assertion that DefaultACPServer satisfies ACPServer.
var _ ACPServer = (*DefaultACPServer)(nil)

// NewDefaultACPServer returns a DefaultACPServer with the given name.
func NewDefaultACPServer(name string) ACPServer {
	return &DefaultACPServer{name: name}
}

// Start marks the server as running.
func (s *DefaultACPServer) Start(_ context.Context) error {
	s.mu.Lock()
	s.running = true
	s.mu.Unlock()
	slog.Info("acp.server.start", "name", s.name)
	return nil
}

// Stop marks the server as stopped.
func (s *DefaultACPServer) Stop(_ context.Context) error {
	s.mu.Lock()
	s.running = false
	s.mu.Unlock()
	slog.Info("acp.server.stop", "name", s.name)
	return nil
}

// Name returns the server identity.
func (s *DefaultACPServer) Name() string { return s.name }

// Running reports whether the server has been started and not yet stopped.
func (s *DefaultACPServer) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// debugMessage logs a received ACP message at debug level. It centralizes the
// ReceiveMessages logging required by the trace design.
func debugMessage(ctx context.Context, operation string, msg ACPMessage) {
	slog.DebugContext(ctx, operation,
		"type", msg.Type,
		"sender_id", msg.SenderID,
		"receiver_id", msg.ReceiverID,
	)
}

// Option configures an ACP client adapter.
type Option func(*adapterOptions)

// adapterOptions carries optional adapter configuration.
type adapterOptions struct {
	name string
}

// WithName overrides the identity reported by an adapter's Name method.
func WithName(name string) Option {
	return func(o *adapterOptions) { o.name = name }
}

// resolveName returns the resolved adapter name, applying any options.
func resolveName(defaultName string, opts []Option) string {
	o := adapterOptions{name: defaultName}
	for _, opt := range opts {
		opt(&o)
	}
	return o.name
}
