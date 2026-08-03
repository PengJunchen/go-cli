package tools

import (
	"context"
	"path/filepath"
	"sync"
)

// fileLockManager provides per-file-path mutexes so that concurrent write/edit
// calls targeting the same file (including via symlinks) are serialized.
// Different files run in parallel.
type fileLockManager struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// newFileLockManager creates an empty fileLockManager.
func newFileLockManager() *fileLockManager {
	return &fileLockManager{locks: make(map[string]*sync.Mutex)}
}

// lockFor returns the mutex for the given path, resolving symlinks so that
// different paths pointing to the same file share a lock.
func (m *fileLockManager) lockFor(path string) *sync.Mutex {
	resolved := path
	if r, err := filepath.EvalSymlinks(path); err == nil {
		resolved = r
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if mu, ok := m.locks[resolved]; ok {
		return mu
	}
	mu := &sync.Mutex{}
	m.locks[resolved] = mu
	return mu
}

// NewMutationWrapper returns a ToolExecutorWrapper that serializes write/edit
// tool calls by file path. Non-mutation tools pass through without locking.
//
// This wires file-level serialization into the tool execution path via the
// MiddlewareToolRegistry decorator, without modifying LoopAgent's core.
func NewMutationWrapper() ToolExecutorWrapper {
	mgr := newFileLockManager()
	return func(next func(ctx context.Context, call ToolCall) (*ToolResult, error)) func(ctx context.Context, call ToolCall) (*ToolResult, error) {
		return func(ctx context.Context, call ToolCall) (*ToolResult, error) {
			if !mutationToolNames[call.Name] {
				return next(ctx, call)
			}
			path := mutationPathFromCall(call)
			mu := mgr.lockFor(path)
			mu.Lock()
			defer mu.Unlock()
			return next(ctx, call)
		}
	}
}
