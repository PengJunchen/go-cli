package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// jsonRPCRequest is a JSON-RPC 2.0 request with an ID expecting a response.
type jsonRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// jsonRPCNotification is a JSON-RPC 2.0 notification (no ID, no response).
type jsonRPCNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// jsonRPCError is a JSON-RPC 2.0 error object.
type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *jsonRPCError) Error() string {
	return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message)
}

// jsonRPCMessage is the wire-level message read from the server. It
// accommodates both responses (with ID, result/error) and notifications
// (with method, no ID).
type jsonRPCMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

// JSONRPCClient is a JSON-RPC 2.0 client using LSP Content-Length header
// framing over separate reader and writer streams.
type JSONRPCClient struct {
	w             io.Writer
	r             *bufio.Reader
	mu            sync.Mutex
	nextID        atomic.Int64
	pending       map[int64]chan jsonRPCMessage
	notifyHandler func(method string, params json.RawMessage)
	done          chan struct{}
	once          sync.Once
}

// NewJSONRPCClient creates a new JSON-RPC client over the given streams.
func NewJSONRPCClient(r io.Reader, w io.Writer) *JSONRPCClient {
	return &JSONRPCClient{
		w:       w,
		r:       bufio.NewReader(r),
		pending: make(map[int64]chan jsonRPCMessage),
		done:    make(chan struct{}),
	}
}

// SetNotifyHandler registers a callback invoked for server-side notifications.
func (c *JSONRPCClient) SetNotifyHandler(handler func(method string, params json.RawMessage)) {
	c.mu.Lock()
	c.notifyHandler = handler
	c.mu.Unlock()
}

// Call sends a JSON-RPC request and waits for the response.
func (c *JSONRPCClient) Call(ctx context.Context, method string, params any, result any) error {
	return fmt.Errorf("jsonrpc: not implemented")
}

// Notify sends a JSON-RPC notification (no response expected).
func (c *JSONRPCClient) Notify(ctx context.Context, method string, params any) error {
	return fmt.Errorf("jsonrpc: not implemented")
}

// Close shuts down the client.
func (c *JSONRPCClient) Close() error {
	return nil
}

// readLoop reads framed messages and dispatches them. Stub: does nothing.
func (c *JSONRPCClient) readLoop() {}
