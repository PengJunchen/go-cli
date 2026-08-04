package compaction

import "fmt"

// CompactorFactory creates Compactor instances based on a strategy name.
type CompactorFactory interface {
	Create(strategy string) (Compactor, error)
}

// DefaultCompactorFactory routes strategy names to Compactor implementations.
type DefaultCompactorFactory struct{}

// var _ CompactorFactory = (*DefaultCompactorFactory)(nil)
var _ CompactorFactory = (*DefaultCompactorFactory)(nil)

// NewDefaultCompactorFactory creates a new DefaultCompactorFactory.
func NewDefaultCompactorFactory() *DefaultCompactorFactory {
	return &DefaultCompactorFactory{}
}

// Create returns a Compactor for the given strategy. Supported strategies:
//   - "unified" (default): UnifiedCompactor with automatic degradation
//   - "micro": MicroCompactor (zero-LLM placeholder replacement)
//   - "summary": SummaryCompactor (requires a Summarizer; uses a no-op fallback)
//   - "truncating": TruncatingCompactor (simple truncation fallback)
func (f *DefaultCompactorFactory) Create(strategy string) (Compactor, error) {
	switch strategy {
	case "", "unified", "micro_first":
		return NewUnifiedCompactor(), nil
	case "micro":
		return NewMicroCompactor(), nil
	case "summary":
		return NewSummaryCompactor(nil), nil
	case "truncating":
		return NewTruncatingCompactor(), nil
	default:
		return nil, fmt.Errorf("compaction: unknown strategy %q (supported: unified, micro, summary, truncating)", strategy)
	}
}
