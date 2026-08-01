package session

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// BranchSummary summarizes the entries of a departed session branch so that
// switching between branches preserves a compact recollection of where each
// branch was headed without retaining the full raw transcript on every branch.
type BranchSummary interface {
	// Summarize returns a concise summary of the departed branch's entries.
	Summarize(ctx context.Context, entries []SessionEntry) (string, error)
	// Name returns a stable identifier for the implementation.
	Name() string
}

// SummarizeFunc adapts a concrete LLM call (or a test double) to the narrow
// text-in/text-out shape that DefaultBranchSummary needs. It mirrors the
// summarizer contract used by internal/compaction's SummaryCompactor, so the
// real wiring can reuse the same LLM invocation logic at the composition layer
// without creating a package dependency.
type SummarizeFunc func(ctx context.Context, text string) (string, error)

// errNoSummarizer is returned by the fallback default when no SummarizeFunc has
// been wired in, so missing configuration surfaces loudly instead of emitting
// an empty summary.
var errNoSummarizer = errors.New("session: no branch summarizer configured")

// DefaultBranchSummary is the default BranchSummary. It builds a compaction
// prompt from the departed branch's entries and delegates the actual model call
// to an injected SummarizeFunc.
type DefaultBranchSummary struct {
	name      string
	summarize SummarizeFunc
}

// Compile-time assertion that DefaultBranchSummary satisfies BranchSummary.
var _ BranchSummary = (*DefaultBranchSummary)(nil)

// BranchSummaryOption configures a DefaultBranchSummary.
type BranchSummaryOption func(*DefaultBranchSummary)

// WithBranchSummaryName overrides the identifier returned by Name.
func WithBranchSummaryName(name string) BranchSummaryOption {
	return func(d *DefaultBranchSummary) { d.name = name }
}

// NewDefaultBranchSummary returns a DefaultBranchSummary that delegates
// summarization to summarizer. When summarizer is nil, a fallback that errors is
// used so misconfiguration is obvious.
func NewDefaultBranchSummary(summarizer SummarizeFunc, opts ...BranchSummaryOption) *DefaultBranchSummary {
	d := &DefaultBranchSummary{
		name:      "default-branch-summary",
		summarize: summarizer,
	}
	if d.summarize == nil {
		d.summarize = func(context.Context, string) (string, error) { return "", errNoSummarizer }
	}
	for _, opt := range opts {
		if opt != nil {
			opt(d)
		}
	}
	return d
}

// Name returns the identifier of this BranchSummary.
func (d *DefaultBranchSummary) Name() string { return d.name }

// Summarize emits a compaction span, aggregates the departed branch's entries
// into a single prompt, and delegates to the injected SummarizeFunc. The result
// of the model call is returned verbatim.
func (d *DefaultBranchSummary) Summarize(ctx context.Context, entries []SessionEntry) (string, error) {
	span, _ := tracing.SpanFromContext(ctx, "compaction.branch_summary", tracing.SpanKindInternal)
	defer span.End()
	logger := tracing.NewTraceLogger(span, slog.Default())
	span.SetAttributes(
		tracing.Attribute{Key: "strategy_used", Value: "branch_summary"},
		tracing.Attribute{Key: "entry_count", Value: len(entries)},
	)

	prompt := d.buildPrompt(entries)
	summary, err := d.summarize(ctx, prompt)
	if err != nil {
		span.SetStatus(tracing.SpanStatusError, err.Error())
		logger.Error("compaction_branch_summary", "op", "compaction.branch_summary", "error_type", "summarize_failed", "err", err)
		return "", err
	}
	span.SetAttributes(
		tracing.Attribute{Key: "summary_length", Value: len(summary)},
	)
	logger.Info("compaction_branch_summary", "op", "compaction.branch_summary", "strategy_used", "branch_summary", "entry_count", len(entries), "summary_length", len(summary))
	span.SetStatus(tracing.SpanStatusOK, "")
	return summary, nil
}

// buildPrompt condenses the departed branch's entries into a self-contained
// summarization instruction, mirroring internal/compaction's prompt style.
func (d *DefaultBranchSummary) buildPrompt(entries []SessionEntry) string {
	var sb strings.Builder
	sb.WriteString("Summarize the following departed conversation branch, focusing on the ongoing task and decisions. Keep the summary concise and self-contained.\n\n[messages]\n")
	for _, e := range entries {
		if e.Content != "" {
			sb.WriteString(e.Content)
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

// Process-wide registry for the active BranchSummary, mirroring the lazy
// nil-default pattern in internal/production/registry.go.
var (
	branchSummaryMu          sync.RWMutex
	defaultBranchSummaryInst BranchSummary
)

// RegisterBranchSummary sets the active BranchSummary. A nil value resets to a
// fresh DefaultBranchSummary with no summarizer (which errors on use).
func RegisterBranchSummary(s BranchSummary) {
	branchSummaryMu.Lock()
	defer branchSummaryMu.Unlock()
	if s == nil {
		s = NewDefaultBranchSummary(nil)
	}
	slog.Info("session.register.branch_summary", "name", s.Name())
	defaultBranchSummaryInst = s
}

// GetBranchSummary returns the active BranchSummary, lazily defaulting to a
// fresh DefaultBranchSummary when none has been registered.
func GetBranchSummary() BranchSummary {
	branchSummaryMu.RLock()
	defer branchSummaryMu.RUnlock()
	if defaultBranchSummaryInst == nil {
		return NewDefaultBranchSummary(nil)
	}
	return defaultBranchSummaryInst
}
