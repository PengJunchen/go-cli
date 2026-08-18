package llm

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// makeSSEData generates n SSE events in wire format. Each event has an
// event: field and a data: field with a small JSON-like payload, separated
// by blank lines as required by the SSE spec.
func makeSSEData(n int) string {
	var sb strings.Builder
	for i := 0; i < n; i++ {
		sb.WriteString(fmt.Sprintf("event: delta\ndata: {\"choices\":[{\"delta\":{\"content\":\"chunk-%d\"}}]}\n\n", i))
	}
	return sb.String()
}

// BenchmarkSSEParse measures parsing SSE streams with different numbers of
// events. Each iteration parses a pre-built SSE string and drains all events
// from the returned channel.
func BenchmarkSSEParse(b *testing.B) {
	parser := NewDefaultSSEParser()
	ctx := context.Background()

	for _, n := range []int{10, 100, 1000} {
		b.Run(fmt.Sprintf("events_%d", n), func(b *testing.B) {
			data := makeSSEData(n)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				ch, _ := parser.Parse(ctx, strings.NewReader(data))
				for range ch {
				}
			}
		})
	}
}
