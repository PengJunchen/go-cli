package production

import (
	"context"
	"log/slog"
	"regexp"
	"strconv"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// This file defines the OutputGuard system. It
// provides the OutputGuard contract, the GuardResult value, the GuardSeverity
// enumeration and four default guard implementations (Regex, PII,
// CodeInjection, Length) together with an OutputGuardChain that composes them.
//
// Design notes:
//   - The task spec named the guarded message type "AgentMessage". To keep this
//     package decoupled from any specific model/message type (it deliberately
//     avoids importing internal/llm), guards operate on plain text: `Check(ctx,
//     text string)` and `GuardResult.Sanitized string`. Downstream adapters can
//     project any message type to its text field before guarding.

// GuardSeverity categorizes the seriousness of a guard decision.
type GuardSeverity string

const (
	// GuardLow indicates a minor adjustment (e.g. output was truncated).
	GuardLow GuardSeverity = "low"
	// GuardMedium indicates a moderate risk detected.
	GuardMedium GuardSeverity = "medium"
	// GuardHigh indicates sensitive content was detected.
	GuardHigh GuardSeverity = "high"
	// GuardCritical indicates a critical risk (e.g. code injection).
	GuardCritical GuardSeverity = "critical"
)

// GuardResult is the outcome of running an OutputGuard against a text output.
type GuardResult struct {
	// Allowed reports whether the output may propagate. A false value (e.g.
	// truncation) still produces a usable Sanitized output.
	Allowed bool
	// Reason describes why the guard blocked, sanitized, or allowed the output.
	Reason string
	// Sanitized is the safe/truncated text to use in place of the original
	// output. It equals the input when Allowed is true.
	Sanitized string
	// Severity indicates how serious the finding is.
	Severity GuardSeverity
}

// OutputGuard inspects a plain-text model output and decides whether it may
// propagate. It returns a GuardResult describing the decision.
type OutputGuard interface {
	// Check evaluates text and returns a guard decision. It returns an error
	// only when the guard itself cannot run (e.g. the context is canceled).
	Check(ctx context.Context, text string) (*GuardResult, error)
	// Name returns the guard identifier.
	Name() string
}

// severityRank returns a numeric rank used to combine severities, higher means
// more critical. Zero/unknown severities rank last.
func severityRank(s GuardSeverity) int {
	switch s {
	case GuardCritical:
		return 4
	case GuardHigh:
		return 3
	case GuardMedium:
		return 2
	case GuardLow:
		return 1
	default:
		return 0
	}
}

// emitGuardResult records a guard decision on the span and via the logger.
func emitGuardResult(ctx context.Context, logger *slog.Logger, name string, res *GuardResult) {
	span, _ := tracing.SpanFromContext(ctx, "production.output_guard", tracing.SpanKindInternal)
	defer span.End()

	span.SetAttributes(
		tracing.Attribute{Key: "guard_name", Value: name},
		tracing.Attribute{Key: "allowed", Value: res.Allowed},
		tracing.Attribute{Key: "severity", Value: string(res.Severity)},
		tracing.Attribute{Key: "reason", Value: res.Reason},
	)
	logger.InfoContext(ctx, "output_guard",
		"guard_name", name,
		"allowed", res.Allowed,
		"severity", string(res.Severity),
		"reason", res.Reason,
	)
	if res.Allowed {
		span.SetStatus(tracing.SpanStatusOK, "")
	} else {
		span.SetStatus(tracing.SpanStatusError, res.Reason)
	}
}

// checkContext returns early with context.Canceled when ctx is already done so
// guard evaluation honors cancellation.
func checkContext(ctx context.Context) error {
	return ctx.Err()
}

// RegexOutputGuard blocks outputs that match any configured regex pattern.
type RegexOutputGuard struct {
	name     string
	patterns []*regexp.Regexp
	severity GuardSeverity
}

// Compile-time assertion that RegexOutputGuard satisfies OutputGuard.
var _ OutputGuard = (*RegexOutputGuard)(nil)

// NewRegexOutputGuard returns a RegexOutputGuard that denies any output
// matching at least one of patterns. Options may override the name and the
// severity of the denial (default GuardHigh).
func NewRegexOutputGuard(patterns []string, opts ...Option) *RegexOutputGuard {
	o := applyOptions(opts)
	name := o.name
	if name == "" {
		name = "regex-output-guard"
	}
	severity := GuardHigh
	if o.guardSeverity != "" {
		severity = o.guardSeverity
	}
	g := &RegexOutputGuard{
		name:     name,
		patterns: make([]*regexp.Regexp, 0, len(patterns)),
		severity: severity,
	}
	for _, p := range patterns {
		if re, err := regexp.Compile(p); err == nil {
			g.patterns = append(g.patterns, re)
		}
	}
	return g
}

// Check denies the text when any configured pattern matches.
func (g *RegexOutputGuard) Check(ctx context.Context, text string) (*GuardResult, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	res := &GuardResult{Allowed: true, Sanitized: text}
	logger := slog.Default()
	for _, re := range g.patterns {
		if re.MatchString(text) {
			res.Allowed = false
			res.Sanitized = ""
			res.Reason = "output matches blocked pattern " + re.String()
			res.Severity = g.severity
			break
		}
	}
	emitGuardResult(ctx, logger, g.Name(), res)
	return res, nil
}

// Name returns the guard identifier.
func (g *RegexOutputGuard) Name() string { return g.name }

// PII patterns detect personal-identifiable information.
var piiPatterns = []string{
	// Email addresses.
	`[A-Za-z0-9._%+\-]+@[A-Za-z0-9\-]+(\.[A-Za-z0-9\-]+)+`,
	// Mainland China mobile phone numbers (11 digits starting 1[3-9]).
	`\b1[3-9][0-9]{9}\b`,
	// Mainland China resident ID card numbers (18 chars, check digit 0-9/X).
	`\b[1-9][0-9]{5}(18|19|20)[0-9]{2}(0[1-9]|1[0-2])(0[1-9]|[12][0-9]|3[01])[0-9]{3}[0-9Xx]\b`,
}

// PIIOutputGuard detects and blocks personal-identifiable information.
type PIIOutputGuard struct {
	name     string
	patterns []*regexp.Regexp
}

// Compile-time assertion that PIIOutputGuard satisfies OutputGuard.
var _ OutputGuard = (*PIIOutputGuard)(nil)

// NewPIIOutputGuard returns a PIIOutputGuard that blocks emails, phone
// numbers and Chinese ID card numbers.
func NewPIIOutputGuard(opts ...Option) *PIIOutputGuard {
	o := applyOptions(opts)
	name := o.name
	if name == "" {
		name = "pii-output-guard"
	}
	g := &PIIOutputGuard{name: name}
	for _, p := range piiPatterns {
		if re, err := regexp.Compile(p); err == nil {
			g.patterns = append(g.patterns, re)
		}
	}
	return g
}

// Check denies the text when any PII regex matches.
func (g *PIIOutputGuard) Check(ctx context.Context, text string) (*GuardResult, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	res := &GuardResult{Allowed: true, Sanitized: text, Severity: GuardHigh}
	logger := slog.Default()
	for _, re := range g.patterns {
		if re.MatchString(text) {
			res.Allowed = false
			res.Sanitized = ""
			res.Reason = "output contains PII matching " + re.String()
			break
		}
	}
	emitGuardResult(ctx, logger, g.Name(), res)
	return res, nil
}

// Name returns the guard identifier.
func (g *PIIOutputGuard) Name() string { return g.name }

// CodeInjection patterns detect common code-injection indicators.
var codeInjectionPatterns = []string{
	`(?i)\bdrop\s+table\b`,
	`(?i)os\.system\s*\(`,
	`(?i)subprocess[\w.]*\.?\s+`,
	`(?i)<\s*script`,
	`(?i)__import__\s*\(`,
	`(?i)\beval\s*\(`,
	`(?i)\bexec\s*\(`,
	`(?i)\bselect\b[^;]*\bfrom\b`,
	`(?i)\binsert\s+into\b`,
	`(?i)\bdelete\s+from\b`,
	`(?i)\bupdate\s+\w+\s+set\b`,
}

// CodeInjectionGuard detects and blocks code-injection indicators.
type CodeInjectionGuard struct {
	name     string
	patterns []*regexp.Regexp
}

// Compile-time assertion that CodeInjectionGuard satisfies OutputGuard.
var _ OutputGuard = (*CodeInjectionGuard)(nil)

// NewCodeInjectionGuard returns a CodeInjectionGuard that blocks outputs
// containing code-injection indicators.
func NewCodeInjectionGuard(opts ...Option) *CodeInjectionGuard {
	o := applyOptions(opts)
	name := o.name
	if name == "" {
		name = "code-injection-guard"
	}
	g := &CodeInjectionGuard{name: name}
	for _, p := range codeInjectionPatterns {
		if re, err := regexp.Compile(p); err == nil {
			g.patterns = append(g.patterns, re)
		}
	}
	return g
}

// Check denies the text when any injection regex matches.
func (g *CodeInjectionGuard) Check(ctx context.Context, text string) (*GuardResult, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	res := &GuardResult{Allowed: true, Sanitized: text, Severity: GuardCritical}
	logger := slog.Default()
	for _, re := range g.patterns {
		if re.MatchString(text) {
			res.Allowed = false
			res.Sanitized = ""
			res.Reason = "output contains code-injection indicator " + re.String()
			break
		}
	}
	emitGuardResult(ctx, logger, g.Name(), res)
	return res, nil
}

// Name returns the guard identifier.
func (g *CodeInjectionGuard) Name() string { return g.name }

// LengthGuard enforces a maximum output length, truncating over-long output.
type LengthGuard struct {
	name     string
	maxRunes int
}

// Compile-time assertion that LengthGuard satisfies OutputGuard.
var _ OutputGuard = (*LengthGuard)(nil)

// NewLengthGuard returns a LengthGuard that truncates outputs longer than
// maxChars runes. A non-positive max disables limiting.
func NewLengthGuard(maxChars int, opts ...Option) *LengthGuard {
	o := applyOptions(opts)
	name := o.name
	if name == "" {
		name = "length-guard"
	}
	if maxChars < 0 {
		maxChars = 0
	}
	return &LengthGuard{name: name, maxRunes: maxChars}
}

// Check truncates the text to maxRunes when it exceeds that limit.
func (g *LengthGuard) Check(ctx context.Context, text string) (*GuardResult, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	res := &GuardResult{Allowed: true, Sanitized: text, Severity: GuardLow}
	logger := slog.Default()
	if g.maxRunes > 0 && len([]rune(text)) > g.maxRunes {
		runes := []rune(text)
		res.Allowed = false
		res.Sanitized = string(runes[:g.maxRunes])
		res.Reason = "output truncated from " + strconv.Itoa(len(runes)) + " runes to " + strconv.Itoa(g.maxRunes)
	}
	emitGuardResult(ctx, logger, g.Name(), res)
	return res, nil
}

// Name returns the guard identifier.
func (g *LengthGuard) Name() string { return g.name }

// OutputGuardChain runs multiple guards in order and combines their results.
// Sanitized text produced by one guard (e.g. truncation) is fed to the next.
type OutputGuardChain struct {
	name   string
	guards []OutputGuard
}

// Compile-time assertion that OutputGuardChain satisfies OutputGuard.
var _ OutputGuard = (*OutputGuardChain)(nil)

// NewOutputGuardChain returns a chain that runs guards in the given order.
func NewOutputGuardChain(guards []OutputGuard, opts ...Option) *OutputGuardChain {
	o := applyOptions(opts)
	name := o.name
	if name == "" {
		name = "output-guard-chain"
	}
	c := &OutputGuardChain{name: name, guards: make([]OutputGuard, 0, len(guards))}
	for _, g := range guards {
		if g != nil {
			c.guards = append(c.guards, g)
		}
	}
	return c
}

// Check runs each guard in order, carrying sanitized text forward and combining
// the severities and reasons of any denial.
func (c *OutputGuardChain) Check(ctx context.Context, text string) (*GuardResult, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	combined := &GuardResult{Allowed: true, Sanitized: text}
	logger := slog.Default()
	// The chain records the overall decision on its own span, but each member
	// guard still emits its own span via its Check method.
	span, _ := tracing.SpanFromContext(ctx, "production.output_guard", tracing.SpanKindInternal)
	defer span.End()

	topSeverity := GuardSeverity("")
	for _, g := range c.guards {
		if err := checkContext(ctx); err != nil {
			return nil, err
		}
		res, err := g.Check(ctx, combined.Sanitized)
		if err != nil {
			return nil, err
		}
		combined.Sanitized = res.Sanitized
		if !res.Allowed {
			combined.Allowed = false
			if topSeverity == "" || severityRank(res.Severity) > severityRank(topSeverity) {
				topSeverity = res.Severity
				combined.Severity = res.Severity
			}
			if combined.Reason == "" {
				combined.Reason = res.Reason
			} else {
				combined.Reason = combined.Reason + "; " + res.Reason
			}
		}
	}
	span.SetAttributes(
		tracing.Attribute{Key: "guard_name", Value: c.Name()},
		tracing.Attribute{Key: "allowed", Value: combined.Allowed},
		tracing.Attribute{Key: "severity", Value: string(combined.Severity)},
		tracing.Attribute{Key: "reason", Value: combined.Reason},
	)
	logger.InfoContext(ctx, "output_guard_chain",
		"guard_name", c.Name(),
		"allowed", combined.Allowed,
		"severity", string(combined.Severity),
		"reason", combined.Reason,
	)
	if combined.Allowed {
		span.SetStatus(tracing.SpanStatusOK, "")
	} else {
		span.SetStatus(tracing.SpanStatusError, combined.Reason)
	}
	return combined, nil
}

// Guards returns the ordered member guards.
func (c *OutputGuardChain) Guards() []OutputGuard {
	out := make([]OutputGuard, len(c.guards))
	copy(out, c.guards)
	return out
}

// Name returns the chain identifier.
func (c *OutputGuardChain) Name() string { return c.name }
