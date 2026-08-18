package tools

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

// --- GrepTool path security tests (SEC-1) ---

// TestGrep_RejectPathTraversal verifies AC-1: grep rejects ../../etc/passwd
// path traversal. The error must mention "escapes workdir" and no file reading
// may occur.
func TestGrep_RejectPathTraversal(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := setupGrepDir(t)
	tool := NewGrepTool(WithGrepWorkdir(dir), WithForcePureGo(true))

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"pattern": "root", "path": "../../etc/passwd"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "escapes workdir")
	assert.Nil(t, res)
}

// TestGrep_RejectAbsolutePath verifies that grep rejects an absolute path
// outside the workdir when a whitelist is configured.
func TestGrep_RejectAbsolutePath(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := setupGrepDir(t)
	tool := NewGrepTool(
		WithGrepWorkdir(dir),
		WithForcePureGo(true),
		WithGrepPathWhitelist([]string{dir}),
	)

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"pattern": "root", "path": "/etc"},
	})
	require.Error(t, err)
	assert.Nil(t, res)
}

// TestGrep_ValidPathInWorkdir verifies AC-4: a valid path inside the workdir
// executes normally and returns correct matches.
func TestGrep_ValidPathInWorkdir(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := setupGrepDir(t)
	tool := NewGrepTool(
		WithGrepWorkdir(dir),
		WithForcePureGo(true),
		WithGrepPathWhitelist([]string{dir}),
	)

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"pattern": "TODO"},
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Contains(t, res.Output, "a.go:2:// TODO: fix me")
	assert.Contains(t, res.Output, "c.txt:2:TODO here too")
}

// TestGrep_PathValidationRace verifies AC-5: concurrent path validation in
// grep has no data race under -race.
func TestGrep_PathValidationRace(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := setupGrepDir(t)
	tool := NewGrepTool(WithGrepWorkdir(dir), WithForcePureGo(true))

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, err := tool.Execute(context.Background(), ToolCall{
				Args: map[string]any{"pattern": "TODO", "path": "../../etc/passwd"},
			})
			assert.Error(t, err, "expected path traversal to be rejected")
		}()
	}
	wg.Wait()
}

// --- FindTool path security tests (SEC-1) ---

// TestFind_RejectPathTraversal verifies that find rejects ../../etc/passwd
// path traversal.
func TestFind_RejectPathTraversal(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := setupFindDir(t)
	tool := NewFindTool(WithFindWorkdir(dir), WithFindForceNode(true))

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"path": "../../etc/passwd"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "escapes workdir")
	assert.Nil(t, res)
}

// TestFind_RejectAbsolutePath verifies AC-2: find rejects absolute path /etc
// when a whitelist is configured.
func TestFind_RejectAbsolutePath(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := setupFindDir(t)
	tool := NewFindTool(
		WithFindWorkdir(dir),
		WithFindForceNode(true),
		WithFindPathWhitelist([]string{dir}),
	)

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"path": "/etc"},
	})
	require.Error(t, err)
	assert.Nil(t, res)
}

// TestFind_ValidPathInWorkdir verifies that a valid path inside the workdir
// executes normally.
func TestFind_ValidPathInWorkdir(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := setupFindDir(t)
	tool := NewFindTool(
		WithFindWorkdir(dir),
		WithFindForceNode(true),
		WithFindPathWhitelist([]string{dir}),
	)

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"pattern": "*.go"},
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Contains(t, res.Output, "b.go")
	assert.Contains(t, res.Output, "sub/c.go")
}

// TestFind_PathValidationRace verifies that concurrent path validation in
// find has no data race under -race.
func TestFind_PathValidationRace(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := setupFindDir(t)
	tool := NewFindTool(WithFindWorkdir(dir), WithFindForceNode(true))

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, err := tool.Execute(context.Background(), ToolCall{
				Args: map[string]any{"path": "../../etc/passwd"},
			})
			assert.Error(t, err, "expected path traversal to be rejected")
		}()
	}
	wg.Wait()
}

// --- LSTool path security tests (SEC-1) ---

// TestLS_RejectPathTraversal verifies that ls rejects ../../etc/passwd path
// traversal.
func TestLS_RejectPathTraversal(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := setupFindDir(t)
	tool := NewLSTool(WithLSWorkdir(dir))

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"path": "../../etc/passwd"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "escapes workdir")
	assert.Nil(t, res)
}

// TestLS_RejectHomeDir verifies AC-3: ls rejects ~/.ssh path. Since there is
// no shell expansion, ~/.ssh resolves to a literal path inside the workdir
// that does not exist, so the tool returns an error without listing anything.
func TestLS_RejectHomeDir(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := setupFindDir(t)
	tool := NewLSTool(WithLSWorkdir(dir))

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"path": "~/.ssh"},
	})
	require.Error(t, err)
	// No directory contents should be listed.
	assert.Nil(t, res)
}

// TestLS_RejectAbsolutePath verifies that ls rejects an absolute path outside
// the workdir when a whitelist is configured.
func TestLS_RejectAbsolutePath(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := setupFindDir(t)
	tool := NewLSTool(
		WithLSWorkdir(dir),
		WithLSPathWhitelist([]string{dir}),
	)

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"path": "/etc"},
	})
	require.Error(t, err)
	assert.Nil(t, res)
}

// TestLS_ValidPathInWorkdir verifies that a valid path inside the workdir
// executes normally.
func TestLS_ValidPathInWorkdir(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := setupFindDir(t)
	tool := NewLSTool(
		WithLSWorkdir(dir),
		WithLSPathWhitelist([]string{dir}),
	)

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{},
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Contains(t, res.Output, "a.txt")
	assert.Contains(t, res.Output, "b.go")
	assert.Contains(t, res.Output, "sub/")
}

// TestLS_PathValidationRace verifies that concurrent path validation in ls
// has no data race under -race.
func TestLS_PathValidationRace(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := setupFindDir(t)
	tool := NewLSTool(WithLSWorkdir(dir))

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, err := tool.Execute(context.Background(), ToolCall{
				Args: map[string]any{"path": "../../etc/passwd"},
			})
			assert.Error(t, err, "expected path traversal to be rejected")
		}()
	}
	wg.Wait()
}
