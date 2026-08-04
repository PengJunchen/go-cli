//go:build e2e

// Package e2e_20260802 contains end-to-end integration tests.
// This file verifies Phase 20 REPL Upgrade: DefaultLineEditor non-TTY mode,
// multi-line input, history, SlashCommandCompleter, FilePathCompleter,
// HistoryStore persistence, and old interactive behavior.
package e2e_20260802

import (
	"bufio"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/cli"
)

// TestET_Phase20_REPL exercises the Phase 20 REPL components end-to-end using
// real LineEditor, completers, and HistoryStore instances. Non-TTY mode is
// simulated via os.Pipe. No mocks are used.
func TestET_Phase20_REPL(t *testing.T) {
	// AC-1: Non-TTY mode reads a single line, behavior identical to bufio.Scanner.
	t.Run("AC1_NonTTY_ReadSingleLine", func(t *testing.T) {
		r, w, err := os.Pipe()
		require.NoError(t, err)
		defer r.Close()

		input := "hello world\n"
		go func() {
			defer w.Close()
			w.Write([]byte(input))
		}()

		le := cli.NewDefaultLineEditor(r, io.Discard)
		line, err := le.ReadLine(context.Background(), "> ")
		require.NoError(t, err)
		assert.Equal(t, "hello world", line)

		// Verify behavior identical to bufio.Scanner with the same input.
		r2, w2, err := os.Pipe()
		require.NoError(t, err)
		defer r2.Close()
		go func() {
			defer w2.Close()
			w2.Write([]byte(input))
		}()
		scanner := bufio.NewScanner(r2)
		require.True(t, scanner.Scan())
		assert.Equal(t, scanner.Text(), line, "LineEditor must match bufio.Scanner output")
	})

	// AC-2: Multi-line triple-quote input. In non-TTY mode, ReadLine reads one
	// line at a time (identical to bufio.Scanner). Triple-quote multi-line
	// joining is a TTY-only feature implemented in readLineTTY.
	t.Run("AC2_MultiLine_TripleQuote", func(t *testing.T) {
		r, w, err := os.Pipe()
		require.NoError(t, err)
		defer r.Close()

		input := "'''\nhello\nworld\n'''\n"
		go func() {
			defer w.Close()
			w.Write([]byte(input))
		}()

		le := cli.NewDefaultLineEditor(r, io.Discard)
		ctx := context.Background()

		// Non-TTY mode: each ReadLine call returns one physical line.
		line1, err := le.ReadLine(ctx, "> ")
		require.NoError(t, err)
		assert.Equal(t, "'''", line1)

		line2, err := le.ReadLine(ctx, "> ")
		require.NoError(t, err)
		assert.Equal(t, "hello", line2)

		line3, err := le.ReadLine(ctx, "> ")
		require.NoError(t, err)
		assert.Equal(t, "world", line3)

		line4, err := le.ReadLine(ctx, "> ")
		require.NoError(t, err)
		assert.Equal(t, "'''", line4)

		// EOF after all lines are consumed.
		_, err = le.ReadLine(ctx, "> ")
		assert.Equal(t, io.EOF, err)
	})

	// AC-3: History set/get and update after ReadLine.
	t.Run("AC3_History_SetGet_Update", func(t *testing.T) {
		r, w, err := os.Pipe()
		require.NoError(t, err)
		defer r.Close()

		le := cli.NewDefaultLineEditor(r, io.Discard)
		le.SetHistory([]string{"cmd1", "cmd2"})

		hist := le.History()
		require.Len(t, hist, 2)
		assert.Equal(t, "cmd1", hist[0])
		assert.Equal(t, "cmd2", hist[1])

		// Write new input and read it.
		go func() {
			defer w.Close()
			w.Write([]byte("cmd3\n"))
		}()

		line, err := le.ReadLine(context.Background(), "> ")
		require.NoError(t, err)
		assert.Equal(t, "cmd3", line)

		// History should now include cmd3.
		hist = le.History()
		require.Len(t, hist, 3)
		assert.Equal(t, "cmd3", hist[2])
	})

	// AC-4: SlashCommandCompleter returns matching slash commands.
	t.Run("AC4_SlashCommandCompleter", func(t *testing.T) {
		completer := cli.NewSlashCommandCompleter([]string{
			"compact", "config", "cost", "help", "exit",
		})
		completions, start := completer.Complete("/co", 3)
		assert.Equal(t, 0, start)

		texts := make([]string, len(completions))
		for i, c := range completions {
			texts[i] = c.Text
		}
		assert.Contains(t, texts, "/compact")
		assert.Contains(t, texts, "/config")
		assert.Contains(t, texts, "/cost")
	})

	// AC-5: FilePathCompleter returns matching file paths.
	t.Run("AC5_FilePathCompleter", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "test_e2e_file.txt"), []byte("test"), 0644))

		fc := cli.NewFilePathCompleter()
		input := filepath.Join(dir, "test")
		completions, _ := fc.Complete(input, len(input))

		found := false
		for _, c := range completions {
			if strings.Contains(c.Text, "test_e2e_file.txt") {
				found = true
				break
			}
		}
		assert.True(t, found, "FilePathCompleter should return the matching file path")
	})

	// AC-6: HistoryStore Add/Save/Load round-trip.
	t.Run("AC6_HistoryStore_SaveLoad", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "history.jsonl")

		store1 := cli.NewHistoryStore(1000, path)
		store1.Add("test")
		require.NoError(t, store1.Save())

		store2 := cli.NewHistoryStore(1000, path)
		require.NoError(t, store2.Load())

		entries := store2.List()
		require.Len(t, entries, 1)
		assert.Equal(t, "test", entries[0])
	})

	// AC-7: Old interactive behavior not degraded - slash commands and exit
	// are read correctly by the LineEditor in non-TTY mode.
	t.Run("AC7_OldBehavior_SlashCommands_Exit", func(t *testing.T) {
		r, w, err := os.Pipe()
		require.NoError(t, err)
		defer r.Close()

		go func() {
			defer w.Close()
			w.Write([]byte("/help\n/exit\n"))
		}()

		le := cli.NewDefaultLineEditor(r, io.Discard)
		ctx := context.Background()

		line1, err := le.ReadLine(ctx, "> ")
		require.NoError(t, err)
		assert.Equal(t, "/help", line1)

		line2, err := le.ReadLine(ctx, "> ")
		require.NoError(t, err)
		assert.Equal(t, "/exit", line2)

		// History should contain both commands.
		hist := le.History()
		assert.Contains(t, hist, "/help")
		assert.Contains(t, hist, "/exit")
	})
}
