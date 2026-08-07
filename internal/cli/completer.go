package cli

import (
	"os"
	"strings"
)

// SlashCommandCompleter completes slash commands (e.g. /help, /cost).
type SlashCommandCompleter struct {
	commands []string
}

// NewSlashCommandCompleter creates a completer for the given command names
// (without the leading "/").
func NewSlashCommandCompleter(commands []string) *SlashCommandCompleter {
	return &SlashCommandCompleter{commands: commands}
}

// Complete matches the input against known slash commands. Only the first
// word (command name) is completed; once a space is present the completer
// returns no matches.
func (c *SlashCommandCompleter) Complete(input string, pos int) ([]Completion, int) {
	if !strings.HasPrefix(input, "/") {
		return nil, 0
	}
	if strings.Contains(input, " ") {
		return nil, 0
	}
	var matches []Completion
	for _, cmd := range c.commands {
		if strings.HasPrefix("/"+cmd, input) {
			matches = append(matches, Completion{
				Text:        "/" + cmd,
				Description: "slash command",
			})
		}
	}
	return matches, 0
}

var _ Completer = (*SlashCommandCompleter)(nil)

// FilePathCompleter completes file paths relative to the current working
// directory using os.ReadDir.
type FilePathCompleter struct{}

// NewFilePathCompleter creates a FilePathCompleter.
func NewFilePathCompleter() *FilePathCompleter {
	return &FilePathCompleter{}
}

// Complete extracts the word around the cursor position and matches files
// in the corresponding directory.
func (f *FilePathCompleter) Complete(input string, pos int) ([]Completion, int) {
	if pos > len(input) {
		pos = len(input)
	}
	start := pos
	for start > 0 && !isSpaceByte(input[start-1]) {
		start--
	}
	word := input[start:pos]

	dir := "."
	prefix := word
	if idx := strings.LastIndex(word, "/"); idx >= 0 {
		dir = word[:idx]
		if dir == "" {
			dir = "/"
		}
		prefix = word[idx+1:]
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, start
	}

	var matches []Completion
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		text := name
		if idx := strings.LastIndex(word, "/"); idx >= 0 {
			text = word[:idx+1] + name
		}
		desc := "file"
		if entry.IsDir() {
			desc = "dir"
			text += "/"
		}
		matches = append(matches, Completion{
			Text:        text,
			Description: desc,
		})
	}
	return matches, start
}

// isSpaceByte reports whether b is a space or tab.
func isSpaceByte(b byte) bool { return b == ' ' || b == '\t' }

var _ Completer = (*FilePathCompleter)(nil)

// CompositeCompleter delegates completion to the first child that returns
// non-empty results. Children are consulted in order; nil children are
// skipped. If no child produces matches, it returns nil, 0.
type CompositeCompleter struct {
	children []Completer
}

var _ Completer = (*CompositeCompleter)(nil)

// NewCompositeCompleter creates a CompositeCompleter from the given child
// completers.
func NewCompositeCompleter(children ...Completer) *CompositeCompleter {
	return &CompositeCompleter{children: children}
}

// Complete iterates children in order, returning the first non-empty result.
func (c *CompositeCompleter) Complete(input string, pos int) ([]Completion, int) {
	for _, child := range c.children {
		if child == nil {
			continue
		}
		completions, start := child.Complete(input, pos)
		if len(completions) > 0 {
			return completions, start
		}
	}
	return nil, 0
}
