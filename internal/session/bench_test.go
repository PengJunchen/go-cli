package session

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// BenchmarkBuildContext measures context reconstruction from a session tree
// across different branch depths. Each branch is a linear chain of
// user/assistant entries with no compaction, so BuildContext walks the full
// chain and estimates tokens for every entry.
func BenchmarkBuildContext(b *testing.B) {
	ctx := context.Background()

	for _, depth := range []int{10, 100, 500, 1000} {
		b.Run(fmt.Sprintf("depth_%d", depth), func(b *testing.B) {
			tree := NewDefaultSessionTree()
			var prevID string
			for i := 0; i < depth; i++ {
				id := fmt.Sprintf("e-%d", i)
				entry := &SessionEntry{
					ID:        id,
					ParentID:  prevID,
					Type:      EntryTypeUser,
					Content:   fmt.Sprintf("message content for entry %d", i),
					Timestamp: time.Now(),
				}
				if i%2 == 1 {
					entry.Type = EntryTypeAssistant
				}
				if err := tree.Append(ctx, entry); err != nil {
					b.Fatalf("append entry %d: %v", i, err)
				}
				prevID = id
			}
			leafID := fmt.Sprintf("e-%d", depth-1)
			mgr := NewDefaultContextManager(tree)

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, _ = mgr.BuildContext(ctx, leafID)
			}
		})
	}
}

// BenchmarkJSONLAppend measures appending entries to a file-backed JSONL
// store across different entry counts. File creation and cleanup are excluded
// from the timed region.
func BenchmarkJSONLAppend(b *testing.B) {
	ctx := context.Background()

	for _, n := range []int{10, 100, 1000} {
		b.Run(fmt.Sprintf("entries_%d", n), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				f, err := os.CreateTemp("", "bench-jsonl-*.jsonl")
				if err != nil {
					b.Fatal(err)
				}
				path := f.Name()
				f.Close()
				os.Remove(path)

				store := NewJSONLSessionStore(path)
				b.StartTimer()

				var parentID string
				for j := 0; j < n; j++ {
					id := fmt.Sprintf("e-%d", j)
					if err := store.Append(ctx, &SessionEntry{
						ID:        id,
						ParentID:  parentID,
						Type:      EntryTypeUser,
						Content:   fmt.Sprintf("benchmark content for entry %d", j),
						Timestamp: time.Now(),
					}); err != nil {
						b.Fatalf("append entry %d: %v", j, err)
					}
					parentID = id
				}

				b.StopTimer()
				_ = store.Close()
				_ = os.Remove(path)
				b.StartTimer()
			}
		})
	}
}
