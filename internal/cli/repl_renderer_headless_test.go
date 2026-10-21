//go:build headless

package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/tui"
)

func headlessTestApp(events <-chan tui.AgentEvent, durable bool) *replApp {
	return &replApp{
		events: events, durable: durable,
		ready: make(chan struct{}), done: make(chan struct{}),
		sourceDone: make(chan struct{}), quit: make(chan struct{}),
	}
}

func awaitHeadless(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("headless consumer did not join")
	}
}

func TestHeadlessDrainsLargeOutputWithoutRetainingChunks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan tui.AgentEvent, 4)
	app := headlessTestApp(events, false)
	go func() { _ = app.Run(ctx) }()
	// A bounded channel and large repeated chunks exercise backpressure.
	chunk := strings.Repeat("x", 64<<10)
	producerDone := make(chan struct{})
	go func() {
		defer close(producerDone)
		defer close(events)
		for i := 0; i < 1024; i++ {
			select {
			case events <- tui.AgentEvent{Content: chunk, Incremental: true}:
			case <-ctx.Done():
				return
			}
		}
	}()
	awaitHeadless(t, producerDone)
	awaitHeadless(t, app.EventSourceDone())
	awaitHeadless(t, app.Done())
	if app.snapshot != nil || len(app.todo.Items) != 0 {
		t.Fatal("stream output was retained as durable/control state")
	}
}

func TestHeadlessDurableRefreshChecksIdentityAndClonesBeforeACK(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan tui.AgentEvent)
	app := headlessTestApp(events, true)
	initial := tui.CanonicalTranscriptSnapshot{PhysicalSessionID: "session", BranchID: "branch"}
	app.snapshot = &initial
	go func() { _ = app.Run(ctx) }()
	close(events)
	awaitHeadless(t, app.EventSourceDone())
	select {
	case <-app.Done():
		t.Fatal("durable consumer exited before final refresh")
	default:
	}
	refresh := tui.TranscriptRefresh{
		Snapshot: tui.CanonicalTranscriptSnapshot{
			PhysicalSessionID: "session", BranchID: "branch", Revision: "new",
			Rows: []tui.CanonicalTranscriptRow{{
				Key:  tui.DurableTranscriptKey{PhysicalSessionID: "session", BranchID: "branch", EntryID: "entry"},
				Kind: tui.TranscriptRowAssistant, Message: core.AgentMessage{Role: "assistant", Content: "done"},
			}},
		},
		Publication: core.DurablePublication{State: core.DurablePublicationUnbound},
	}
	wrong := refresh
	wrong.Snapshot.BranchID = "other"
	if err := app.ApplyTranscriptRefresh(ctx, wrong); err == nil {
		t.Fatal("accepted another branch")
	}
	if err := app.ApplyTranscriptRefresh(ctx, refresh); err != nil {
		t.Fatal(err)
	}
	refresh.Snapshot.Rows[0].Message.Content = "mutated"
	if app.snapshot.Rows[0].Message.Content != "done" {
		t.Fatal("ACK preceded an independent installed snapshot")
	}
	app.Quit()
	app.Quit()
	awaitHeadless(t, app.Done())
	if err := app.ApplyTranscriptRefresh(ctx, refresh); !errors.Is(err, tui.ErrTranscriptRefreshClosed) {
		t.Fatalf("refresh after close = %v", err)
	}
}

func TestHeadlessCancellationJoinsIdleConsumer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	app := headlessTestApp(make(chan tui.AgentEvent), true)
	go func() { _ = app.Run(ctx) }()
	cancel()
	awaitHeadless(t, app.Done())
	awaitHeadless(t, app.EventSourceDone())
}

// The work sizes match BenchmarkBubbleTeaTranscriptUpdateView: fixed history
// rows plus 1,000 x 1 KiB events. Full rendering and a plain drain are different
// presentation features; this measures their costs, not UX equivalence.
func BenchmarkHeadlessTranscriptStream(b *testing.B) {
	for _, historyRows := range []int{1000, 5000, 10000} {
		b.Run(fmt.Sprintf("history-%d", historyRows), func(b *testing.B) {
			base := tui.CanonicalTranscriptSnapshot{PhysicalSessionID: "s", BranchID: "b", Revision: "r"}
			for i := 0; i < historyRows; i++ {
				base.Rows = append(base.Rows, tui.CanonicalTranscriptRow{
					Key:  tui.DurableTranscriptKey{PhysicalSessionID: "s", BranchID: "b", EntryID: fmt.Sprint(i)},
					Kind: tui.TranscriptRowAssistant, Message: core.AgentMessage{Role: "assistant", Content: "history"},
				})
			}
			chunk := strings.Repeat("x", 1024)
			b.ReportAllocs()
			b.SetBytes(1000 * 1024)
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				events := make(chan tui.AgentEvent, 64)
				app := headlessTestApp(events, false)
				snapshot := base.Clone()
				app.snapshot = &snapshot
				go func() { _ = app.Run(context.Background()) }()
				for chunkIndex := 0; chunkIndex < 1000; chunkIndex++ {
					events <- tui.AgentEvent{Type: "message", Content: chunk, Incremental: true}
				}
				close(events)
				<-app.Done()
			}
		})
	}
}
