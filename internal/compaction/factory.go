package compaction

import "fmt"

// CompactorFactory creates Compactor instances based on a strategy name.
type CompactorFactory interface {
	Create(strategy string) (Compactor, error)
}

// DefaultCompactorFactory routes strategy names to Compactor implementations.
// A Summarizer injected via WithFactorySummarizer is forwarded to the
// SummaryCompactor (for the "summary" strategy) and wired into the
// UnifiedCompactor (for the "unified" strategy). When no summarizer is
// configured the factory preserves the original behaviour, so a no-arg
// NewDefaultCompactorFactory call is fully backward compatible.
type DefaultCompactorFactory struct {
	summarizer Summarizer
}

// var _ CompactorFactory = (*DefaultCompactorFactory)(nil)
var _ CompactorFactory = (*DefaultCompactorFactory)(nil)

// DefaultCompactorFactoryOption configures a DefaultCompactorFactory.
type DefaultCompactorFactoryOption func(*DefaultCompactorFactory)

// WithFactorySummarizer injects a Summarizer into the factory. When set, the
// "summary" strategy returns a SummaryCompactor backed by this summarizer
// (instead of the no-op fallback), and the "unified" strategy wires the same
// SummaryCompactor into the UnifiedCompactor. The option is named with the
// "Factory" prefix to avoid colliding with unified.go's WithSummary.
func WithFactorySummarizer(s Summarizer) DefaultCompactorFactoryOption {
	return func(f *DefaultCompactorFactory) { f.summarizer = s }
}

// NewDefaultCompactorFactory creates a new DefaultCompactorFactory. The
// variadic options keep the no-arg call site backward compatible: callers that
// do not pass options get a factory with no summarizer, matching the previous
// behaviour exactly.
func NewDefaultCompactorFactory(opts ...DefaultCompactorFactoryOption) *DefaultCompactorFactory {
	f := &DefaultCompactorFactory{}
	for _, opt := range opts {
		opt(f)
	}
	return f
}

// Create returns a Compactor for the given strategy. Supported strategies:
//   - "unified" (default): UnifiedCompactor with automatic degradation; when a
//     summarizer is configured it is wired in via WithSummary.
//   - "micro": MicroCompactor (zero-LLM placeholder replacement)
//   - "summary": SummaryCompactor backed by the configured summarizer (or the
//     no-op fallback when none is set)
//   - "truncating": TruncatingCompactor (simple truncation fallback)
func (f *DefaultCompactorFactory) Create(strategy string) (Compactor, error) {
	switch strategy {
	case "", "unified", "micro_first":
		opts := []UnifiedCompactorOption{}
		if f.summarizer != nil {
			opts = append(opts, WithSummary(NewSummaryCompactor(f.summarizer)))
		}
		return NewUnifiedCompactor(opts...), nil
	case "micro":
		return NewMicroCompactor(), nil
	case "summary":
		return NewSummaryCompactor(f.summarizer), nil
	case "truncating":
		return NewTruncatingCompactor(), nil
	default:
		return nil, fmt.Errorf("compaction: unknown strategy %q (supported: unified, micro, summary, truncating)", strategy)
	}
}
