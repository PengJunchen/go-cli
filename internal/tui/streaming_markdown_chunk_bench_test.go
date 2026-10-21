//go:build !headless

package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func BenchmarkStreamingMarkdownAppendOneMiB1000Chunks(b *testing.B) {
	benchmarkStreamingMarkdownAppendChunks(b, 1000)
}

func BenchmarkStreamingMarkdownAppendChunkScaling(b *testing.B) {
	for _, chunks := range []int{1, 16, 256, 1000} {
		b.Run(fmt.Sprintf("chunks_%d", chunks), func(b *testing.B) {
			benchmarkStreamingMarkdownAppendChunks(b, chunks)
		})
	}
}

func benchmarkStreamingMarkdownAppendChunks(b *testing.B, chunks int) {
	const totalBytes = 1 << 20
	opts := RenderOpts{ContentType: ContentTypeStreaming, Width: 120}
	ctx := context.Background()

	b.ReportAllocs()
	b.SetBytes(totalBytes)
	b.ResetTimer()
	var lastStats streamingMarkdownStats
	for i := 0; i < b.N; i++ {
		renderer := NewStreamingMarkdownRenderer(NewMarkdownRenderer())
		chunkSize := totalBytes / chunks
		remainder := totalBytes % chunks
		for chunkIndex := range chunks {
			size := chunkSize
			if chunkIndex < remainder {
				size++
			}
			renderer.AppendChunk(ctx, strings.Repeat("x", size), opts)
		}
		lastStats = renderer.stats()
	}
	if lastStats.inputBytes != totalBytes {
		b.Fatalf("benchmark supplied %d bytes; want %d", lastStats.inputBytes, totalBytes)
	}
	b.ReportMetric(float64(lastStats.renderedBytes), "window-B")
	b.ReportMetric(float64(lastStats.retainedSourceBytes), "source-tail-B")
	b.ReportMetric(float64(lastStats.parserBytes), "parser-B")
	b.ReportMetric(float64(lastStats.inputBytes), "input-B/op")
}
