package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pengjunchen/go-cli/internal/tracing"
	"log/slog"
)

// MarkdownCommand represents a user-defined slash command loaded from a
// Markdown file with YAML frontmatter.
//
//	---
//	name: review
//	description: Review code changes
//	---
//	Review the following code changes and provide feedback:
//	$ARGUMENTS

type MarkdownCommand struct {
	name        string
	description string
	content     string // body markdown, used as the prompt template
}

// MarkdownCommandLoader loads MarkdownCommand definitions from a directory.
// Each .md file is expected to have YAML frontmatter (delimited by ---) with
// optional name and description fields, followed by a Markdown body that
// serves as the prompt template.
type MarkdownCommandLoader struct{}

// cmdFrontmatterDelimiter is the line that opens and closes the frontmatter block.
const cmdFrontmatterDelimiter = "---"

// Frontmatter field keys for custom commands.
const (
	cmdKeyName        = "name"
	cmdKeyDescription = "description"
)

// LoadDir scans dirPath for .md files and loads each as a MarkdownCommand.
// Files without valid frontmatter are skipped with a warning.
func (l *MarkdownCommandLoader) LoadDir(ctx context.Context, dirPath string) ([]MarkdownCommand, error) {
	span, spanCtx := tracing.SpanFromContext(ctx, "command.load_dir", tracing.SpanKindInternal)
	logger := tracing.NewTraceLogger(span, slog.Default())
	defer span.End()

	var cmds []MarkdownCommand
	err := filepath.WalkDir(dirPath, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		cmd, err := l.Load(spanCtx, path)
		if err != nil {
			logger.Warn("command.load_dir.skipped", "path", path, "err", err)
			return nil
		}
		cmds = append(cmds, cmd)
		return nil
	})
	if err != nil {
		span.SetStatus(tracing.SpanStatusError, err.Error())
		return nil, fmt.Errorf("command: scan dir %s: %w", dirPath, err)
	}

	span.SetAttributes(tracing.Attribute{Key: "count", Value: len(cmds)})
	span.SetStatus(tracing.SpanStatusOK, "")
	logger.Info("command.load_dir", "dir", dirPath, "count", len(cmds))
	return cmds, nil
}

// Load reads a single .md file and parses it into a MarkdownCommand.
func (l *MarkdownCommandLoader) Load(ctx context.Context, path string) (MarkdownCommand, error) {
	file, err := os.Open(path)
	if err != nil {
		return MarkdownCommand{}, fmt.Errorf("command: open %s: %w", path, err)
	}
	defer func() { _ = file.Close() }() //nolint:errcheck

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if scanErr := scanner.Err(); scanErr != nil {
		return MarkdownCommand{}, fmt.Errorf("command: read %s: %w", path, scanErr)
	}

	cmd, err := parseCommandFrontmatter(lines)
	if err != nil {
		return MarkdownCommand{}, fmt.Errorf("command: parse %s: %w", path, err)
	}

	// Default name to filename without extension.
	if cmd.name == "" {
		base := filepath.Base(path)
		cmd.name = strings.TrimSuffix(base, ".md")
	}
	return cmd, nil
}

// parseCommandFrontmatter splits lines into frontmatter and body, parsing
// only name and description from the frontmatter. The body becomes the
// command content. If no frontmatter is present, the entire file is the body
// and the name defaults to the filename.
func parseCommandFrontmatter(lines []string) (MarkdownCommand, error) {
	cmd := MarkdownCommand{}

	// If the file doesn't start with ---, treat the entire content as body.
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != cmdFrontmatterDelimiter {
		cmd.content = strings.Join(lines, "\n")
		return cmd, nil
	}

	frontEnd := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == cmdFrontmatterDelimiter {
			frontEnd = i
			break
		}
	}
	if frontEnd == -1 {
		// No closing delimiter — treat entire file as body.
		cmd.content = strings.Join(lines, "\n")
		return cmd, nil
	}

	bodyLines := lines[frontEnd+1:]
	cmd.content = strings.TrimSuffix(strings.Join(bodyLines, "\n"), "\n")

	// Parse frontmatter key:value pairs.
	for _, line := range lines[1:frontEnd] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		key, rest, ok := splitCommandKeyValue(trimmed)
		if !ok {
			continue
		}
		rest = strings.TrimSpace(rest)
		value := parseCommandScalar(rest)
		switch key {
		case cmdKeyName:
			cmd.name = value
		case cmdKeyDescription:
			cmd.description = value
		}
	}

	return cmd, nil
}

// splitCommandKeyValue splits a line at the first colon.
func splitCommandKeyValue(line string) (key, rest string, ok bool) {
	idx := strings.Index(line, ":")
	if idx <= 0 {
		return "", "", false
	}
	key = strings.TrimSpace(line[:idx])
	if key == "" {
		return "", "", false
	}
	rest = line[idx+1:]
	return key, rest, true
}

// parseCommandScalar strips surrounding quotes and trims spaces.
func parseCommandScalar(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			s = s[1 : len(s)-1]
		}
	}
	return s
}

// MarkdownCommandHandler implements SlashCommandHandler for a loaded
// MarkdownCommand. When invoked, it concatenates the command's Markdown
// content with the user-provided args and sets the result as pendingInput
// on the slashContext, so the REPL loop processes it as the next user
// message.
type MarkdownCommandHandler struct {
	cmd MarkdownCommand
}

// Name implements SlashCommandHandler.
func (h *MarkdownCommandHandler) Name() string { return h.cmd.name }

// Description implements SlashCommandHandler.
func (h *MarkdownCommandHandler) Description() string { return h.cmd.description }

// Handle implements SlashCommandHandler. It builds the prompt by concatenating
// the command's Markdown content with any args, then sets it as the pending
// input for the REPL loop to process. If the command has no content and no
// args, it writes a diagnostic instead of setting empty pendingInput.
func (h *MarkdownCommandHandler) Handle(ctx context.Context, args []string, sc *slashContext) error {
	prompt := h.cmd.content
	if len(args) > 0 {
		prompt = prompt + "\n\n" + strings.Join(args, " ")
	}
	if strings.TrimSpace(prompt) == "" {
		fmt.Fprintf(sc.out, "Command /%s has no content. Edit its Markdown file to add a prompt body.\n", h.cmd.name) //nolint:errcheck
		return nil
	}
	sc.pendingInput = prompt
	return nil
}
