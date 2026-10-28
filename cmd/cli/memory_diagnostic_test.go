package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/config"
)

func TestMemoryDiagnosticUsesInvocationErrorWriter(t *testing.T) {
	for _, mode := range []string{"json", "stream", "text"} {
		t.Run(mode, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Chdir(t.TempDir())
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Stream bool `json:"stream"`
				}
				require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
				if request.Stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
			}))
			defer server.Close()
			path := filepath.Join(home, "memory-is-directory")
			require.NoError(t, os.Mkdir(path, 0700))
			cfg := &config.Config{Provider: config.ProviderConfig{Name: "test", BaseURL: server.URL, APIKey: "test", Model: "test-model"}, Memory: config.MemoryConfig{StorePath: path}}
			var output, diagnostics bytes.Buffer
			code := run([]string{"prompt", "--output", mode, "question"}, &output, &diagnostics, func() (*config.Config, error) { return cfg, nil })
			require.Zero(t, code, diagnostics.String())
			require.Contains(t, diagnostics.String(), "Memory store unavailable; continuing without memory")
			require.NotContains(t, output.String(), "Memory store unavailable")
			if mode == "json" {
				decoder := json.NewDecoder(&output)
				count := 0
				for {
					var event map[string]any
					err := decoder.Decode(&event)
					if err == io.EOF {
						break
					}
					require.NoError(t, err)
					count++
				}
				require.Positive(t, count)
			}
		})
	}
}
