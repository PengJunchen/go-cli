package llm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClaudeStreamFailureKeepsObservedUsage(t *testing.T) {
	for _, test := range []struct {
		name, extra string
		coverage    PrimaryUsageCoverage
	}{
		{"before-output", "", UsageCoverageUnknown},
		{"after-output", "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n", UsageCoveragePartial},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"role\":\"assistant\",\"usage\":{\"input_tokens\":100,\"output_tokens\":0}}}\n\n%sevent: message_stop\ndata: {invalid\n\n", test.extra)
			}))
			defer server.Close()
			model, _, err := NewClaudeProvider(WithNativeBaseURL(server.URL), WithNativeAPIKey("fixture")).Build(context.Background(), ModelConfig{Model: "m"})
			require.NoError(t, err)
			stream, err := model.Stream(context.Background(), nil)
			require.NoError(t, err)
			var streamErr error
			var observed *Usage
			for chunk := range stream {
				if chunk.Error != nil {
					streamErr = chunk.Error
					observed = chunk.Usage
				}
			}
			require.Error(t, streamErr)
			require.NotNil(t, observed)
			assert.Equal(t, 100, observed.InputTokens)
			assert.Equal(t, 0, observed.OutputTokens)
			assert.Equal(t, test.coverage, observed.PrimaryCoverage)
		})
	}
}
