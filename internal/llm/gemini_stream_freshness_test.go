package llm

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGeminiStreamPrimaryFreshness(t *testing.T) {
	for _, test := range []struct {
		name     string
		frames   []string
		coverage PrimaryUsageCoverage
		output   int
		failed   bool
	}{
		{"early-then-text", []string{`{"candidates":[{"content":{"parts":[{"text":"a"}]}}],"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":0}}`, `{"candidates":[{"content":{"parts":[{"text":"b"}]},"finishReason":"STOP"}]}`}, UsageCoveragePartial, 0, false},
		{"early-then-tool", []string{`{"candidates":[{"content":{"parts":[{"text":"a"}]}}],"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":0}}`, `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"tool","args":{}}}]},"finishReason":"STOP"}]}`}, UsageCoveragePartial, 0, false},
		{"terminal-usage-omitted-no-output", []string{`{"candidates":[{"content":{"parts":[{"text":"a"}]}}],"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":0}}`, `{"candidates":[{"finishReason":"STOP"}]}`}, UsageCoverageUnknown, 0, false},
		{"final-usage-refresh", []string{`{"candidates":[{"content":{"parts":[{"text":"a"}]}}],"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":0}}`, `{"candidates":[{"content":{"parts":[{"text":"b"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":5}}`}, UsageCoverageComplete, 5, false},
		{"usage-after-terminal", []string{`{"candidates":[{"content":{"parts":[{"text":"a"}]}}],"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":0}}`, `{"candidates":[{"content":{"parts":[{"text":"b"}]},"finishReason":"STOP"}]}`, `{"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":5}}`}, UsageCoverageComplete, 5, false},
		{"invalid-terminal-does-not-overwrite", []string{`{"candidates":[{"content":{"parts":[{"text":"a"}]}}],"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":0}}`, `{"candidates":[{"content":{"parts":[{"functionCall":{"args":{}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":200,"candidatesTokenCount":20}}`}, UsageCoverageUnknown, 0, true},
		{"overflowing-terminal", []string{`{"candidates":[{"content":{"parts":[{"text":"a"}]}}],"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":0}}`, fmt.Sprintf(`{"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":%d,"candidatesTokenCount":1}}`, math.MaxInt)}, UsageCoverageUnknown, 0, false},
		{"failure-before-output", []string{`{"candidates":[{"content":{"parts":[{"text":"a"}]}}],"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":0}}`, `{invalid`}, UsageCoverageUnknown, 0, true},
		{"failure-after-output", []string{`{"candidates":[{"content":{"parts":[{"text":"a"}]}}],"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":0}}`, `{"candidates":[{"content":{"parts":[{"text":"b"}]}}]}`, `{invalid`}, UsageCoveragePartial, 0, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				for _, frame := range test.frames {
					writeSSE(w, frame)
				}
			}))
			defer server.Close()
			model, _, err := NewGeminiProvider(WithNativeBaseURL(server.URL), WithNativeAPIKey("fixture")).Build(context.Background(), ModelConfig{Model: "m"})
			require.NoError(t, err)
			stream, err := model.Stream(context.Background(), nil)
			require.NoError(t, err)
			var got *Usage
			var streamErr error
			for chunk := range stream {
				if chunk.Final || chunk.Error != nil {
					got = chunk.Usage
				}
				if chunk.Error != nil {
					streamErr = chunk.Error
				}
			}
			if test.failed {
				require.Error(t, streamErr)
			} else {
				require.NoError(t, streamErr)
			}
			require.NotNil(t, got, "prior valid usage must survive")
			assert.Equal(t, 100, got.InputTokens)
			assert.Equal(t, test.output, got.OutputTokens)
			assert.Equal(t, test.coverage, got.PrimaryCoverage)
		})
	}
}
