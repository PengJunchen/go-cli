package production

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/extension"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// severityRank ordering: critical > high > medium > low > unknown.
func TestSeverityRankOrdering(t *testing.T) {
	assert.Greater(t, severityRank(GuardCritical), severityRank(GuardHigh))
	assert.Greater(t, severityRank(GuardHigh), severityRank(GuardMedium))
	assert.Greater(t, severityRank(GuardMedium), severityRank(GuardLow))
	assert.Greater(t, severityRank(GuardLow), severityRank(GuardSeverity("unknown")))

	// Unknown/zero severities rank last (returns 0).
	assert.Equal(t, 0, severityRank(GuardSeverity("")))
	assert.Equal(t, 0, severityRank(GuardSeverity("weird")))
}

// RegexOutputGuard: invalid patterns are silently ignored, empty pattern list
// never blocks, and every configured severity can be selected.
func TestRegexOutputGuardEdgeCases(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()

	// Invalid regex pattern is ignored -> nothing blocks.
	g := NewRegexOutputGuard([]string{`[invalid`})
	res, err := g.Check(ctx, "arr")
	require.NoError(t, err)
	assert.True(t, res.Allowed)
	assert.Equal(t, "arr", res.Sanitized)

	// No patterns -> never blocks.
	empty := NewRegexOutputGuard(nil)
	res, err = empty.Check(ctx, "anything at all")
	require.NoError(t, err)
	assert.True(t, res.Allowed)

	// An empty pattern compiles and matches every string (blocks everything).
	every := NewRegexOutputGuard([]string{``})
	res, err = every.Check(ctx, "nope")
	require.NoError(t, err)
	assert.False(t, res.Allowed)

	// Each severity is honored via the option.
	for _, sev := range []GuardSeverity{GuardLow, GuardMedium, GuardHigh, GuardCritical} {
		sg := NewRegexOutputGuard([]string{`banned`}, WithGuardSeverity(sev))
		res, err = sg.Check(ctx, "this is banned")
		require.NoError(t, err)
		assert.False(t, res.Allowed)
		assert.Equal(t, sev, res.Severity)
		assert.Empty(t, res.Sanitized, "regex guard clears sanitized on block")
	}
}

// PIIOutputGuard custom name and that a block clears Sanitized.
func TestPIIOutputGuardNameAndBlock(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	g := NewPIIOutputGuard(WithName("pii-custom"))
	assert.Equal(t, "pii-custom", g.Name())

	res, err := g.Check(ctx, "mail me at a@b.co")
	require.NoError(t, err)
	assert.False(t, res.Allowed)
	assert.Equal(t, GuardHigh, res.Severity)
	assert.Empty(t, res.Sanitized)
}

// CodeInjectionGuard custom name and sanitized clearing.
func TestCodeInjectionGuardNameAndBlock(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	g := NewCodeInjectionGuard(WithName("inj-custom"))
	assert.Equal(t, "inj-custom", g.Name())

	res, err := g.Check(ctx, "run exec('x')")
	require.NoError(t, err)
	assert.False(t, res.Allowed)
	assert.Equal(t, GuardCritical, res.Severity)
	assert.Empty(t, res.Sanitized)
}

// LengthGuard: non-positive max disables limiting; negative is clamped to 0;
// truncation counts runes (multibyte safe), not bytes.
func TestLengthGuardDisableAndRuneCounting(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()

	// maxChars <= 0 disables limiting.
	for _, m := range []int{0, -3} {
		g := NewLengthGuard(m)
		res, err := g.Check(ctx, "this text is long enough to matter")
		require.NoError(t, err)
		assert.True(t, res.Allowed, "maxChars=%d should disable limiting", m)
		assert.Equal(t, "this text is long enough to matter", res.Sanitized)
	}

	// Rune-safe truncation: 3 Chinese runes kept even though UTF-8 encoding spans 9 bytes.
	g := NewLengthGuard(3)
	res, err := g.Check(ctx, "你好世界extra")
	require.NoError(t, err)
	assert.False(t, res.Allowed)
	assert.Equal(t, "你好世", res.Sanitized)
	assert.Equal(t, GuardLow, res.Severity)

	// Exactly at the limit is allowed.
	res, err = g.Check(ctx, "你好世")
	require.NoError(t, err)
	assert.True(t, res.Allowed)
}

// OutputGuardChain: nil members filtered, empty chain always allows, context
// cancellation mid-chain short-circuits.
func TestOutputGuardChainFilteringAndEmpty(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()

	// nil members are dropped.
	chain := NewOutputGuardChain([]OutputGuard{nil, NewRegexOutputGuard([]string{`banned`}), nil})
	require.Len(t, chain.Guards(), 1)

	// Empty chain: always allowed, sanitized passes through.
	empty := NewOutputGuardChain(nil)
	res, err := empty.Check(ctx, "keep me")
	require.NoError(t, err)
	assert.True(t, res.Allowed)
	assert.Equal(t, "keep me", res.Sanitized)
	assert.Empty(t, res.Reason)
}

func TestOutputGuardChainContextCancellationMidRun(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, cancel := context.WithCancel(context.Background())

	chain := NewOutputGuardChain([]OutputGuard{
		NewRegexOutputGuard([]string{`first`}),
		&cancelingGuard{onCheck: cancel, called: &cancelable{}},
		NewRegexOutputGuard([]string{`last`}),
	})
	_, err := chain.Check(ctx, "some harmless text")
	assert.True(t, errors.Is(err, context.Canceled), "cancellation mid-chain must surface context.Canceled, got %v", err)
}

// cancelable is a small bool holder so cancelingGuard can be reconstructed
// without closure aliasing.
type cancelable struct{ v bool }

// cancelingGuard cancels the context on its first Check but returns success so
// the chain advances to the next guard where checkContext detects the cancel.
type cancelingGuard struct {
	onCheck func()
	called  *cancelable
}

func (g *cancelingGuard) Name() string { return "canceling-guard" }

func (g *cancelingGuard) Check(_ context.Context, text string) (*GuardResult, error) {
	if !g.called.v {
		g.called.v = true
		if g.onCheck != nil {
			g.onCheck() // cancel ctx AFTER the pre-iteration checkContext for this guard
		}
	}
	return &GuardResult{Allowed: true, Sanitized: text}, nil
}

// Guards returns a defensive copy: mutating the slice does not affect the chain.
func TestOutputGuardChainGuardsReturnsCopy(t *testing.T) {
	chain := NewOutputGuardChain([]OutputGuard{
		NewRegexOutputGuard([]string{`a`}),
		NewLengthGuard(5),
	})
	guards := chain.Guards()
	require.Len(t, guards, 2)
	guards[0] = nil // caller mutation must not affect the chain
	assert.Len(t, chain.Guards(), 2)
}

// OutputGuard registry default chain composes the four built-in guards and
// blocks via each member.
func TestDefaultOutputGuardChainComposition(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	chain := defaultOutputGuardChain()
	c, ok := chain.(*OutputGuardChain)
	require.True(t, ok, "default guard chain must be an OutputGuardChain")
	require.Len(t, c.Guards(), 4)

	// Code injection is denied with critical severity.
	res, err := chain.Check(ctx, "DROP TABLE users")
	require.NoError(t, err)
	assert.False(t, res.Allowed)
	assert.Equal(t, GuardCritical, res.Severity)

	// PII is denied with high severity.
	res, err = chain.Check(ctx, "phone 13912345678")
	require.NoError(t, err)
	assert.False(t, res.Allowed)
	assert.Equal(t, GuardHigh, res.Severity)
}

// OutputGuard registry: concurrent register/get under race detector.
func TestOutputGuardRegistryConcurrent(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	orig := GetOutputGuard()
	defer RegisterOutputGuard(orig)

	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			if g%2 == 0 {
				RegisterOutputGuard(nil)
			} else {
				RegisterOutputGuard(NewRegexOutputGuard([]string{`x`}, WithName("conc")))
			}
			_ = GetOutputGuard()
		}(g)
	}
	wg.Wait()
	assert.NotNil(t, GetOutputGuard())
}

// Middleware: when a guard errors (fail closed) the original text never leaks,
// and the guard error is surfaced to the caller.
func TestOutputGuardMiddlewareFailClosedOnGuardError(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	const sensitiveText = "the real sensitive output"

	mw := NewOutputGuardMiddleware(&failingGuard{err: errors.New("guard blew up")})
	wrapped := mw.WrapModel(func(_ context.Context, _ extension.ModelRequest) (extension.ModelResponse, error) {
		return extension.ModelResponse{Text: sensitiveText}, nil
	})

	resp, err := wrapped(ctx, extension.ModelRequest{Prompt: "x"})
	require.Error(t, err)
	assert.Equal(t, blockedOutputMessage, resp.Text, "fail-closed output must not leak raw text")
	assert.NotContains(t, resp.Text, sensitiveText)
}

// failingGuard always returns the configured error.
type failingGuard struct {
	err error
}

func (g *failingGuard) Name() string { return "failing-guard" }

func (g *failingGuard) Check(context.Context, string) (*GuardResult, error) {
	return nil, g.err
}
