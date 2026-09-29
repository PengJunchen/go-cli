package tui

import (
	"fmt"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/pengjunchen/go-cli/internal/core"
)

// BenchmarkBubbleTeaTranscriptUpdateView exercises the real app model and
// renderer: each operation streams 1 MiB in 1,000 typed events through
// teaModel.Update and calls BubbleteaApp.View after every event.
func BenchmarkBubbleTeaTranscriptUpdateView(b *testing.B) {
	for _, historyRows := range []int{1_000, 5_000, 10_000} {
		b.Run(fmt.Sprintf("history-%d", historyRows), func(b *testing.B) {
			const chunkCount = 1_000
			const chunkBytes = 1 << 10
			chunk := strings.Repeat("x", chunkBytes)
			index := uint64(1)

			// Collect fixed-workload latency and memory measurements before the
			// benchmark calibration loop. Every history size gets exactly one
			// 1 MiB stream (1,000 x 1 KiB events), so retained-heap values are
			// directly comparable instead of depending on b.N.
			app := newBenchmarkBubbleteaApp(b, historyRows)
			setRowsAtStart := app.model.transcriptViewport.setRowsCalls
			sameRowsAtStart := app.model.transcriptViewport.sameRowsCalls
			runtime.GC()
			var loadedHeap runtime.MemStats
			runtime.ReadMemStats(&loadedHeap)
			latencies := make([]time.Duration, 0, chunkCount)
			var firstStreamAllocBytes uint64
			var before runtime.MemStats
			runtime.ReadMemStats(&before)
			for chunkIndex := 0; chunkIndex < chunkCount; chunkIndex++ {
				started := time.Now()
				app.model.Update(agentEventMsg{event: AgentEvent{
					Type:                     "message",
					ContentType:              ContentTypeAssistant,
					Content:                  chunk,
					Incremental:              true,
					RunID:                    "benchmark-run",
					CanonicalTranscriptIndex: &index,
				}})
				_ = app.View()
				latencies = append(latencies, time.Since(started))
			}
			var after runtime.MemStats
			runtime.ReadMemStats(&after)
			firstStreamAllocBytes = after.TotalAlloc - before.TotalAlloc
			firstStreamSetRows := app.model.transcriptViewport.setRowsCalls - setRowsAtStart
			firstStreamSameRows := app.model.transcriptViewport.sameRowsCalls - sameRowsAtStart
			runtime.GC()
			var retained runtime.MemStats
			runtime.ReadMemStats(&retained)

			if len(latencies) != chunkCount {
				b.Fatalf("captured %d chunk timings, want %d", len(latencies), chunkCount)
			}
			if firstStreamSetRows != 0 || firstStreamSameRows != 0 {
				b.Fatalf("per-chunk app path rescanned/replaced rows: SetRows=%d sameRows=%d", firstStreamSetRows, firstStreamSameRows)
			}
			if got := app.model.transcriptViewport.updateRowCalls; got < chunkCount-1 {
				b.Fatalf("per-chunk app path did not update the live row: updates=%d", got)
			}
			sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })

			b.ReportAllocs()
			b.SetBytes(chunkCount * chunkBytes)
			b.ResetTimer()
			for operation := 0; operation < b.N; operation++ {
				benchmarkApp := newBenchmarkBubbleteaApp(b, historyRows)
				for chunkIndex := 0; chunkIndex < chunkCount; chunkIndex++ {
					benchmarkApp.model.Update(agentEventMsg{event: AgentEvent{
						Type:                     "message",
						ContentType:              ContentTypeAssistant,
						Content:                  chunk,
						Incremental:              true,
						RunID:                    "benchmark-run",
						CanonicalTranscriptIndex: &index,
					}})
					_ = benchmarkApp.View()
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(latencies[(len(latencies)-1)/2].Microseconds()), "p50_us/chunk")
			b.ReportMetric(float64(latencies[(len(latencies)*95/100)-1].Microseconds()), "p95_us/chunk")
			b.ReportMetric(float64(firstStreamAllocBytes)/chunkCount, "alloc_B/chunk")
			b.ReportMetric(float64(loadedHeap.HeapAlloc), "loaded_heap_B")
			b.ReportMetric(float64(retained.HeapAlloc), "retained_heap_B")
		})
	}
}

func newBenchmarkBubbleteaApp(b *testing.B, historyRows int) *BubbleteaApp {
	b.Helper()
	app, err := NewBubbleteaAppWithTranscript(nil, benchmarkTranscriptSnapshot(historyRows), WithWidth(80))
	if err != nil {
		b.Fatal(err)
	}
	app.model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	_ = app.View()
	return app
}

func benchmarkTranscriptSnapshot(rowCount int) CanonicalTranscriptSnapshot {
	snapshot := CanonicalTranscriptSnapshot{
		PhysicalSessionID: "benchmark-session",
		BranchID:          "benchmark-branch",
		Revision:          "benchmark-revision",
		Rows:              make([]CanonicalTranscriptRow, 0, rowCount),
	}
	for index := 0; index < rowCount; index++ {
		snapshot.Rows = append(snapshot.Rows, CanonicalTranscriptRow{
			Key: DurableTranscriptKey{
				PhysicalSessionID: snapshot.PhysicalSessionID,
				BranchID:          snapshot.BranchID,
				EntryID:           fmt.Sprintf("entry-%05d", index),
			},
			Kind:      TranscriptRowAssistant,
			Message:   core.AgentMessage{Role: "assistant", Content: fmt.Sprintf("loaded canonical row %d", index)},
			SourceSeq: uint64(index + 1),
		})
	}
	return snapshot
}
