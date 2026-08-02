// Package e2e_20260802 contains end-to-end integration tests for the production
// and MCP modules of go-cli. It exercises retry policies (classification,
// exponential backoff with jitter), circuit breakers (three-state machine,
// fallback, reset), loop detection (edit count, test failure, same-tool-call),
// idempotent cache (FIFO eviction), audit log (JSONL persistence, query
// filtering), telemetry (record/snapshot), output guards (regex, PII, code
// injection, length, chain), output guard middleware, error categorization,
// plus MCP server lifecycle, tool registration/calling, MCPToolAdapter
// wrapping, tool name normalization, hot reload lifecycle, and a complex
// pipeline through MockMCPServer → ApprovalMiddleware → ToolRegistry.
package e2e_20260802

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/extension"
	"github.com/pengjunchen/go-cli/internal/mcp"
	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/production"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/tracing"
)

// =============================================================================
// PRODUCTION: Retry Policy
// =============================================================================

func TestProduction_RetryPolicy_TransientErrors(t *testing.T) {
	ctx := context.Background()
	p := production.NewDefaultRetryPolicy(production.RetryConfig{
		MaxAttempts: 3,
		BaseDelay:   1 * time.Millisecond,
		Jitter:      0,
	})
	err := production.NewError(production.ErrorTransient, errors.New("connection reset"))
	assert.True(t, p.ShouldRetry(ctx, err, 0))
	assert.True(t, p.ShouldRetry(ctx, err, 1))
	assert.True(t, p.ShouldRetry(ctx, err, 2))
	assert.False(t, p.ShouldRetry(ctx, err, 3))
}

func TestProduction_RetryPolicy_FatalErrors(t *testing.T) {
	ctx := context.Background()
	p := production.NewDefaultRetryPolicy(production.RetryConfig{MaxAttempts: 5})
	err := production.NewError(production.ErrorFatal, errors.New("permission denied"))
	assert.False(t, p.ShouldRetry(ctx, err, 0))
}

func TestProduction_RetryPolicy_ExponentialBackoff(t *testing.T) {
	ctx := context.Background()
	p := production.NewDefaultRetryPolicy(production.RetryConfig{
		MaxAttempts: 5,
		BaseDelay:   10 * time.Millisecond,
		MaxDelay:    200 * time.Millisecond,
		Jitter:      0,
	})
	assert.Equal(t, 10*time.Millisecond, p.NextBackoff(ctx, 0))
	assert.Equal(t, 20*time.Millisecond, p.NextBackoff(ctx, 1))
	assert.Equal(t, 40*time.Millisecond, p.NextBackoff(ctx, 2))
	assert.Equal(t, 80*time.Millisecond, p.NextBackoff(ctx, 3))
}

func TestProduction_RetryPolicy_ClassifyTextErrors(t *testing.T) {
	p, ok := production.NewDefaultRetryPolicy(production.RetryConfig{}).(*production.DefaultRetryPolicy)
	require.True(t, ok)
	assert.Equal(t, production.ErrorTransient, p.Classify(errors.New("connection refused")))
	assert.Equal(t, production.ErrorRateLimit, p.Classify(errors.New("429 too many requests")))
	assert.Equal(t, production.ErrorTimeout, p.Classify(errors.New("context deadline exceeded")))
	assert.Equal(t, production.ErrorFatal, p.Classify(errors.New("unknown error")))
}

// =============================================================================
// PRODUCTION: Circuit Breaker
// =============================================================================

func TestProduction_CircuitBreaker_ThreeStateMachine(t *testing.T) {
	clock := &fakeClock{t: time.Now()}
	cb := production.NewDefaultCircuitBreaker(production.CircuitBreakerConfig{
		FailureThreshold: 2,
		RecoveryTimeout:  30 * time.Second,
		HalfOpenMaxCalls: 1,
		SuccessThreshold: 1,
	}, production.WithClock(clock.Now))
	assert.Equal(t, production.CircuitClosed, cb.State())

	// Two failures open the circuit.
	_, err := cb.Execute(context.Background(), func() (any, error) { return nil, errors.New("fail") })
	assert.Error(t, err)
	_, err = cb.Execute(context.Background(), func() (any, error) { return nil, errors.New("fail") })
	assert.Error(t, err)
	assert.Equal(t, production.CircuitOpen, cb.State())

	// Advance time past recovery → HalfOpen.
	clock.Advance(31 * time.Second)
	val, err := cb.Execute(context.Background(), func() (any, error) { return "ok", nil })
	assert.NoError(t, err)
	assert.Equal(t, "ok", val)
	assert.Equal(t, production.CircuitClosed, cb.State())
}

func TestProduction_CircuitBreaker_Reset(t *testing.T) {
	cb := production.NewDefaultCircuitBreaker(production.CircuitBreakerConfig{
		FailureThreshold: 1,
	})
	_, _ = cb.Execute(context.Background(), func() (any, error) { return nil, errors.New("fail") })
	assert.Equal(t, production.CircuitOpen, cb.State())
	require.NoError(t, cb.Reset(context.Background()))
	assert.Equal(t, production.CircuitClosed, cb.State())
}

func TestProduction_CircuitBreaker_Fallback(t *testing.T) {
	clock := &fakeClock{t: time.Now()}
	cb := production.NewDefaultCircuitBreaker(production.CircuitBreakerConfig{
		FailureThreshold: 1,
		RecoveryTimeout:  30 * time.Second,
	}, production.WithClock(clock.Now), production.WithFallback(func() (any, error) {
		return "cached-value", nil
	}))
	_, _ = cb.Execute(context.Background(), func() (any, error) { return nil, errors.New("fail") })
	assert.Equal(t, production.CircuitOpen, cb.State())

	val, err := cb.Execute(context.Background(), func() (any, error) { return "fresh", nil })
	assert.NoError(t, err)
	assert.Equal(t, "cached-value", val)
}

// =============================================================================
// PRODUCTION: Loop Detector
// =============================================================================

func TestProduction_LoopDetector_RepeatedToolCalls(t *testing.T) {
	ld := production.NewDefaultLoopDetector(production.LoopDetectionConfig{
		SameToolCallThreshold: 3,
		Disposition:           production.DispositionWarn,
	})
	ctx := context.Background()
	require.NoError(t, ld.Observe(ctx, core.AgentEvent{Kind: production.KindToolCall, Content: `{"tool":"read","args":{"path":"a"}}`}))
	require.NoError(t, ld.Observe(ctx, core.AgentEvent{Kind: production.KindToolCall, Content: `{"tool":"read","args":{"path":"a"}}`}))
	require.NoError(t, ld.Observe(ctx, core.AgentEvent{Kind: production.KindToolCall, Content: `{"tool":"read","args":{"path":"a"}}`}))
	res := ld.Check(ctx)
	assert.True(t, res.Detected)
	assert.Equal(t, production.DimensionSameToolCall, res.Dimension)
	assert.Equal(t, production.DispositionWarn, res.Disposition)
}

func TestProduction_LoopDetector_EditCount(t *testing.T) {
	ld := production.NewDefaultLoopDetector(production.LoopDetectionConfig{
		EditThreshold: 3,
	})
	ctx := context.Background()
	require.NoError(t, ld.Observe(ctx, core.AgentEvent{Kind: production.KindEdit, Content: "main.go"}))
	require.NoError(t, ld.Observe(ctx, core.AgentEvent{Kind: production.KindEdit, Content: "main.go"}))
	require.NoError(t, ld.Observe(ctx, core.AgentEvent{Kind: production.KindEdit, Content: "main.go"}))
	res := ld.Check(ctx)
	assert.True(t, res.Detected)
	assert.Equal(t, production.DimensionEditCount, res.Dimension)
}

func TestProduction_LoopDetector_TestFailure(t *testing.T) {
	ld := production.NewDefaultLoopDetector(production.LoopDetectionConfig{
		TestFailureThreshold: 2,
	})
	ctx := context.Background()
	require.NoError(t, ld.Observe(ctx, core.AgentEvent{Kind: production.KindTestFailure, Content: "FAIL"}))
	require.NoError(t, ld.Observe(ctx, core.AgentEvent{Kind: production.KindTestFailure, Content: "FAIL"}))
	res := ld.Check(ctx)
	assert.True(t, res.Detected)
	assert.Equal(t, production.DimensionTestFailure, res.Dimension)
}

func TestProduction_LoopDetector_Reset(t *testing.T) {
	ld := production.NewDefaultLoopDetector(production.LoopDetectionConfig{
		SameToolCallThreshold: 2,
	})
	ctx := context.Background()
	require.NoError(t, ld.Observe(ctx, core.AgentEvent{Kind: production.KindToolCall, Content: `{"tool":"read"}`}))
	require.NoError(t, ld.Observe(ctx, core.AgentEvent{Kind: production.KindToolCall, Content: `{"tool":"read"}`}))
	assert.True(t, ld.Check(ctx).Detected)
	require.NoError(t, ld.Reset(ctx))
	assert.False(t, ld.Check(ctx).Detected)
}

// =============================================================================
// PRODUCTION: Idempotent Cache
// =============================================================================

func TestProduction_IdempotentCache_GetSetDelete(t *testing.T) {
	ctx := context.Background()
	c := production.NewFIFOIdempotentCache(10)
	_, ok := c.Get(ctx, "key1")
	assert.False(t, ok)

	require.NoError(t, c.Set(ctx, "key1", "value1"))
	val, ok := c.Get(ctx, "key1")
	assert.True(t, ok)
	assert.Equal(t, "value1", val)

	require.NoError(t, c.Delete(ctx, "key1"))
	_, ok = c.Get(ctx, "key1")
	assert.False(t, ok)
}

func TestProduction_IdempotentCache_FIFOEviction(t *testing.T) {
	ctx := context.Background()
	c := production.NewFIFOIdempotentCache(2)
	require.NoError(t, c.Set(ctx, "a", 1))
	require.NoError(t, c.Set(ctx, "b", 2))
	require.NoError(t, c.Set(ctx, "c", 3))

	_, ok := c.Get(ctx, "a")
	assert.False(t, ok, "oldest entry 'a' should be evicted")
	v, ok := c.Get(ctx, "b")
	assert.True(t, ok)
	assert.Equal(t, 2, v)
	v, ok = c.Get(ctx, "c")
	assert.True(t, ok)
	assert.Equal(t, 3, v)
}

// =============================================================================
// PRODUCTION: Audit Log
// =============================================================================

func TestProduction_AuditLog_LogAndQuery(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "audit.jsonl")

	audit := production.NewDefaultAuditLog(logPath)
	ctx := context.Background()

	require.NoError(t, audit.Log(ctx, production.AuditEntry{
		Timestamp: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Operation: "tool.run",
		ToolName:  "bash",
		Args:      map[string]any{"cmd": "ls"},
		Result:    map[string]any{"output": "file1.go"},
		UserID:    "user-1",
		SessionID: "sess-1",
	}))
	require.NoError(t, audit.Log(ctx, production.AuditEntry{
		Timestamp: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
		Operation: "config.set",
		ToolName:  "",
		UserID:    "user-2",
	}))

	entries, err := audit.Query(ctx, production.AuditFilter{Operation: "tool.run"})
	require.NoError(t, err)
	assert.Len(t, entries, 1)
	assert.Equal(t, "tool.run", entries[0].Operation)
	assert.Equal(t, "bash", entries[0].ToolName)

	entries, err = audit.Query(ctx, production.AuditFilter{})
	require.NoError(t, err)
	assert.Len(t, entries, 2)
}

func TestProduction_AuditLog_QueryTimeRange(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "audit_range.jsonl")

	audit := production.NewDefaultAuditLog(logPath)
	ctx := context.Background()

	require.NoError(t, audit.Log(ctx, production.AuditEntry{
		Timestamp: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Operation: "tool.run",
		ToolName:  "read",
	}))
	require.NoError(t, audit.Log(ctx, production.AuditEntry{
		Timestamp: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		Operation: "tool.run",
		ToolName:  "write",
	}))

	entries, err := audit.Query(ctx, production.AuditFilter{
		From: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	assert.Len(t, entries, 1)
	assert.Equal(t, "write", entries[0].ToolName)
}

// =============================================================================
// PRODUCTION: Telemetry
// =============================================================================

func TestProduction_Telemetry_RecordEvents(t *testing.T) {
	ctx := context.Background()
	tel := production.NewDefaultTelemetry()
	require.NoError(t, tel.Record(ctx, production.TelemetryMetric{Name: "tool.calls", Value: 1}))
	require.NoError(t, tel.Record(ctx, production.TelemetryMetric{Name: "tool.calls", Value: 2}))
	require.NoError(t, tel.Record(ctx, production.TelemetryMetric{Name: "tokens", Value: 100}))

	dt, ok := tel.(*production.DefaultTelemetry)
	require.True(t, ok)
	snap := dt.Snapshot()
	assert.Equal(t, float64(3), snap["tool.calls"])
	assert.Equal(t, float64(100), snap["tokens"])
}

// =============================================================================
// PRODUCTION: Output Guards
// =============================================================================

func TestProduction_OutputGuard_RegexGuard(t *testing.T) {
	ctx := context.Background()
	g := production.NewRegexOutputGuard([]string{"secret-key", "tok_[A-Za-z0-9]+"})

	res, err := g.Check(ctx, "this is safe text")
	require.NoError(t, err)
	assert.True(t, res.Allowed)

	res, err = g.Check(ctx, "here is secret-key: abc123")
	require.NoError(t, err)
	assert.False(t, res.Allowed)
	assert.Contains(t, res.Reason, "secret-key")
}

func TestProduction_OutputGuard_PIIGuard(t *testing.T) {
	ctx := context.Background()
	g := production.NewPIIOutputGuard()

	res, err := g.Check(ctx, "my email is user@example.com")
	require.NoError(t, err)
	assert.False(t, res.Allowed)

	res, err = g.Check(ctx, "call me at 13812345678")
	require.NoError(t, err)
	assert.False(t, res.Allowed)

	res, err = g.Check(ctx, "hello world this is safe")
	require.NoError(t, err)
	assert.True(t, res.Allowed)
}

func TestProduction_OutputGuard_CodeInjectionGuard(t *testing.T) {
	ctx := context.Background()
	g := production.NewCodeInjectionGuard()

	res, err := g.Check(ctx, "please DROP TABLE users")
	require.NoError(t, err)
	assert.False(t, res.Allowed)
	assert.Equal(t, production.GuardCritical, res.Severity)

	res, err = g.Check(ctx, "safe content without injection")
	require.NoError(t, err)
	assert.True(t, res.Allowed)
}

func TestProduction_OutputGuard_LengthGuard(t *testing.T) {
	ctx := context.Background()
	g := production.NewLengthGuard(10)

	res, err := g.Check(ctx, "short")
	require.NoError(t, err)
	assert.True(t, res.Allowed)

	res, err = g.Check(ctx, "this is a very long string")
	require.NoError(t, err)
	assert.False(t, res.Allowed)
	assert.True(t, len(res.Sanitized) <= 10)
}

func TestProduction_OutputGuard_OutputGuardChain(t *testing.T) {
	ctx := context.Background()
	chain := production.NewOutputGuardChain([]production.OutputGuard{
		production.NewLengthGuard(100),
		production.NewCodeInjectionGuard(),
	})

	res, err := chain.Check(ctx, "safe text")
	require.NoError(t, err)
	assert.True(t, res.Allowed)

	res, err = chain.Check(ctx, "DROP TABLE users")
	require.NoError(t, err)
	assert.False(t, res.Allowed)
	assert.Equal(t, production.GuardCritical, res.Severity)
}

// =============================================================================
// PRODUCTION: Output Guard Middleware
// =============================================================================

func TestProduction_OutputGuardMiddleware_Integration(t *testing.T) {
	ctx := context.Background()
	guard := production.NewRegexOutputGuard([]string{"bad-word"})
	mw := production.NewOutputGuardMiddleware(guard)

	base := func(_ context.Context, req extension.ModelRequest) (extension.ModelResponse, error) {
		return extension.ModelResponse{Text: req.Prompt}, nil
	}
	wrapped := mw.WrapModel(base)

	resp, err := wrapped(ctx, extension.ModelRequest{Prompt: "safe content"})
	require.NoError(t, err)
	assert.Equal(t, "safe content", resp.Text)

	resp, err = wrapped(ctx, extension.ModelRequest{Prompt: "this has bad-word inside"})
	require.NoError(t, err)
	assert.NotEqual(t, "this has bad-word inside", resp.Text)
}

// =============================================================================
// PRODUCTION: Error Categorization & Classification
// =============================================================================

func TestProduction_ErrorCategory_String(t *testing.T) {
	assert.Equal(t, "transient", production.ErrorTransient.String())
	assert.Equal(t, "rate_limit", production.ErrorRateLimit.String())
	assert.Equal(t, "timeout", production.ErrorTimeout.String())
	assert.Equal(t, "fatal", production.ErrorFatal.String())
}

func TestProduction_CategorizedError_ExplicitPrecedence(t *testing.T) {
	p, ok := production.NewDefaultRetryPolicy(production.RetryConfig{MaxAttempts: 2}).(*production.DefaultRetryPolicy)
	require.True(t, ok)
	// An error whose text says "timeout" but has an explicit RateLimit category.
	explicit := production.NewError(production.ErrorRateLimit, errors.New("timeout detected"))
	assert.Equal(t, production.ErrorRateLimit, p.Classify(explicit))
	assert.True(t, p.ShouldRetry(context.Background(), explicit, 0))
}

func TestProduction_ErrorClassification_TransientRateLimitTimeoutFatal(t *testing.T) {
	p, ok := production.NewDefaultRetryPolicy(production.RetryConfig{}).(*production.DefaultRetryPolicy)
	require.True(t, ok)
	assert.Equal(t, production.ErrorTransient, p.Classify(production.NewError(production.ErrorTransient, errors.New("x"))))
	assert.Equal(t, production.ErrorRateLimit, p.Classify(production.NewError(production.ErrorRateLimit, errors.New("x"))))
	assert.Equal(t, production.ErrorTimeout, p.Classify(production.NewError(production.ErrorTimeout, errors.New("x"))))
	assert.Equal(t, production.ErrorFatal, p.Classify(production.NewError(production.ErrorFatal, errors.New("x"))))
	assert.False(t, p.ShouldRetry(context.Background(), production.NewError(production.ErrorFatal, errors.New("x")), 0))
}

// =============================================================================
// MCP: Server Lifecycle
// =============================================================================

func TestMCP_ServerLifecycle_StartStop(t *testing.T) {
	server := mock.NewMockMCPServer("test-server")
	err := server.Start(context.Background())
	require.NoError(t, err)
	err = server.Stop(context.Background())
	require.NoError(t, err)
}

func TestMCP_ServerLifecycle_ConnectDisconnect(t *testing.T) {
	server := mock.NewMockMCPServer("mcp-srv")
	err := server.Connect(context.Background())
	require.NoError(t, err)
	err = server.Disconnect(context.Background())
	require.NoError(t, err)
}

// =============================================================================
// MCP: Tool Registration and Calling
// =============================================================================

func TestMCP_ToolRegistrationAndCalling(t *testing.T) {
	server := mock.NewMockMCPServer("tools-srv")
	server.RegisterTool("echo", "echoes input", func(args map[string]any) (any, error) {
		return "echo:" + safeString(args["msg"]), nil
	})

	tools, err := server.ListTools(context.Background())
	require.NoError(t, err)
	assert.Len(t, tools, 1)
	assert.Equal(t, "echo", tools[0].Name)

	result, err := server.CallTool(context.Background(), "echo", map[string]any{"msg": "hello"})
	require.NoError(t, err)
	assert.Equal(t, "echo:hello", result.Content)
	assert.False(t, result.IsError)
}

func TestMCP_CallToolNotFound(t *testing.T) {
	server := mock.NewMockMCPServer("tools-srv")
	_, err := server.CallTool(context.Background(), "missing", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

// =============================================================================
// MCP: MCPToolAdapter wrapping
// =============================================================================

func TestMCP_ToolAdapter_Wrapping(t *testing.T) {
	server := mock.NewMockMCPServer("adapter-srv")
	server.RegisterTool("search", "search tool", func(args map[string]any) (any, error) {
		return "results for " + safeString(args["query"]), nil
	})

	adapter := mcp.NewMCPToolAdapter(server, mcp.MCPTool{Name: "search", Description: "search tool"})
	assert.Equal(t, "mcp__adapter-srv__search", adapter.Name())
	assert.Equal(t, "search tool", adapter.Description())

	result, err := adapter.Execute(context.Background(), tools.ToolCall{
		ID:   "call-1",
		Name: "mcp__adapter-srv__search",
		Args: map[string]any{"query": "go"},
	})
	require.NoError(t, err)
	assert.Equal(t, "results for go", result.Output)
	assert.Equal(t, "call-1", result.ToolCallID)
}

// =============================================================================
// MCP: Tool name format (mcp__{server}__{tool})
// =============================================================================

func TestMCP_ToolNameFormat_NormalizeAndParse(t *testing.T) {
	name := mcp.NormalizeToolName("github", "list-repos")
	assert.Equal(t, "mcp__github__list-repos", name)

	srv, tool, isMCP := mcp.ParseToolName(name)
	assert.True(t, isMCP)
	assert.Equal(t, "github", srv)
	assert.Equal(t, "list-repos", tool)

	_, _, isMCP = mcp.ParseToolName("normal-tool")
	assert.False(t, isMCP)

	name = mcp.NormalizeToolName("github", "compare__commits")
	srv, tool, isMCP = mcp.ParseToolName(name)
	assert.True(t, isMCP)
	assert.Equal(t, "github", srv)
	assert.Equal(t, "compare__commits", tool)
}

// =============================================================================
// MCP: Hot Reload Lifecycle
// =============================================================================

func TestMCP_HotReload_Lifecycle(t *testing.T) {
	server := mock.NewMockMCPServer("hot-srv")
	server.RegisterTool("ping", "ping tool", func(args map[string]any) (any, error) {
		return "pong", nil
	})

	var registeredCount int
	var lastTools []mcp.MCPTool
	registerFn := func(tools []mcp.MCPTool) {
		registeredCount++
		lastTools = tools
	}

	reloader := mcp.NewDefaultHotReloader(server, registerFn, mcp.WithPollInterval(50*time.Millisecond))
	assert.Equal(t, "mcp-hot-reloader", reloader.Name())

	// Not watching yet → Reload should error.
	err := reloader.Reload(context.Background())
	assert.Error(t, err)

	// Start watching a temp config file (non-existent, so baseline is missing).
	tmpFile := filepath.Join(t.TempDir(), "mcp_config.yaml")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = reloader.Watch(ctx, tmpFile)
	require.NoError(t, err)

	// Trigger a manual reload.
	err = reloader.Reload(context.Background())
	require.NoError(t, err)

	// Give it time to reconnect.
	time.Sleep(200 * time.Millisecond)
	assert.Equal(t, 1, registeredCount)
	assert.Len(t, lastTools, 1)
	assert.Equal(t, "ping", lastTools[0].Name)

	require.NoError(t, reloader.Stop())
}

// =============================================================================
// MCP: Complex Pipeline (MockMCPServer → MCPToolAdapter → Registry)
// =============================================================================

func TestMCP_Pipeline_ServerToAdapterToRegistry(t *testing.T) {
	server := mock.NewMockMCPServer("pipeline-srv")
	server.RegisterTool("transform", "transforms input", func(args map[string]any) (any, error) {
		return "transformed:" + safeString(args["input"]), nil
	})

	adapter := mcp.NewMCPToolAdapter(server, mcp.MCPTool{
		Name:        "transform",
		Description: "transforms input",
	})

	reg := tools.NewDefaultToolRegistry().(*tools.DefaultToolRegistry)
	ctx := context.Background()
	require.NoError(t, reg.Register(ctx, adapter))

	// List the registry and find the tool.
	list, err := reg.List(ctx)
	require.NoError(t, err)
	assert.Len(t, list, 1)
	assert.Equal(t, "mcp__pipeline-srv__transform", list[0].Name())

	// Execute through the registry.
	call := tools.ToolCall{ID: "pipe-1", Name: "mcp__pipeline-srv__transform", Args: map[string]any{"input": "raw"}}
	result, err := reg.Execute(ctx, call)
	require.NoError(t, err)
	assert.Equal(t, "transformed:raw", result.Output)

	// Verify call log.
	logs := server.CallLog()
	require.Len(t, logs, 1)
	assert.Equal(t, "transform", logs[0].ToolName)
}

// =============================================================================
// MCP: Client Registry
// =============================================================================

func TestMCP_ClientRegistry_RegisterGetList(t *testing.T) {
	reg := mcp.NewMCPClientRegistry()
	s1 := mock.NewMockMCPServer("server-a")
	s2 := mock.NewMockMCPServer("server-b")

	require.NoError(t, reg.Register("server-a", s1))
	require.NoError(t, reg.Register("server-b", s2))

	got, err := reg.Get("server-a")
	require.NoError(t, err)
	assert.Equal(t, "server-a", got.Name())

	_, err = reg.Get("missing")
	assert.ErrorIs(t, err, mcp.ErrMCPClientNotFound)

	list := reg.List(context.Background())
	assert.Len(t, list, 2)
}

func TestMCP_ClientRegistry_HotReloader(t *testing.T) {
	reg := mcp.NewMCPClientRegistry()
	server := mock.NewMockMCPServer("hot-reg-srv")
	reloader := mcp.NewDefaultHotReloader(server, nil)

	require.NoError(t, reg.RegisterHotReloader("hot-reg-srv", reloader))
	got, ok := reg.HotReloader("hot-reg-srv")
	assert.True(t, ok)
	assert.Equal(t, "mcp-hot-reloader", got.Name())

	_, ok = reg.HotReloader("missing")
	assert.False(t, ok)
}

// =============================================================================
// MCP: Connect / Disconnect idempotent
// =============================================================================

func TestMCP_MockServerCallLog(t *testing.T) {
	server := mock.NewMockMCPServer("log-srv")
	server.RegisterTool("add", "adds numbers", func(args map[string]any) (any, error) {
		return 42, nil
	})

	assert.Empty(t, server.CallLog())

	_, _ = server.CallTool(context.Background(), "add", map[string]any{"a": 1})
	logs := server.CallLog()
	assert.Len(t, logs, 1)
	assert.Equal(t, "add", logs[0].ToolName)
	assert.False(t, logs[0].Timestamp.IsZero())
}

// =============================================================================
// Helpers
// =============================================================================

type fakeClock struct {
	t time.Time
}

func (c *fakeClock) Now() time.Time {
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.t = c.t.Add(d)
}

func safeString(v any) string {
	s, _ := v.(string)
	return s
}

// Prevent unused import warnings.
var _ = production.RegisterOutputGuard
var _ = production.RegisterRetryPolicy
var _ = production.GetOutputGuard
var _ = production.GetRetryPolicy
var _ = tracing.SpanKindInternal
var _ = os.DevNull
