package cli

import (
	"os"
	"strings"
)

// Subcommand represents a single subcommand for tab completion.
type Subcommand struct {
	Name        string
	Description string
}

// SubcommandProvider is an optional interface that slash command handlers
// can implement to provide subcommand completion.
type SubcommandProvider interface {
	Subcommands() []Subcommand
}

// SlashCommandCompleter completes slash commands (e.g. /help, /cost).
type SlashCommandCompleter struct {
	commands []string
	reg      *SlashCommandRegistry
}

// NewSlashCommandCompleter creates a completer for the given command names
// (without the leading "/"). Descriptions default to "slash command".
func NewSlashCommandCompleter(commands []string) *SlashCommandCompleter {
	return &SlashCommandCompleter{commands: commands}
}

// NewSlashCommandCompleterFromRegistry creates a completer backed by the given
// registry. Command descriptions are fetched from the registered handlers, and
// handlers implementing SubcommandProvider gain subcommand completion after a
// space.
func NewSlashCommandCompleterFromRegistry(reg *SlashCommandRegistry) *SlashCommandCompleter {
	return &SlashCommandCompleter{
		commands: reg.Names(),
		reg:      reg,
	}
}

// Complete matches the input against known slash commands. When no space is
// present, command names are completed with their real Description (when a
// registry is available). When a space is present and the resolved handler
// implements SubcommandProvider, subcommand names are completed; otherwise nil
// is returned (backward compatible with the old behaviour).
func (c *SlashCommandCompleter) Complete(input string, pos int) ([]Completion, int) {
	if !strings.HasPrefix(input, "/") {
		return nil, 0
	}

	// Subcommand completion: input contains a space.
	if spaceIdx := strings.Index(input, " "); spaceIdx >= 0 {
		if c.reg == nil {
			return nil, 0
		}
		cmdName := strings.TrimPrefix(input[:spaceIdx], "/")
		handler, ok := c.reg.Lookup(cmdName)
		if !ok {
			return nil, 0
		}
		provider, ok := handler.(SubcommandProvider)
		if !ok {
			return nil, 0
		}
		subStart := spaceIdx + 1
		if pos > len(input) {
			pos = len(input)
		}
		if pos < subStart {
			pos = subStart
		}
		prefix := input[subStart:pos]
		var matches []Completion
		for _, sub := range provider.Subcommands() {
			if strings.HasPrefix(sub.Name, prefix) {
				matches = append(matches, Completion{
					Text:        sub.Name,
					Description: sub.Description,
				})
			}
		}
		return matches, subStart
	}

	// Command name completion.
	var matches []Completion
	for _, cmd := range c.commands {
		if strings.HasPrefix("/"+cmd, input) {
			desc := "slash command"
			if c.reg != nil {
				if h, ok := c.reg.Lookup(cmd); ok {
					desc = h.Description()
				}
			}
			matches = append(matches, Completion{
				Text:        "/" + cmd,
				Description: desc,
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
