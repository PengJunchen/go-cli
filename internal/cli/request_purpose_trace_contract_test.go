package cli

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tracing"
)

func TestCLILLMRequestSpansMatchMainSubagentAndSummaryHTTPRequests(t *testing.T) {
	for _, provider := range []struct {
		name     string
		provider string
		model    string
	}{
		{name: "openai-compatible", provider: "test", model: "test-model"},
		{name: "native-openai", provider: "openai", model: "gpt-4o-mini"},
	} {
		t.Run(provider.name, func(t *testing.T) {
			responses := []string{
				`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"delegate-child","type":"function","function":{"name":"dispatch_subagent","arguments":"{\"prompt\":\"child task\"}"}}]}}]}`,
				`{"choices":[{"message":{"role":"assistant","content":"child answer"}}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`,
				`{"choices":[{"message":{"role":"assistant","content":"parent final"}}],"usage":{"prompt_tokens":4,"completion_tokens":1}}`,
				`{"choices":[{"message":{"role":"assistant","content":"summary answer"}}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`,
			}
			var requestCount atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodPost, r.Method)
				assert.Equal(t, "/chat/completions", r.URL.Path)
				index := int(requestCount.Add(1)) - 1
				if index >= len(responses) {
					http.Error(w, "unexpected provider request", http.StatusInternalServerError)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if _, err := fmt.Fprint(w, responses[index]); err != nil {
					t.Errorf("write fake provider response: %v", err)
				}
			}))
			t.Cleanup(server.Close)

			exporter := &assemblyTraceExporter{}
			tracer := tracing.NewTracer("cli-request-purpose", exporter)
			root, ctx := tracer.Start(context.Background(), "cli.invocation", tracing.SpanKindInternal)
			t.Cleanup(root.End)
			cfg := newPromptConfig(t, server.URL)
			cfg.Provider.Name = provider.provider
			cfg.Provider.Model = provider.model
			var output bytes.Buffer
			err := newPromptCmd(&output).Run(ctx, cfg, []string{"-approve=auto", "parent request"})
			require.NoError(t, err)
			assert.Contains(t, output.String(), "parent final")

			summaryConfig := *cfg
			summaryConfig.SmallModel = config.SmallModelConfig{
				Provider: provider.provider,
				APIKey:   "test",
				BaseURL:  server.URL,
				Model:    provider.model,
			}
			smallModel, _, cleanup, err := buildSmallModel(ctx, &summaryConfig, provider.provider, provider.provider, provider.model)
			require.NoError(t, err)
			if cleanup != nil {
				t.Cleanup(cleanup)
			}
			factory := &recordingSummaryOperationFactory{}
			summary, err := (&llmModelSummarizer{model: smallModel}).Summarize(llm.WithRunOperationFactory(ctx, factory), "summary input")
			require.NoError(t, err)
			assert.Equal(t, "summary answer", strings.TrimSpace(summary))
			root.End()
			require.NoError(t, exporter.Shutdown(context.Background()))
			require.EqualValues(t, len(responses), requestCount.Load(), "two main calls, one child call, and one summary call reach the fake provider")

			var requestSpans, attemptSpans int
			purposes := make(map[string]int)
			taskTypes := make(map[string]int)
			for _, span := range exporter.exportedSpans() {
				switch span.Name {
				case "llm.request":
					requestSpans++
					for _, attribute := range span.Attributes {
						switch attribute.Key {
						case "purpose":
							purposes[fmt.Sprint(attribute.Value)]++
						case "task_type":
							taskTypes[fmt.Sprint(attribute.Value)]++
						}
					}
				case "llm.attempt_usage":
					attemptSpans++
				}
			}
			assert.Equal(t, int(requestCount.Load()), requestSpans, "each provider HTTP request has exactly one request span")
			assert.Equal(t, int(requestCount.Load()), attemptSpans, "each provider attempt emits one usage span")
			assert.Equal(t, map[string]int{"main": 2, "subagent": 1, "compaction": 1}, purposes)
			assert.Equal(t, map[string]int{"chat": 3, "summary": 1}, taskTypes)
		})
	}
}
