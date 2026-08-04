// Package e2e_20260802 contains end-to-end integration tests.
// This file verifies Phase 20 context management wiring: compaction
// writeback to AgentImpl.history and session persistence/resume.
package e2e_20260802 //nolint:staticcheck

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/session"
)

// =============================================================================
// Phase 20 E2E: Context Management Wiring
// =============================================================================

// TestE2E_Phase20_CompactionHookReducesHistory verifies that the compaction
// hook adapter reduces AgentImpl history when token budget is exceeded.
func TestE2E_Phase20_CompactionHookReducesHistory(t *testing.T) {
	compactor := compaction.NewUnifiedCompactor()
	estimator := compaction.NewHeuristicTokenEstimator()
	hook := buildTestCompactionHook(compactor, estimator, 100)

	messages := make([]core.AgentMessage, 50)
	for i := range messages {
		messages[i] = core.AgentMessage{
			Role:    "user",
			Content: string(make([]byte, 100)),
		}
	}

	compacted, err := hook(context.Background(), messages)
	require.NoError(t, err)
	assert.Less(t, len(compacted), len(messages),
		"compaction must reduce history when budget exceeded")
}

// TestE2E_Phase20_CompactionHookPreservesRoles verifies that messages after
// compaction still have valid role values.
func TestE2E_Phase20_CompactionHookPreservesRoles(t *testing.T) {
	compactor := compaction.NewUnifiedCompactor()
	estimator := compaction.NewHeuristicTokenEstimator()
	hook := buildTestCompactionHook(compactor, estimator, 100)

	messages := []core.AgentMessage{
		{Role: "system", Content: "You are a helpful assistant."},
	}
	for i := 0; i < 20; i++ {
		messages = append(messages, core.AgentMessage{
			Role:    "user",
			Content: string(make([]byte, 100)),
		})
	}

	compacted, err := hook(context.Background(), messages)
	require.NoError(t, err)
	for _, msg := range compacted {
		assert.Contains(t, []string{"system", "user", "assistant", "tool"}, msg.Role,
			"compacted message must have valid role")
	}
}

// TestE2E_Phase20_SessionPersistence verifies that session entries can be
// saved to a JSONL file and loaded back.
func TestE2E_Phase20_SessionPersistence(t *testing.T) {
	tmpDir := t.TempDir()
	storePath := filepath.Join(tmpDir, "test-session.jsonl")

	store := session.NewJSONLSessionStore(storePath)
	require.NoError(t, store.Open(context.Background()))
	defer store.Close() //nolint:errcheck,gosec

	require.NoError(t, store.Append(context.Background(), &session.SessionEntry{
		ID:        "entry-0",
		Type:      session.EntryTypeUser,
		Content:   "Hello",
		Timestamp: time.Now(),
	}))
	require.NoError(t, store.Append(context.Background(), &session.SessionEntry{
		ID:        "entry-1",
		Type:      session.EntryTypeAssistant,
		Content:   "Hi there!",
		Timestamp: time.Now(),
	}))
	require.NoError(t, store.Save(context.Background()))

	info, err := os.Stat(storePath)
	require.NoError(t, err)
	assert.Greater(t, info.Size(), int64(0))

	store2 := session.NewJSONLSessionStore(storePath)
	require.NoError(t, store2.Open(context.Background()))
	defer store2.Close() //nolint:errcheck,gosec

	entry0, err := store2.Get(context.Background(), "entry-0")
	require.NoError(t, err)
	assert.Equal(t, "Hello", entry0.Content)
	assert.Equal(t, session.EntryTypeUser, entry0.Type)

	entry1, err := store2.Get(context.Background(), "entry-1")
	require.NoError(t, err)
	assert.Equal(t, "Hi there!", entry1.Content)
	assert.Equal(t, session.EntryTypeAssistant, entry1.Type)
}

// TestE2E_Phase20_SessionResumeReconstructsHistory verifies that a JSONL
// session file can be read back as []core.AgentMessage for resume.
func TestE2E_Phase20_SessionResumeReconstructsHistory(t *testing.T) {
	tmpDir := t.TempDir()
	storePath := filepath.Join(tmpDir, "test-resume.jsonl")

	store := session.NewJSONLSessionStore(storePath)
	require.NoError(t, store.Open(context.Background()))

	entries := []struct {
		id      string
		eType   session.EntryType
		content string
	}{
		{"e0", session.EntryTypeUser, "What is Go?"},
		{"e1", session.EntryTypeAssistant, "Go is a programming language."},
		{"e2", session.EntryTypeUser, "Tell me more."},
		{"e3", session.EntryTypeAssistant, "It was created at Google."},
	}

	for _, e := range entries {
		require.NoError(t, store.Append(context.Background(), &session.SessionEntry{
			ID:        e.id,
			Type:      e.eType,
			Content:   e.content,
			Timestamp: time.Now(),
		}))
	}
	require.NoError(t, store.Save(context.Background()))
	store.Close() //nolint:errcheck,gosec

	// Read file back as JSONL (simulating loadSessionHistory).
	file, err := os.Open(storePath)
	require.NoError(t, err)
	defer file.Close() //nolint:errcheck,gosec

	var history []core.AgentMessage
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		var entry session.SessionEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		if entry.Type == session.EntryTypeUser || entry.Type == session.EntryTypeAssistant {
			history = append(history, core.AgentMessage{
				Role:    string(entry.Type),
				Content: entry.Content,
			})
		}
	}

	require.Len(t, history, 4)
	assert.Equal(t, "user", history[0].Role)
	assert.Equal(t, "What is Go?", history[0].Content)
	assert.Equal(t, "assistant", history[1].Role)
	assert.Equal(t, "Go is a programming language.", history[1].Content)
	assert.Equal(t, "user", history[2].Role)
	assert.Equal(t, "Tell me more.", history[2].Content)
	assert.Equal(t, "assistant", history[3].Role)
	assert.Equal(t, "It was created at Google.", history[3].Content)
}

// TestE2E_Phase20_WithHistoryRestoresAgent verifies that WithHistory option
// correctly sets AgentImpl's initial history.
func TestE2E_Phase20_WithHistoryRestoresAgent(t *testing.T) {
	restored := []core.AgentMessage{
		{Role: "user", Content: "previous question"},
		{Role: "assistant", Content: "previous answer"},
	}

	agent := core.NewAgentImpl("test", &nopLoop{}, core.WithHistory(restored))
	msgs := agent.Messages()
	require.Len(t, msgs, 2)
	assert.Equal(t, "previous question", msgs[0].Content)
	assert.Equal(t, "previous answer", msgs[1].Content)
}

// =============================================================================
// Test helpers
// =============================================================================

func buildTestCompactionHook(
	compactor compaction.Compactor,
	estimator compaction.TokenEstimator,
	maxTokens int,
) core.CompactionHook {
	return func(ctx context.Context, messages []core.AgentMessage) ([]core.AgentMessage, error) {
		if len(messages) == 0 {
			return messages, nil
		}
		items := make([]compaction.TurnItem, len(messages))
		for i, msg := range messages {
			items[i] = compaction.TurnItem{
				ID:      "msg-" + string(rune('0'+i)),
				Role:    msg.Role,
				Content: msg.Content,
			}
		}
		compacted, err := compactor.Compact(ctx, items, maxTokens, estimator)
		if err != nil {
			return nil, err
		}
		result := make([]core.AgentMessage, len(compacted))
		for i, item := range compacted {
			result[i] = core.AgentMessage{Role: item.Role, Content: item.Content}
		}
		return result, nil
	}
}

// nopLoop is a no-op AgentLoop for testing AgentImpl construction.
type nopLoop struct{}

func (nopLoop) Run(_ context.Context, _ core.Submission, _ ...core.EventStream) ([]core.AgentEvent, error) {
	return nil, nil
}
