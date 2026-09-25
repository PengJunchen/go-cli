package tui

import (
	"context"
	"strings"
	"testing"
)

func BenchmarkStreamingMarkdownAppendOneMiB1000Chunks(b *testing.B) {
	const totalBytes = 1 << 20
	const chunks = 1000
	chunkSize := totalBytes / chunks
	chunk := strings.Repeat("x", chunkSize)
	opts := RenderOpts{ContentType: ContentTypeStreaming, Width: 120}
	ctx := context.Background()

	b.ReportAllocs()
	b.SetBytes(totalBytes)
	b.ResetTimer()
	var lastStats streamingMarkdownStats
	for i := 0; i < b.N; i++ {
		renderer := NewStreamingMarkdownRenderer(NewMarkdownRenderer())
		for range chunks {
			renderer.AppendChunk(ctx, chunk, opts)
		}
		lastStats = renderer.stats()
	}
	b.ReportMetric(float64(lastStats.renderedBytes), "window-B")
	b.ReportMetric(float64(lastStats.retainedSourceBytes), "source-tail-B")
	b.ReportMetric(float64(lastStats.parserBytes), "parser-B")
}
