package tui

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/pengjunchen/go-cli/internal/core"
)

// Default tuning for the ACP SSE stream adapter. The heartbeat window is
// generous because SSE servers commonly emit keep-alive comments every few
// seconds; only a truly silent link is treated as dead.
const (
	acpDefaultHeartbeat  = 30 * time.Second
	acpDefaultBackoff    = 1 * time.Second
	acpDefaultMaxBackoff = 30 * time.Second
)

// ACPStreamAdapter consumes SSE events from a remote ACP HTTP server and
// converts them into a channel of tui.AgentEvent, mimicking the in-process
// BridgeEvents flow. This enables the TUI to run in remote mode, decoupled
// from the core runtime process that owns the agent loop.
//
// The adapter is resilient: when the connection drops, the server returns a
// non-200 status, or the link goes silent past the heartbeat window, it
// reconnects with exponential backoff and resumes the event stream using the
// Last-Event-ID header. Reconnect behavior is configurable via ACPOption.
type ACPStreamAdapter struct {
	remoteURL string

	httpClient       *http.Client
	maxReconnects    int // 0 = unlimited
	heartbeatTimeout time.Duration
	backoffBase      time.Duration
	backoffMax       time.Duration
}

// ACPOption configures an ACPStreamAdapter.
type ACPOption func(*ACPStreamAdapter)

// WithMaxReconnects caps the number of reconnect attempts after the initial
// connection. A value of 0 (the default) means retry indefinitely. A negative
// value is treated as 0.
func WithMaxReconnects(n int) ACPOption {
	return func(a *ACPStreamAdapter) { a.maxReconnects = n }
}

// WithHeartbeatTimeout sets how long the adapter waits for any data (including
// keep-alive comments) before declaring the link dead and reconnecting.
func WithHeartbeatTimeout(d time.Duration) ACPOption {
	return func(a *ACPStreamAdapter) { a.heartbeatTimeout = d }
}

// WithBackoffBase sets the initial reconnect delay. Each subsequent failure
// doubles the delay, capped by WithBackoffMax.
func WithBackoffBase(d time.Duration) ACPOption {
	return func(a *ACPStreamAdapter) { a.backoffBase = d }
}

// WithBackoffMax caps the exponential backoff delay between reconnects.
func WithBackoffMax(d time.Duration) ACPOption {
	return func(a *ACPStreamAdapter) { a.backoffMax = d }
}

// NewACPStreamAdapter creates an adapter that connects to the given SSE
// endpoint URL. The URL should include any required query parameters (e.g.
// sender_id). Optional configuration may be supplied via ACPOption.
func NewACPStreamAdapter(remoteURL string, opts ...ACPOption) *ACPStreamAdapter {
	a := &ACPStreamAdapter{remoteURL: remoteURL}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// acpDefaultHTTPClient returns an *http.Client tuned for long-lived SSE
// streams: dial and TLS timeouts guard against stuck handshakes, while the
// overall Client.Timeout is intentionally left at zero so an idle-but-alive
// stream is not killed mid-stream. Liveness is instead enforced by the
// adapter's heartbeat watchdog.
func acpDefaultHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}
}

// Stream opens an HTTP connection to the remote SSE endpoint and returns a
// channel of tui.AgentEvent. The channel stays open across reconnects and only
// closes when the context is canceled, the reconnect limit is exhausted, or an
// unrecoverable request-construction error occurs.
func (a *ACPStreamAdapter) Stream(ctx context.Context) <-chan AgentEvent {
	ch := make(chan AgentEvent, 64)
	go func() {
		defer close(ch)

		client := a.httpClient
		if client == nil {
			client = acpDefaultHTTPClient()
		}
		heartbeat := a.heartbeatTimeout
		if heartbeat <= 0 {
			heartbeat = acpDefaultHeartbeat
		}
		base := a.backoffBase
		if base <= 0 {
			base = acpDefaultBackoff
		}
		ceil := a.backoffMax
		if ceil <= 0 {
			ceil = acpDefaultMaxBackoff
		}

		var lastEventID string
		reconnects := 0
		wait := base

		for {
			if ctx.Err() != nil {
				return
			}

			req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.remoteURL, nil)
			if err != nil {
				// A malformed URL or canceled context leaves nothing to retry.
				slog.Warn("tui.http_client.request_failed", "url", a.remoteURL, "err", err)
				return
			}
			req.Header.Set("Accept", "text/event-stream")
			if lastEventID != "" {
				req.Header.Set("Last-Event-ID", lastEventID)
			}

			resp, err := client.Do(req)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				slog.Warn("tui.http_client.connect_failed", "url", a.remoteURL, "err", err)
				if !a.scheduleReconnect(ctx, wait, reconnects) {
					return
				}
				reconnects++
				wait = growBackoff(wait, base, ceil)
				continue
			}

			if resp.StatusCode != http.StatusOK {
				slog.Warn("tui.http_client.bad_status", "url", a.remoteURL, "status", resp.StatusCode)
				resp.Body.Close()
				if !a.scheduleReconnect(ctx, wait, reconnects) {
					return
				}
				reconnects++
				wait = growBackoff(wait, base, ceil)
				continue
			}

			if reconnects > 0 {
				slog.Info("tui.http_client.reconnected", "url", a.remoteURL, "attempt", reconnects)
			}

			ctxCanceled, id := a.processStream(ctx, resp.Body, ch, heartbeat)
			resp.Body.Close()

			if id != "" {
				lastEventID = id
			}

			if ctxCanceled || ctx.Err() != nil {
				return
			}

			if !a.scheduleReconnect(ctx, wait, reconnects) {
				return
			}
			reconnects++
			wait = growBackoff(wait, base, ceil)
		}
	}()
	return ch
}

// scheduleReconnect waits for the backoff delay, respecting context
// cancellation and the reconnect limit. It returns true if the caller should
// retry, false if it must stop (context done or limit reached).
func (a *ACPStreamAdapter) scheduleReconnect(ctx context.Context, wait time.Duration, reconnects int) bool {
	if a.maxReconnects > 0 && reconnects >= a.maxReconnects {
		slog.Warn("tui.http_client.max_reconnects_exceeded", "url", a.remoteURL, "attempts", reconnects)
		return false
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// growBackoff doubles the current backoff, clamped to [base, ceil].
func growBackoff(current, base, ceil time.Duration) time.Duration {
	next := current * 2
	if next > ceil {
		next = ceil
	}
	if next < base {
		next = base
	}
	return next
}

// processStream reads SSE lines from body, dispatches decoded events to ch,
// and watches a heartbeat timer. Any received line (including keep-alive
// comments) resets the timer. It returns ctxCanceled=true if the context was
// canceled (the caller should stop), and the last seen event id for resume.
func (a *ACPStreamAdapter) processStream(ctx context.Context, body io.ReadCloser, ch chan<- AgentEvent, heartbeat time.Duration) (ctxCanceled bool, lastEventID string) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	lineCh := make(chan string)
	done := make(chan struct{})
	scanDone := make(chan struct{})

	// Reader goroutine: feed scanned lines to lineCh, or stop when done is
	// closed (heartbeat/ctx shutdown) so we never block on an unattended send.
	go func() {
		defer close(scanDone)
		defer close(lineCh)
		for scanner.Scan() {
			select {
			case lineCh <- scanner.Text():
			case <-done:
				return
			}
		}
	}()

	heartbeatTimer := time.NewTimer(heartbeat)
	defer heartbeatTimer.Stop()

	var dataLines []string
	var eventID string

	// stopReader tears down the reader goroutine and the underlying body so
	// processStream can return. Safe to call exactly once per invocation.
	stopReader := func() {
		close(done)
		body.Close()
		<-scanDone
	}

	// dispatch parses accumulated SSE data lines into a core.AgentEvent and
	// forwards it as a tui.AgentEvent. Returns false if the context was
	// canceled while sending.
	dispatch := func() bool {
		defer func() { dataLines = nil }()
		if len(dataLines) == 0 {
			return true
		}
		data := strings.Join(dataLines, "\n")
		var ev core.AgentEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			slog.Error("tui.http_client.parse_failed", "err", err, "data", data)
			return true
		}
		select {
		case ch <- CoreEventToAgentEvent(ev):
			return true
		case <-ctx.Done():
			return false
		}
	}

	for {
		select {
		case <-ctx.Done():
			stopReader()
			return true, eventID
		case <-heartbeatTimer.C:
			slog.Warn("tui.http_client.heartbeat_timeout", "url", a.remoteURL, "timeout", heartbeat)
			stopReader()
			return false, eventID
		case line, ok := <-lineCh:
			if !ok {
				// Stream ended (EOF or read error). Drain the reader and flush
				// any trailing event without a final blank line.
				<-scanDone
				if err := scanner.Err(); err != nil {
					slog.Warn("tui.http_client.read_failed", "url", a.remoteURL, "err", err)
				}
				if len(dataLines) > 0 {
					dispatch()
				}
				return false, eventID
			}
			// Any traffic — including keep-alive comments — proves liveness.
			if !heartbeatTimer.Stop() {
				select {
				case <-heartbeatTimer.C:
				default:
				}
			}
			heartbeatTimer.Reset(heartbeat)

			switch {
			case line == "":
				if !dispatch() {
					stopReader()
					return true, eventID
				}
			case strings.HasPrefix(line, ":"):
				// SSE comment (keep-alive); liveness already recorded above.
			case strings.HasPrefix(line, "id:"):
				eventID = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
			case strings.HasPrefix(line, "event:"):
				// Event type is redundant with the JSON kind field; ignore.
			case strings.HasPrefix(line, "data:"):
				dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
	}
}
