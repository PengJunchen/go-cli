package production

import (
	"context"
	"log/slog"
	"regexp"
	"sync"
)

// redactedPlaceholder is the text substituted for every match of a redact
// pattern.
const redactedPlaceholder = "[REDACTED]"

// RedactingOutputGuard masks sensitive content in model output by replacing
// every match of a configured regex pattern with [REDACTED]. Unlike the
// blocking guards (RegexOutputGuard, PIIOutputGuard) which deny the entire
// output when a pattern matches, the redacting guard preserves the surrounding
// text and only masks the sensitive fragment, allowing the output to propagate.
//
// Patterns are added via AddRedactPattern. Common use cases include masking
// API keys, bearer tokens, passwords in connection strings, and other secrets
// that may leak into model output.
type RedactingOutputGuard struct {
	name     string
	mu       sync.RWMutex
	patterns []*regexp.Regexp
}

// Compile-time assertion that RedactingOutputGuard satisfies OutputGuard.
var _ OutputGuard = (*RedactingOutputGuard)(nil)

// NewRedactingOutputGuard returns a RedactingOutputGuard with no patterns.
// Patterns are added via AddRedactPattern. Options may override the name.
func NewRedactingOutputGuard(opts ...Option) *RedactingOutputGuard {
	o := applyOptions(opts)
	name := o.name
	if name == "" {
		name = "redacting-output-guard"
	}
	return &RedactingOutputGuard{name: name}
}

// AddRedactPattern compiles pattern and registers it for masking. Patterns
// added after the first Check call are still applied on subsequent checks.
// Invalid patterns and empty patterns are silently ignored — an empty pattern
// matches at every position and would corrupt the output — to match the
// behaviour of the other regex-based guards.
func (g *RedactingOutputGuard) AddRedactPattern(pattern string) {
	if pattern == "" {
		return
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.patterns = append(g.patterns, re)
}

// Check masks every match of the registered patterns in text, replacing each
// match with [REDACTED]. The output is always Allowed (severity GuardLow) so
// downstream guards in a chain can still inspect the redacted text.
func (g *RedactingOutputGuard) Check(ctx context.Context, text string) (*GuardResult, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}

	g.mu.RLock()
	patterns := make([]*regexp.Regexp, len(g.patterns))
	copy(patterns, g.patterns)
	g.mu.RUnlock()

	sanitized := text
	for _, re := range patterns {
		sanitized = re.ReplaceAllString(sanitized, redactedPlaceholder)
	}

	res := &GuardResult{
		Allowed:   true,
		Sanitized: sanitized,
		Severity:  GuardLow,
	}
	if sanitized != text {
		res.Reason = "output redacted: sensitive patterns masked"
	}
	emitGuardResult(ctx, slog.Default(), g.Name(), res)
	return res, nil
}

// Name returns the guard identifier.
func (g *RedactingOutputGuard) Name() string { return g.name }
