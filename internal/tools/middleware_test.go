package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

// fakeQueue is a minimal FileMutationQueue giving tests synchronous control
// over the result channel and captured mutations without spawning workers.
type fakeQueue struct {
	mu         sync.Mutex
	captured   []FileMutation
	enqueueErr error
	resultCh   chan FileMutationResult
}

func newFakeQueue() *fakeQueue {
	return &fakeQueue{resultCh: make(chan FileMutationResult, 1)}
}

func (f *fakeQueue) Enqueue(_ context.Context, m FileMutation) (<-chan FileMutationResult, error) {
	f.mu.Lock()
	f.captured = append(f.captured, m)
	f.mu.Unlock()
	if f.enqueueErr != nil {
		return nil, f.enqueueErr
	}
	return f.resultCh, nil
}

func (f *fakeQueue) Name() string { return "fake" }

func (f *fakeQueue) mutations() []FileMutation {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]FileMutation(nil), f.captured...)
}

// TestWithMutationQueueNilNextReturnsError guards the constructor precondition:
// a nil next handler must surface an error instead of panicking on dispatch.
func TestWithMutationQueueNilNextReturnsError(t *testing.T) {
	wrapped := WithMutationQueue(newFakeQueue(), nil)
	_, err := wrapped(context.Background(), ToolCall{Name: "write", Args: map[string]any{"path": "a", "content": "b"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "non-nil next")
}

// TestWithMutationQueueEditMutationQueued proves the edit branch routes through
// the queue with file_path extraction and an {old_string,new_string} payload,
// distinct from the already-tested write branch.
func TestWithMutationQueueEditMutationQueued(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	q := newFakeQueue()
	wrapped := WithMutationQueue(q, func(_ context.Context, _ ToolCall) (*ToolResult, error) {
		t.Fatal("next must not run for mutation tools")
		return nil, nil
	})

	q.resultCh <- FileMutationResult{Success: true}
	res, err := wrapped(context.Background(), ToolCall{
		Name: "edit",
		Args: map[string]any{"file_path": "main.go", "old_string": "a", "new_string": "b"},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "edit queued and applied for main.go")
	assert.Equal(t, "main.go", res.Metadata["path"])
	assert.Equal(t, true, res.Metadata["queued"])

	ms := q.mutations()
	require.Len(t, ms, 1)
	assert.Equal(t, "main.go", ms[0].FilePath)
	assert.Equal(t, "edit", ms[0].Operation)
	assert.Equal(t, "edit", ms[0].ToolName)
	content, ok := ms[0].Content.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "a", content["old_string"])
	assert.Equal(t, "b", content["new_string"])
}

// TestWithMutationQueueEnqueueErrorWrapped ensures a failed enqueue is wrapped
// with the tool name so callers can attribute the failure.
func TestWithMutationQueueEnqueueErrorWrapped(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	q := newFakeQueue()
	q.enqueueErr = errors.New("queue closed")
	wrapped := WithMutationQueue(q, func(_ context.Context, _ ToolCall) (*ToolResult, error) {
		t.Fatal("next must not run when enqueue fails")
		return nil, nil
	})

	_, err := wrapped(context.Background(), ToolCall{Name: "write", Args: map[string]any{"path": "a.txt", "content": "x"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "enqueue write")
	assert.ErrorIs(t, err, q.enqueueErr)
}

// TestWithMutationQueueResultErrorPropagated verifies a worker-reported failure
// is returned as the tool error with a nil ToolResult (no synthesized result).
func TestWithMutationQueueResultErrorPropagated(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	boom := errors.New("disk full")
	q := newFakeQueue()
	wrapped := WithMutationQueue(q, func(_ context.Context, _ ToolCall) (*ToolResult, error) {
		t.Fatal("next must not run for mutation tools")
		return nil, nil
	})

	q.resultCh <- FileMutationResult{Success: false, Error: boom}
	res, err := wrapped(context.Background(), ToolCall{Name: "write", Args: map[string]any{"path": "a.txt", "content": "x"}})
	require.ErrorIs(t, err, boom)
	assert.Nil(t, res, "error branch must return nil ToolResult")
}

// TestWithMutationQueueContextCancellation confirms a canceled context wins the
// select when no result has been produced, returning ctx.Err() deterministically.
func TestWithMutationQueueContextCancellation(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	q := newFakeQueue()
	wrapped := WithMutationQueue(q, func(_ context.Context, _ ToolCall) (*ToolResult, error) {
		t.Fatal("next must not run for mutation tools")
		return nil, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := wrapped(ctx, ToolCall{Name: "write", Args: map[string]any{"path": "a.txt", "content": "x"}})
	assert.ErrorIs(t, err, context.Canceled)
}

// TestMutationPathFromCall covers path extraction across the write (path) and
// edit (file_path) argument shapes plus the degenerate cases.
func TestMutationPathFromCall(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
		want string
	}{
		{"write path key", map[string]any{"path": "/a/b.txt"}, "/a/b.txt"},
		{"edit file_path key", map[string]any{"file_path": "/c/d.go"}, "/c/d.go"},
		{"path takes precedence over file_path", map[string]any{"path": "p", "file_path": "fp"}, "p"},
		{"missing both keys", map[string]any{"content": "x"}, ""},
		{"non-string path ignored", map[string]any{"path": 123}, ""},
		{"non-string file_path ignored", map[string]any{"file_path": true}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, mutationPathFromCall(ToolCall{Args: tt.args}))
		})
	}
}

// TestMutationContentFromCall covers payload extraction for write/edit and the
// default nil for unknown tool names.
func TestMutationContentFromCall(t *testing.T) {
	t.Run("write returns content string", func(t *testing.T) {
		assert.Equal(t, "hello", mutationContentFromCall(ToolCall{Name: "write", Args: map[string]any{"content": "hello"}}))
	})
	t.Run("write without content returns empty", func(t *testing.T) {
		assert.Equal(t, "", mutationContentFromCall(ToolCall{Name: "write", Args: map[string]any{}}))
	})
	t.Run("write non-string content returns empty", func(t *testing.T) {
		assert.Equal(t, "", mutationContentFromCall(ToolCall{Name: "write", Args: map[string]any{"content": 42}}))
	})
	t.Run("edit returns old/new map", func(t *testing.T) {
		got := mutationContentFromCall(ToolCall{Name: "edit", Args: map[string]any{"old_string": "a", "new_string": "b"}})
		m, ok := got.(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "a", m["old_string"])
		assert.Equal(t, "b", m["new_string"])
	})
	t.Run("edit partial keys", func(t *testing.T) {
		got := mutationContentFromCall(ToolCall{Name: "edit", Args: map[string]any{"old_string": "a"}})
		m, ok := got.(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "a", m["old_string"])
		_, hasNew := m["new_string"]
		assert.False(t, hasNew)
	})
	t.Run("unknown tool returns nil", func(t *testing.T) {
		assert.Nil(t, mutationContentFromCall(ToolCall{Name: "grep", Args: map[string]any{}}))
	})
}

// schemaToolDef is a mock ToolDefinition that also implements Parameterized so
// SchemaValidator tests can look it up via a registry and validate args.
type schemaToolDef struct {
	name   string
	schema any
}

func (d *schemaToolDef) Name() string        { return d.name }
func (d *schemaToolDef) Description() string { return "schema test tool" }
func (d *schemaToolDef) Execute(_ context.Context, _ ToolCall) (*ToolResult, error) {
	return &ToolResult{Output: "ok"}, nil
}
func (d *schemaToolDef) Parameters() any { return d.schema }

// plainToolDef is a minimal ToolDefinition that does NOT implement Parameterized,
// used to verify SchemaValidator skips tools without a schema.
type plainToolDef struct {
	name string
}

func (d *plainToolDef) Name() string        { return d.name }
func (d *plainToolDef) Description() string { return "plain tool" }
func (d *plainToolDef) Execute(_ context.Context, _ ToolCall) (*ToolResult, error) {
	return &ToolResult{Output: "ok"}, nil
}

// TestPrepareArguments_PathNormalization verifies that a relative path argument
// is resolved to an absolute path while non-path keys are left untouched, and
// that the original call's Args map is not mutated.
func TestPrepareArguments_PathNormalization(t *testing.T) {
	n := NewPathNormalizer("/base/dir")
	original := map[string]any{"path": "src/main.go", "content": "hello"}
	call := ToolCall{Name: "write", Args: original}

	got, err := n.PrepareArguments(context.Background(), call)
	require.NoError(t, err)

	assert.Equal(t, "/base/dir/src/main.go", got.Args["path"])
	assert.Equal(t, "hello", got.Args["content"])
	// Original map must not be mutated.
	assert.Equal(t, "src/main.go", original["path"])
}

// TestPrepareArguments_PathNormalization_AbsUnchanged verifies that absolute
// paths are left as-is.
func TestPrepareArguments_PathNormalization_AbsUnchanged(t *testing.T) {
	n := NewPathNormalizer("/base/dir")
	call := ToolCall{Name: "read", Args: map[string]any{"file_path": "/etc/hosts"}}

	got, err := n.PrepareArguments(context.Background(), call)
	require.NoError(t, err)
	assert.Equal(t, "/etc/hosts", got.Args["file_path"])
}

// TestPrepareArguments_PathNormalization_AllKeys checks every recognized
// path-like key gets normalized.
func TestPrepareArguments_PathNormalization_AllKeys(t *testing.T) {
	n := NewPathNormalizer("/root")
	call := ToolCall{
		Name: "tool",
		Args: map[string]any{
			"path":      "a",
			"file_path": "b",
			"dir":       "c",
			"directory": "d",
			"cwd":       "e",
			"workspace": "f",
		},
	}

	got, err := n.PrepareArguments(context.Background(), call)
	require.NoError(t, err)
	for _, key := range pathArgKeys {
		assert.True(t, filepath.IsAbs(got.Args[key].(string)), "key %s should be absolute", key)
	}
}

// TestPrepareArguments_EmptyArgs is a no-op when the call has no arguments.
func TestPrepareArguments_EmptyArgs(t *testing.T) {
	n := NewPathNormalizer("/base")
	call := ToolCall{Name: "noop"}
	got, err := n.PrepareArguments(context.Background(), call)
	require.NoError(t, err)
	assert.Nil(t, got.Args)
}

// TestSchemaValidation_MissingRequired verifies that a missing required
// parameter surfaces an error wrapped with "schema validation".
func TestSchemaValidation_MissingRequired(t *testing.T) {
	def := &schemaToolDef{
		name: "write",
		schema: map[string]any{
			"required": []any{"path"},
			"properties": map[string]any{
				"path":    map[string]any{"type": "string"},
				"content": map[string]any{"type": "string"},
			},
		},
	}
	reg := NewDefaultToolRegistry()
	require.NoError(t, reg.Register(context.Background(), def))

	v := NewSchemaValidator(reg)
	_, err := v.PrepareArguments(context.Background(), ToolCall{
		Name: "write",
		Args: map[string]any{"content": "x"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing required parameter: path")
}

// TestSchemaValidation_ValidArgs verifies that valid arguments pass through
// without error.
func TestSchemaValidation_ValidArgs(t *testing.T) {
	def := &schemaToolDef{
		name: "write",
		schema: map[string]any{
			"required": []any{"path"},
			"properties": map[string]any{
				"path":    map[string]any{"type": "string"},
				"content": map[string]any{"type": "string"},
			},
		},
	}
	reg := NewDefaultToolRegistry()
	require.NoError(t, reg.Register(context.Background(), def))

	v := NewSchemaValidator(reg)
	call := ToolCall{
		Name: "write",
		Args: map[string]any{"path": "/a.txt", "content": "x"},
	}
	got, err := v.PrepareArguments(context.Background(), call)
	require.NoError(t, err)
	assert.Equal(t, call, got)
}

// TestSchemaValidation_TypeMismatch verifies that a wrong type is caught.
func TestSchemaValidation_TypeMismatch(t *testing.T) {
	def := &schemaToolDef{
		name: "write",
		schema: map[string]any{
			"properties": map[string]any{
				"content": map[string]any{"type": "string"},
			},
		},
	}
	reg := NewDefaultToolRegistry()
	require.NoError(t, reg.Register(context.Background(), def))

	v := NewSchemaValidator(reg)
	_, err := v.PrepareArguments(context.Background(), ToolCall{
		Name: "write",
		Args: map[string]any{"content": 42},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `parameter "content" must be a string`)
}

// TestSchemaValidation_NilRegistryNoOp verifies that a nil registry disables
// validation (no error, call returned unchanged).
func TestSchemaValidation_NilRegistryNoOp(t *testing.T) {
	v := NewSchemaValidator(nil)
	call := ToolCall{Name: "write", Args: map[string]any{"path": "x"}}
	got, err := v.PrepareArguments(context.Background(), call)
	require.NoError(t, err)
	assert.Equal(t, call, got)
}

// TestSchemaValidation_UnknownToolSkipped verifies that when the tool is not in
// the registry, validation is skipped (the executor will surface the real
// error).
func TestSchemaValidation_UnknownToolSkipped(t *testing.T) {
	reg := NewDefaultToolRegistry()
	v := NewSchemaValidator(reg)
	call := ToolCall{Name: "ghost", Args: map[string]any{}}
	got, err := v.PrepareArguments(context.Background(), call)
	require.NoError(t, err)
	assert.Equal(t, call, got)
}

// TestSchemaValidation_NoParameterizedSkipped verifies that a tool without a
// schema (not implementing Parameterized) skips validation.
func TestSchemaValidation_NoParameterizedSkipped(t *testing.T) {
	reg := NewDefaultToolRegistry()
	require.NoError(t, reg.Register(context.Background(), &plainToolDef{name: "plain"}))
	v := NewSchemaValidator(reg)
	call := ToolCall{Name: "plain", Args: map[string]any{"foo": "bar"}}
	got, err := v.PrepareArguments(context.Background(), call)
	require.NoError(t, err)
	assert.Equal(t, call, got)
}

// TestWithArgumentPreparation_NilPreparer verifies that nil preparers are
// skipped without panicking and the call reaches the underlying executor.
func TestWithArgumentPreparation_NilPreparer(t *testing.T) {
	executed := false
	wrapper := WithArgumentPreparation(nil, nil)
	wrapped := wrapper(func(_ context.Context, call ToolCall) (*ToolResult, error) {
		executed = true
		return &ToolResult{Output: "done"}, nil
	})

	res, err := wrapped(context.Background(), ToolCall{Name: "read", Args: map[string]any{"path": "x"}})
	require.NoError(t, err)
	assert.True(t, executed)
	assert.Equal(t, "done", res.Output)
}

// TestWithArgumentPreparation_ChainsPreparers verifies that multiple preparers
// run in order and the output of one feeds into the next.
func TestWithArgumentPreparation_ChainsPreparers(t *testing.T) {
	// Preparer 1: tag the call.
	tagPreparer := &taggingPreparer{tag: "first"}
	// Preparer 2: PathNormalizer.
	pn := NewPathNormalizer("/base")

	wrapper := WithArgumentPreparation(tagPreparer, pn)
	var received ToolCall
	wrapped := wrapper(func(_ context.Context, call ToolCall) (*ToolResult, error) {
		received = call
		return &ToolResult{Output: "ok"}, nil
	})

	_, err := wrapped(context.Background(), ToolCall{
		Name: "write",
		Args: map[string]any{"path": "rel/file.txt"},
	})
	require.NoError(t, err)
	// The tagging preparer added a tag, and PathNormalizer resolved the path.
	assert.Equal(t, "first", received.Args["__tag"])
	assert.Equal(t, "/base/rel/file.txt", received.Args["path"])
}

// taggingPreparer is a test preparer that injects a marker key so we can verify
// ordering in a chain.
type taggingPreparer struct {
	tag string
}

func (p *taggingPreparer) PrepareArguments(_ context.Context, call ToolCall) (ToolCall, error) {
	if call.Args == nil {
		call.Args = map[string]any{}
	} else {
		// Copy to avoid mutating the caller's map.
		newArgs := make(map[string]any, len(call.Args))
		for k, v := range call.Args {
			newArgs[k] = v
		}
		call.Args = newArgs
	}
	call.Args["__tag"] = p.tag
	return call, nil
}

// TestWithArgumentPreparation_ErrorAborts verifies that when a preparer returns
// an error, execution is aborted and the error is wrapped with the tool name.
func TestWithArgumentPreparation_ErrorAborts(t *testing.T) {
	boom := &erroringPreparer{}
	wrapper := WithArgumentPreparation(boom)
	wrapped := wrapper(func(_ context.Context, _ ToolCall) (*ToolResult, error) {
		t.Fatal("next must not run when a preparer errors")
		return nil, nil
	})

	_, err := wrapped(context.Background(), ToolCall{Name: "read"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `prepare arguments for "read"`)
}

// erroringPreparer always returns an error.
type erroringPreparer struct{}

func (p *erroringPreparer) PrepareArguments(_ context.Context, _ ToolCall) (ToolCall, error) {
	return ToolCall{}, errors.New("boom")
}

// TestPathNormalizer_EmptyBaseDirUsesWd verifies that when baseDir is empty,
// the process working directory is used so relative paths become absolute
// relative to cwd.
func TestPathNormalizer_EmptyBaseDirUsesWd(t *testing.T) {
	n := NewPathNormalizer("")
	wd, err := os.Getwd()
	require.NoError(t, err)

	got, err := n.PrepareArguments(context.Background(), ToolCall{
		Name: "read",
		Args: map[string]any{"path": "main.go"},
	})
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(wd, "main.go"), got.Args["path"])
}
