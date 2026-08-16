package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHelpOverlayToggle verifies that pressing '?' opens the help overlay and
// pressing '?' again closes it.
func TestHelpOverlayToggle(t *testing.T) {
	m := newTestModel(make(chan AgentEvent, 1))

	// '?' opens the overlay.
	m.handleKey(runeKey('?'))
	assert.True(t, m.helpOverlay, "helpOverlay should be true after pressing '?'")

	// '?' closes the overlay.
	m.handleKey(runeKey('?'))
	assert.False(t, m.helpOverlay, "helpOverlay should be false after pressing '?' again")
}

// TestHelpOverlayEscClose verifies that pressing Esc closes an open help overlay.
func TestHelpOverlayEscClose(t *testing.T) {
	m := newTestModel(make(chan AgentEvent, 1))

	// Open the overlay.
	m.handleKey(runeKey('?'))
	require.True(t, m.helpOverlay)

	// Esc closes it (does not quit the app).
	cmd := m.handleKey(keyMsg(tea.KeyEsc))
	assert.Nil(t, cmd, "Esc should close the overlay, not quit")
	assert.False(t, m.helpOverlay, "helpOverlay should be false after Esc")
	assert.False(t, m.quitting, "Esc on overlay must not quit the app")
}

// TestHelpOverlayContent verifies the rendered overlay contains all the
// expected shortcut descriptions.
func TestHelpOverlayContent(t *testing.T) {
	m := newTestModel(make(chan AgentEvent, 1))
	m.mu.Lock()
	m.helpOverlay = true
	out := m.renderHelpOverlay()
	m.mu.Unlock()

	plain := stripEscape(out)
	for _, want := range []string{
		"Toggle steer mode",
		"Pause/resume agent",
		"Quit",
		"Expand all tool calls",
		"Follow-up mode",
		"Show this help",
		"Force quit",
	} {
		assert.True(t, strings.Contains(plain, want),
			"renderHelpOverlay should contain %q, got: %s", want, plain)
	}
}

// TestHelpOverlayViewRendering verifies that View returns the help overlay
// content (not the normal view) when helpOverlay is true.
func TestHelpOverlayViewRendering(t *testing.T) {
	m := newTestModel(make(chan AgentEvent, 1))
	// Add some content to the normal view.
	m.addEntry(ContentTypeStatus, "normal-content", "")

	// Open the overlay.
	m.handleKey(runeKey('?'))
	require.True(t, m.helpOverlay)

	view := m.View()
	plain := stripEscape(view)
	assert.Contains(t, plain, "Keyboard Shortcuts")
	// The normal content should not appear while the overlay is open.
	assert.False(t, strings.Contains(plain, "normal-content"),
		"normal view content should be hidden behind the overlay")
}

// TestHelpOverlayModalIgnoresKeys verifies that while the overlay is open,
// other keys (q, Space, Tab, etc.) are ignored.
func TestHelpOverlayModalIgnoresKeys(t *testing.T) {
	m := newTestModel(make(chan AgentEvent, 1))
	m.handleKey(runeKey('?'))
	require.True(t, m.helpOverlay)

	// 'q' should be ignored (not quit).
	cmd := m.handleKey(runeKey('q'))
	assert.Nil(t, cmd, "q should be ignored while overlay is open")
	assert.True(t, m.helpOverlay, "overlay should stay open")
	assert.False(t, m.quitting, "q must not quit while overlay is open")

	// Space should be ignored (not toggle pause).
	m.handleKey(keyMsg(tea.KeySpace))
	assert.True(t, m.helpOverlay, "overlay should stay open after Space")
	assert.False(t, m.paused, "Space must not toggle pause while overlay is open")

	// Tab should be ignored (not enter steer mode).
	m.handleKey(keyMsg(tea.KeyTab))
	assert.True(t, m.helpOverlay, "overlay should stay open after Tab")
	assert.False(t, m.steerInputMode, "Tab must not enter steer mode while overlay is open")
}

// TestHelpOverlayDoesNotInterfereWithModes verifies that in steer input mode,
// '?' is typed into the steer buffer instead of triggering the help overlay.
func TestHelpOverlayDoesNotInterfereWithModes(t *testing.T) {
	m := newTestModel(make(chan AgentEvent, 1))

	// Enter steer mode.
	m.handleKey(keyMsg(tea.KeyTab))
	require.True(t, m.steerInputMode)

	// Type '?' — it should go into the steer buffer, not open the overlay.
	m.handleKey(runeKey('?'))
	assert.False(t, m.helpOverlay, "'?' must not open overlay in steer mode")
	assert.Contains(t, m.steerInput, "?", "'?' should be typed into the steer buffer")
}

// TestHelpOverlayDoesNotInterfereWithFollowUp verifies that in follow-up input
// mode, '?' is typed into the follow-up buffer instead of triggering the help
// overlay.
func TestHelpOverlayDoesNotInterfereWithFollowUp(t *testing.T) {
	m := newTestModel(make(chan AgentEvent, 1))

	// Enter follow-up mode.
	m.handleKey(runeKey('f'))
	require.True(t, m.followUpInputMode)

	// Type '?' — it should go into the follow-up buffer, not open the overlay.
	m.handleKey(runeKey('?'))
	assert.False(t, m.helpOverlay, "'?' must not open overlay in follow-up mode")
	assert.Contains(t, m.followUpInput, "?", "'?' should be typed into the follow-up buffer")
}

// TestHelpOverlayDoesNotInterfereWithApproval verifies that during a pending
// approval, '?' does not trigger the help overlay.
func TestHelpOverlayDoesNotInterfereWithApproval(t *testing.T) {
	m := newTestModel(make(chan AgentEvent, 1))
	m.mu.Lock()
	m.pendingApproval = &ApprovalRequest{
		ResponseCh: make(chan ApprovalResponse, 1),
	}
	m.mu.Unlock()

	// '?' should be intercepted by the approval handler, not open the overlay.
	m.handleKey(runeKey('?'))
	assert.False(t, m.helpOverlay, "'?' must not open overlay during approval")
}

// TestHelpOverlayCtrlCNotIntercepted verifies that Ctrl+C is ignored while the
// overlay is open (modal), consistent with the overlay being modal — only Esc
// and '?' close it.
func TestHelpOverlayCtrlCNotIntercepted(t *testing.T) {
	m := newTestModel(make(chan AgentEvent, 1))
	m.handleKey(runeKey('?'))
	require.True(t, m.helpOverlay)

	// Ctrl+C should be ignored while the overlay is open (modal).
	cmd := m.handleKey(keyMsg(tea.KeyCtrlC))
	assert.Nil(t, cmd, "Ctrl+C should be ignored while overlay is open")
	assert.True(t, m.helpOverlay, "overlay should stay open after Ctrl+C")
	assert.False(t, m.quitting, "Ctrl+C must not quit while overlay is open")
}
