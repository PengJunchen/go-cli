package acp

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// SSEWriter wraps an http.ResponseWriter for Server-Sent Events streaming.
// It flushes after each event so clients receive data incrementally.
type SSEWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

// NewSSEWriter creates an SSEWriter. It sets the Content-Type and Cache-Control
// headers. Returns false if the ResponseWriter does not support flushing.
func NewSSEWriter(w http.ResponseWriter) (*SSEWriter, bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, false
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	return &SSEWriter{w: w, flusher: flusher}, true
}

// WriteEvent marshals data as JSON and writes it as an SSE event with the given
// event type. It flushes after writing.
func (s *SSEWriter) WriteEvent(eventType string, data interface{}) error {
	jsonBytes, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", eventType, jsonBytes); err != nil { //nolint:errcheck
		return err
	}
	s.flusher.Flush()
	return nil
}

// WriteEventWithID marshals data as JSON and writes it as an SSE event with
// the given event type and an explicit id field. The id enables Last-Event-ID
// reconnection: clients send the last received id via the Last-Event-ID
// request header, and the server replays events with ids greater than that.
func (s *SSEWriter) WriteEventWithID(id uint64, eventType string, data interface{}) error {
	jsonBytes, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.w, "id: %d\nevent: %s\ndata: %s\n\n", id, eventType, jsonBytes); err != nil { //nolint:errcheck
		return err
	}
	s.flusher.Flush()
	return nil
}

// WriteComment writes an SSE comment line (used for keep-alive).
func (s *SSEWriter) WriteComment(comment string) {
	fmt.Fprintf(s.w, ": %s\n\n", comment) //nolint:errcheck // best-effort keep-alive
	s.flusher.Flush()
}
