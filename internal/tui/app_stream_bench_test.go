//go:build !headless

package tui

import (
	"strings"
	"testing"
)

// BenchmarkBubbleTeaIncrementalTinyChunkUpdateView measures the legacy live
// assistant path through the real Bubble Tea model. It includes both Update
// and View for every small event so a renderer-only microbenchmark cannot hide
// app-level work such as source copying and accordion rendering.
func BenchmarkBubbleTeaIncrementalTinyChunkUpdateView(b *testing.B) {
	const (
		totalBytes = 256 << 10
		chunkBytes = 32
	)
	chunk := strings.Repeat("x", chunkBytes)
	const chunks = totalBytes / chunkBytes
	b.ReportAllocs()
	b.SetBytes(totalBytes)
	b.ResetTimer()
	for operation := 0; operation < b.N; operation++ {
		app := NewBubbleteaApp(nil, WithWidth(80))
		for chunkIndex := 0; chunkIndex < chunks; chunkIndex++ {
			app.model.Update(agentEventMsg{event: AgentEvent{
				Type:        "message",
				ContentType: ContentTypeAssistant,
				Content:     chunk,
				Incremental: true,
			}})
			_ = app.model.View()
		}
	}
}
