package tools

import (
	"errors"
	"sync"
)

type processSessionBuffer struct {
	mu       sync.Mutex
	capacity int
	data     []byte
	start    int64
	total    int64
}

func newProcessSessionBuffer(capacity int) *processSessionBuffer {
	return &processSessionBuffer{capacity: capacity}
}

func (b *processSessionBuffer) writer() *processSessionWriter {
	return &processSessionWriter{buffer: b}
}

func (b *processSessionBuffer) append(source []byte) {
	if len(source) == 0 {
		return
	}
	b.mu.Lock()
	b.total += int64(len(source))
	if len(source) >= b.capacity {
		b.data = append(b.data[:0], source[len(source)-b.capacity:]...)
		b.start = b.total - int64(len(b.data))
		b.mu.Unlock()
		return
	}
	b.data = append(b.data, source...)
	if len(b.data) > b.capacity {
		drop := len(b.data) - b.capacity
		copy(b.data, b.data[drop:])
		b.data = b.data[:b.capacity]
		b.start += int64(drop)
	}
	b.mu.Unlock()
}

func (b *processSessionBuffer) read(offset int64, maxBytes int) (string, int64, int64, bool, int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if offset > b.total {
		return "", b.start, b.total, b.start > 0, offset, errors.New("tools: process-session output offset exceeds total bytes")
	}
	wasTruncated := offset < b.start
	if wasTruncated {
		offset = b.start
	}
	index := int(offset - b.start)
	end := index + maxBytes
	if end > len(b.data) {
		end = len(b.data)
	}
	if index > len(b.data) {
		index = len(b.data)
	}
	result := string(b.data[index:end])
	next := offset + int64(len(result))
	return result, b.start, b.total, wasTruncated, next, nil
}

func (b *processSessionBuffer) snapshot() (start, total int64, truncated bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.start, b.total, b.start > 0
}

type processSessionWriter struct{ buffer *processSessionBuffer }

func (w *processSessionWriter) Write(p []byte) (int, error) {
	w.buffer.append(p)
	return len(p), nil
}
