package core

import (
	"fmt"
	"testing"
)

// BenchmarkEventStreamPushPop measures Send + drain throughput across
// different buffer capacities. Each iteration creates a stream, fills it
// to capacity, closes it, and drains all events.
func BenchmarkEventStreamPushPop(b *testing.B) {
	for _, bufSize := range []int{10, 100, 1000} {
		b.Run(fmt.Sprintf("buf_%d", bufSize), func(b *testing.B) {
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				stream := NewEventStream(bufSize, WithEventDiscardPolicy(DiscardOldest))
				for j := 0; j < bufSize; j++ {
					_ = stream.Send(AgentEvent{
						Kind:    "message",
						Content: "bench-event",
					})
				}
				stream.Close()
				for range stream.Events() {
				}
			}
		})
	}
}
