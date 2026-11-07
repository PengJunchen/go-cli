package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
)

func TestAssembleAgentWiresConfiguredMultimodalReserveIntoLoop(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"image received\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":3}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)

	cfg := newAssembleTestConfig(t)
	cfg.Provider.BaseURL = server.URL
	cfg.Agent.SetMultimodalTokensPerItemReserve(256)
	assembly, err := AssembleAgent(context.Background(), cfg, cfg.Provider.Name, "multimodal-reserve-model", io.Discard)
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)

	_, err = assembly.LoopAgent.Run(context.Background(), core.Submission{
		Content: "Describe this image.",
		History: []core.AgentMessage{{
			Role: "user",
			ContentBlocks: []llm.ContentBlock{{
				Type:     "image_url",
				ImageURL: &llm.ImageURL{URL: "https://example.invalid/image.png"},
			}},
		}},
	})
	require.NoError(t, err)
	require.EqualValues(t, 1, requests.Load(), "a finite configured reserve should let the assembled loop pass multimodal preflight")
}
