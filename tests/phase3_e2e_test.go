// Package tests contains end-to-end integration tests for go-cli.
//
// This file is the Phase 3 end-to-end gate. It proves, in one trace
// rooted at a single span with a consistent trace_id:
//
//  1. 审批门控 (approval gating): the deny-first ApprovalMiddleware refuses a
//     dangerous tool call (ErrToolDenied) before any executor runs.
//  2. MCP工具调用 (MCP tool call): an MCP tool (MockMCPServer) is adapted into a
//     real tools registry via MCPToolAdapter and invoked through the registry.
//  3. 循环检测 (loop detection): DefaultLoopDetector flags repeated identical
//     tool calls once they cross the threshold.
//  4. 熔断降级 (circuit breaker): DefaultCircuitBreaker opens after repeated
//     failures and then serves the configured fallback instead of calling out.
//  5. 重试 (retry): DefaultRetryPolicy classifies a transient error and
//     retries a failing callable up to MaxAttempts.
//  6. 幂等去重 (idempotency): FIFOIdempotentCache serves a second identical
//     operation from cache without re-executing the backend.
//  7. 审计记录 (audit): DefaultAuditLog records every step and Query returns
//     them oldest first.
//
// All steps run in-memory (real default implementations wired together), so the
// test compiles and passes in the default `go test -race ./internal/... ./tests/...`
// build that `make verify` runs - no mock build tag is required. The
// MockMCPServer and MockTraceExporter helpers live in internal/mock and are NOT
// build-tagged, so they are available.
package tests

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/approval"
	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/mcp"
	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/production"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/tracing"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// TestPhase3EndToEnd exercises the full Phase 3 resilience chain under a single
// tracing root so the span chain can be asserted end to end:
// approval gating -> MCP tool call -> loop detection -> circuit breaker ->
// retry -> idempotent cache dedup -> audit log.
func TestPhase3EndToEnd(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	exporter := mock.NewMockTraceExporter()
	tracer := tracing.NewTracer("phase3-e2e", exporter)
	root, spanCtx := tracer.Start(context.Background(), "phase3.root", tracing.SpanKindInternal)

	auditPath := filepath.Join(t.TempDir(), "audit.jsonl")
	audit := production.NewDefaultAuditLog(auditPath)

	// Shared idempotent cache and breaker used across the chained steps so the
	// audit trail records one coherent pipeline.
	cache := production.NewFIFOIdempotentCache(32)
	breaker := production.NewDefaultCircuitBreaker(production.CircuitBreakerConfig{
		FailureThreshold: 2,
		RecoveryTimeout:  5 * time.Second,
		HalfOpenMaxCalls: 1,
		SuccessThreshold: 1,
	})

	t.Run("approval_gating_and_mcp_call", func(t *testing.T) {
		phaseApprovalAndMCP(spanCtx, t, audit, cache, breaker)
	})

	t.Run("loop_detection", func(t *testing.T) {
		phaseLoopDetection(spanCtx, t, audit)
	})

	t.Run("circuit_breaker_open_and_fallback", func(t *testing.T) {
		phaseCircuitBreaker(spanCtx, t, audit, breaker)
	})

	t.Run("retry_policy", func(t *testing.T) {
		phaseRetry(spanCtx, t, audit)
	})

	t.Run("idempotent_cache_dedup", func(t *testing.T) {
		phaseIdempotentDedup(spanCtx, t, audit, cache)
	})

	t.Run("audit_records_every_step", func(t *testing.T) {
		phaseAuditQuery(spanCtx, t, audit)
	})

	root.End()

	// Wait for async span exports to settle, then assert the whole chain
	// resolves to the root and every span shares the trace root id.
	waitForSpans(t, exporter)
	exporter.AssertSpanChain(t)
	for _, s := range exporter.Spans() {
		require.Equal(t, tracer.TraceID(), s.TraceID,
			"span %s (%s) must share the trace root id", s.SpanID, s.Name)
	}
}

// phaseApprovalAndMCP wires a deny-first ApprovalMiddleware in front of a real
// MCP tool adapter and asserts:
//   - a denied call returns ErrToolDenied without reaching the executor, and
//   - an allowed call drives the MCP round trip and returns the tool output.
//
// The allowed call is run, cached idempotently, and audited.
func phaseApprovalAndMCP(ctx context.Context, t *testing.T, audit production.AuditLog, cache production.IdempotentCache, breaker production.CircuitBreaker) {
	t.Helper()

	// A real MCP server/client (in-process, no build tag).
	server := mock.NewMockMCPServer("weather")
	require.NoError(t, server.Connect(ctx))
	server.RegisterTool("lookup", "look up the weather for a city",
		func(args map[string]any) (any, error) {
			val, ok := args["city"].(string)
			if !ok {
				return nil, errors.New("phase3: city argument must be a string")
			}
			return "sunny in " + val, nil
		})

	// Adapt the MCP tool into a tools.ToolDefinition and register it.
	reg := tools.NewDefaultToolRegistry()
	adapter := mcp.NewMCPToolAdapter(server, mcp.MCPTool{Name: "lookup", Description: "weather lookup"})
	require.NoError(t, reg.Register(ctx, adapter))
	mcpName := mcp.NormalizeToolName("weather", "lookup")

	// Deny-first approval middleware guarding the same registry.
	mw := approval.NewApprovalMiddleware(
		approval.NewStaticClassifier([]string{mcpName}, []string{"rm_rf"}),
		approval.NewInMemoryApprovalStore(),
	)

	// A denied call: rm_rf is on the deny list, so the middleware returns
	// ErrToolDenied. The call never reaches the executor (idempotent cache and
	// audit are also bypassed by design).
	denied := tools.ToolCall{ID: "call-denied", Name: "rm_rf", Args: map[string]any{"path": "/"}}
	_, err := mw.WrapToolCall(func(_ context.Context, _ tools.ToolCall) (*tools.ToolResult, error) {
		t.Fatal("denied tool call must not reach the executor")
		return nil, nil
	})(ctx, denied)
	require.ErrorIs(t, err, approval.ErrToolDenied, "denied call must return ErrToolDenied")

	// The approved call runs the MCP executor. We route it through the real
	// registry Execute so the "tool.call" span and the MCP round trip both fire.
	allowed := tools.ToolCall{ID: "call-weather-01", Name: mcpName, Args: map[string]any{"city": "Shanghai"}}
	exec := func(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
		return reg.Execute(ctx, call)
	}
	approvedExec := mw.WrapToolCall(exec)

	// Wrap the whole approved call in circuit breaker + idempotency + audit so
	// the later dedup/audit steps share this one MCP call.
	run := func(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
		if h, ok := cache.Get(ctx, call.ID); ok {
			if r, ok := h.(*tools.ToolResult); ok {
				entry := production.AuditEntry{Operation: "tool.run", ToolName: call.Name, Result: map[string]any{"hit": "cache"}}
				if aerr := audit.Log(ctx, entry); aerr != nil {
					return r, aerr
				}
				return r, nil
			}
		}

		execResult, execErr := breaker.Execute(ctx, func() (any, error) { return approvedExec(ctx, call) })
		if execErr != nil {
			errEntry := production.AuditEntry{Operation: "tool.run", ToolName: call.Name, Result: map[string]any{"error": execErr.Error()}}
			if aerr := audit.Log(ctx, errEntry); aerr != nil {
				return nil, aerr
			}
			return nil, execErr
		}
		res, ok := execResult.(*tools.ToolResult)
		if !ok {
			return nil, errors.New("phase3: circuit breaker returned an unexpected result type")
		}
		if cerr := cache.Set(ctx, call.ID, res); cerr != nil {
			return nil, cerr
		}
		okEntry := production.AuditEntry{Operation: "tool.run", ToolName: call.Name, Args: call.Args, Result: map[string]any{"output": res.Output}}
		if aerr := audit.Log(ctx, okEntry); aerr != nil {
			return res, aerr
		}
		return res, nil
	}

	res, err := run(ctx, allowed)
	require.NoError(t, err, "approved MCP call must succeed")
	require.Contains(t, res.Output, "sunny in Shanghai")

	// A second identical MCP call is served from the idempotent cache (dedup).
	res2, err := run(ctx, tools.ToolCall{ID: "call-weather-01", Name: mcpName, Args: map[string]any{"city": "Shanghai"}})
	require.NoError(t, err)
	require.Equal(t, res.Output, res2.Output, "cached repeat must equal original output")

	require.GreaterOrEqual(t, len(server.CallLog()), 1, "MCP server must have recorded at least one real call")
}

// phaseLoopDetection feeds repeated identical tool-call events into a
// DefaultLoopDetector and asserts it detects the loop once the same-tool
// threshold is exceeded.
func phaseLoopDetection(ctx context.Context, t *testing.T, audit production.AuditLog) {
	t.Helper()

	ld := production.NewDefaultLoopDetector(production.LoopDetectionConfig{
		SameToolCallThreshold: 3,
	})
	payload := "lookup:{\"city\":\"Shanghai\"}"
	for i := 0; i < 3; i++ {
		require.NoError(t, ld.Observe(ctx, core.AgentEvent{Kind: "tool", Content: payload}))
	}
	res := ld.Check(ctx)
	require.True(t, res.Detected, "repeated identical tool calls must trip the loop detector")
	require.Equal(t, production.DimensionSameToolCall, res.Dimension)
	require.Equal(t, 3, res.Count)

	require.NoError(t, audit.Log(ctx, production.AuditEntry{
		Operation: "loop_detect",
		ToolName:  "lookup",
		Result:    map[string]any{"detected": true, "dimension": res.Dimension},
	}))

	// Reset clears the counters so it does not leak state into other steps.
	require.NoError(t, ld.Reset(ctx))
}

// phaseCircuitBreaker drives a DefaultCircuitBreaker to the Open state by
// repeatedly failing a callable, then asserts the configured fallback is used
// instead of invoking the (still-failing) callable.
func phaseCircuitBreaker(ctx context.Context, t *testing.T, audit production.AuditLog, breaker production.CircuitBreaker) {
	t.Helper()

	fallback := func() (any, error) { return "degraded-fallback", nil }
	b := production.NewDefaultCircuitBreaker(production.CircuitBreakerConfig{
		FailureThreshold: 2,
		RecoveryTimeout:  30 * time.Second,
		HalfOpenMaxCalls: 1,
		SuccessThreshold: 1,
	}, production.WithFallback(fallback))

	// Two consecutive failures open the breaker.
	for i := 0; i < 2; i++ {
		_, err := b.Execute(ctx, func() (any, error) {
			return nil, production.NewError(production.ErrorTransient, context.DeadlineExceeded)
		})
		require.Error(t, err, "both initial calls must fail")
	}
	require.Equal(t, production.CircuitOpen, b.State(), "breaker must be open after repeated failures")

	// A third call is refused and served by the fallback - the failing callable
	// is never run again.
	out, err := b.Execute(ctx, func() (any, error) { return nil, nil })
	require.NoError(t, err, "fallback must be served when the breaker is open")
	require.Equal(t, "degraded-fallback", out)

	require.NoError(t, audit.Log(ctx, production.AuditEntry{
		Operation: "circuit_breaker",
		ToolName:  "lookup",
		Result:    map[string]any{"state": b.State().String(), "fallback": true},
	}))
}

// phaseRetry exercises DefaultRetryPolicy classification + retry gating on a
// transient failing callable, asserting the callable is retried and eventually
// succeeds.
func phaseRetry(ctx context.Context, t *testing.T, audit production.AuditLog) {
	t.Helper()

	policy := production.NewDefaultRetryPolicy(production.RetryConfig{
		MaxAttempts: 3,
		BaseDelay:   1 * time.Millisecond,
	})

	attempts := 0
	run := func(ctx context.Context) (string, error) {
		for {
			attempts++
			err := production.NewError(production.ErrorTransient, context.DeadlineExceeded)
			if attempts >= 3 {
				return "ok-after-retry", nil
			}
			if !policy.ShouldRetry(ctx, err, attempts-1) {
				return "", err
			}
		}
	}

	out, err := run(ctx)
	require.NoError(t, err)
	require.Equal(t, "ok-after-retry", out)
	require.Equal(t, 3, attempts, "transient failure must be retried to the success attempt")

	require.NoError(t, audit.Log(ctx, production.AuditEntry{
		Operation: "retry",
		ToolName:  "lookup",
		Result:    map[string]any{"attempts": attempts, "policy": policy.Name()},
	}))
}

// phaseIdempotentDedup asserts FIFOIdempotentCache returns a stored value on an
// identical repeat key, proving the second identical operation is not
// re-executed.
func phaseIdempotentDedup(ctx context.Context, t *testing.T, audit production.AuditLog, cache production.IdempotentCache) {
	t.Helper()

	key := "mcp__weather__lookup:call-weather-01"
	executions := 0
	produce := func() string {
		executions++
		return "sunny in Shanghai (execution " + time.Now().Format("150405.000000000") + ")"
	}

	stored, hit := cache.Get(ctx, key)
	require.False(t, hit, "first lookup must be a miss")
	if !hit {
		stored = produce()
		require.NoError(t, cache.Set(ctx, key, stored))
	}

	cached, hit2 := cache.Get(ctx, key)
	require.True(t, hit2, "repeat lookup must hit the cache")
	require.Equal(t, stored, cached, "cached repeat must equal the value produced once")
	require.Equal(t, 1, executions, "identical repeat must NOT re-execute the backend")

	require.NoError(t, audit.Log(ctx, production.AuditEntry{
		Operation: "idempotent",
		ToolName:  "lookup",
		Result:    map[string]any{"hit": hit2, "executions": executions},
	}))
}

// phaseAuditQuery asserts every audited step from the pipeline is queryable
// from the DefaultAuditLog, oldest first.
func phaseAuditQuery(ctx context.Context, t *testing.T, audit production.AuditLog) {
	t.Helper()

	entries, err := audit.Query(ctx, production.AuditFilter{})
	require.NoError(t, err, "audit query must not error")
	require.GreaterOrEqual(t, len(entries), 6,
		"audit log must record tool.run (x2), loop_detect, circuit_breaker, retry, idempotent")

	// Operations must be present across the chain.
	ops := map[string]bool{}
	for _, e := range entries {
		ops[e.Operation] = true
	}
	for _, want := range []string{"tool.run", "loop_detect", "circuit_breaker", "retry", "idempotent"} {
		require.True(t, ops[want], "expected audit operation %q to be recorded", want)
	}
}

// waitForSpans polls the exporter until every collected span's parent resolves
// to a root (i.e. the ended root span and all of its children have been
// exported), bounding the wait so an export backlog cannot cause a flaky chain.
func waitForSpans(t *testing.T, exporter *mock.MockTraceExporter) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		spans := exporter.Spans()
		if len(spans) > 0 && chainResolvesToRoot(spans) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	exporter.AssertSpanExists(t, "phase3.root")
}

// TestPhase3RegistryInventory verifies the Registry extension point inventory: every
// process-wide RegisterXxx entry point exists and accepts its interface, and
// each is wired to a default implementation that satisfies the interface.
// The register calls pass nil to reset to defaults, so they are idempotent and
// do not perturb other tests.
func TestPhase3RegistryInventory(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	// Approval module extension points.
	approval.RegisterApprovalClassifier(nil)
	approval.RegisterApprovalStore(nil)
	approval.RegisterPermissionModeResolver(nil)
	approval.RegisterTrustManager(nil)
	require.Equal(t, "allow_all", approval.GetApprovalClassifier().Name())
	require.NotNil(t, approval.GetApprovalStore())
	require.NotNil(t, approval.GetTrustManager())

	// MCP module extension point: RegisterMCPClient (package func) plus the
	// per-registry RegisterHotReloader method.
	client := mock.NewMockMCPServer("inventory")
	reg := mcp.NewMCPClientRegistry()
	require.NoError(t, mcp.RegisterMCPClient(reg, "inventory", client))
	got, err := reg.Get("inventory")
	require.NoError(t, err)
	require.Equal(t, "inventory", got.Name())
	require.NoError(t, reg.RegisterHotReloader("inventory", mcp.NewDefaultHotReloader(client, nil)))

	// Production module extension points.
	production.RegisterLoopDetector(nil)
	production.RegisterCircuitBreaker(nil)
	production.RegisterRetryPolicy(nil)
	production.RegisterIdempotentCache(nil)
	production.RegisterAuditLog(nil)
	production.RegisterTelemetry(nil)
	require.Equal(t, "loop-detector", production.GetLoopDetector().Name())
	require.Equal(t, "circuit-breaker", production.GetCircuitBreaker().Name())
	require.NotNil(t, production.GetRetryPolicy())
	require.NotNil(t, production.GetIdempotentCache())
	require.NotNil(t, production.GetAuditLog())
	require.NotNil(t, production.GetTelemetry())

	// Config module extension point: RegisterSettings / GetSettings.
	config.RegisterSettings(nil)
	require.NotNil(t, config.GetSettings())
}

// TestPhase3NoProductionMockImport verifies that no production
// package under internal/ (other than internal/mock itself) imports
// internal/mock. It walks the source tree and scans import lines.
func TestPhase3NoProductionMockImport(t *testing.T) {
	// `go test` runs with the package directory as the working directory, so
	// the repo's internal/ tree lives one level up.
	internalDir := filepath.Join("..", "internal")

	err := filepath.Walk(internalDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		// Test files are not "production code" and may legitimately import the
		// mock framework; the scan rule only constrains non-test source.
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// Skip the mock package itself.
		if strings.HasPrefix(path, filepath.Join(internalDir, "mock")) {
			return nil
		}
		data, rerr := os.ReadFile(path) //nolint:gosec // G122: test-only scan over the trusted, fixed repo tree under internal/
		if rerr != nil {
			return rerr
		}
		for _, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "\"github.com/pengjunchen/go-cli/internal/mock\"") ||
				strings.HasPrefix(trimmed, "github.com/pengjunchen/go-cli/internal/mock\"") {
				t.Errorf("mock import violation: %s imports internal/mock", path)
			}
		}
		return nil
	})
	require.NoError(t, err, "failed to walk internal/ for mock import scan")
}
