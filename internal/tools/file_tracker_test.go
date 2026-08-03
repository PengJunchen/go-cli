package tools

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFileTrackerTrackNewFile(t *testing.T) {
	ft := NewFileTracker()
	ft.Track("/a.txt", "hello")

	changes := ft.GetChanges()
	require.Len(t, changes, 1)
	assert.Equal(t, "/a.txt", changes[0].Path)
	assert.Empty(t, changes[0].OldHash)
	assert.NotEmpty(t, changes[0].NewHash)
	assert.False(t, changes[0].Timestamp.IsZero())
}

func TestFileTrackerTrackUnchanged(t *testing.T) {
	ft := NewFileTracker()
	ft.Track("/a.txt", "hello")
	ft.Track("/a.txt", "hello") // same content

	changes := ft.GetChanges()
	assert.Len(t, changes, 1, "no new change for identical content")
}

func TestFileTrackerTrackChanged(t *testing.T) {
	ft := NewFileTracker()
	ft.Track("/a.txt", "hello")
	ft.Track("/a.txt", "world")

	changes := ft.GetChanges()
	require.Len(t, changes, 2)
	assert.Equal(t, "/a.txt", changes[1].Path)
	assert.Equal(t, changes[0].NewHash, changes[1].OldHash)
	assert.NotEqual(t, changes[1].OldHash, changes[1].NewHash)
}

func TestFileTrackerHasChanged(t *testing.T) {
	ft := NewFileTracker()
	ft.Track("/a.txt", "hello")
	ft.Track("/b.txt", "world")

	assert.True(t, ft.HasChanged("/a.txt"))
	assert.True(t, ft.HasChanged("/b.txt"))
	assert.False(t, ft.HasChanged("/c.txt"))
}

func TestFileTrackerHasChangedAfterModification(t *testing.T) {
	ft := NewFileTracker()
	ft.Track("/a.txt", "hello")
	ft.Track("/a.txt", "world")

	assert.True(t, ft.HasChanged("/a.txt"))
}

func TestFileTrackerHasChangedFalseForUntracked(t *testing.T) {
	ft := NewFileTracker()
	assert.False(t, ft.HasChanged("/never.txt"))
}

func TestFileTrackerReset(t *testing.T) {
	ft := NewFileTracker()
	ft.Track("/a.txt", "hello")
	ft.Track("/b.txt", "world")

	ft.Reset()

	assert.Empty(t, ft.GetChanges())
	assert.False(t, ft.HasChanged("/a.txt"))
	assert.False(t, ft.HasChanged("/b.txt"))
}

func TestFileTrackerResetAllowsRetracking(t *testing.T) {
	ft := NewFileTracker()
	ft.Track("/a.txt", "hello")
	ft.Reset()
	ft.Track("/a.txt", "hello")

	changes := ft.GetChanges()
	require.Len(t, changes, 1)
	assert.Empty(t, changes[0].OldHash, "after reset, old hash should be empty")
}

func TestFileTrackerGetChangesReturnsCopy(t *testing.T) {
	ft := NewFileTracker()
	ft.Track("/a.txt", "hello")

	changes := ft.GetChanges()
	changes[0].Path = "/mutated"

	original := ft.GetChanges()
	assert.Equal(t, "/a.txt", original[0].Path, "internal slice is not affected by external mutation")
}

func TestFileTrackerMultipleFiles(t *testing.T) {
	ft := NewFileTracker()
	ft.Track("/a.txt", "content-a")
	ft.Track("/b.txt", "content-b")
	ft.Track("/c.txt", "content-c")

	changes := ft.GetChanges()
	require.Len(t, changes, 3)
	assert.Equal(t, "/a.txt", changes[0].Path)
	assert.Equal(t, "/b.txt", changes[1].Path)
	assert.Equal(t, "/c.txt", changes[2].Path)
}

func TestFileTrackerConcurrent(t *testing.T) {
	ft := NewFileTracker()
	var wg sync.WaitGroup

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			ft.Track("/concurrent.txt", "data")
			_ = ft.HasChanged("/concurrent.txt")
			_ = ft.GetChanges()
		}(i)
	}
	wg.Wait()

	// The first Track records a change; subsequent identical Tracks do not.
	assert.True(t, ft.HasChanged("/concurrent.txt"))
}
