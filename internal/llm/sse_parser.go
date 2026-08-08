package llm

import (
	"bufio"
	"bytes"
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
}

// SSEParser parses Server-Sent Events from an io.Reader.
type SSEParser interface {
	Parse(reader io.Reader) (<-chan SSEEvent, error)
}

// DefaultSSEParser implements SSEParser using bufio.Scanner. It follows the
// W3C EventSource specification: "event:" sets the event type, "data:" appends
// to the data payload, a blank line dispatches the accumulated event, and
// lines starting with ":" are comments.
type DefaultSSEParser struct{}

// Compile-time assertion that DefaultSSEParser satisfies SSEParser.
var _ SSEParser = (*DefaultSSEParser)(nil)

// NewDefaultSSEParser creates a new DefaultSSEParser.
func NewDefaultSSEParser() *DefaultSSEParser {
	return &DefaultSSEParser{}
}

// Parse reads SSE events from reader and sends them to the returned unbuffered
// channel. The channel is closed when the reader is exhausted. The returned
// error is always nil; malformed lines are silently skipped.
func (p *DefaultSSEParser) Parse(reader io.Reader) (<-chan SSEEvent, error) {
	ch := make(chan SSEEvent, 4)

	go func() {
		defer close(ch)
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)

		var eventType string
		var dataLines []string

		dispatch := func() {
			// Only emit when we have data or an explicit event type.
			if eventType == "" && len(dataLines) == 0 {
				return
			}
			ch <- SSEEvent{
				Type: eventType,
				Data: strings.Join(dataLines, "\n"),
			}
			eventType = ""
			dataLines = dataLines[:0]
		}

		for scanner.Scan() {
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
				eventType = strings.TrimSpace(line[len("event:"):])
				continue
			}

			// data: field
			if strings.HasPrefix(line, "data:") {
				val := line[len("data:"):]
				// Per SSE spec, strip a single leading space.
				if len(val) > 0 && val[0] == ' ' {
					val = val[1:]
				}
				dataLines = append(dataLines, val)
				continue
			}

			// id: and retry: fields are ignored.
		}

		// Dispatch any remaining buffered event when the stream ends
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
		all, err := io.ReadAll(reader)
		if err != nil {
			return false, nil, err
		}
		return true, all, nil
	}
	return false, nil, nil
}
