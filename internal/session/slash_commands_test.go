package session

import (
	"context"
	"path/filepath"
	"testing"
	"time"

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

// --- /resume list selector tests ---

func TestSessionSlashHandler_ResumeListSelector(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	storeDir := t.TempDir()

	// Create two sessions in the store directory.
	store1 := NewJSONLSessionStore(storeDir)
	require.NoError(t, store1.SetSessionID("sess-alpha", true))
	require.NoError(t, store1.Append(context.Background(), &SessionEntry{
		ID: "a1", Type: EntryTypeUser, Content: "alpha message",
		Timestamp: time.Now(),
	}))
	require.NoError(t, store1.Save(context.Background()))
	require.NoError(t, store1.Close())

	time.Sleep(10 * time.Millisecond)

	store2 := NewJSONLSessionStore(storeDir)
	require.NoError(t, store2.SetSessionID("sess-beta", true))
	require.NoError(t, store2.Append(context.Background(), &SessionEntry{
		ID: "b1", Type: EntryTypeUser, Content: "beta message",
		Timestamp: time.Now(),
	}))
	require.NoError(t, store2.Save(context.Background()))
	require.NoError(t, store2.Close())

	// Create a handler with the store and call /resume without args.
	store3 := NewJSONLSessionStore(storeDir)
	tree := NewDefaultSessionTree()
	handler := NewSessionSlashHandler(tree, store3)

	out, err := handler.Handle(context.Background(), SlashCommand{Name: "resume"})
	require.NoError(t, err)
	assert.Contains(t, out, "Available sessions")
	assert.Contains(t, out, "sess-alpha")
	assert.Contains(t, out, "sess-beta")
	assert.Contains(t, out, "alpha message")
	assert.Contains(t, out, "beta message")

	require.NoError(t, store3.Close())
}

func TestSessionSlashHandler_ResumeListSelectorEmpty(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	storeDir := t.TempDir()
	store := NewJSONLSessionStore(storeDir)
	tree := NewDefaultSessionTree()
	handler := NewSessionSlashHandler(tree, store)

	out, err := handler.Handle(context.Background(), SlashCommand{Name: "resume"})
	require.NoError(t, err)
	assert.Contains(t, out, "No previous sessions")

	require.NoError(t, store.Close())
}

func TestSessionSlashHandler_ResumeListSelectorLegacyStore(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	// Legacy (single-file) store does not implement SessionLister in a
	// meaningful way — ListSessions returns nil. The /resume without args
	// should still list "No previous sessions" since the list is empty.
	path := filepath.Join(t.TempDir(), "session.jsonl")
	store := NewJSONLSessionStore(path)
	tree := NewDefaultSessionTree()
	handler := NewSessionSlashHandler(tree, store)

	out, err := handler.Handle(context.Background(), SlashCommand{Name: "resume"})
	require.NoError(t, err)
	assert.Contains(t, out, "No previous sessions")

	require.NoError(t, store.Close())
}
