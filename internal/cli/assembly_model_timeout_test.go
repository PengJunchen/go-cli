package cli

import (
	"context"
	"encoding/json"
	"errors"
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
	const (
		configuredTotal = 2 * time.Second
		configuredIdle  = 500 * time.Millisecond
		maxCallElapsed  = 4 * time.Second
	)
	type requestObservation struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	var requests atomic.Int32
	entered := make(chan requestObservation, 2)
	canceled := make(chan requestObservation, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		var request requestObservation
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode configured-timeout request: %v", err)
			http.Error(w, "invalid fixture request", http.StatusBadRequest)
			return
		}
		select {
		case entered <- request:
		default:
			t.Error("unexpected extra configured-timeout request")
			http.Error(w, "unexpected extra request", http.StatusBadRequest)
			return
		}
		if request.Stream {
			writeAssemblyStreamHeaders(w)
			// Let the concrete provider finish synchronous stream setup before
			// withholding later chunks to exercise the configured idle deadline.
			writeAssemblyChunk(w, map[string]any{"role": "assistant", "content": "probe"}, nil)
		}
		// Withhold all model output until the actual client request is canceled.
		// This observes configured cancellation after HTTP dispatch rather than
		// racing a millisecond budget against the scheduler before handler entry.
		<-r.Context().Done()
		select {
		case canceled <- request:
		default:
			t.Error("unexpected extra configured-timeout cancellation")
		}
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
			Total:      configuredTotal.String(),
			StreamIdle: configuredIdle.String(),
		}},
	}
	assembly, err := AssembleAgent(context.Background(), rc, providerName, rc.Model.Name, io.Discard)
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	t.Cleanup(cancel)
	observe := func(observations <-chan requestObservation) requestObservation {
		t.Helper()
		select {
		case request := <-observations:
			return request
		case <-ctx.Done():
			t.Fatal("configured-timeout request observation did not arrive before the outer guard")
			return requestObservation{}
		}
	}
	primaryStart := time.Now()
	stream, err := assembly.Harness.Submit(ctx, "timeout probe")
	require.NoError(t, err)
	primaryRequest := observe(entered)
	require.Equal(t, requestObservation{Model: "primary-model", Stream: true}, primaryRequest)
	for range stream.Events() {
	}
	_, primaryErr := stream.Result()
	primaryElapsed := time.Since(primaryStart)
	require.Error(t, primaryErr)
	assert.ErrorIs(t, primaryErr, context.DeadlineExceeded)
	var primaryAbort *llm.LocalStreamAbortCause
	idleCause := primaryErr
	if errors.As(primaryErr, &primaryAbort) {
		idleCause = primaryAbort.Unwrap()
	}
	assert.Contains(t, idleCause.Error(), "stream idle exceeded "+configuredIdle.String())
	assert.GreaterOrEqual(t, primaryElapsed, configuredIdle)
	assert.Less(t, primaryElapsed, maxCallElapsed, "the configured idle timeout must precede the outer guard and default budgets")
	assert.Equal(t, primaryRequest, observe(canceled), "the primary HTTP request actually observes cancellation")

	small := assembly.ModelConfig.ModelSelector.SmallModel()
	require.NotNil(t, small)
	smallStart := time.Now()
	_, smallErr := small.Generate(withCLITestOperation(ctx), []llm.Message{{Role: llm.RoleUser, Content: "timeout probe"}})
	smallElapsed := time.Since(smallStart)
	smallRequest := observe(entered)
	require.Equal(t, requestObservation{Model: "small-model", Stream: false}, smallRequest)
	require.Error(t, smallErr)
	assert.ErrorIs(t, smallErr, context.DeadlineExceeded)
	assert.Contains(t, smallErr.Error(), "generate exceeded "+configuredTotal.String())
	assert.GreaterOrEqual(t, smallElapsed, configuredTotal)
	assert.Less(t, smallElapsed, maxCallElapsed, "the small-model total timeout must precede the outer guard and default budgets")
	assert.Equal(t, smallRequest, observe(canceled), "the small-model HTTP request actually observes cancellation")
	assert.EqualValues(t, 2, requests.Load())
}
