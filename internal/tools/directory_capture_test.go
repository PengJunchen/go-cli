package tools

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBoundedTopKRetainsSortedPrefixAndCapsRetainedKeys(t *testing.T) {
	acc := newBoundedTopK[string](3, 8, func(value string) string { return value }, func(a, b string) bool { return a < b })
	for _, value := range []string{"zeta", "delta", "alpha", "bravo", "charlie"} {
		require.NoError(t, acc.Add(value))
	}
	assert.Equal(t, []string{"alpha", "bravo", "charlie"}, acc.Sorted())
	assert.True(t, acc.Truncated())
	assert.LessOrEqual(t, acc.RetainedKeyBytes(), 3*8)
}

func TestBoundedTopKUsesExplicitTieBreakAndRejectsOversizedKey(t *testing.T) {
	type candidate struct {
		score int
		name  string
	}
	less := func(a, b candidate) bool {
		if a.score != b.score {
			return a.score > b.score
		}
		return a.name < b.name
	}
	acc := newBoundedTopK[candidate](3, 8, func(value candidate) string { return value.name }, less)
	for _, value := range []candidate{{score: 1, name: "z"}, {score: 2, name: "b"}, {score: 2, name: "a"}, {score: 2, name: "c"}} {
		require.NoError(t, acc.Add(value))
	}
	got := acc.Sorted()
	assert.Equal(t, []candidate{{score: 2, name: "a"}, {score: 2, name: "b"}, {score: 2, name: "c"}}, got)
	assert.True(t, acc.Truncated())
	err := acc.Add(candidate{score: 3, name: strings.Repeat("x", 9)})
	assert.ErrorIs(t, err, errDirectoryCaptureLimit)
}

func TestBoundedDirectoryOutputKeepsWholeRecords(t *testing.T) {
	output := newBoundedDirectoryOutput(7)
	assert.True(t, output.AppendLine("one"))
	assert.False(t, output.AppendLine("two"))
	assert.Equal(t, "one", output.String())
	assert.Equal(t, 1, output.Records())
	assert.True(t, output.Truncated())
}

func TestScanDirectoryBatchesBoundsBatchAndPropagatesCancellation(t *testing.T) {
	entries := make([]fs.DirEntry, 300)
	for i := range entries {
		entries[i] = syntheticCaptureDirEntry{name: "entry"}
	}
	reader := &syntheticCaptureDirReader{entries: entries}
	visited := 0
	require.NoError(t, scanDirectoryBatches(context.Background(), reader, 128, func(fs.DirEntry) error { visited++; return nil }))
	assert.Equal(t, len(entries), visited)
	assert.LessOrEqual(t, reader.maxReturned, 128)
	assert.Equal(t, 3, reader.calls)

	ctx, cancel := context.WithCancel(context.Background())
	reader = &syntheticCaptureDirReader{entries: entries}
	err := scanDirectoryBatches(ctx, reader, 128, func(fs.DirEntry) error { cancel(); return nil })
	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, reader.calls)

	readErr := errors.New("root read failed")
	err = scanDirectoryBatches(context.Background(), failingCaptureDirReader{err: readErr}, 128, func(fs.DirEntry) error { return nil })
	assert.ErrorIs(t, err, readErr)
}

func TestScanDirectoryBatchesDoesNotSwallowCancellationDuringRead(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader := &blockingCaptureDirReader{entered: entered, release: release}
	done := make(chan error, 1)
	go func() {
		done <- scanDirectoryBatches(ctx, reader, 128, func(fs.DirEntry) error { return nil })
	}()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("ReadDir was not entered")
	}
	cancel()
	close(release)

	select {
	case err := <-done:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("directory scan did not return after ReadDir completed")
	}
}

func TestFindPathCaptureWriterDrainsAndReportsCaptureLimit(t *testing.T) {
	writer := newFindPathCaptureWriter(10, 4)
	chunk := []byte("ok\x0012345\x00ignored\x00")
	n, err := writer.Write(chunk)
	require.NoError(t, err)
	assert.Equal(t, len(chunk), n, "capture errors must not stop pipe draining")
	assert.ErrorIs(t, writer.Finish(), errDirectoryCaptureLimit)
	assert.Equal(t, []string{"ok"}, writer.Results())
	assert.Empty(t, writer.line, "the rest of the same read chunk must not be retained after the limit")
}

func TestFindPathCaptureWriterUsesNULRecordsAndPreservesRawNames(t *testing.T) {
	writer := newFindPathCaptureWriter(4, 32)
	payload := []byte("line\nbreak\x00   \x00")
	payload = append(payload, 0xff, 0, 0)
	n, err := writer.Write(payload)
	require.NoError(t, err)
	assert.Equal(t, len(payload), n)
	require.NoError(t, writer.Finish())

	assert.Equal(t, []string{"   ", "line\nbreak", string([]byte{0xff})}, writer.Results())
}

func TestFindPathCaptureWriterFinishesUnterminatedLine(t *testing.T) {
	writer := newFindPathCaptureWriter(2, 8)
	_, err := writer.Write([]byte("path"))
	require.NoError(t, err)
	require.NoError(t, writer.Finish())
	assert.Equal(t, []string{"path"}, writer.Results())
}

func TestFindPathCaptureWriterKeepsSortedTopKAcrossChunks(t *testing.T) {
	writer := newFindPathCaptureWriter(2, 16)
	for _, chunk := range []string{"z\x00b", "\x00a\x00"} {
		n, err := writer.Write([]byte(chunk))
		require.NoError(t, err)
		assert.Equal(t, len(chunk), n)
	}
	require.NoError(t, writer.Finish())
	assert.Equal(t, []string{"a", "b"}, writer.Results())
	assert.True(t, writer.Truncated())
}

func TestFindPathCaptureWriterNormalizesDirectoryRecordsBeforeTopK(t *testing.T) {
	writer := newFindPathCaptureWriter(3, 64)
	writer.setDirectoryRoot("nested")
	payload := []byte("nested/z.txt\x00./nested/a.txt\x00nested\x00")
	n, err := writer.Write(payload)
	require.NoError(t, err)
	assert.Equal(t, len(payload), n)
	require.NoError(t, writer.Finish())
	assert.Equal(t, []string{"a.txt", "z.txt"}, writer.Results())
	assert.False(t, writer.Truncated())
}

func TestFindPathCaptureWriterNormalizesParentRelativeRoot(t *testing.T) {
	writer := newFindPathCaptureWriter(2, 64)
	writer.setDirectoryRoot("..")
	payload := []byte("../child.txt\x00")
	n, err := writer.Write(payload)
	require.NoError(t, err)
	assert.Equal(t, len(payload), n)
	require.NoError(t, writer.Finish())
	assert.Equal(t, []string{"child.txt"}, writer.Results())
}

func TestFindPathCaptureWriterRejectsPathsOutsideDirectoryRootAndKeepsDraining(t *testing.T) {
	writer := newFindPathCaptureWriter(3, 64)
	writer.setDirectoryRoot(filepath.Join(string(filepath.Separator), "workspace"))
	payload := []byte("/workspace/../outside.txt\x00later.txt\x00")
	n, err := writer.Write(payload)
	require.NoError(t, err)
	assert.Equal(t, len(payload), n)
	assert.ErrorIs(t, writer.Finish(), errDirectoryCaptureLimit)
	assert.Empty(t, writer.Results())
}

type syntheticCaptureDirReader struct {
	entries     []fs.DirEntry
	index       int
	calls       int
	maxReturned int
}

type failingCaptureDirReader struct{ err error }

func (r failingCaptureDirReader) ReadDir(int) ([]fs.DirEntry, error) { return nil, r.err }

func (r *syntheticCaptureDirReader) ReadDir(n int) ([]fs.DirEntry, error) {
	r.calls++
	if r.index >= len(r.entries) {
		return nil, io.EOF
	}
	end := r.index + n
	if end > len(r.entries) {
		end = len(r.entries)
	}
	result := append([]fs.DirEntry(nil), r.entries[r.index:end]...)
	r.index = end
	if len(result) > r.maxReturned {
		r.maxReturned = len(result)
	}
	if r.index == len(r.entries) {
		return result, io.EOF
	}
	return result, nil
}

type blockingCaptureDirReader struct {
	entered chan<- struct{}
	release <-chan struct{}
	once    sync.Once
}

func (r *blockingCaptureDirReader) ReadDir(int) ([]fs.DirEntry, error) {
	r.once.Do(func() { close(r.entered) })
	<-r.release
	return nil, io.EOF
}

type syntheticCaptureDirEntry struct{ name string }

func (e syntheticCaptureDirEntry) Name() string             { return e.name }
func (syntheticCaptureDirEntry) IsDir() bool                { return false }
func (syntheticCaptureDirEntry) Type() fs.FileMode          { return 0 }
func (syntheticCaptureDirEntry) Info() (fs.FileInfo, error) { return nil, errors.New("unused") }
