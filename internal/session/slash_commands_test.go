package session

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

func TestParseSlashCommand_Tree(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	cmd, ok := ParseSlashCommand("/tree")
	require.True(t, ok)
	assert.Equal(t, "tree", cmd.Name)
	assert.Empty(t, cmd.Args)
}

func TestParseSlashCommand_ForkWithArgs(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	cmd, ok := ParseSlashCommand("/fork branch-1")
	require.True(t, ok)
	assert.Equal(t, "fork", cmd.Name)
	require.Len(t, cmd.Args, 1)
	assert.Equal(t, "branch-1", cmd.Args[0])
}

func TestParseSlashCommand_ResumeWithArg(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	cmd, ok := ParseSlashCommand("/resume sess-123")
	require.True(t, ok)
	assert.Equal(t, "resume", cmd.Name)
	require.Len(t, cmd.Args, 1)
	assert.Equal(t, "sess-123", cmd.Args[0])
}

func TestParseSlashCommand_MultipleArgs(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	cmd, ok := ParseSlashCommand("/fork my-branch extra info")
	require.True(t, ok)
	assert.Equal(t, "fork", cmd.Name)
	require.Len(t, cmd.Args, 3)
	assert.Equal(t, "my-branch", cmd.Args[0])
}

func TestParseSlashCommand_NotASlashCommand(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	cases := []string{
		"",
		"hello world",
		"not a command",
		"/",
		"   /   ",
	}
	for _, input := range cases {
		_, ok := ParseSlashCommand(input)
		require.False(t, ok, "input %q should not be parsed as a slash command", input)
	}
}

func TestParseSlashCommand_LeadingTrailingWhitespace(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	cmd, ok := ParseSlashCommand("  /tree  ")
	require.True(t, ok)
	assert.Equal(t, "tree", cmd.Name)
}

func TestSlashCommand_String(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	assert.Equal(t, "/tree", (SlashCommand{Name: "tree"}).String())
	assert.Equal(t, "/fork branch-1", (SlashCommand{Name: "fork", Args: []string{"branch-1"}}).String())
}

func TestSessionSlashHandler_HandleTree(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tree := NewDefaultSessionTree()
	require.NoError(t, tree.Append(context.Background(), newTestEntry("e1", "", EntryTypeUser)))
	require.NoError(t, tree.Append(context.Background(), newTestEntry("e2", "e1", EntryTypeAssistant)))
	require.NoError(t, tree.MoveTo(context.Background(), "e2"))

	handler := NewSessionSlashHandler(tree, nil)
	out, err := handler.Handle(context.Background(), SlashCommand{Name: "tree"})
	require.NoError(t, err)
	assert.Contains(t, out, "e1")
	assert.Contains(t, out, "e2")
	assert.Contains(t, out, "current leaf")
}

func TestSessionSlashHandler_HandleTreeEmpty(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tree := NewDefaultSessionTree()
	handler := NewSessionSlashHandler(tree, nil)
	out, err := handler.Handle(context.Background(), SlashCommand{Name: "tree"})
	require.NoError(t, err)
	assert.Contains(t, out, "empty")
}

func TestSessionSlashHandler_HandleFork(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tree := NewDefaultSessionTree().(*DefaultSessionTree) //nolint:errcheck
	require.NoError(t, tree.Append(context.Background(), newTestEntry("e1", "", EntryTypeUser)))
	require.NoError(t, tree.Append(context.Background(), newTestEntry("e2", "e1", EntryTypeUser)))
	require.NoError(t, tree.MoveTo(context.Background(), "e2"))

	handler := NewSessionSlashHandler(tree, nil)
	out, err := handler.Handle(context.Background(), SlashCommand{Name: "fork", Args: []string{"my-branch"}})
	require.NoError(t, err)
	assert.Contains(t, out, "my-branch")

	// Verify the branch was registered.
	meta, ok := tree.BranchMetaFor("my-branch")
	require.True(t, ok)
	assert.Equal(t, "e2", meta.BaseLeafID)
}

func TestSessionSlashHandler_HandleForkDefaultName(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tree := NewDefaultSessionTree().(*DefaultSessionTree) //nolint:errcheck
	require.NoError(t, tree.Append(context.Background(), newTestEntry("e1", "", EntryTypeUser)))

	handler := NewSessionSlashHandler(tree, nil)
	out, err := handler.Handle(context.Background(), SlashCommand{Name: "fork"})
	require.NoError(t, err)
	assert.Contains(t, out, "fork-e1")
}

func TestSessionSlashHandler_HandleForkEmptyTree(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tree := NewDefaultSessionTree()
	handler := NewSessionSlashHandler(tree, nil)
	_, err := handler.Handle(context.Background(), SlashCommand{Name: "fork", Args: []string{"b1"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty tree")
}

func TestSessionSlashHandler_HandleResume(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tree := NewDefaultSessionTree().(*DefaultSessionTree) //nolint:errcheck
	require.NoError(t, tree.Append(context.Background(), newTestEntry("e1", "", EntryTypeUser)))
	require.NoError(t, tree.Append(context.Background(), newTestEntry("e2", "e1", EntryTypeUser)))
	require.NoError(t, tree.Append(context.Background(), newTestEntry("e3", "e2", EntryTypeUser)))

	// Move to e3.
	require.NoError(t, tree.MoveTo(context.Background(), "e3"))
	assert.Equal(t, "e3", tree.CurrentLeaf())

	// Move back to e1.
	handler := NewSessionSlashHandler(tree, nil)
	out, err := handler.Handle(context.Background(), SlashCommand{Name: "resume", Args: []string{"e1"}})
	require.NoError(t, err)
	assert.Contains(t, out, "e1")
	assert.Equal(t, "e1", tree.CurrentLeaf())
}

func TestSessionSlashHandler_HandleResumeMissingArg(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tree := NewDefaultSessionTree()
	handler := NewSessionSlashHandler(tree, nil)
	_, err := handler.Handle(context.Background(), SlashCommand{Name: "resume"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "session id")
}

func TestSessionSlashHandler_HandleResumeNotFound(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tree := NewDefaultSessionTree()
	require.NoError(t, tree.Append(context.Background(), newTestEntry("e1", "", EntryTypeUser)))

	handler := NewSessionSlashHandler(tree, nil)
	_, err := handler.Handle(context.Background(), SlashCommand{Name: "resume", Args: []string{"nonexistent"}})
	require.Error(t, err)
}

func TestSessionSlashHandler_HandleUnknownCommand(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tree := NewDefaultSessionTree()
	handler := NewSessionSlashHandler(tree, nil)
	_, err := handler.Handle(context.Background(), SlashCommand{Name: "unknown"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown slash command")
}

func TestSessionSlashHandler_NilTree(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	handler := NewSessionSlashHandler(nil, nil)
	_, err := handler.Handle(context.Background(), SlashCommand{Name: "tree"})
	require.Error(t, err)
}
