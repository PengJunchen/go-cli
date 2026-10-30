package tools

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBashPerCallTimeoutPreparedSchema(t *testing.T) {
	ctx := context.Background()
	registry := NewMiddlewareToolRegistry(NewDefaultToolRegistry())
	require.NoError(t, RegisterDefaults(ctx, registry, WithRegisteredBuiltinWhitelist([]string{"bash"})))
	configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{})
	for _, value := range []any{1, 100, json.Number("100"), float64(100), int64(9223372036854)} {
		_, err := registry.PrepareToolCall(ctx, ToolCall{ID: "explicit-ms", Name: "bash", Args: map[string]any{"command": "true", "timeout_ms": value}})
		require.NoError(t, err)
	}
	for _, alias := range []string{"timeout", "timeout_seconds"} {
		_, err := registry.PrepareToolCall(ctx, ToolCall{ID: "not-an-alias", Name: "bash", Args: map[string]any{"command": "true", alias: 100}})
		require.Error(t, err)
	}
	for _, value := range []any{0, -1, 1.5, nil, true, "100", math.NaN(), math.Inf(1), int64(9223372036855), json.Number(strings.Repeat("9", 300))} {
		_, err := registry.PrepareToolCall(ctx, ToolCall{ID: "invalid-prepared-ms", Name: "bash", Args: map[string]any{"command": "true", "timeout_ms": value}})
		require.Error(t, err)
	}
	require.Contains(t, NewBashTool().Description(), "timeout_ms")
}

func executePerCallTimeout(ctx context.Context, mode string, call ToolCall, opts ...BashToolOption) (*ToolResult, error) {
	if mode == "ordinary" {
		return NewBashTool(opts...).Execute(ctx, call)
	}
	tool := NewStreamingBashTool(opts...)
	if mode == "stream-execute" {
		return tool.Execute(ctx, call)
	}
	return tool.ExecuteStreaming(ctx, call, &mockStreamSink{})
}

func TestBashPerCallTimeoutInvalidNeverExecutes(t *testing.T) {
	values := []struct {
		name  string
		value any
	}{
		{"zero", 0}, {"negative", -1}, {"fraction", 1.5}, {"null", nil}, {"bool", true}, {"string", "100"},
		{"nan", math.NaN()}, {"infinity", math.Inf(1)}, {"overflow", int64(9223372036855)}, {"huge-number", json.Number(strings.Repeat("9", 300))},
	}
	for _, mode := range []string{"ordinary", "stream-execute", "stream-sink"} {
		for _, entry := range values {
			t.Run(mode+"/"+entry.name, func(t *testing.T) {
				dir := t.TempDir()
				_, err := executePerCallTimeout(t.Context(), mode, ToolCall{ID: "invalid-ms", Name: "bash", Args: map[string]any{"command": "printf effect > marker", "timeout_ms": entry.value}}, WithNoSandbox(), WithBashWorkdir(dir), WithTimeout(3*time.Second))
				require.Error(t, err)
				require.Contains(t, err.Error(), "timeout_ms")
				_, err = os.Stat(filepath.Join(dir, "marker"))
				require.ErrorIs(t, err, os.ErrNotExist, "invalid argument must be rejected before an OS effect")
			})
		}
	}
}

func TestBashPerCallTimeoutBoundSelection(t *testing.T) {
	for _, tc := range []struct {
		name    string
		host    time.Duration
		tier    bool
		command string
		args    map[string]any
		want    time.Duration
	}{
		{name: "omitted", host: 30 * time.Second, want: 30 * time.Second},
		{name: "100ms", host: 3 * time.Second, args: map[string]any{"timeout_ms": 100}, want: 100 * time.Millisecond},
		{name: "no-host-extension", host: 30 * time.Second, args: map[string]any{"timeout_ms": 60000}, want: 30 * time.Second},
		{name: "explicit-ms-under300", host: 300 * time.Second, args: map[string]any{"timeout_ms": 60000}, want: 60 * time.Second},
		{name: "tier-slow", host: 30 * time.Second, tier: true, command: "go test", args: map[string]any{"timeout_ms": 60000}, want: 60 * time.Second},
		{name: "tier-fast", host: 30 * time.Second, tier: true, command: "ls", args: map[string]any{"timeout_ms": 60000}, want: 5 * time.Second},
		{name: "zero-host", args: map[string]any{"timeout_ms": 100}, want: 0},
		{name: "negative-host", host: -time.Second, args: map[string]any{"timeout_ms": 100}, want: -time.Second},
		{name: "tier-still-overrides-zero", tier: true, command: "go test", args: map[string]any{"timeout_ms": 60000}, want: 60 * time.Second},
		{name: "integral-float", host: time.Second, args: map[string]any{"timeout_ms": float64(1)}, want: time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool := NewBashTool(WithTimeout(tc.host), WithTimeoutTier(tc.tier))
			got, err := tool.executionTimeout(t.Context(), tc.command, tc.args)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
			require.Equal(t, tc.host, tool.Timeout)
			require.Equal(t, tc.tier, tool.TimeoutTier)
		})
	}
	tool := NewBashTool(WithTimeout(3 * time.Second))
	got, err := tool.executionTimeout(WithBashRuntimeExecutionTimeout(t.Context(), 50*time.Millisecond), "sleep", map[string]any{"timeout_ms": 100})
	require.NoError(t, err)
	require.Equal(t, 50*time.Millisecond, got)
	got, err = tool.executionTimeout(WithBashRuntimeExecutionTimeout(t.Context(), 0), "sleep", map[string]any{"timeout_ms": 100})
	require.NoError(t, err)
	require.Zero(t, got)
}
