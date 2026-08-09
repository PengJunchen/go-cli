package tui

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/pengjunchen/go-cli/internal/core"
)

// ACPStreamAdapter consumes SSE events from a remote ACP HTTP server and
// converts them into a channel of tui.AgentEvent, mimicking the in-process
// BridgeEvents flow. This enables the TUI to run in remote mode, decoupled
// from the core runtime process that owns the agent loop.
type ACPStreamAdapter struct {
	remoteURL string
}

// NewACPStreamAdapter creates an adapter that connects to the given SSE
// endpoint URL. The URL should include any required query parameters (e.g.
// sender_id).
func NewACPStreamAdapter(remoteURL string) *ACPStreamAdapter {
	return &ACPStreamAdapter{remoteURL: remoteURL}
}

// Stream opens an HTTP connection to the remote SSE endpoint and returns a
// channel of tui.AgentEvent. The channel closes when the server ends the
// stream, the context is canceled, or the response body encounters an error.
func (a *ACPStreamAdapter) Stream(ctx context.Context) <-chan AgentEvent {
	ch := make(chan AgentEvent, 64)
	go func() {
		defer close(ch)

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.remoteURL, nil)
		if err != nil {
			slog.Debug("tui.http_client.request_failed", "url", a.remoteURL, "err", err)
			return
		}
		req.Header.Set("Accept", "text/event-stream")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			slog.Debug("tui.http_client.connect_failed", "url", a.remoteURL, "err", err)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			slog.Debug("tui.http_client.bad_status", "url", a.remoteURL, "status", resp.StatusCode)
			return
		}

		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

		var dataLines []string

		// dispatch parses accumulated SSE data lines into a core.AgentEvent
		// and forwards it as a tui.AgentEvent. Returns false if the context
		// was canceled while sending.
		dispatch := func() bool {
			defer func() { dataLines = nil }()
			if len(dataLines) == 0 {
				return true
			}
			data := strings.Join(dataLines, "\n")
			var ev core.AgentEvent
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				slog.Debug("tui.http_client.parse_failed", "err", err, "data", data)
				return true
			}
			select {
			case ch <- CoreEventToAgentEvent(ev):
				return true
			case <-ctx.Done():
				return false
			}
		}

		for scanner.Scan() {
			if ctx.Err() != nil {
				return
			}
			line := scanner.Text()
			switch {
			case line == "":
				if !dispatch() {
					return
				}
			case strings.HasPrefix(line, ":"):
				// SSE comment (keep-alive); ignore.
			case strings.HasPrefix(line, "event:"):
				// Event type is redundant with the JSON kind field; ignore.
			case strings.HasPrefix(line, "data:"):
				dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
		// Flush a trailing event without a final blank line.
		if len(dataLines) > 0 {
			dispatch()
		}
		if err := scanner.Err(); err != nil {
			slog.Debug("tui.http_client.read_failed", "err", err)
		}
	}()
	return ch
}
