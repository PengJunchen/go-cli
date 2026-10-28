package memory_test

import (
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/memory"
)

type controlledExtractor struct {
	calls        atomic.Int32
	started      chan struct{}
	release      chan struct{}
	empty        bool
	ignoreCancel bool
	captured     chan []llm.Message
}

func (e *controlledExtractor) Extract(ctx context.Context, messages []llm.Message) ([]memory.Memory, error) {
	e.calls.Add(1)
	if e.captured != nil {
		e.captured <- messages
	}
	if e.started != nil {
		select {
		case e.started <- struct{}{}:
		default:
		}
	}
	if e.release != nil {
		if e.ignoreCancel {
			<-e.release
		} else {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-e.release:
			}
		}
	}
	if e.empty {
		return nil, nil
	}
	return []memory.Memory{{Content: "Use focused tests", Category: "preference"}}, nil
}

func TestMemoryWorkerBoundsTextAndExcludesToolsAndSystem(t *testing.T) {
	store, err := memory.NewFileMemoryStore(filepath.Join(t.TempDir(), "memory.jsonl"))
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	extractor := &controlledExtractor{captured: make(chan []llm.Message, 1), empty: true}
	worker := memory.NewWorker(t.Context(), store, extractor, memory.WorkerOptions{})
	defer worker.Stop()
	job := memoryJob("large")
	job.Messages = []llm.Message{{Role: llm.RoleSystem, Content: "private system"}, {Role: llm.RoleUser, Content: strings.Repeat("u", 20000)}, {Role: llm.RoleTool, Content: "private tool"}, {Role: llm.RoleAssistant, Content: strings.Repeat("a", 20000)}}
	require.True(t, worker.Submit(job))
	require.NoError(t, worker.Drain(t.Context()))
	messages := <-extractor.captured
	require.Len(t, messages, 2)
	require.Equal(t, llm.RoleUser, messages[0].Role)
	require.Equal(t, llm.RoleAssistant, messages[1].Role)
	require.LessOrEqual(t, len(messages[0].Content)+len(messages[1].Content), 16<<10)
	job.Source.SourceIDs = make([]string, 257)
	require.False(t, worker.Submit(job))
}

func TestMemoryWorkerKeepsOnlyOnePendingSnapshot(t *testing.T) {
	store, err := memory.NewFileMemoryStore(filepath.Join(t.TempDir(), "memory.jsonl"))
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	extractor := &controlledExtractor{started: make(chan struct{}, 1), release: make(chan struct{})}
	worker := memory.NewWorker(t.Context(), store, extractor, memory.WorkerOptions{})
	defer worker.Stop()
	require.True(t, worker.Submit(memoryJob("one")))
	<-extractor.started
	require.True(t, worker.Submit(memoryJob("two")))
	require.True(t, worker.Submit(memoryJob("three")))
	close(extractor.release)
	require.NoError(t, worker.Drain(t.Context()))
	require.Equal(t, int32(2), extractor.calls.Load())
	for _, testCase := range []struct {
		id   string
		want bool
	}{{"one", true}, {"two", false}, {"three", true}} {
		processed, err := store.Processed(t.Context(), memoryJob(testCase.id).Source)
		require.NoError(t, err)
		require.Equal(t, testCase.want, processed)
	}
}

func TestMemoryWorkerDisableRevokesInFlightAndCanReenable(t *testing.T) {
	store, err := memory.NewFileMemoryStore(filepath.Join(t.TempDir(), "memory.jsonl"))
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	extractor := &controlledExtractor{started: make(chan struct{}, 1), release: make(chan struct{})}
	worker := memory.NewWorker(t.Context(), store, extractor, memory.WorkerOptions{})
	defer worker.Stop()
	require.True(t, worker.Submit(memoryJob("one")))
	<-extractor.started
	worker.SetEnabled(false)
	require.NoError(t, worker.Drain(t.Context()))
	processed, err := store.Processed(t.Context(), memoryJob("one").Source)
	require.NoError(t, err)
	require.False(t, processed)
	require.False(t, worker.Submit(memoryJob("disabled")))
	worker.SetEnabled(true)
	close(extractor.release)
	require.True(t, worker.Submit(memoryJob("new")))
	require.NoError(t, worker.Drain(t.Context()))
	processed, err = store.Processed(t.Context(), memoryJob("new").Source)
	require.NoError(t, err)
	require.True(t, processed)
}

func TestMemoryWorkerChangedPlanEpochRevokesPublication(t *testing.T) {
	store, err := memory.NewFileMemoryStore(filepath.Join(t.TempDir(), "memory.jsonl"))
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	var epoch atomic.Uint64
	extractor := &controlledExtractor{started: make(chan struct{}, 1), release: make(chan struct{})}
	worker := memory.NewWorker(t.Context(), store, extractor, memory.WorkerOptions{})
	defer worker.Stop()
	job := memoryJob("plan-source")
	job.Eligible = func() bool { return epoch.Load() == 0 }
	require.True(t, worker.Submit(job))
	<-extractor.started
	epoch.Add(2)
	close(extractor.release)
	require.NoError(t, worker.Drain(t.Context()))
	items, err := store.List(t.Context())
	require.NoError(t, err)
	require.Empty(t, items)
	processed, err := store.Processed(t.Context(), job.Source)
	require.NoError(t, err)
	require.False(t, processed)
}

func TestMemoryWorkerRetainsOwnershipUntilIgnoringExtractorJoins(t *testing.T) {
	store, err := memory.NewFileMemoryStore(filepath.Join(t.TempDir(), "memory.jsonl"))
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	extractor := &controlledExtractor{started: make(chan struct{}, 1), release: make(chan struct{}), ignoreCancel: true}
	worker := memory.NewWorker(t.Context(), store, extractor, memory.WorkerOptions{})
	require.True(t, worker.Submit(memoryJob("one")))
	<-extractor.started
	worker.Stop()
	select {
	case <-worker.Done():
		t.Fatal("ignoring extractor was reported joined")
	default:
	}
	close(extractor.release)
	select {
	case <-worker.Done():
	case <-time.After(time.Second):
		t.Fatal("worker did not join")
	}
	items, err := store.List(t.Context())
	require.NoError(t, err)
	require.Empty(t, items)
}

func memoryJob(id string) memory.ExtractionJob {
	return memory.ExtractionJob{Source: memory.Provenance{WorkspaceID: "workspace", SessionID: "session", SourceIDs: []string{id}}, Messages: []llm.Message{{Role: llm.RoleUser, Content: "remember focused tests"}, {Role: llm.RoleAssistant, Content: "understood"}}}
}

func TestMemoryWorkerPersistsZeroFactProcessedMarker(t *testing.T) {
	store, err := memory.NewFileMemoryStore(filepath.Join(t.TempDir(), "memory.jsonl"))
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	extractor := &controlledExtractor{empty: true}
	worker := memory.NewWorker(t.Context(), store, extractor, memory.WorkerOptions{})
	defer worker.Stop()
	require.True(t, worker.Submit(memoryJob("one")))
	require.NoError(t, worker.Drain(t.Context()))
	require.True(t, worker.Submit(memoryJob("one")))
	require.NoError(t, worker.Drain(t.Context()))
	require.Equal(t, int32(1), extractor.calls.Load())
	items, err := store.List(t.Context())
	require.NoError(t, err)
	require.Empty(t, items)
	processed, err := store.Processed(t.Context(), memoryJob("one").Source)
	require.NoError(t, err)
	require.True(t, processed)
}

func TestMemoryWorkerStopCancelsAndPreventsLatePublication(t *testing.T) {
	store, err := memory.NewFileMemoryStore(filepath.Join(t.TempDir(), "memory.jsonl"))
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	extractor := &controlledExtractor{started: make(chan struct{}, 1), release: make(chan struct{})}
	worker := memory.NewWorker(t.Context(), store, extractor, memory.WorkerOptions{})
	require.True(t, worker.Submit(memoryJob("one")))
	select {
	case <-extractor.started:
	case <-time.After(time.Second):
		t.Fatal("extraction did not start")
	}
	worker.Stop()
	select {
	case <-worker.Done():
	case <-time.After(time.Second):
		t.Fatal("extraction did not join")
	}
	require.False(t, worker.Submit(memoryJob("late")))
	items, err := store.List(t.Context())
	require.NoError(t, err)
	require.Empty(t, items)
	processed, err := store.Processed(t.Context(), memoryJob("one").Source)
	require.NoError(t, err)
	require.False(t, processed)
}
