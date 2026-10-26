//go:build !headless

package cli

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/tui"
)

func TestThemeHandler(t *testing.T) {
	// --- List themes (no args) ---
	c, buf := newTestCmd()
	mgr := tui.NewThemeManager()
	sc := &slashContext{out: buf, themeMgr: mgr}
	c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "theme"}, sc)
	output := buf.String()
	assert.Contains(t, output, "dark")
	assert.Contains(t, output, "light")
	assert.Contains(t, output, "monokai")
	assert.Contains(t, output, "solarized")
	assert.Contains(t, output, "(active)") // dark should be marked active

	// --- Switch to a valid theme ---
	buf.Reset()
	c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "theme", Args: []string{"light"}}, sc)
	assert.Contains(t, buf.String(), "Theme switched to: light")
	assert.Equal(t, "light", mgr.CurrentName())

	// --- Switch to an invalid theme ---
	buf.Reset()
	c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "theme", Args: []string{"nonexistent"}}, sc)
	output = buf.String()
	assert.Contains(t, output, "unknown theme")
	assert.Contains(t, output, "Available themes:")
	// Current theme should be unchanged.
	assert.Equal(t, "light", mgr.CurrentName())

	// --- Nil themeMgr graceful degradation ---
	buf.Reset()
	scNil := &slashContext{out: buf, themeMgr: nil}
	c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "theme"}, scNil)
	assert.Contains(t, buf.String(), "only available in interactive TUI mode")
}
