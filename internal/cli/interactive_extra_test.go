package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/mcp"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// ---------------------------------------------------------------------------
// Mock types
// ---------------------------------------------------------------------------

// testMCPClient is a mock mcp.MCPClient for testing tool registration.
type testMCPClient struct {
	name       string
	connectErr error
	listErr    error
	tools      []mcp.MCPTool
	connected  bool
}

func (c *testMCPClient) Connect(_ context.Context) error {
	if c.connectErr != nil {
		return c.connectErr
	}
	c.connected = true
	return nil
}

func (c *testMCPClient) Disconnect(_ context.Context) error { return nil }

func (c *testMCPClient) ListTools(_ context.Context) ([]mcp.MCPTool, error) {
	if c.listErr != nil {
		return nil, c.listErr
	}
	return c.tools, nil
}

func (c *testMCPClient) CallTool(_ context.Context, _ string, _ map[string]any) (*mcp.MCPToolResult, error) {
	return &mcp.MCPToolResult{Content: "ok"}, nil
}

func (c *testMCPClient) Name() string { return c.name }

var _ mcp.MCPClient = (*testMCPClient)(nil)

// alwaysFailRegistry is a tools.ToolRegistry whose Register always errors.
type alwaysFailRegistry struct{}

func (alwaysFailRegistry) Register(_ context.Context, _ tools.ToolDefinition) error {
	return fmt.Errorf("mock: registration always fails")
}
func (alwaysFailRegistry) Get(_ context.Context, _ string) (tools.ToolDefinition, error) {
	return nil, tools.ErrToolNotFound
}
func (alwaysFailRegistry) List(_ context.Context) ([]tools.ToolDefinition, error) {
	return nil, nil
}

var _ tools.ToolRegistry = alwaysFailRegistry{}

// blockingCommand blocks until its context is cancelled, then returns the
// context error. It signals startup via the started channel.
type blockingCommand struct {
	started chan struct{}
}

func (c *blockingCommand) Name() string     { return "block" }
func (c *blockingCommand) Synopsis() string { return "Blocks until cancelled" }
func (c *blockingCommand) Run(ctx context.Context, _ Config, _ []string) error {
	close(c.started)
	<-ctx.Done()
	return ctx.Err()
}

var _ Command = (*blockingCommand)(nil)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// writeSkillFile writes content to a file named name inside dir.
func writeSkillFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write skill file %s: %v", path, err)
	}
}

// ---------------------------------------------------------------------------
// RegisterMCPToolsFromClients tests
// ---------------------------------------------------------------------------

func TestRegisterMCPToolsFromClients_Success(t *testing.T) {
	ctx := t.Context()
	tr := tools.NewDefaultToolRegistry()
	client := &testMCPClient{
		name: "test-server",
		tools: []mcp.MCPTool{
			{Name: "tool1", Description: "First tool"},
			{Name: "tool2", Description: "Second tool"},
		},
	}

	err := RegisterMCPToolsFromClients(ctx, []mcp.MCPClient{client}, tr)
	require.NoError(t, err)
	assert.True(t, client.connected)

	tool1, err := tr.Get(ctx, mcp.NormalizeToolName("test-server", "tool1"))
	require.NoError(t, err)
	assert.NotNil(t, tool1)

	tool2, err := tr.Get(ctx, mcp.NormalizeToolName("test-server", "tool2"))
	require.NoError(t, err)
	assert.NotNil(t, tool2)
}

func TestRegisterMCPToolsFromClients_ConnectFailure(t *testing.T) {
	ctx := t.Context()
	tr := tools.NewDefaultToolRegistry()
	client := &testMCPClient{
		name:       "failing-server",
		connectErr: fmt.Errorf("connection refused"),
		tools:      []mcp.MCPTool{{Name: "tool1"}},
	}

	err := RegisterMCPToolsFromClients(ctx, []mcp.MCPClient{client}, tr)
	require.NoError(t, err) // Connect failures are logged, not propagated.
	assert.False(t, client.connected)

	registered, err := tr.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, registered)
}

func TestRegisterMCPToolsFromClients_ListToolsFailure(t *testing.T) {
	ctx := t.Context()
	tr := tools.NewDefaultToolRegistry()
	client := &testMCPClient{
		name:    "list-fail-server",
		listErr: fmt.Errorf("list tools failed"),
	}

	err := RegisterMCPToolsFromClients(ctx, []mcp.MCPClient{client}, tr)
	require.NoError(t, err)
	assert.True(t, client.connected)

	registered, err := tr.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, registered)
}

func TestRegisterMCPToolsFromClients_EmptyClients(t *testing.T) {
	ctx := t.Context()
	tr := tools.NewDefaultToolRegistry()

	err := RegisterMCPToolsFromClients(ctx, nil, tr)
	require.NoError(t, err)

	err = RegisterMCPToolsFromClients(ctx, []mcp.MCPClient{}, tr)
	require.NoError(t, err)
}

func TestRegisterMCPToolsFromClients_RegisterFailure(t *testing.T) {
	ctx := t.Context()
	tr := alwaysFailRegistry{}
	client := &testMCPClient{
		name: "reg-fail-server",
		tools: []mcp.MCPTool{
			{Name: "tool1", Description: "First tool"},
			{Name: "tool2", Description: "Second tool"},
		},
	}

	err := RegisterMCPToolsFromClients(ctx, []mcp.MCPClient{client}, tr)
	require.NoError(t, err) // Registration failures are logged, not propagated.
	assert.True(t, client.connected)
}

// ---------------------------------------------------------------------------
// RegisterSkillToolsFromDir tests
// ---------------------------------------------------------------------------

func TestRegisterSkillToolsFromDir_ValidDir(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()

	skill1 := `---
name: test-skill
description: A test skill
version: "1.0"
category: test
prompt: Do something useful
---
This is the skill body.
`
	skill2 := `---
name: another-skill
description: Another test skill
version: "2.0"
category: test
prompt: Do another thing
---
Another body.
`
	writeSkillFile(t, dir, "test-skill.md", skill1)
	writeSkillFile(t, dir, "another-skill.yaml", skill2)

	tr := tools.NewDefaultToolRegistry()
	err := RegisterSkillToolsFromDir(ctx, dir, tr)
	require.NoError(t, err)

	tool, err := tr.Get(ctx, "test-skill")
	require.NoError(t, err)
	assert.NotNil(t, tool)
	assert.Equal(t, "test-skill", tool.Name())

	tool2, err := tr.Get(ctx, "another-skill")
	require.NoError(t, err)
	assert.NotNil(t, tool2)
	assert.Equal(t, "another-skill", tool2.Name())
}

func TestRegisterSkillToolsFromDir_InvalidDir(t *testing.T) {
	ctx := t.Context()
	tr := tools.NewDefaultToolRegistry()

	err := RegisterSkillToolsFromDir(ctx, "/nonexistent/directory/path", tr)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nonexistent")
}

func TestRegisterSkillToolsFromDir_EmptyDir(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	tr := tools.NewDefaultToolRegistry()

	err := RegisterSkillToolsFromDir(ctx, dir, tr)
	require.NoError(t, err)

	registered, err := tr.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, registered)
}

// ---------------------------------------------------------------------------
// interactiveCmd.registerMCPTools tests
// ---------------------------------------------------------------------------

func TestInteractiveCmd_RegisterMCPTools_NilConfig(t *testing.T) {
	ctx := t.Context()
	cmd := newInteractiveCmd(nil, &bytes.Buffer{})
	tr := tools.NewDefaultToolRegistry()

	err := cmd.registerMCPTools(ctx, nil, tr)
	require.NoError(t, err)
}

func TestInteractiveCmd_RegisterMCPTools_EmptyServers(t *testing.T) {
	ctx := t.Context()
	cmd := newInteractiveCmd(nil, &bytes.Buffer{})
	tr := tools.NewDefaultToolRegistry()
	rc := &config.Config{}

	err := cmd.registerMCPTools(ctx, rc, tr)
	require.NoError(t, err)
}

func TestInteractiveCmd_RegisterMCPTools_StdioServer(t *testing.T) {
	ctx := t.Context()
	cmd := newInteractiveCmd(nil, &bytes.Buffer{})
	tr := tools.NewDefaultToolRegistry()
	rc := &config.Config{
		MCP: config.MCPConfig{
			Servers: []config.MCPServerConfig{
				{
					Name:    "stdio-server",
					Command: "/nonexistent/command/xyz",
					Args:    []string{"--flag"},
					Env:     map[string]string{"KEY": "VALUE"},
				},
			},
		},
	}

	err := cmd.registerMCPTools(ctx, rc, tr)
	require.NoError(t, err) // Connect failures are logged, not propagated.

	registered, err := tr.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, registered)
}

func TestInteractiveCmd_RegisterMCPTools_SSEServer(t *testing.T) {
	ctx := t.Context()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":0,"result":{"tools":[{"name":"sse-tool","description":"An SSE tool"}]}}`))
		} else {
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	cmd := newInteractiveCmd(nil, &bytes.Buffer{})
	tr := tools.NewDefaultToolRegistry()
	rc := &config.Config{
		MCP: config.MCPConfig{
			Servers: []config.MCPServerConfig{
				{Name: "sse-server", URL: srv.URL},
			},
		},
	}

	err := cmd.registerMCPTools(ctx, rc, tr)
	require.NoError(t, err)

	tool, err := tr.Get(ctx, mcp.NormalizeToolName("sse-server", "sse-tool"))
	require.NoError(t, err)
	assert.NotNil(t, tool)
	assert.Equal(t, "mcp__sse-server__sse-tool", tool.Name())
}

func TestInteractiveCmd_RegisterMCPTools_NoTransport(t *testing.T) {
	ctx := t.Context()
	cmd := newInteractiveCmd(nil, &bytes.Buffer{})
	tr := tools.NewDefaultToolRegistry()
	rc := &config.Config{
		MCP: config.MCPConfig{
			Servers: []config.MCPServerConfig{
				{Name: "no-transport-server"},
			},
		},
	}

	err := cmd.registerMCPTools(ctx, rc, tr)
	require.NoError(t, err)

	registered, err := tr.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, registered)
}

// ---------------------------------------------------------------------------
// interactiveCmd.registerSkillTools tests
// ---------------------------------------------------------------------------

func TestInteractiveCmd_RegisterSkillTools_NilConfig(t *testing.T) {
	ctx := t.Context()
	cmd := newInteractiveCmd(nil, &bytes.Buffer{})
	tr := tools.NewDefaultToolRegistry()

	err := cmd.registerSkillTools(ctx, nil, tr)
	require.NoError(t, err)
}

func TestInteractiveCmd_RegisterSkillTools_EmptyDir(t *testing.T) {
	ctx := t.Context()
	cmd := newInteractiveCmd(nil, &bytes.Buffer{})
	tr := tools.NewDefaultToolRegistry()
	rc := &config.Config{}

	err := cmd.registerSkillTools(ctx, rc, tr)
	require.NoError(t, err)
}

func TestInteractiveCmd_RegisterSkillTools_LoadError(t *testing.T) {
	ctx := t.Context()
	cmd := newInteractiveCmd(nil, &bytes.Buffer{})
	tr := tools.NewDefaultToolRegistry()
	rc := &config.Config{
		Skill: config.SkillConfig{
			Dir: "/nonexistent/directory/path",
		},
	}

	err := cmd.registerSkillTools(ctx, rc, tr)
	require.NoError(t, err) // Load errors are logged, not propagated.

	registered, err := tr.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, registered)
}

func TestInteractiveCmd_RegisterSkillTools_ValidDir(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()

	skillContent := `---
name: registered-skill
description: A skill loaded via registerSkillTools
version: "1.0"
category: test
prompt: Do something useful
---
Skill body text.
`
	writeSkillFile(t, dir, "registered-skill.md", skillContent)

	cmd := newInteractiveCmd(nil, &bytes.Buffer{})
	tr := tools.NewDefaultToolRegistry()
	rc := &config.Config{
		Skill: config.SkillConfig{Dir: dir},
	}

	err := cmd.registerSkillTools(ctx, rc, tr)
	require.NoError(t, err)

	tool, err := tr.Get(ctx, "registered-skill")
	require.NoError(t, err)
	assert.NotNil(t, tool)
	assert.Equal(t, "registered-skill", tool.Name())
}

// ---------------------------------------------------------------------------
// buildModel tests
// ---------------------------------------------------------------------------

func TestPromptCmd_BuildModel_WithConfig(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	cmd := newPromptCmd(&bytes.Buffer{})
	rc := &config.Config{
		Provider: config.ProviderConfig{
			Name:    "test",
			BaseURL: srv.URL,
			APIKey:  "test",
		},
	}

	model, cleanup, err := cmd.buildModel(t.Context(), rc, "test", "test-model")
	require.NoError(t, err)
	assert.NotNil(t, model)
	require.NotNil(t, cleanup)
	cleanup()
}

func TestPromptCmd_BuildModel_WithNilConfig(t *testing.T) {
	cmd := newPromptCmd(&bytes.Buffer{})

	t.Run("default provider succeeds", func(t *testing.T) {
		model, cleanup, err := cmd.buildModel(t.Context(), nil, "eino", "test-model")
		require.NoError(t, err)
		assert.NotNil(t, model)
		require.NotNil(t, cleanup)
		cleanup()
	})

	t.Run("nonexistent provider fails", func(t *testing.T) {
		model, _, err := cmd.buildModel(t.Context(), nil, "nonexistent-provider", "test-model")
		require.Error(t, err)
		assert.Nil(t, model)
	})
}

func TestInteractiveCmd_BuildModel_WithConfig(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	cmd := newInteractiveCmd(nil, &bytes.Buffer{})
	rc := &config.Config{
		Provider: config.ProviderConfig{
			Name:    "test",
			BaseURL: srv.URL,
			APIKey:  "test",
		},
	}

	model, cleanup, err := cmd.buildModel(t.Context(), rc, "test", "test-model")
	require.NoError(t, err)
	assert.NotNil(t, model)
	require.NotNil(t, cleanup)
	cleanup()
}

func TestInteractiveCmd_BuildModel_WithNilConfig(t *testing.T) {
	cmd := newInteractiveCmd(nil, &bytes.Buffer{})

	t.Run("default provider succeeds", func(t *testing.T) {
		model, cleanup, err := cmd.buildModel(t.Context(), nil, "eino", "test-model")
		require.NoError(t, err)
		assert.NotNil(t, model)
		require.NotNil(t, cleanup)
		cleanup()
	})

	t.Run("nonexistent provider fails", func(t *testing.T) {
		model, _, err := cmd.buildModel(t.Context(), nil, "nonexistent-provider", "test-model")
		require.Error(t, err)
		assert.Nil(t, model)
	})
}

// ---------------------------------------------------------------------------
// interactiveCmd.Run conversation tests
// ---------------------------------------------------------------------------

func TestInteractiveCmd_RunConversation(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Hello from the assistant"}}]}`))
	}))
	defer srv.Close()

	var out bytes.Buffer
	in := strings.NewReader("hello\nexit\n")
	cmd := newInteractiveCmd(in, &out)
	cfg := &config.Config{
		Provider: config.ProviderConfig{
			Name:    "test",
			BaseURL: srv.URL,
			APIKey:  "test",
			Model:   "test-model",
		},
	}

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	err := cmd.Run(ctx, cfg, nil)
	require.NoError(t, err)
	assert.Contains(t, out.String(), "Session ended")
	assert.Contains(t, out.String(), "Hello")
}

func TestInteractiveCmd_MaxTokensFlag(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer srv.Close()

	makeCfg := func() *config.Config {
		return &config.Config{
			Provider: config.ProviderConfig{
				Name:    "test",
				BaseURL: srv.URL,
				APIKey:  "test",
				Model:   "test-model",
			},
		}
	}

	t.Run("valid value", func(t *testing.T) {
		defer verify.AssertNoGoroutineLeak(t)()
		var out bytes.Buffer
		cmd := newInteractiveCmd(strings.NewReader("exit\n"), &out)
		err := cmd.Run(t.Context(), makeCfg(), []string{"-max-tokens", "1000"})
		require.NoError(t, err)
		assert.Contains(t, out.String(), "Session ended")
	})

	t.Run("zero resets to default", func(t *testing.T) {
		defer verify.AssertNoGoroutineLeak(t)()
		var out bytes.Buffer
		cmd := newInteractiveCmd(strings.NewReader("exit\n"), &out)
		err := cmd.Run(t.Context(), makeCfg(), []string{"-max-tokens", "0"})
		require.NoError(t, err)
		assert.Contains(t, out.String(), "Session ended")
	})
}

// ---------------------------------------------------------------------------
// runCommand context cancellation test
// ---------------------------------------------------------------------------

func TestRunCommand_ContextCancelledDuringExecution(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cmd := &blockingCommand{started: make(chan struct{})}

	go func() {
		<-cmd.started
		cancel()
	}()

	err := runCommand(ctx, NewDefaultConfig(false), cmd, nil)
	require.Error(t, err)

	var execErr *ExecutionError
	assert.True(t, errors.As(err, &execErr))
	assert.Equal(t, "block", execErr.msg)
}
