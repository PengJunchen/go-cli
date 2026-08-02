package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// recordingExporter is an in-memory tracing.TraceExporter used to assert that
// adapters emit acp.connect/acp.send spans. It mirrors internal/mock's
// MockTraceExporter so the acp tests do not need to import internal/mock
// (avoiding a dependency on the mock module).
type recordingExporter struct {
	mu    sync.Mutex
	spans []tracing.SpanData
}

func (e *recordingExporter) ExportSpan(_ context.Context, s tracing.TraceSpan) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.spans = append(e.spans, tracing.SpanToData(s))
	return nil
}

func (e *recordingExporter) Shutdown(context.Context) error { return nil }

func (e *recordingExporter) Spans() []tracing.SpanData {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]tracing.SpanData, len(e.spans))
	copy(out, e.spans)
	return out
}

// spanByName returns the first collected span with the given name, or nil.
func (e *recordingExporter) spanByName(name string) *tracing.SpanData {
	spans := e.Spans() // copy-safe snapshot
	for i := range spans {
		if spans[i].Name == name {
			return &spans[i]
		}
	}
	return nil
}

// spanAttr returns the value of the named attribute of a span.
func spanAttr(s *tracing.SpanData, key string) any {
	for _, a := range s.Attributes {
		if a.Key == key {
			return a.Value
		}
	}
	return nil
}

// closeIgnored best-effort closes a resource during test cleanup.
func closeIgnored(c io.Closer) { _ = c.Close() } //nolint:errcheck // best-effort cleanup

// newTraceContext builds a context whose tracing is wired to a recording
// exporter backed by a root span. It returns the context, the exporter, and the
// root span's ID.
func newTraceContext(t *testing.T, traceID string) (context.Context, *recordingExporter, string) {
	t.Helper()
	exporter := &recordingExporter{}
	tracer := tracing.NewTracer(traceID, exporter)
	span, ctx := tracer.Start(context.Background(), "acp.test.root", tracing.SpanKindInternal)
	t.Cleanup(span.End)
	return ctx, exporter, span.SpanID()
}

// MockACPServer simulates an ACP peer for integration tests. It implements
// ACPServer and, depending on its transport mode, either serves the gRPC-style
// HTTP routes (GPRC mode) or reads/writes newline-delimited JSON over an
// io.Reader/io.Writer pair (Stdio mode).
type MockACPServer struct {
	name string
	mode ACPTransport

	// Stdio plumbing: recv is read by the server, send is written by it.
	recv io.Reader
	send io.Writer

	// httpSrv backs the gRPC-style routes.
	httpSrv    *httptest.Server
	started    int32
	replyFunc  func(ACPMessage) *ACPMessage
	closeSendF func()

	mu       sync.Mutex
	received []ACPMessage
	pending  []ACPMessage
}

// Compile-time assertion that MockACPServer satisfies ACPServer.
var _ ACPServer = (*MockACPServer)(nil)

// NewMockACPServer returns a MockACPServer for the given transport mode.
func NewMockACPServer(name string, mode ACPTransport) *MockACPServer {
	return &MockACPServer{
		name: name,
		mode: mode,
		replyFunc: func(msg ACPMessage) *ACPMessage {
			if msg.Type == TypeMessage {
				return &ACPMessage{
					Type:       TypeResponse,
					SenderID:   name,
					ReceiverID: msg.SenderID,
					Content:    "echo:" + msg.Content,
					Timestamp:  msg.Timestamp,
				}
			}
			return nil
		},
	}
}

// SetIO wires the stdio server to the given reader and writer. It must be
// called before Start for Stdio mode.
func (s *MockACPServer) SetIO(recv io.Reader, send io.Writer, closeSend func()) {
	s.recv = recv
	s.send = send
	s.closeSendF = closeSend
}

// SetReplyFunc overrides the reply generator used for received messages.
func (s *MockACPServer) SetReplyFunc(f func(ACPMessage) *ACPMessage) {
	s.replyFunc = f
}

// Endpoint returns the HTTP endpoint for gRPC mode, or "" for stdio mode.
func (s *MockACPServer) Endpoint() string {
	if s.httpSrv == nil {
		return ""
	}
	return s.httpSrv.URL
}

// Start brings the mock up and begins serving.
func (s *MockACPServer) Start(ctx context.Context) error {
	if !atomic.CompareAndSwapInt32(&s.started, 0, 1) {
		return nil
	}
	if s.mode == ACPTransportGRPC {
		s.httpSrv = httptest.NewServer(http.HandlerFunc(s.httpHandler))
		_ = ctx
		return nil
	}
	go s.stdioLoop(ctx)
	return nil
}

// Stop shuts the mock down and releases resources.
func (s *MockACPServer) Stop(_ context.Context) error {
	if !atomic.CompareAndSwapInt32(&s.started, 1, 0) {
		return nil
	}
	if s.httpSrv != nil {
		s.httpSrv.Close()
		s.httpSrv = nil
	}
	if s.closeSendF != nil {
		s.closeSendF()
	}
	return nil
}

// Name returns the mock's identity.
func (s *MockACPServer) Name() string { return s.name }

// Received returns a copy of all messages the mock has ingested.
func (s *MockACPServer) Received() []ACPMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ACPMessage, len(s.received))
	copy(out, s.received)
	return out
}

// record appends a message to the received log and queues an optional reply.
func (s *MockACPServer) record(msg ACPMessage) {
	s.mu.Lock()
	s.received = append(s.received, msg)
	if reply := s.buildReplyLocked(msg); reply != nil {
		s.pending = append(s.pending, *reply)
	}
	s.mu.Unlock()
}

func (s *MockACPServer) buildReplyLocked(msg ACPMessage) *ACPMessage {
	if s.replyFunc == nil {
		return nil
	}
	return s.replyFunc(msg)
}

// httpHandler routes the gRPC-style JSON-over-HTTP requests.
func (s *MockACPServer) httpHandler(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/connect", "/disconnect":
		w.WriteHeader(http.StatusOK)
	case "/send":
		var msg ACPMessage
		if err := json.NewDecoder(r.Body).Decode(&msg); err == nil {
			s.record(msg)
		}
		w.WriteHeader(http.StatusOK)
	case "/stream":
		s.mu.Lock()
		out := s.pending
		s.pending = nil
		s.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		for _, msg := range out {
			data, err := json.Marshal(msg)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "%s\n", data) //nolint:errcheck // best-effort stream write
		}
	default:
		http.NotFound(w, r)
	}
}

// stdioLoop reads newline-delimited JSON frames from recv until it reaches
// end-of-stream, recording received messages and writing replies to send.
func (s *MockACPServer) stdioLoop(ctx context.Context) {
	sc := bufio.NewScanner(s.recv)
	for sc.Scan() {
		var msg ACPMessage
		if err := json.Unmarshal(sc.Bytes(), &msg); err != nil {
			continue
		}
		reply := s.recordAndReply(msg)
		if reply != nil {
			if data, err := json.Marshal(*reply); err == nil {
				if _, werr := fmt.Fprintln(s.send, string(data)); werr != nil {
					return
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		default:
		}
	}
}

// recordAndReply records a received message and returns its generated reply.
func (s *MockACPServer) recordAndReply(msg ACPMessage) *ACPMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.received = append(s.received, msg)
	return s.buildReplyLocked(msg)
}
