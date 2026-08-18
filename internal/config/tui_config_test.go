package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTUIConfig_Defaults(t *testing.T) {
	// The zero value leaves runtime defaults (applied elsewhere) to kick in.
	cfg := Config{}
	assert.Equal(t, "", cfg.TUI.Theme)
	assert.Equal(t, 0, cfg.TUI.WordWrap)
	assert.Equal(t, "", cfg.TUI.DiffStyle)
}

func TestTUIConfig_ValidTheme(t *testing.T) {
	cfg := defaultConfig()
	cfg.TUI = TUIConfig{Theme: "dark"}
	err := NewDefaultValidator().Validate(*cfg)
	require.NoError(t, err)
}

func TestTUIConfig_InvalidTheme(t *testing.T) {
	cfg := defaultConfig()
	cfg.TUI = TUIConfig{Theme: "rainbow"}
	err := NewDefaultValidator().Validate(*cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "theme")
}

func TestTUIConfig_ValidDiffStyle(t *testing.T) {
	cfg := defaultConfig()
	cfg.TUI = TUIConfig{DiffStyle: "split"}
	err := NewDefaultValidator().Validate(*cfg)
	require.NoError(t, err)
}

func TestTUIConfig_InvalidDiffStyle(t *testing.T) {
	cfg := defaultConfig()
	cfg.TUI = TUIConfig{DiffStyle: "weird"}
	err := NewDefaultValidator().Validate(*cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "diff_style")
}

func TestTUIConfig_NegativeWordWrap(t *testing.T) {
	cfg := defaultConfig()
	cfg.TUI = TUIConfig{WordWrap: -1}
	err := NewDefaultValidator().Validate(*cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "word_wrap")
}

func TestTUIConfig_AllValid(t *testing.T) {
	cfg := defaultConfig()
	cfg.TUI = TUIConfig{Theme: "solarized", DiffStyle: "unified", WordWrap: 80}
	err := NewDefaultValidator().Validate(*cfg)
	require.NoError(t, err)
}
