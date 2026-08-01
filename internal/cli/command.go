package cli

import (
	"context"
	"fmt"
	"io"
)

// Command is a single CLI subcommand that can be invoked by name.
type Command interface {
	// Name returns the command's name used to invoke it from the CLI.
	Name() string
	// Synopsis returns a one-line description of the command for usage output.
	Synopsis() string
	// Run executes the command with the given configuration and arguments.
	Run(ctx context.Context, cfg Config, args []string) error
}

// versionCmd implements Command and prints the CLI version.
type versionCmd struct {
	out io.Writer
}

// newVersionCmd creates a version command writing to out.
func newVersionCmd(out io.Writer) *versionCmd {
	return &versionCmd{out: out}
}

// Name implements Command.
func (c *versionCmd) Name() string { return "version" }

// Synopsis implements Command.
func (c *versionCmd) Synopsis() string { return "Print version" }

// Run implements Command.
func (c *versionCmd) Run(ctx context.Context, cfg Config, args []string) error {
	fmt.Fprintf(c.out, "go-cli %s\n", Version)
	return nil
}

// helpCmd implements Command and prints the usage message.
type helpCmd struct {
	out   io.Writer
	usage func()
}

// newHelpCmd creates a help command writing to out and calling usage.
func newHelpCmd(out io.Writer, usage func()) *helpCmd {
	return &helpCmd{out: out, usage: usage}
}

// Name implements Command.
func (c *helpCmd) Name() string { return "help" }

// Synopsis implements Command.
func (c *helpCmd) Synopsis() string { return "Print help" }

// Run implements Command.
func (c *helpCmd) Run(ctx context.Context, cfg Config, args []string) error {
	c.usage()
	return nil
}

var _ Command = (*versionCmd)(nil)
var _ Command = (*helpCmd)(nil)
