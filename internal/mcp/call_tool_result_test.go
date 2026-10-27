package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHTTPAdapterRejectsMalformedCallToolResults(t *testing.T) {
	invalid := []string{
		`{}`, `{"content":null}`, `{"content":"legacy string"}`, `{"content":{},"isError":false}`,
		`{"content":[],"isError":"false"}`, `{"content":[],"isError":null}`,
		`{"content":[{"type":"text"}]}`, `{"content":[{"type":"text","text":12}]}`,
		`{"content":[{"type":"unsupported","text":"x"}]}`,
		`{"content":[],"content":[]}`, `{"Content":[],"content":[]}`,
		`{"content":[],"isError":true,"isError":false}`, `{"content":[],"IsError":false}`,
		`{"content":[{"type":"text","text":"first","text":"second"}]}`,
		`{"content":[{"type":"text","Text":"hidden","text":"visible"}]}`,
	}
	for _, sse := range []bool{false, true} {
		for index, rawResult := range invalid {
			t.Run(fmt.Sprintf("sse=%t/case=%d", sse, index), func(t *testing.T) {
				peer := newHTTPMCPServer(t, sse)
				original := peer.Config.Handler
				peer.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, err := io.ReadAll(r.Body)
					if err != nil {
						http.Error(w, "read", 400)
						return
					}
					r.Body = io.NopCloser(bytes.NewReader(body))
					var request struct {
						ID     json.RawMessage `json:"id"`
						Method string          `json:"method"`
					}
					if json.Unmarshal(body, &request) != nil || request.Method != "tools/call" {
						original.ServeHTTP(w, r)
						return
					}
					frame := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":%s}`, request.ID, rawResult)
					if sse {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = fmt.Fprintf(w, "event:message\ndata:%s\n\n", frame)
					} else {
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, frame)
					}
				})
				adapter := newHTTPTestAdapter(peer)
				require.NoError(t, adapter.Connect(context.Background()))
				defer func() { _ = adapter.Disconnect(context.Background()) }()
				result, err := adapter.CallTool(context.Background(), "write", map[string]any{})
				require.Error(t, err, "invalid result must not establish successful method completion: %s", rawResult)
				require.Nil(t, result)
				var unknown OutcomeUnknown
				require.ErrorAs(t, err, &unknown)
				require.True(t, unknown.OutcomeUnknown())
			})
		}
	}
}

func mcpTextContent(text string) []any { return []any{map[string]any{"type": "text", "text": text}} }

func TestCallToolResultOptionalFieldsValidateSchema(t *testing.T) {
	invalid := []string{
		`{"content":[],"_meta":42}`,
		`{"content":[],"_meta":null}`,
		`{"content":[{"type":"text","text":"ok","_meta":[]}]}`,
		`{"content":[{"type":"text","text":"ok","annotations":{"audience":"user"}}]}`,
		`{"content":[{"type":"text","text":"ok","annotations":{"audience":["tool"]}}]}`,
		`{"content":[{"type":"text","text":"ok","annotations":{"audience":[1]}}]}`,
		`{"content":[{"type":"text","text":"ok","annotations":{"priority":"high"}}]}`,
		`{"content":[{"type":"text","text":"ok","annotations":{"priority":-0.01}}]}`,
		`{"content":[{"type":"text","text":"ok","annotations":{"priority":1.01}}]}`,
		`{"content":[{"type":"text","text":"ok","annotations":{"lastModified":42}}]}`,
		`{"content":[{"type":"resource_link","uri":"x://x","name":"ok","size":"huge"}]}`,
		`{"content":[{"type":"resource_link","uri":"x://x","name":"ok","size":null}]}`,
		`{"content":[{"type":"resource_link","uri":"x://x","name":"ok","title":2}]}`,
		`{"content":[{"type":"resource_link","uri":"x://x","name":"ok","description":false}]}`,
		`{"content":[{"type":"resource","resource":{"uri":"x://x","text":"ok","_meta":false}}]}`,
	}
	for index, raw := range invalid {
		t.Run(fmt.Sprintf("invalid-%d", index), func(t *testing.T) {
			result, err := parseJSONRPCResponse([]byte(`{"jsonrpc":"2.0","id":1,"result":`+raw+`}`), 1)
			require.NoError(t, err)
			_, err = decodeCallToolResult(result)
			require.Error(t, err)
		})
	}
	valid := []string{
		`{"content":[],"_meta":{"vendor/arbitrary":[1,null,{"future":false}]},"vendorExtension":{"anything":true}}`,
		`{"content":[{"type":"text","text":"ok","_meta":{"futureField":42},"annotations":{"audience":[],"priority":0,"lastModified":"2026-10-27T12:30:00Z","futureHint":{"open":true}}}]}`,
		`{"content":[{"type":"text","text":"ok","annotations":{"audience":["user","assistant"],"priority":1}}]}`,
		`{"content":[{"type":"resource_link","uri":"x://x","name":"ok","size":0.5,"title":"display","description":"desc","mimeType":"text/plain","_meta":{"new":false}}]}`,
	}
	for index, raw := range valid {
		t.Run(fmt.Sprintf("valid-%d", index), func(t *testing.T) {
			result, err := parseJSONRPCResponse([]byte(`{"jsonrpc":"2.0","id":1,"result":`+raw+`}`), 1)
			require.NoError(t, err)
			_, err = decodeCallToolResult(result)
			require.NoError(t, err, "known fields must not constrain vendor extensions or metadata contents")
		})
	}
}
