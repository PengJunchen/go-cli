package llm

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
)

// SSEEvent is a single Server-Sent Event parsed from an SSE stream.
type SSEEvent struct {
	// Type is the event type (e.g., "message_start", "content_block_delta").
	// Empty when the stream does not use "event:" lines (OpenAI style).
	Type string
	// Data is the data payload, typically a JSON string. When multiple
	// "data:" lines precede a blank-line dispatch, they are joined with "\n".
	Data string
	// Error reports a read or scanner failure encountered after parsing began.
	// It is delivered on the event channel because those failures happen in the
	// parser goroutine, after Parse has returned.
	Error error
}

// SSEParser parses Server-Sent Events from an io.Reader.
type SSEParser interface {
	Parse(ctx context.Context, reader io.Reader) (<-chan SSEEvent, error)
}

// DefaultSSEParser implements SSEParser using bufio.Scanner. It follows the
// W3C EventSource specification: "event:" sets the event type, "data:" appends
// to the data payload, a blank line dispatches the accumulated event, and
// lines starting with ":" are comments.
type DefaultSSEParser struct{}

const maxSSEFrameLines = GuardedStreamMaxChunks

// Compile-time assertion that DefaultSSEParser satisfies SSEParser.
var _ SSEParser = (*DefaultSSEParser)(nil)

// NewDefaultSSEParser creates a new DefaultSSEParser.
func NewDefaultSSEParser() *DefaultSSEParser {
	return &DefaultSSEParser{}
}

// Parse reads SSE events from reader and sends them to the returned buffered
// channel. The channel is closed when the reader is exhausted. Scanner errors
// are delivered as SSEEvent.Error. Parse respects ctx: when ctx is canceled
// the goroutine stops scanning and closes the channel promptly.
func (p *DefaultSSEParser) Parse(ctx context.Context, reader io.Reader) (<-chan SSEEvent, error) {
	ch := make(chan SSEEvent, 4)

	go func() {
		defer close(ch)
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 64*1024), GuardedStreamMaxBytes)

		var eventType string
		var data strings.Builder
		dataLines := 0
		var frameErr error
		sendErr := func(err error) {
			select {
			case ch <- SSEEvent{Error: err}:
			case <-ctx.Done():
			}
		}

		dispatch := func() {
			// Only emit when we have data or an explicit event type.
			if eventType == "" && dataLines == 0 {
				return
			}
			select {
			case ch <- SSEEvent{Type: eventType, Data: data.String()}:
			case <-ctx.Done():
				return
			}
			eventType = ""
			data.Reset()
			dataLines = 0
		}

		for scanner.Scan() {
			select {
			case <-ctx.Done():
				return
			default:
			}
			line := scanner.Text()

			// A blank line dispatches the accumulated event.
			if line == "" {
				dispatch()
				continue
			}

			// Comment line (starts with ':').
			if line[0] == ':' {
				continue
			}

			// event: field
			if strings.HasPrefix(line, "event:") {
				nextType := strings.TrimSpace(line[len("event:"):])
				if len(nextType)+data.Len() > GuardedStreamMaxBytes {
					frameErr = fmt.Errorf("llm: SSE event exceeds %d-byte limit", GuardedStreamMaxBytes)
					break
				}
				eventType = nextType
				continue
			}

			// data: field
			if strings.HasPrefix(line, "data:") {
				val := line[len("data:"):]
				// Per SSE spec, strip a single leading space.
				if len(val) > 0 && val[0] == ' ' {
					val = val[1:]
				}
				if dataLines >= maxSSEFrameLines {
					frameErr = fmt.Errorf("llm: SSE event exceeds %d data lines", maxSSEFrameLines)
					break
				}
				extra := len(val)
				if dataLines > 0 {
					extra++
				}
				if len(eventType)+data.Len()+extra > GuardedStreamMaxBytes {
					frameErr = fmt.Errorf("llm: SSE event exceeds %d-byte limit", GuardedStreamMaxBytes)
					break
				}
				if dataLines > 0 {
					data.WriteByte('\n')
				}
				data.WriteString(val)
				dataLines++
				continue
			}

			// id: and retry: fields are ignored.
		}
		if frameErr != nil {
			sendErr(frameErr)
			return
		}

		if ctx.Err() != nil {
			return
		}
		if err := scanner.Err(); err != nil {
			sendErr(fmt.Errorf("llm: read SSE stream: %w", err))
			return
		}

		// Dispatch any remaining buffered event when the stream ends cleanly
		// without a trailing blank line.
		dispatch()
	}()

	return ch, nil
}

// detectJSONResponse peeks at the reader to determine if the response is a
// plain JSON document (non-SSE) rather than an SSE stream. When the first
// non-whitespace byte is '{' or '[', the entire body is read and returned as
// JSON. Otherwise the reader is left untouched for SSE parsing. Returns
// (isJSON, peekedBytes, error).
func detectJSONResponse(reader *bufio.Reader) (bool, []byte, error) {
	peeked, err := reader.Peek(64)
	if err != nil && err != io.EOF {
		return false, nil, err
	}
	trimmed := bytes.TrimLeft(peeked, " \t\r\n")
	if len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') {
		all, err := io.ReadAll(io.LimitReader(reader, GuardedStreamMaxBytes+1))
		if err != nil {
			return false, nil, err
		}
		if len(all) > GuardedStreamMaxBytes {
			return false, nil, fmt.Errorf("llm: JSON stream response exceeds %d-byte limit", GuardedStreamMaxBytes)
		}
		return true, all, nil
	}
	return false, nil, nil
}
