package tracing

import (
	"context"
	"fmt"
	"testing"
)

// BenchmarkSpanLifecycle measures the full span lifecycle (Start →
// SetAttributes → End) across different numbers of attributes. A nil exporter
// is used so the benchmark isolates span bookkeeping cost without export I/O.
func BenchmarkSpanLifecycle(b *testing.B) {
	tracer := NewTracer("bench", nil)
	ctx := context.Background()

	for _, nAttrs := range []int{1, 10, 100} {
		b.Run(fmt.Sprintf("attrs_%d", nAttrs), func(b *testing.B) {
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				span, _ := tracer.Start(ctx, "bench.span", SpanKindInternal)
				for j := 0; j < nAttrs; j++ {
					span.SetAttributes(Attribute{
						Key:   fmt.Sprintf("key_%d", j),
						Value: j,
					})
				}
				span.End()
			}
		})
	}
}
