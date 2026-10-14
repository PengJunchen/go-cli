package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveModelTimeoutsKeepsDefaultsAndAppliesPartialOverrides(t *testing.T) {
	tests := []struct {
		name       string
		config     config.ModelTimeoutConfig
		wantTotal  time.Duration
		wantIdle   time.Duration
		wantActive bool
	}{
		{name: "unset", wantTotal: 30 * time.Second, wantIdle: 10 * time.Second},
		{name: "total override", config: config.ModelTimeoutConfig{Total: "5m"}, wantTotal: 5 * time.Minute, wantIdle: 10 * time.Second, wantActive: true},
		{name: "idle override", config: config.ModelTimeoutConfig{StreamIdle: "20s"}, wantTotal: 30 * time.Second, wantIdle: 20 * time.Second, wantActive: true},
		{name: "both overrides", config: config.ModelTimeoutConfig{Total: "5m", StreamIdle: "90s"}, wantTotal: 5 * time.Minute, wantIdle: 90 * time.Second, wantActive: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rc := &config.Config{}
			rc.Production.ModelTimeout = tt.config
			total, idle, active, err := resolveModelTimeouts(rc)
			require.NoError(t, err)
			assert.Equal(t, tt.wantTotal, total)
			assert.Equal(t, tt.wantIdle, idle)
			assert.Equal(t, tt.wantActive, active)
		})
	}

	bad := &config.Config{}
	bad.Production.ModelTimeout = config.ModelTimeoutConfig{Total: "5s", StreamIdle: "6s"}
	_, _, _, err := resolveModelTimeouts(bad)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stream_idle must be no greater than total")
}

func TestAssembledConfiguredModelTimeoutAppliesToPrimaryAndSmallModel(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		var request struct {
			Stream bool `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		if request.Stream {
			writeAssemblyStreamHeaders(w)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(150 * time.Millisecond):
			}
			writeAssemblyTextResponse(w, []string{"ok"}, "stop")
			return
		}
		time.Sleep(150 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	t.Cleanup(server.Close)

	const providerName = "openai"
	rc := &config.Config{
		Provider: config.ProviderConfig{Name: providerName, BaseURL: server.URL, APIKey: newTestCredentialCanary(t)},
		Model:    config.ModelConfig{Name: "primary-model"},
		SmallModel: config.SmallModelConfig{
			Model: "small-model",
		},
		Production: config.ProductionConfig{ModelTimeout: config.ModelTimeoutConfig{
			Total:      "25ms",
			StreamIdle: "15ms",
		}},
	}
	assembly, err := AssembleAgent(context.Background(), rc, providerName, rc.Model.Name, io.Discard)
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	stream, err := assembly.Harness.Submit(ctx, "timeout probe")
	require.NoError(t, err)
	for range stream.Events() {
	}
	_, primaryErr := stream.Result()
	require.Error(t, primaryErr)
	assert.ErrorIs(t, primaryErr, context.DeadlineExceeded)

	small := assembly.ModelConfig.ModelSelector.SmallModel()
	require.NotNil(t, small)
	_, smallErr := small.Generate(withCLITestOperation(context.Background()), []llm.Message{{Role: llm.RoleUser, Content: "timeout probe"}})
	require.Error(t, smallErr)
	assert.ErrorIs(t, smallErr, context.DeadlineExceeded)
	assert.EqualValues(t, 2, requests.Load())
}
