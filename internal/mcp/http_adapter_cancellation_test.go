package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHTTPAdapterSendsBestEffortCancellationNotification(t *testing.T) {
	tests := []struct {
		name               string
		notificationStatus int
	}{
		{name: "accepted", notificationStatus: http.StatusAccepted},
		{name: "notification rejected", notificationStatus: http.StatusServiceUnavailable},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			const sessionID = "session-cancel-test"
			type capturedNotification struct {
				ID     json.RawMessage
				Method string
				Params struct {
					RequestID json.RawMessage `json:"requestId"`
					Reason    string          `json:"reason"`
				} `json:"params"`
			}

			callStarted := make(chan json.RawMessage, 1)
			notificationSeen := make(chan capturedNotification, 1)
			var toolCalls atomic.Int32
			var cancelNotifications atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				if r.Method != http.MethodPost {
					t.Errorf("unexpected HTTP method %q", r.Method)
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				method, id, params, err := decodeHTTPMCPRequest(r)
				if err != nil {
					t.Errorf("decode MCP request: %v", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				switch method {
				case "initialize":
					w.Header().Set("Mcp-Session-Id", sessionID)
					writeHTTPMCPJSONRPCResponse(w, id, map[string]any{
						"protocolVersion": LatestProtocolVersion,
						"capabilities":    map[string]any{},
						"serverInfo":      map[string]any{"name": "cancel-test", "version": "1"},
					})
				case "notifications/initialized":
					w.WriteHeader(http.StatusAccepted)
				case "tools/call":
					toolCalls.Add(1)
					callStarted <- id
					<-r.Context().Done()
				case "notifications/cancelled":
					cancelNotifications.Add(1)
					assert.Empty(t, id, "cancellation must be a JSON-RPC notification")
					assert.Equal(t, LatestProtocolVersion, r.Header.Get("MCP-Protocol-Version"))
					assert.Equal(t, sessionID, r.Header.Get("Mcp-Session-Id"))
					var notification capturedNotification
					notification.Method = method
					if err := json.Unmarshal(params, &notification.Params); err != nil {
						t.Errorf("decode cancellation params: %v", err)
					}
					notificationSeen <- notification
					w.WriteHeader(test.notificationStatus)
				default:
					t.Errorf("unexpected JSON-RPC method %q", method)
					w.WriteHeader(http.StatusBadRequest)
				}
			}))
			t.Cleanup(srv.Close)

			adapter := newHTTPTestAdapter(srv)
			require.NoError(t, adapter.Connect(context.Background()))

			callCtx, cancel := context.WithCancel(context.Background())
			callDone := make(chan error, 1)
			go func() {
				_, err := adapter.CallTool(callCtx, "write", map[string]any{"path": "file"})
				callDone <- err
			}()

			var callID json.RawMessage
			select {
			case callID = <-callStarted:
			case <-time.After(2 * time.Second):
				cancel()
				t.Fatal("tools/call was not received")
			}
			cancel()

			var callErr error
			select {
			case callErr = <-callDone:
			case <-time.After(3 * time.Second):
				t.Fatal("cancelled tools/call did not return")
			}
			require.Error(t, callErr)
			assertToolOutcomeUnknown(t, callErr)
			assert.EqualValues(t, 1, toolCalls.Load(), "an uncertain tool call must not be replayed")

			select {
			case notification := <-notificationSeen:
				assert.Equal(t, "notifications/cancelled", notification.Method)
				assert.JSONEq(t, string(callID), string(notification.Params.RequestID), "notification must target the exact request")
				assert.Equal(t, "local run cancelled", notification.Params.Reason)
			case <-time.After(3 * time.Second):
				t.Fatal("server did not receive notifications/cancelled")
			}
			assert.EqualValues(t, 1, cancelNotifications.Load())
			assertToolOutcomeUnknown(t, callErr) // A failed notification cannot establish remote quiescence.
			require.NoError(t, adapter.Disconnect(context.Background()))
		})
	}
}
