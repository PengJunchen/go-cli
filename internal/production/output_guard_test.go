package production

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/extension"
	"github.com/pengjunchen/go-cli/internal/tracing"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// passThroughGuard is a test-local OutputGuard that always allows, used to
// assert middleware pass-through behavior without importing internal/mock
// (which imports internal/production and would create an import cycle).
type passThroughGuard struct {
	name string
}

func (g *passThroughGuard) Name() string { return g.name }

func (g *passThroughGuard) Check(ctx context.Context, text string) (*GuardResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &GuardResult{Allowed: true, Sanitized: text}, nil
}

// errGuard is a test-local OutputGuard that always surfaces the context error,
// used to verify that context cancellation propagates through the middleware.
type errGuard struct{}

func (errGuard) Name() string { return "err-guard" }

func (errGuard) Check(ctx context.Context, _ string) (*GuardResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &GuardResult{Allowed: true}, nil
}

// spanAttrs indexes a span's attributes by key for assertions.
func spanAttrs(t *testing.T, span tracing.SpanData) map[string]any {
	t.Helper()
	out := make(map[string]any, len(span.Attributes))
	for _, a := range span.Attributes {
		out[a.Key] = a.Value
	}
	return out
}

// findSpans returns the collected spans whose Name equals want.
func findSpans(exporter *captureExporter, want string) []tracing.SpanData {
	var matches []tracing.SpanData
	for _, s := range exporter.allSpans() {
		if s.Name == want {
			matches = append(matches, s)
		}
	}
	return matches
}

// waitForSpans polls until at least count spans named want are collected. Span
// export is asynchronous (spawned goroutines), so assertions must settle first.
func (e *captureExporter) waitForSpans(t *testing.T, want string, count int) []tracing.SpanData {
	t.Helper()
	var matches []tracing.SpanData
	require.Eventually(t, func() bool {
		matches = findSpans(e, want)
		return len(matches) >= count
	}, 2*time.Second, 5*time.Millisecond, "expected at least %d %q span(s)", count, want)
	return matches
}

// newGuardTraceCtx wires a captureExporter + Tracer into a root context and
// ends the root span so child spans have a resolvable parent_span_id.
func newGuardTraceCtx(t *testing.T) (context.Context, *captureExporter, string) {
	t.Helper()
	exporter := newCaptureExporter()
	tr := tracing.NewTracer("guard-trace", exporter)
	root, ctx := tr.Start(context.Background(), "guard-root", tracing.SpanKindInternal)
	root.End()
	return ctx, exporter, root.SpanID()
}

// RegexOutputGuard matches a regex and blocks sensitive content.
func TestRegexOutputGuardBlocksSensitiveContent(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	g := NewRegexOutputGuard([]string{`secret-[\d]+`, `\bCREDIT\b`})

	res, err := g.Check(ctx, "the code is secret-1337 please keep it")
	require.NoError(t, err)
	assert.False(t, res.Allowed)
	assert.Equal(t, GuardHigh, res.Severity)

	res, err = g.Check(ctx, "everything is fine here")
	require.NoError(t, err)
	assert.True(t, res.Allowed)

	assert.Equal(t, "regex-output-guard", g.Name())
}

// PIIOutputGuard detects emails, phone numbers and Chinese ID numbers.
func TestPIIOutputGuardDetectsEmailsPhonesAndIDs(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	g := NewPIIOutputGuard()

	cases := []struct {
		name  string
		input string
	}{
		{"email", "reach me at joe@example.com"},
		{"phone", "call me at 13812345678 now"},
		{"idcard", "id 110101199003078888 on file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := g.Check(ctx, tc.input)
			require.NoError(t, err)
			assert.False(t, res.Allowed, "should detect %s", tc.name)
			assert.Equal(t, GuardHigh, res.Severity)
		})
	}

	res, err := g.Check(ctx, "no personal data in this plain message")
	require.NoError(t, err)
	assert.True(t, res.Allowed)
}

// CodeInjectionGuard detects injection indicators.
func TestCodeInjectionGuardDetectsInjection(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	g := NewCodeInjectionGuard()

	inputs := []string{
		"run os.system('rm -rf /')",
		"SELECT * FROM users WHERE id = 1",
		"now eval(__import__('os').system('id'))",
		"<script>alert('xss')</script>",
	}
	for _, in := range inputs {
		res, err := g.Check(ctx, in)
		require.NoError(t, err)
		assert.False(t, res.Allowed, "should flag: %s", in)
		assert.Equal(t, GuardCritical, res.Severity)
	}

	res, err := g.Check(ctx, "the answer is 42")
	require.NoError(t, err)
	assert.True(t, res.Allowed)
}

// LengthGuard limits length and truncates.
func TestLengthGuardLimitsAndTruncates(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	g := NewLengthGuard(5)

	res, err := g.Check(ctx, "hello world")
	require.NoError(t, err)
	assert.False(t, res.Allowed)
	assert.Equal(t, "hello", res.Sanitized, "Sanitized should contain the truncated output")
	assert.Equal(t, GuardLow, res.Severity)

	res, err = g.Check(ctx, "short")
	require.NoError(t, err)
	assert.True(t, res.Allowed)
	assert.Equal(t, "short", res.Sanitized)
}

// Middleware wraps a ModelFunc; allowed responses pass through.
func TestOutputGuardMiddlewarePassesThroughWhenAllowed(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	nextCalls := 0
	next := func(_ context.Context, _ extension.ModelRequest) (extension.ModelResponse, error) {
		nextCalls++
		return extension.ModelResponse{Text: "allowed response"}, nil
	}

	mw := NewOutputGuardMiddleware(&passThroughGuard{name: "pass"})
	wrapped := mw.WrapModel(next)

	resp, err := wrapped(ctx, extension.ModelRequest{Prompt: "hi"})
	require.NoError(t, err)
	assert.Equal(t, 1, nextCalls, "next must be invoked")
	assert.Equal(t, "allowed response", resp.Text)
	assert.Equal(t, "output-guard-middleware", mw.Name())
}

// When blocked, output does NOT propagate (replaced with blocked text).
func TestOutputGuardMiddlewareBlocksOutput(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	next := func(_ context.Context, _ extension.ModelRequest) (extension.ModelResponse, error) {
		return extension.ModelResponse{Text: "the secret-777 leaked"}, nil
	}

	guard := NewRegexOutputGuard([]string{`secret-[\d]+`})
	mw := NewOutputGuardMiddleware(guard)
	wrapped := mw.WrapModel(next)

	resp, err := wrapped(ctx, extension.ModelRequest{Prompt: "is it secret?"})
	require.NoError(t, err)
	assert.Equal(t, blockedOutputMessage, resp.Text, "blocked output must not propagate")
	assert.NotEqual(t, "the secret-777 leaked", resp.Text)
}

// When truncated, GuardResult.Sanitized contains the sanitized output.
func TestOutputGuardMiddlewareSanitizesOnTruncation(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	next := func(_ context.Context, _ extension.ModelRequest) (extension.ModelResponse, error) {
		return extension.ModelResponse{Text: "a very long output that exceeds the limit"}, nil
	}

	mw := NewOutputGuardMiddleware(NewLengthGuard(12))
	wrapped := mw.WrapModel(next)

	resp, err := wrapped(ctx, extension.ModelRequest{Prompt: "go"})
	require.NoError(t, err)
	assert.Equal(t, "a very long ", resp.Text, "Sanitized must contain the truncated output")
	assert.NotEqual(t, "a very long output that exceeds the limit", resp.Text)
}

// production.output_guard span emitted with guard_name/allowed/severity/reason.
func TestGuardEmitsSpanWithAttributes(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, exporter, _ := newGuardTraceCtx(t)

	_, err := NewRegexOutputGuard([]string{`banned`}).Check(ctx, "contains banned text")
	require.NoError(t, err)

	spans := exporter.waitForSpans(t, "production.output_guard", 1)
	attrs := spanAttrs(t, spans[0])
	assert.Equal(t, "regex-output-guard", attrs["guard_name"])
	assert.Equal(t, false, attrs["allowed"])
	assert.Equal(t, "high", attrs["severity"])
	assert.Contains(t, attrs["reason"], "banned")
}

// trace_id consistent and parent_span_id traceable on guard spans.
func TestGuardSpansShareTraceAndParent(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, exporter, rootID := newGuardTraceCtx(t)

	chain := NewOutputGuardChain([]OutputGuard{
		NewRegexOutputGuard([]string{`drop table`}),
		NewCodeInjectionGuard(),
	})
	_, err := chain.Check(ctx, "execute drop table users")
	require.NoError(t, err)

	spans := exporter.waitForSpans(t, "production.output_guard", 3)
	for _, s := range spans {
		assert.Equal(t, "guard-trace", s.TraceID, "trace_id must be consistent")
		assert.Equal(t, rootID, s.ParentSpanID, "parent_span_id must be traceable to the root")
	}
}

// context cancellation propagates out of the guards and middleware.
func TestGuardContextCancellation(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := NewRegexOutputGuard([]string{`x`}).Check(ctx, "anything")
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled))

	_, err = NewLengthGuard(3).Check(ctx, "hello")
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled))

	// Middleware surface the context error through guard evaluation.
	next := func(ctx context.Context, _ extension.ModelRequest) (extension.ModelResponse, error) {
		return extension.ModelResponse{Text: "ok"}, nil
	}
	mw := NewOutputGuardMiddleware(errGuard{})
	wrapped := mw.WrapModel(next)
	_, err = wrapped(ctx, extension.ModelRequest{Prompt: "x"})
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled))
}

// race / leak check plus chain combination correctness.
func TestOutputGuardChainCombinesResults(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	chain := NewOutputGuardChain([]OutputGuard{
		NewCodeInjectionGuard(),
		NewLengthGuard(8),
		NewRegexOutputGuard([]string{`banned+`}),
	})

	res, err := chain.Check(ctx, "hello world")
	require.NoError(t, err)
	assert.False(t, res.Allowed, "truncation marks the chain as not-allowed")
	assert.Equal(t, "hello wo", res.Sanitized)
	assert.Equal(t, GuardLow, res.Severity)

	res, err = chain.Check(ctx, "DROP TABLE users; more text beyond limit")
	require.NoError(t, err)
	assert.False(t, res.Allowed)
	assert.Equal(t, GuardCritical, res.Severity)

	assert.Len(t, chain.Guards(), 3)
	assert.Equal(t, "output-guard-chain", chain.Name())
}

func TestOutputGuardChainBlocksClearSanitized(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()

	chain := NewOutputGuardChain([]OutputGuard{
		NewPIIOutputGuard(),
	})

	res, err := chain.Check(ctx, "contact me at joe@example.com please")
	require.NoError(t, err)
	assert.False(t, res.Allowed, "PII guard should block")
	assert.Empty(t, res.Sanitized, "chain must clear Sanitized when guard blocks with empty Sanitized")
}

func TestOutputGuardMiddlewareChainBlocksDoesNotLeak(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	secret := "the secret-999 is here"
	next := func(_ context.Context, _ extension.ModelRequest) (extension.ModelResponse, error) {
		return extension.ModelResponse{Text: secret}, nil
	}

	chain := NewOutputGuardChain([]OutputGuard{
		NewRegexOutputGuard([]string{`secret-[\d]+`}),
	})
	mw := NewOutputGuardMiddleware(chain)
	wrapped := mw.WrapModel(next)

	resp, err := wrapped(ctx, extension.ModelRequest{Prompt: "leak?"})
	require.NoError(t, err)
	assert.Equal(t, blockedOutputMessage, resp.Text, "chain-blocked output must not leak original text")
	assert.NotContains(t, resp.Text, "secret-999")
}

func TestOutputGuardRegistry(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	orig := GetOutputGuard()
	defer RegisterOutputGuard(orig)

	// Lazy nil-default.
	RegisterOutputGuard(nil)
	got := GetOutputGuard()
	require.NotNil(t, got)
	assert.Equal(t, "output-guard-chain", got.Name())

	// Register custom.
	custom := NewRegexOutputGuard([]string{`a`}, WithName("custom-guard"))
	RegisterOutputGuard(custom)
	assert.Equal(t, "custom-guard", GetOutputGuard().Name())
}

func TestWithGuardSeverityOverridesDefault(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()

	defaultGuard := NewRegexOutputGuard([]string{`banned`})
	res, err := defaultGuard.Check(ctx, "contains banned text")
	require.NoError(t, err)
	assert.False(t, res.Allowed)
	assert.Equal(t, GuardHigh, res.Severity)

	criticalGuard := NewRegexOutputGuard([]string{`banned`}, WithGuardSeverity(GuardCritical))
	res, err = criticalGuard.Check(ctx, "contains banned text")
	require.NoError(t, err)
	assert.False(t, res.Allowed)
	assert.Equal(t, GuardCritical, res.Severity)
}
