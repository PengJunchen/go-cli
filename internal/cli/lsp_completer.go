package cli

import (
	"context"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/pengjunchen/go-cli/internal/tools"
)

// LSPCompleter provides code completion suggestions from a Language Server
// Protocol server. It implements the Completer interface and bridges
// tools.CompletionItem to cli.Completion. When the LSP client is nil or a
// completion request fails, it returns an empty list (graceful degradation).
type LSPCompleter struct {
	client        tools.LSPClient
	workspaceRoot string
}

// NewLSPCompleter creates an LSPCompleter. If client is nil the completer
// is a no-op — Complete always returns nil. The workspaceRoot is used to
// construct the synthetic file URI sent to the LSP server.
func NewLSPCompleter(client tools.LSPClient, workspaceRoot string) *LSPCompleter {
	return &LSPCompleter{
		client:        client,
		workspaceRoot: workspaceRoot,
	}
}

// replBufferName is the synthetic file name used as the document URI for
// REPL completion requests. The ".go" extension ensures routing works with
// gopls and MultiLSPClient (extension-based routing).
const replBufferName = ".repl-buffer.go"

// isCodeDelimiter reports whether b is a character that terminates a code
// identifier prefix (e.g. '.', ':', '(', ',', '=', '<'). These delimiters
// mark the boundary where completion replacement should start, so that
// qualified names like "fmt.Pr" are preserved when replacing "Pr" with
// "Println".
func isCodeDelimiter(b byte) bool {
	switch b {
	case '.', ':', '(', ')', ',', '=', '<', '>', '{', '}', '[', ']', ';', '"', '\'', '`':
		return true
	}
	return isSpaceByte(b)
}

// Complete calls the LSP server for completion suggestions at the cursor
// position. The input text is sent as a single-line document via DidOpen,
// then Completion is requested at (line 0, character pos). Results are
// converted from tools.CompletionItem to cli.Completion.
//
// Graceful degradation: returns nil when the client is nil, DidOpen fails,
// Completion fails, or no items are returned.
func (c *LSPCompleter) Complete(input string, pos int) ([]Completion, int) {
	if c == nil || c.client == nil {
		return nil, 0
	}
	if pos > len(input) {
		pos = len(input)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	uri := "file://" + filepath.Join(c.workspaceRoot, replBufferName)

	if err := c.client.DidOpen(ctx, uri, input, 1); err != nil {
		slog.Debug("lsp_completer_didopen_failed", "err", err)
		return nil, 0
	}

	items, err := c.client.Completion(ctx, uri, 0, pos)
	if err != nil {
		slog.Debug("lsp_completer_completion_failed", "err", err)
		return nil, 0
	}

	// Determine the word boundary for replacement start. Stop at code
	// delimiters (., :, (, etc.) so that qualified names like "fmt.Pr"
	// only replace "Pr", preserving the "fmt." prefix.
	start := pos
	for start > 0 && !isCodeDelimiter(input[start-1]) {
		start--
	}

	completions := make([]Completion, 0, len(items))
	for _, item := range items {
		completions = append(completions, Completion{
			Text:        item.Label,
			Description: item.Detail,
		})
	}
	return completions, start
}

var _ Completer = (*LSPCompleter)(nil)
