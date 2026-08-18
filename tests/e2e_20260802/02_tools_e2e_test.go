package e2e_20260802 //nolint:staticcheck // package name with underscores required by test convention

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/tools"
)

// ---------------------------------------------------------------------------
// Test 1: Read tool (read file, read directory, read non-existent)
// ---------------------------------------------------------------------------
func TestReadTool(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "test.txt")
	content := "hello world\nline two\n"
	require.NoError(t, os.WriteFile(fp, []byte(content), 0o600))

	read := tools.NewReadTool(tools.WithWorkdir(dir))

	// read file
	res, err := read.Execute(context.Background(), tools.ToolCall{
		Args: map[string]any{"path": fp},
	})
	require.NoError(t, err)
	assert.Equal(t, content, res.Output)

	// read directory
	res, err = read.Execute(context.Background(), tools.ToolCall{
		Args: map[string]any{"path": dir},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "test.txt")

	// read non-existent
	_, err = read.Execute(context.Background(), tools.ToolCall{
		Args: map[string]any{"path": filepath.Join(dir, "nope.txt")},
	})
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// Test 2: Write tool (atomic write, overwrite, append)
// ---------------------------------------------------------------------------
func TestWriteTool(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "out.txt")

	// atomic write
	wt := tools.NewWriteTool(tools.WithWriteWorkdir(dir))
	res, err := wt.Execute(context.Background(), tools.ToolCall{
		Args: map[string]any{"path": fp, "content": "hello"},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "wrote")
	data, _ := os.ReadFile(fp) //nolint:errcheck,gosec
	assert.Equal(t, "hello", string(data))

	// overwrite disabled by default -> error
	_, err = wt.Execute(context.Background(), tools.ToolCall{
		Args: map[string]any{"path": fp, "content": "overwrite attempt"},
	})
	require.Error(t, err)

	// overwrite enabled
	wt2 := tools.NewWriteTool(tools.WithWriteWorkdir(dir), tools.WithOverwrite(true))
	_, err = wt2.Execute(context.Background(), tools.ToolCall{
		Args: map[string]any{"path": fp, "content": "overwritten"},
	})
	require.NoError(t, err)
	data, _ = os.ReadFile(fp) //nolint:errcheck,gosec
	assert.Equal(t, "overwritten", string(data))

	// append
	_, err = wt2.Execute(context.Background(), tools.ToolCall{
		Args: map[string]any{"path": fp, "content": "-appended", "append": true},
	})
	require.NoError(t, err)
	data, _ = os.ReadFile(fp) //nolint:errcheck,gosec
	assert.Equal(t, "overwritten-appended", string(data))
}

// ---------------------------------------------------------------------------
// Test 3: Edit tool (single replacement, no match, multi match)
// ---------------------------------------------------------------------------
func TestEditTool(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "edit.go")

	initial := "package main\n\nfunc main() {\n    // old code\n}\n"
	require.NoError(t, os.WriteFile(fp, []byte(initial), 0o600))

	edit := tools.NewEditFileTool()
	edit.Workdir = dir

	// single replacement
	res, err := edit.Execute(context.Background(), tools.ToolCall{
		Args: map[string]any{
			"file_path":  fp,
			"old_string": "// old code",
			"new_string": "// new code",
		},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "replaced")
	data, _ := os.ReadFile(fp) //nolint:errcheck,gosec
	assert.Contains(t, string(data), "// new code")
	assert.NotContains(t, string(data), "// old code")

	// no match
	_, err = edit.Execute(context.Background(), tools.ToolCall{
		Args: map[string]any{
			"file_path":  fp,
			"old_string": "nonexistent text here",
			"new_string": "replacement",
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")

	// multi match
	dupPath := filepath.Join(dir, "dup.txt")
	dupContent := "foo\nfoo\nfoo\n"
	require.NoError(t, os.WriteFile(dupPath, []byte(dupContent), 0o600))
	_, err = edit.Execute(context.Background(), tools.ToolCall{
		Args: map[string]any{
			"file_path":  dupPath,
			"old_string": "foo",
			"new_string": "bar",
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "matches")
}

// ---------------------------------------------------------------------------
// Test 4: Bash tool (echo, pipe, error exit, timeout)
// ---------------------------------------------------------------------------
func TestBashTool(t *testing.T) {
	bash := tools.NewBashTool(tools.WithNoSandbox())

	// echo
	res, err := bash.Execute(context.Background(), tools.ToolCall{
		Args: map[string]any{"command": "echo hello bash"},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "hello bash")

	// pipe
	res, err = bash.Execute(context.Background(), tools.ToolCall{
		Args: map[string]any{"command": "echo hello | tr a-z A-Z"},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "HELLO")

	// error exit
	_, err = bash.Execute(context.Background(), tools.ToolCall{
		Args: map[string]any{"command": "exit 42"},
	})
	require.Error(t, err)

	// timeout
	bashTimeout := tools.NewBashTool(tools.WithTimeout(10*time.Millisecond), tools.WithNoSandbox())
	_, err = bashTimeout.Execute(context.Background(), tools.ToolCall{
		Args: map[string]any{"command": "sleep 5"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "timed out")
}

// ---------------------------------------------------------------------------
// Test 5: Grep tool (pattern match, no match)
// ---------------------------------------------------------------------------
func TestGrepTool(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "grep_test.go")
	content := "package test\n\nfunc Hello() {\n    return\n}\n"
	require.NoError(t, os.WriteFile(fp, []byte(content), 0o600))

	grep := tools.NewGrepTool(tools.WithGrepWorkdir(dir), tools.WithForcePureGo(true))

	// pattern match
	res, err := grep.Execute(context.Background(), tools.ToolCall{
		Args: map[string]any{"pattern": "Hello", "path": dir},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "Hello")

	// no match
	res, err = grep.Execute(context.Background(), tools.ToolCall{
		Args: map[string]any{"pattern": "NoSuchFunction", "path": dir},
	})
	require.NoError(t, err)
	assert.Empty(t, res.Output)
}

// ---------------------------------------------------------------------------
// Test 6: Find tool (find files, find dirs)
// ---------------------------------------------------------------------------
func TestFindTool(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "src"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "src", "lib.go"), []byte("package src"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".hidden"), []byte("hidden"), 0o600))

	find := tools.NewFindTool(tools.WithFindWorkdir(dir), tools.WithFindForceNode(true))

	// find files by pattern
	res, err := find.Execute(context.Background(), tools.ToolCall{
		Args: map[string]any{"path": dir, "pattern": "*.go", "type": "f"},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "main.go")
	assert.Contains(t, res.Output, "lib.go")

	// find dirs
	res, err = find.Execute(context.Background(), tools.ToolCall{
		Args: map[string]any{"path": dir, "type": "d"},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "src")
}

// ---------------------------------------------------------------------------
// Test 7: LS tool (list dir, dotfiles, sort)
// ---------------------------------------------------------------------------
func TestLSTool(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.txt"), []byte("bb"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".hidden"), []byte("h"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o750))

	ls := tools.NewLSTool()
	ls.Workdir = dir

	// list dir
	res, err := ls.Execute(context.Background(), tools.ToolCall{
		Args: map[string]any{"path": dir},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "a.txt")
	assert.Contains(t, res.Output, "b.txt")
	assert.Contains(t, res.Output, "sub")
	assert.NotContains(t, res.Output, ".hidden")

	// dotfiles
	res, err = ls.Execute(context.Background(), tools.ToolCall{
		Args: map[string]any{"path": dir, "all": true},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, ".hidden")

	// sort by size
	res, err = ls.Execute(context.Background(), tools.ToolCall{
		Args: map[string]any{"path": dir, "sort": "size", "long": true},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "a.txt")
}

// ---------------------------------------------------------------------------
// Test 8: ToolSearchTool (search by keyword)
// ---------------------------------------------------------------------------
func TestToolSearchTool(t *testing.T) {
	reg := tools.NewDefaultToolRegistry()

	reg.Register(context.Background(), tools.NewReadTool())                            //nolint:errcheck,gosec
	reg.Register(context.Background(), tools.NewWriteTool())                           //nolint:errcheck,gosec
	reg.Register(context.Background(), tools.NewBashTool())                            //nolint:errcheck,gosec
	reg.Register(context.Background(), tools.NewGrepTool(tools.WithForcePureGo(true))) //nolint:errcheck,gosec

	search := tools.NewToolSearchTool(reg)

	// search by keyword
	res, err := search.Execute(context.Background(), tools.ToolCall{
		Args: map[string]any{"query": "read"},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "read")

	// search by category
	res, err = search.Execute(context.Background(), tools.ToolCall{
		Args: map[string]any{"query": "", "category": "shell"},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "bash")
	assert.NotContains(t, res.Output, "read")
}

// ---------------------------------------------------------------------------
// Test 9: DefaultRegistry CRUD (Register, Get, List, duplicate)
// ---------------------------------------------------------------------------
func TestDefaultRegistryCRUD(t *testing.T) {
	reg := tools.NewDefaultToolRegistry()
	ctx := context.Background()

	// Register
	read := tools.NewReadTool()
	require.NoError(t, reg.Register(ctx, read))

	// Get
	def, err := reg.Get(ctx, "read")
	require.NoError(t, err)
	assert.Equal(t, "read", def.Name())

	// Get unknown
	_, err = reg.Get(ctx, "nonexistent")
	require.Error(t, err)

	// List
	list, err := reg.List(ctx)
	require.NoError(t, err)
	assert.Len(t, list, 1)

	// duplicate Register (overwrites, no new entry)
	require.NoError(t, reg.Register(ctx, read))
	list, err = reg.List(ctx)
	require.NoError(t, err)
	assert.Len(t, list, 1)

	// Register nil
	err = reg.Register(ctx, nil)
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// Test 10: DeferredToolRegistry (lazy load, cache, failed load stub)
// ---------------------------------------------------------------------------
func TestDeferredToolRegistry(t *testing.T) {
	underlying := tools.NewDefaultToolRegistry()
	deferred := tools.NewDefaultDeferredToolRegistry(underlying)
	ctx := context.Background()

	loadCount := 0
	err := deferred.RegisterDeferred(ctx, "lazy-read", func() (tools.ToolDefinition, error) {
		loadCount++
		return tools.NewReadTool(), nil
	})
	require.NoError(t, err)

	// not loaded yet
	assert.False(t, deferred.IsLoaded("lazy-read"))

	// Load triggers the loader
	def, err := deferred.Load(ctx, "lazy-read")
	require.NoError(t, err)
	assert.Equal(t, "read", def.Name())
	assert.Equal(t, 1, loadCount)
	assert.True(t, deferred.IsLoaded("lazy-read"))

	// second Load uses cache
	def2, err := deferred.Load(ctx, "lazy-read")
	require.NoError(t, err)
	assert.Same(t, def, def2)
	assert.Equal(t, 1, loadCount)

	// failed load stub
	err = deferred.RegisterDeferred(ctx, "will-fail", func() (tools.ToolDefinition, error) {
		return nil, fmt.Errorf("simulated failure")
	})
	require.NoError(t, err)
	_, err = deferred.Load(ctx, "will-fail")
	require.Error(t, err)
	assert.True(t, deferred.IsLoaded("will-fail"))

	// unknown tool
	_, err = deferred.Load(ctx, "unknown")
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// Test 11: FileMutationQueue (enqueue write/edit)
// ---------------------------------------------------------------------------
func TestFileMutationQueue(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "mutated.txt")
	require.NoError(t, os.WriteFile(fp, []byte("original"), 0o600))

	q := tools.NewDefaultFileMutationQueue()

	// enqueue write
	writeCh, err := q.Enqueue(context.Background(), tools.FileMutation{
		FilePath:  fp,
		Operation: "write",
		Content:   "new content",
		ToolName:  "write",
	})
	require.NoError(t, err)

	res := <-writeCh
	assert.True(t, res.Success)
	assert.Nil(t, res.Error)

	data, _ := os.ReadFile(fp) //nolint:errcheck,gosec
	assert.Equal(t, "new content", string(data))

	// enqueue edit
	editCh, err := q.Enqueue(context.Background(), tools.FileMutation{
		FilePath:  fp,
		Operation: "edit",
		Content: map[string]any{
			"old_string": "new content",
			"new_string": "edited content",
		},
		ToolName: "edit",
	})
	require.NoError(t, err)

	res = <-editCh
	assert.True(t, res.Success)

	data, _ = os.ReadFile(fp) //nolint:errcheck,gosec
	assert.Equal(t, "edited content", string(data))
}

// ---------------------------------------------------------------------------
// Test 12: Complex orchestration (write→read→grep→edit→verify)
// ---------------------------------------------------------------------------
func TestComplexOrchestration(t *testing.T) {
	dir := t.TempDir()

	write := tools.NewWriteTool(tools.WithWriteWorkdir(dir), tools.WithOverwrite(true))
	read := tools.NewReadTool(tools.WithWorkdir(dir))
	grep := tools.NewGrepTool(tools.WithGrepWorkdir(dir), tools.WithForcePureGo(true))
	edit := tools.NewEditFileTool()
	edit.Workdir = dir

	ctx := context.Background()
	fp := filepath.Join(dir, "orchestrated.go")

	// write
	content := "package main\n\nconst Version = \"1.0\"\n\nfunc main() {}\n"
	_, err := write.Execute(ctx, tools.ToolCall{
		Args: map[string]any{"path": fp, "content": content},
	})
	require.NoError(t, err)

	// read
	res, err := read.Execute(ctx, tools.ToolCall{
		Args: map[string]any{"path": fp},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "Version")

	// grep
	grepRes, err := grep.Execute(ctx, tools.ToolCall{
		Args: map[string]any{"pattern": "Version", "path": dir},
	})
	require.NoError(t, err)
	assert.Contains(t, grepRes.Output, "Version")

	// edit
	_, err = edit.Execute(ctx, tools.ToolCall{
		Args: map[string]any{
			"file_path":  fp,
			"old_string": "1.0",
			"new_string": "2.0",
		},
	})
	require.NoError(t, err)

	// verify
	res, err = read.Execute(ctx, tools.ToolCall{
		Args: map[string]any{"path": fp},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "2.0")
	assert.NotContains(t, res.Output, "1.0")
}

// ---------------------------------------------------------------------------
// Test 13: ConversationRunner with all tools
// ---------------------------------------------------------------------------
func TestConversationRunnerWithAllTools(t *testing.T) {
	tmpl := mock.NewConversationTemplate("tools", "all-tools",
		mock.ConversationTurn{AssistantToolCalls: []mock.ExpectedToolCall{
			{ID: "c1", Name: "read_file", Args: map[string]any{"path": "main.go"}},
			{ID: "c2", Name: "bash", Args: map[string]any{"command": "go test"}},
		}},
		mock.ConversationTurn{AssistantContent: "all good"},
	)
	mockLLM := mock.NewMockLLMServer(tmpl)
	toolSrv := mock.NewMockToolServer()
	_, err := toolSrv.RegisterReadFileTool("source")
	require.NoError(t, err)
	_, err = toolSrv.RegisterBashTool("ok\n", 0)
	require.NoError(t, err)

	runner := mock.NewConversationRunner(mockLLM, toolSrv, nil)
	require.NoError(t, runner.Run(context.Background(), []string{"do it"}))

	runner.AssertToolCalled(t, "read_file", 1)
	runner.AssertToolCalled(t, "bash", 1)
}

// ---------------------------------------------------------------------------
// Test 14: ConversationRunner with trace exporter
// ---------------------------------------------------------------------------
func TestConversationRunnerWithTraceExporter(t *testing.T) {
	tmpl := mock.NewConversationTemplate("trace", "trace-run",
		mock.ConversationTurn{AssistantContent: "response"},
		mock.ConversationTurn{AssistantToolCalls: []mock.ExpectedToolCall{
			{ID: "c1", Name: "read_file", Args: map[string]any{"path": "x.go"}},
		}},
		mock.ConversationTurn{AssistantContent: "final"},
	)
	mockLLM := mock.NewMockLLMServer(tmpl)
	toolSrv := mock.NewMockToolServer()
	_, err := toolSrv.RegisterReadFileTool("data")
	require.NoError(t, err)

	exporter := mock.NewMockTraceExporter()
	runner := mock.NewConversationRunner(mockLLM, toolSrv, exporter)

	require.NoError(t, runner.Run(context.Background(), []string{"msg1", "msg2"}))
	runner.AssertTraceComplete(t)
	runner.AssertNoLLMError(t)
}

// ---------------------------------------------------------------------------
// Test 15: Edge cases (empty args for each tool)
// ---------------------------------------------------------------------------
func TestEdgeCasesEmptyArgs(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	// read with empty path
	read := tools.NewReadTool(tools.WithWorkdir(dir))
	_, err := read.Execute(ctx, tools.ToolCall{Args: map[string]any{}})
	require.Error(t, err)

	// write with empty path
	write := tools.NewWriteTool(tools.WithWriteWorkdir(dir))
	_, err = write.Execute(ctx, tools.ToolCall{Args: map[string]any{}})
	require.Error(t, err)

	// edit with empty path
	edit := tools.NewEditFileTool()
	_, err = edit.Execute(ctx, tools.ToolCall{Args: map[string]any{}})
	require.Error(t, err)

	// bash with empty command
	bash := tools.NewBashTool()
	_, err = bash.Execute(ctx, tools.ToolCall{Args: map[string]any{}})
	require.Error(t, err)

	// grep with empty pattern
	grep := tools.NewGrepTool(tools.WithGrepWorkdir(dir), tools.WithForcePureGo(true))
	_, err = grep.Execute(ctx, tools.ToolCall{Args: map[string]any{}})
	require.Error(t, err)

	// ls with path to non-existent
	ls := tools.NewLSTool()
	_, err = ls.Execute(ctx, tools.ToolCall{
		Args: map[string]any{"path": filepath.Join(dir, "nope")},
	})
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// Test 16: Large output handling (large file read, large bash output)
// ---------------------------------------------------------------------------
func TestLargeOutputHandling(t *testing.T) {
	dir := t.TempDir()

	// large file read
	largeContent := strings.Repeat("abcdefghij", 200000) // ~2MB
	fp := filepath.Join(dir, "large.txt")
	require.NoError(t, os.WriteFile(fp, []byte(largeContent), 0o600))

	read := tools.NewReadTool(tools.WithWorkdir(dir), tools.WithMaxBytes(1024))
	_, err := read.Execute(context.Background(), tools.ToolCall{
		Args: map[string]any{"path": fp},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "too large")

	// large bash output — should be truncated but still succeed (exit 0)
	bash := tools.NewBashTool(tools.WithMaxOutput(50), tools.WithNoSandbox())
	res, err := bash.Execute(context.Background(), tools.ToolCall{
		Args: map[string]any{"command": "dd if=/dev/zero bs=1024 count=1 2>/dev/null | xxd"},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "[output truncated]")
	_ = res
}

// ---------------------------------------------------------------------------
// Test 17: MockToolServer call logging
// ---------------------------------------------------------------------------
func TestMockToolServerCallLogging(t *testing.T) {
	srv := mock.NewMockToolServer()

	def, err := srv.RegisterMockTool("my-tool", func(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
		return &tools.ToolResult{Output: fmt.Sprintf("args: %v", call.Args)}, nil
	})
	require.NoError(t, err)
	assert.Equal(t, "my-tool", def.Name())

	// execute twice
	tick := tools.ToolCall{ID: "id1", Name: "my-tool", Args: map[string]any{"key": "val1"}}
	_, err = srv.Execute(context.Background(), tick)
	require.NoError(t, err)

	tick2 := tools.ToolCall{ID: "id2", Name: "my-tool", Args: map[string]any{"key": "val2"}}
	_, err = srv.Execute(context.Background(), tick2)
	require.NoError(t, err)

	log := srv.CallLog()
	require.Len(t, log, 2)
	assert.Equal(t, "my-tool", log[0].ToolName)
	assert.Equal(t, "val1", log[0].Args["key"])
	assert.Equal(t, "my-tool", log[1].ToolName)
	assert.Equal(t, "val2", log[1].Args["key"])
}

// ---------------------------------------------------------------------------
// Test 18: ToolDefinition interface compliance
// ---------------------------------------------------------------------------
func TestToolDefinitionInterfaceCompliance(t *testing.T) {
	var defs []tools.ToolDefinition

	defs = append(defs,
		tools.NewReadTool(),
		tools.NewWriteTool(),
		tools.NewEditFileTool(),
		tools.NewBashTool(),
		tools.NewGrepTool(tools.WithForcePureGo(true)),
		tools.NewFindTool(tools.WithFindForceNode(true)),
		tools.NewLSTool(),
	)

	for _, def := range defs {
		name := def.Name()
		assert.NotEmpty(t, name, "tool %T has empty name", def)
		desc := def.Description()
		assert.NotEmpty(t, desc, "tool %s has empty description", name)
	}

	// execute each one with minimal valid args (don't care about errors)
	dir := t.TempDir()
	fp := filepath.Join(dir, "comp.txt")
	os.WriteFile(fp, []byte("test content"), 0o600) //nolint:errcheck,gosec

	toolCalls := map[string]tools.ToolCall{
		"read":  {Args: map[string]any{"path": fp}},
		"write": {Args: map[string]any{"path": filepath.Join(dir, "comp-out.txt"), "content": "x"}},
		"edit":  {Args: map[string]any{"file_path": fp, "old_string": "test content", "new_string": "updated"}},
		"bash":  {Args: map[string]any{"command": "echo ok"}},
		"grep":  {Args: map[string]any{"pattern": "test", "path": dir}},
		"find":  {Args: map[string]any{"path": dir, "pattern": "*", "type": "f"}},
		"ls":    {Args: map[string]any{"path": dir}},
	}

	ctx := context.Background()
	for _, def := range defs {
		call, ok := toolCalls[def.Name()]
		if !ok {
			continue
		}
		_, err := def.Execute(ctx, call)
		// execution can fail for various reasons — we just verify no panic
		_ = err
	}

	// also verify ToolSearchTool compliance
	reg := tools.NewDefaultToolRegistry()
	reg.Register(ctx, tools.NewReadTool()) //nolint:errcheck,gosec
	search := tools.NewToolSearchTool(reg)
	assert.Equal(t, "tool_search", search.Name())
	assert.NotEmpty(t, search.Description())
	res, err := search.Execute(ctx, tools.ToolCall{Args: map[string]any{"query": "read"}})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "read")
}

// ---------------------------------------------------------------------------
// Extra: Trace span from ConversationRunner
// ---------------------------------------------------------------------------
func TestConversationRunnerTraceSpan(t *testing.T) {
	tmpl := mock.NewConversationTemplate("span", "span-run",
		mock.ConversationTurn{AssistantContent: "hello"},
	)
	mockLLM := mock.NewMockLLMServer(tmpl)
	toolSrv := mock.NewMockToolServer()
	exporter := mock.NewMockTraceExporter()

	runner := mock.NewConversationRunner(mockLLM, toolSrv, exporter)
	require.NoError(t, runner.Run(context.Background(), []string{"hi"}))

	// check span exists
	assert.GreaterOrEqual(t, exporter.SpanCount(), 1)
	spans := exporter.Spans()
	foundInvocation := false
	for _, span := range spans {
		if span.Name == "cli.invocation" {
			foundInvocation = true
			break
		}
	}
	assert.True(t, foundInvocation, "expected cli.invocation span")
}
