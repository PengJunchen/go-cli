package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"strings"
	"sync"
)

const (
	directoryCaptureMaxEntries     = 1000
	directoryCaptureMaxOutputBytes = 1 << 20
	directoryCaptureMaxEntryBytes  = 4 << 10
	directoryReadBatchSize         = 128
)

var errDirectoryCaptureLimit = errors.New("tools: directory capture limit exceeded")

type directoryCaptureLimitError struct {
	reason string
}

func (e *directoryCaptureLimitError) Error() string {
	if e == nil || e.reason == "" {
		return errDirectoryCaptureLimit.Error()
	}
	return fmt.Sprintf("%s: %s", errDirectoryCaptureLimit, e.reason)
}

func (e *directoryCaptureLimitError) Unwrap() error { return errDirectoryCaptureLimit }

func newDirectoryCaptureLimitError(reason string) error {
	return &directoryCaptureLimitError{reason: reason}
}

// boundedTopK retains only the lexically best limit items under less. Its heap
// root is the worst retained item, so each additional candidate costs O(log K).
type boundedTopK[T any] struct {
	limit        int
	maxKeyBytes  int
	key          func(T) string
	less         func(T, T) bool
	items        []T
	retainedKeys int
	truncated    bool
}

func newBoundedTopK[T any](limit, maxKeyBytes int, key func(T) string, less func(T, T) bool) *boundedTopK[T] {
	if limit < 0 {
		limit = 0
	}
	return &boundedTopK[T]{
		limit:       limit,
		maxKeyBytes: maxKeyBytes,
		key:         key,
		less:        less,
		items:       make([]T, 0, limit),
	}
}

func (b *boundedTopK[T]) Add(item T) error {
	key := b.key(item)
	if len(key) > b.maxKeyBytes {
		return newDirectoryCaptureLimitError("candidate path exceeds the configured byte limit")
	}
	if b.limit == 0 {
		b.truncated = true
		return nil
	}
	if len(b.items) < b.limit {
		b.items = append(b.items, item)
		b.retainedKeys += len(key)
		b.siftUp(len(b.items) - 1)
		return nil
	}
	b.truncated = true
	if !b.less(item, b.items[0]) {
		return nil
	}
	b.retainedKeys -= len(b.key(b.items[0]))
	b.retainedKeys += len(key)
	b.items[0] = item
	b.siftDown(0)
	return nil
}

func (b *boundedTopK[T]) siftUp(index int) {
	for index > 0 {
		parent := (index - 1) / 2
		if !b.less(b.items[parent], b.items[index]) {
			return
		}
		b.items[parent], b.items[index] = b.items[index], b.items[parent]
		index = parent
	}
}

func (b *boundedTopK[T]) siftDown(index int) {
	for {
		left := 2*index + 1
		if left >= len(b.items) {
			return
		}
		worst := left
		right := left + 1
		if right < len(b.items) && b.less(b.items[left], b.items[right]) {
			worst = right
		}
		if !b.less(b.items[index], b.items[worst]) {
			return
		}
		b.items[index], b.items[worst] = b.items[worst], b.items[index]
		index = worst
	}
}

func (b *boundedTopK[T]) Sorted() []T {
	items := append([]T(nil), b.items...)
	sort.Slice(items, func(i, j int) bool { return b.less(items[i], items[j]) })
	return items
}

func (b *boundedTopK[T]) Truncated() bool { return b.truncated }

func (b *boundedTopK[T]) RetainedKeyBytes() int { return b.retainedKeys }

type boundedDirectoryOutput struct {
	maxBytes  int
	builder   strings.Builder
	records   int
	truncated bool
}

func newBoundedDirectoryOutput(maxBytes int) *boundedDirectoryOutput {
	if maxBytes < 0 {
		maxBytes = 0
	}
	return &boundedDirectoryOutput{maxBytes: maxBytes}
}

// AppendLine adds a complete newline-terminated record or rejects it whole.
// Once a record does not fit, later records are also omitted to preserve the
// sorted-prefix contract of the caller.
func (o *boundedDirectoryOutput) AppendLine(line string) bool {
	if o.truncated || len(line)+1 > o.maxBytes-o.builder.Len() {
		o.truncated = true
		return false
	}
	o.builder.WriteString(line)
	o.builder.WriteByte('\n')
	o.records++
	return true
}

func (o *boundedDirectoryOutput) String() string {
	return strings.TrimSuffix(o.builder.String(), "\n")
}

func (o *boundedDirectoryOutput) Records() int { return o.records }

func (o *boundedDirectoryOutput) Truncated() bool { return o.truncated }

type directoryBatchReader interface {
	ReadDir(n int) ([]fs.DirEntry, error)
}

func scanDirectoryBatches(ctx context.Context, reader directoryBatchReader, batchSize int, visit func(fs.DirEntry) error) error {
	if batchSize <= 0 {
		return errors.New("tools: directory read batch size must be positive")
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, readErr := reader.ReadDir(batchSize)
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := visit(entry); err != nil {
				return err
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
		if len(entries) == 0 {
			return io.ErrNoProgress
		}
	}
}

type findPathCaptureWriter struct {
	mu           sync.Mutex
	candidates   *boundedTopK[string]
	line         []byte
	maxLineBytes int
	finished     bool
	captureErr   error
}

func newFindPathCaptureWriter(maxEntries, maxLineBytes int) *findPathCaptureWriter {
	if maxLineBytes < 0 {
		maxLineBytes = 0
	}
	return &findPathCaptureWriter{
		candidates:   newBoundedTopK[string](maxEntries, maxLineBytes, func(path string) string { return path }, func(a, b string) bool { return a < b }),
		maxLineBytes: maxLineBytes,
	}
}

func (w *findPathCaptureWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.finished {
		return 0, io.ErrClosedPipe
	}
	if w.captureErr != nil {
		return len(p), nil
	}
	for _, value := range p {
		if value == '\n' {
			w.captureLineLocked()
			if w.captureErr != nil {
				return len(p), nil
			}
			w.line = w.line[:0]
			continue
		}
		if len(w.line) == w.maxLineBytes {
			w.captureErr = newDirectoryCaptureLimitError("helper output path exceeds the configured byte limit")
			w.line = w.line[:0]
			return len(p), nil
		}
		w.line = append(w.line, value)
	}
	return len(p), nil
}

func (w *findPathCaptureWriter) captureLineLocked() {
	line := string(w.line)
	if strings.TrimSpace(line) == "" {
		return
	}
	if err := w.candidates.Add(line); err != nil {
		w.captureErr = err
	}
}

// Finish processes a final non-newline-terminated path. It must be called only
// after the managed command has joined the stdout reader.
func (w *findPathCaptureWriter) Finish() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.finished {
		if w.captureErr == nil && len(w.line) > 0 {
			w.captureLineLocked()
		}
		w.finished = true
	}
	return w.captureErr
}

func (w *findPathCaptureWriter) CaptureError() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.captureErr
}

func (w *findPathCaptureWriter) Results() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.candidates.Sorted()
}

func (w *findPathCaptureWriter) Truncated() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.candidates.Truncated()
}
