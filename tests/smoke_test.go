// Package tests contains end-to-end smoke tests for the go-cli binary.
package tests

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/pengjunchen/go-cli/internal/cli"
)

// TestVersionSmoke runs the built-in "version" command end to end through the
// CLI entry point and asserts it produces non-empty, version-shaped output.
func TestVersionSmoke(t *testing.T) {
	var out bytes.Buffer
	err := cli.Run(context.Background(), cli.NewDefaultConfig(false), []string{"version"}, &out)
	if err != nil {
		t.Fatalf("cli.Run(version): %v", err)
	}
	if !strings.HasPrefix(out.String(), "go-cli ") {
		t.Fatalf("unexpected version output: %q", out.String())
	}
}

// TestHelpSmoke runs the built-in "help" command and asserts usage text is
// printed without error.
func TestHelpSmoke(t *testing.T) {
	var out bytes.Buffer
	err := cli.Run(context.Background(), cli.NewDefaultConfig(false), []string{"help"}, &out)
	if err != nil {
		t.Fatalf("cli.Run(help): %v", err)
	}
	if !strings.Contains(out.String(), "Usage:") {
		t.Fatalf("expected usage text, got: %q", out.String())
	}
}
