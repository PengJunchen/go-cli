package cli

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/session"
)

func waitForTestFile(ctx context.Context, path string) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func durableTerminalOutcome(snapshot session.PreparedSessionSnapshot, runID string) core.RunOutcome {
	var outcome core.RunOutcome
	for _, entry := range snapshot.DurableEntries {
		if entry.Type == session.EntryTypeCheckpoint && entry.CheckpointKind == session.CheckpointRunTerminal && entry.RunID == runID {
			if outcome != "" {
				return ""
			}
			outcome = entry.RunOutcome
		}
	}
	return outcome
}

func selectedRunID(snapshot session.PreparedSessionSnapshot) string {
	for index := len(snapshot.DurableEntries) - 1; index >= 0; index-- {
		entry := snapshot.DurableEntries[index]
		if entry.Type == session.EntryTypeCheckpoint && entry.CheckpointKind == session.CheckpointRunTerminal {
			return entry.RunID
		}
	}
	return ""
}

func messageContents(snapshot session.PreparedSessionSnapshot) []string {
	contents := make([]string, 0, len(snapshot.PublishedView.Messages))
	for _, message := range snapshot.PublishedView.Messages {
		contents = append(contents, message.Content)
	}
	return contents
}

func newPhysicalSessionIDFromOutput(t *testing.T, output string) string {
	t.Helper()
	const prefix = "Started fresh durable session "
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, prefix) {
			id := strings.TrimSpace(strings.TrimPrefix(line, prefix))
			id = strings.TrimSuffix(id, ". The prior run and any unresolved actions remain unchanged.")
			require.NotEmpty(t, id)
			return id
		}
	}
	t.Fatalf("output does not expose the newly selected physical session ID: %q", output)
	return ""
}
