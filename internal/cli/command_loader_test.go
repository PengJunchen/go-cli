package cli

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/config"
)

// newConfigWithCommandsDir returns a minimal *config.Config with the Commands
// directory set to dir. This is used to test buildDynamicRegistry without
// relying on auto-discovery.
func newConfigWithCommandsDir(dir string) *config.Config {
	return &config.Config{
		Commands: config.CommandsConfig{Dir: dir},
	}
}

func TestParseCommandFrontmatter_WithFrontmatter(t *testing.T) {
	lines := []string{
		"---",
		"name: review",
		"description: Review code changes",
		"---",
		"Review the following code and provide feedback.",
	}
	cmd, err := parseCommandFrontmatter(lines)
	require.NoError(t, err)
	assert.Equal(t, "review", cmd.name)
	assert.Equal(t, "Review code changes", cmd.description)
	assert.Equal(t, "Review the following code and provide feedback.", cmd.content)
}

func TestParseCommandFrontmatter_NoFrontmatter(t *testing.T) {
	lines := []string{
		"Just a plain markdown body.",
		"Second line.",
	}
	cmd, err := parseCommandFrontmatter(lines)
	require.NoError(t, err)
	assert.Equal(t, "", cmd.name)
	assert.Equal(t, "", cmd.description)
	assert.Equal(t, "Just a plain markdown body.\nSecond line.", cmd.content)
}

func TestParseCommandFrontmatter_QuotedDescription(t *testing.T) {
	lines := []string{
		"---",
		"name: test",
		"description: \"A test command with spaces\"",
		"---",
		"Body",
	}
	cmd, err := parseCommandFrontmatter(lines)
	require.NoError(t, err)
	assert.Equal(t, "test", cmd.name)
	assert.Equal(t, "A test command with spaces", cmd.description)
}

func TestCommandLoader_Load(t *testing.T) {
	dir := t.TempDir()
	cmdFile := filepath.Join(dir, "review.md")
	content := "---\nname: review\ndescription: Review code\n---\nPlease review this code.\n"
	require.NoError(t, os.WriteFile(cmdFile, []byte(content), 0644))

	loader := &MarkdownCommandLoader{}
	cmd, err := loader.Load(context.Background(), cmdFile)
	require.NoError(t, err)
	assert.Equal(t, "review", cmd.name)
	assert.Equal(t, "Review code", cmd.description)
	assert.Equal(t, "Please review this code.", cmd.content)
}

func TestCommandLoader_Load_DefaultNameFromFilename(t *testing.T) {
	dir := t.TempDir()
	// No frontmatter — name defaults to filename.
	cmdFile := filepath.Join(dir, "deploy.md")
	content := "Deploy the application to production."
	require.NoError(t, os.WriteFile(cmdFile, []byte(content), 0644))

	loader := &MarkdownCommandLoader{}
	cmd, err := loader.Load(context.Background(), cmdFile)
	require.NoError(t, err)
	assert.Equal(t, "deploy", cmd.name)
	assert.Equal(t, "Deploy the application to production.", cmd.content)
}

func TestCommandLoader_LoadDir(t *testing.T) {
	dir := t.TempDir()
	// Create three command files.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "review.md"),
		[]byte("---\nname: review\ndescription: Review\n---\nReview code\n"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "deploy.md"),
		[]byte("---\nname: deploy\ndescription: Deploy\n---\nDeploy app\n"), 0644))
	// Non-md file should be skipped.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "readme.txt"),
		[]byte("not a command"), 0644))

	loader := &MarkdownCommandLoader{}
	cmds, err := loader.LoadDir(context.Background(), dir)
	require.NoError(t, err)
	assert.Len(t, cmds, 2)

	names := map[string]bool{}
	for _, c := range cmds {
		names[c.name] = true
	}
	assert.True(t, names["review"])
	assert.True(t, names["deploy"])
}

func TestCommandLoader_LoadDir_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	loader := &MarkdownCommandLoader{}
	cmds, err := loader.LoadDir(context.Background(), dir)
	require.NoError(t, err)
	assert.Empty(t, cmds)
}

func TestMarkdownCommandHandler_Name(t *testing.T) {
	h := &MarkdownCommandHandler{cmd: MarkdownCommand{name: "review", description: "desc"}}
	assert.Equal(t, "review", h.Name())
	assert.Equal(t, "desc", h.Description())
}

func TestMarkdownCommandHandler_Handle_SetsPendingInput(t *testing.T) {
	h := &MarkdownCommandHandler{cmd: MarkdownCommand{
		name:    "review",
		content: "Review this code carefully.",
	}}
	sc := &slashContext{}
	err := h.Handle(context.Background(), nil, sc)
	require.NoError(t, err)
	assert.Equal(t, "Review this code carefully.", sc.pendingInput)
}

func TestMarkdownCommandHandler_Handle_WithArgs(t *testing.T) {
	h := &MarkdownCommandHandler{cmd: MarkdownCommand{
		name:    "review",
		content: "Review this code carefully.",
	}}
	sc := &slashContext{}
	err := h.Handle(context.Background(), []string{"src/main.go", "src/util.go"}, sc)
	require.NoError(t, err)
	assert.Contains(t, sc.pendingInput, "Review this code carefully.")
	assert.Contains(t, sc.pendingInput, "src/main.go src/util.go")
}

func TestBuildDynamicRegistry_NoCommandsDir(t *testing.T) {
	// With nil config and no .go-cli/commands dir, should return built-in commands only.
	reg := buildDynamicRegistry(nil)
	require.NotNil(t, reg)

	// Verify built-in commands are present.
	h, ok := reg.Lookup("help")
	require.True(t, ok)
	assert.Equal(t, "help", h.Name())
}

func TestBuildDynamicRegistry_CustomCommands(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "review.md"),
		[]byte("---\nname: review\ndescription: Review code\n---\nReview code\n"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "deploy.md"),
		[]byte("---\nname: deploy\ndescription: Deploy app\n---\nDeploy app\n"), 0644))

	// Build a minimal config pointing to the temp dir.
	cfg := newConfigWithCommandsDir(dir)
	reg := buildDynamicRegistry(cfg)
	require.NotNil(t, reg)

	// Custom commands should be registered.
	h, ok := reg.Lookup("review")
	require.True(t, ok)
	assert.Equal(t, "review", h.Name())
	assert.Equal(t, "Review code", h.Description())

	h, ok = reg.Lookup("deploy")
	require.True(t, ok)
	assert.Equal(t, "deploy", h.Name())

	// Built-in commands should still be present.
	_, ok = reg.Lookup("help")
	assert.True(t, ok)
}

func TestBuildDynamicRegistry_BuiltinPriority(t *testing.T) {
	dir := t.TempDir()
	// Create a custom command with the same name as a built-in ("help").
	require.NoError(t, os.WriteFile(filepath.Join(dir, "help.md"),
		[]byte("---\nname: help\ndescription: Custom help\n---\nCustom help body\n"), 0644))

	cfg := newConfigWithCommandsDir(dir)
	reg := buildDynamicRegistry(cfg)
	require.NotNil(t, reg)

	// Built-in "help" should take priority.
	h, ok := reg.Lookup("help")
	require.True(t, ok)
	assert.Equal(t, "help", h.Name())
	// The built-in HelpHandler has a non-empty description from the handler,
	// while the custom one would have "Custom help". Check it's not the custom.
	assert.NotEqual(t, "Custom help", h.Description())
}

func TestBuildDynamicRegistry_ConfigDirTakesPriority(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "custom.md"),
		[]byte("---\nname: custom\ndescription: Custom\n---\nBody\n"), 0644))

	cfg := newConfigWithCommandsDir(dir)
	reg := buildDynamicRegistry(cfg)
	require.NotNil(t, reg)

	h, ok := reg.Lookup("custom")
	require.True(t, ok)
	assert.Equal(t, "custom", h.Name())
}

// --- Malformed frontmatter edge-case tests ---

func TestParseCommandFrontmatter_MissingClosingDelimiter(t *testing.T) {
	// File starts with --- but has no closing delimiter.
	// The entire content should be treated as body.
	lines := []string{
		"---",
		"name: review",
		"This line is not valid frontmatter but should be in body.",
	}
	cmd, err := parseCommandFrontmatter(lines)
	require.NoError(t, err)
	// No closing delimiter means no frontmatter is parsed; all lines are body.
	assert.Equal(t, "", cmd.name)
	assert.Contains(t, cmd.content, "---")
	assert.Contains(t, cmd.content, "name: review")
}

func TestParseCommandFrontmatter_EmptyFile(t *testing.T) {
	// Zero lines — should return an empty command with no error.
	cmd, err := parseCommandFrontmatter(nil)
	require.NoError(t, err)
	assert.Equal(t, "", cmd.name)
	assert.Equal(t, "", cmd.content)
}

func TestParseCommandFrontmatter_OnlyDelimiters(t *testing.T) {
	// Frontmatter block with no key-value pairs.
	lines := []string{"---", "---", "Body content"}
	cmd, err := parseCommandFrontmatter(lines)
	require.NoError(t, err)
	assert.Equal(t, "", cmd.name)
	assert.Equal(t, "Body content", cmd.content)
}

func TestParseCommandFrontmatter_EmptyBody(t *testing.T) {
	// Frontmatter with no body content.
	lines := []string{"---", "name: empty", "description: Empty body", "---"}
	cmd, err := parseCommandFrontmatter(lines)
	require.NoError(t, err)
	assert.Equal(t, "empty", cmd.name)
	assert.Equal(t, "Empty body", cmd.description)
	assert.Equal(t, "", cmd.content)
}

// --- Empty content diagnostic test ---

func TestMarkdownCommandHandler_Handle_EmptyContent(t *testing.T) {
	h := &MarkdownCommandHandler{cmd: MarkdownCommand{name: "empty"}}
	var out bytes.Buffer
	sc := &slashContext{out: &out}
	err := h.Handle(context.Background(), nil, sc)
	require.NoError(t, err)
	// Should NOT set pendingInput.
	assert.Equal(t, "", sc.pendingInput)
	// Should write a diagnostic message.
	assert.Contains(t, out.String(), "has no content")
	assert.Contains(t, out.String(), "/empty")
}

// --- Integration test: pendingInput → REPL loop ---

// TestPendingInput_REPLIntegration verifies that when a custom Markdown slash
// command sets pendingInput, the REPL loop consumes it and sends it to the
// agent as a user message. This is the end-to-end test for AC-4.
func TestPendingInput_REPLIntegration(t *testing.T) {
	// Create a custom command directory with a "review" command.
	cmdDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(cmdDir, "review.md"),
		[]byte("---\nname: review\ndescription: Review code\n---\nPlease review the provided code carefully.\n"), 0644))

	// Mock LLM server that captures the request body to verify the prompt was sent.
	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body) //nolint:errcheck
		capturedBody = body
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)) //nolint:errcheck,gosec
	}))
	defer srv.Close()

	var out bytes.Buffer
	// User types "/review" then "exit".
	in := strings.NewReader("/review\nexit\n")
	cmd := newInteractiveCmd(in, &out)
	cfg := &config.Config{
		Provider: config.ProviderConfig{
			Name:    "test",
			BaseURL: srv.URL,
			APIKey:  "test",
			Model:   "test-model",
		},
		Commands: config.CommandsConfig{Dir: cmdDir},
	}

	err := cmd.Run(t.Context(), cfg, nil)
	require.NoError(t, err)
	// Verify the REPL sent the custom command's prompt to the agent.
	require.NotEmpty(t, capturedBody, "mock server should have received a request")
	bodyStr := string(capturedBody)
	assert.Contains(t, bodyStr, "Please review the provided code carefully.",
		"the custom command's prompt body should be sent to the agent as a user message")
	assert.Contains(t, out.String(), "Session ended")
}
