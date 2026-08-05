package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
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
//
// Two mutexes are used to avoid a deadlock that occurs when sendMessage
// holds the lock during a blocking write while the read loop needs the same
// lock to dispatch a response: writeMu serializes framed writes to w;
// pendingMu protects the pending map and notifyHandler.
type JSONRPCClient struct {
	w             io.Writer
	r             *bufio.Reader
	writeMu       sync.Mutex
	pendingMu     sync.Mutex
	nextID        atomic.Int64
	pending       map[int64]chan jsonRPCMessage
	notifyHandler func(method string, params json.RawMessage)
	done          chan struct{}
	once          sync.Once
}

// NewJSONRPCClient creates a new JSON-RPC client over the given streams and
// starts the background read loop that dispatches responses and notifications.
func NewJSONRPCClient(r io.Reader, w io.Writer) *JSONRPCClient {
	c := &JSONRPCClient{
		w:       w,
		r:       bufio.NewReader(r),
		pending: make(map[int64]chan jsonRPCMessage),
		done:    make(chan struct{}),
	}
	go c.readLoop()
	return c
}

// SetNotifyHandler registers a callback invoked for server-side notifications.
func (c *JSONRPCClient) SetNotifyHandler(handler func(method string, params json.RawMessage)) {
	c.pendingMu.Lock()
	c.notifyHandler = handler
	c.pendingMu.Unlock()
}

// Call sends a JSON-RPC request and waits for the matching response. The
// result (if non-nil) is JSON-unmarshaled into result. The call honors the
// context's deadline/cancellation.
func (c *JSONRPCClient) Call(ctx context.Context, method string, params any, result any) error {
	id := c.nextID.Add(1)

	ch := make(chan jsonRPCMessage, 1)

	// Register the pending response channel before sending so the read loop
	// can dispatch the response as soon as it arrives.
	c.pendingMu.Lock()
	c.pending[id] = ch
	c.pendingMu.Unlock()

	req := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}

	if err := c.sendMessage(req); err != nil {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return fmt.Errorf("jsonrpc: send %s: %w", method, err)
	}

	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
	}()

	select {
	case resp := <-ch:
		if resp.Error != nil {
			return resp.Error
		}
		if result != nil && len(resp.Result) > 0 {
			if err := json.Unmarshal(resp.Result, result); err != nil {
				return fmt.Errorf("jsonrpc: unmarshal result: %w", err)
			}
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return io.ErrClosedPipe
	}
}

// Notify sends a JSON-RPC notification (no response expected).
func (c *JSONRPCClient) Notify(_ context.Context, method string, params any) error {
	notif := jsonRPCNotification{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
	}
	if err := c.sendMessage(notif); err != nil {
		return fmt.Errorf("jsonrpc: notify %s: %w", method, err)
	}
	return nil
}

// Close shuts down the client, stopping the read loop. It is safe to call
// multiple times.
func (c *JSONRPCClient) Close() error {
	c.once.Do(func() {
		close(c.done)
	})
	return nil
}

// sendMessage marshals msg and writes it with Content-Length framing. The
// write is protected by writeMu to prevent interleaved frames from concurrent
// callers.
func (c *JSONRPCClient) sendMessage(msg any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(data))
	if _, err := c.w.Write([]byte(header)); err != nil {
		return err
	}
	if _, err := c.w.Write(data); err != nil {
		return err
	}
	return nil
}

// readLoop continuously reads framed messages and dispatches them. Responses
// (with ID) are sent to the corresponding pending channel; notifications
// (without ID) are forwarded to the notify handler. On read error, all
// pending requests are signaled and the loop exits.
func (c *JSONRPCClient) readLoop() {
	for {
		msg, err := c.readMessage()
		if err != nil {
			// Signal all pending requests with the error.
			c.pendingMu.Lock()
			for id, ch := range c.pending {
				select {
				case ch <- jsonRPCMessage{Error: &jsonRPCError{Code: -1, Message: err.Error()}}:
				default:
				}
				delete(c.pending, id)
			}
			c.pendingMu.Unlock()
			return
		}

		if msg.ID == nil {
			// Notification — forward to handler if registered.
			c.pendingMu.Lock()
			handler := c.notifyHandler
			c.pendingMu.Unlock()
			if handler != nil {
				handler(msg.Method, msg.Params)
			}
			continue
		}

		// Response — dispatch to the pending caller.
		c.pendingMu.Lock()
		ch, ok := c.pending[*msg.ID]
		if ok {
			delete(c.pending, *msg.ID)
		}
		c.pendingMu.Unlock()

		if ok {
			ch <- msg
		}
	}
}

// readMessage reads a single Content-Length framed message from the reader.
func (c *JSONRPCClient) readMessage() (jsonRPCMessage, error) {
	var contentLength int
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			return jsonRPCMessage{}, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if strings.HasPrefix(line, "Content-Length:") {
			val := strings.TrimSpace(strings.TrimPrefix(line, "Content-Length:"))
			contentLength, err = strconv.Atoi(val)
			if err != nil {
				return jsonRPCMessage{}, fmt.Errorf("jsonrpc: invalid Content-Length %q: %w", val, err)
			}
		}
	}

	if contentLength <= 0 {
		return jsonRPCMessage{}, fmt.Errorf("jsonrpc: missing Content-Length header")
	}

	body := make([]byte, contentLength)
	if _, err := io.ReadFull(c.r, body); err != nil {
		return jsonRPCMessage{}, err
	}

	var msg jsonRPCMessage
	if err := json.Unmarshal(body, &msg); err != nil {
		return jsonRPCMessage{}, fmt.Errorf("jsonrpc: unmarshal message: %w", err)
	}
	return msg, nil
}
