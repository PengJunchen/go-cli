package tui

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRenderApprovalRequest_NoDiff verifies the basic approval entry rendering
// without a diff preview: it contains the [approval] header, tool name, args,
// and the y/a/n option bar.
func TestRenderApprovalRequest_NoDiff(t *testing.T) {
	req := &ApprovalRequest{
		ToolName: "write",
		Args:     map[string]any{"path": "/tmp/test.txt", "content": "hello"},
	}
	out := renderApprovalRequest(req)
	assert.Contains(t, out, "[approval]")
	assert.Contains(t, out, "write")
	assert.Contains(t, out, "path:")
	assert.Contains(t, out, "/tmp/test.txt")
	assert.Contains(t, out, "[y] once")
	assert.Contains(t, out, "[a] always")
	assert.Contains(t, out, "[n] reject")
	// No diff section when DiffPreview is empty.
	assert.NotContains(t, out, "diff:")
}

// TestRenderApprovalRequest_WithDiff verifies the approval entry includes a
// diff section when DiffPreview is non-empty.
func TestRenderApprovalRequest_WithDiff(t *testing.T) {
	req := &ApprovalRequest{
		ToolName:    "edit",
		Args:        map[string]any{"file_path": "main.go"},
		DiffPreview: "+added line\n-deleted line",
	}
	out := renderApprovalRequest(req)
	assert.Contains(t, out, "diff:")
	assert.Contains(t, out, "+added line")
	assert.Contains(t, out, "-deleted line")
	// When a diff is available, the [d] toggle option appears.
	assert.Contains(t, out, "[d] toggle diff")
}

// TestRenderApprovalRequest_NilRequest returns empty string for nil.
func TestRenderApprovalRequest_NilRequest(t *testing.T) {
	assert.Empty(t, renderApprovalRequest(nil))
}

// TestTeaModel_ApprovalEntryDisplayed verifies that when an approval request
// arrives, the model's pendingApproval is set and View() shows the approval UI.
func TestTeaModel_ApprovalEntryDisplayed(t *testing.T) {
	m := &teaModel{
		accordion: NewAccordionModel(),
		msgCh:     make(chan Msg, 1),
	}
	req := ApprovalRequest{
		ToolName:   "write",
		Args:       map[string]any{"path": "/tmp/test"},
		ResponseCh: make(chan ApprovalResponse, 1),
	}
	m.pendingApproval = &req
	view := m.View()
	assert.Contains(t, view, "[approval]")
	assert.Contains(t, view, "write")
	assert.Contains(t, view, "[y] once")
	assert.Contains(t, view, "[n] reject")
}

// TestTeaModel_ApprovalKeyY verifies that pressing 'y' sends ApprovalAllow
// and clears the pending approval.
func TestTeaModel_ApprovalKeyY(t *testing.T) {
	respCh := make(chan ApprovalResponse, 1)
	m := &teaModel{
		accordion: NewAccordionModel(),
		msgCh:     make(chan Msg, 1),
		pendingApproval: &ApprovalRequest{
			ToolName:   "bash",
			Args:       map[string]any{"command": "ls"},
			ResponseCh: respCh,
		},
	}
	cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	assert.Nil(t, cmd)
	assert.Nil(t, m.pendingApproval)
	select {
	case resp := <-respCh:
		assert.Equal(t, ApprovalAllow, resp.Decision)
	case <-time.After(time.Second):
		t.Fatal("did not receive approval response")
	}
}

// TestTeaModel_ApprovalKeyN verifies that pressing 'n' sends ApprovalDeny.
func TestTeaModel_ApprovalKeyN(t *testing.T) {
	respCh := make(chan ApprovalResponse, 1)
	m := &teaModel{
		accordion: NewAccordionModel(),
		msgCh:     make(chan Msg, 1),
		pendingApproval: &ApprovalRequest{
			ToolName:   "bash",
			Args:       map[string]any{"command": "rm -rf /"},
			ResponseCh: respCh,
		},
	}
	cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	assert.Nil(t, cmd)
	assert.Nil(t, m.pendingApproval)
	select {
	case resp := <-respCh:
		assert.Equal(t, ApprovalDeny, resp.Decision)
	case <-time.After(time.Second):
		t.Fatal("did not receive approval response")
	}
}

// TestTeaModel_ApprovalKeyA verifies that pressing 'a' sends ApprovalAlwaysAllow.
func TestTeaModel_ApprovalKeyA(t *testing.T) {
	respCh := make(chan ApprovalResponse, 1)
	m := &teaModel{
		accordion: NewAccordionModel(),
		msgCh:     make(chan Msg, 1),
		pendingApproval: &ApprovalRequest{
			ToolName:   "git",
			Args:       map[string]any{"command": "status"},
			ResponseCh: respCh,
		},
	}
	cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	assert.Nil(t, cmd)
	assert.Nil(t, m.pendingApproval)
	select {
	case resp := <-respCh:
		assert.Equal(t, ApprovalAlwaysAllow, resp.Decision)
	case <-time.After(time.Second):
		t.Fatal("did not receive approval response")
	}
}

// TestTeaModel_ApprovalDiffPreview verifies the diff preview is rendered in the
// approval entry view.
func TestTeaModel_ApprovalDiffPreview(t *testing.T) {
	m := &teaModel{
		accordion: NewAccordionModel(),
		msgCh:     make(chan Msg, 1),
		pendingApproval: &ApprovalRequest{
			ToolName:    "edit",
			Args:        map[string]any{"file_path": "main.go"},
			DiffPreview: "+ added line",
			ResponseCh:  make(chan ApprovalResponse, 1),
		},
	}
	view := m.View()
	assert.Contains(t, view, "diff:")
	assert.Contains(t, view, "+ added line")
}

// TestTeaModel_ApprovalKeyIgnoredWhenNoPending verifies that when no approval
// is pending, y/a/n keys do normal accordion navigation.
func TestTeaModel_ApprovalKeyIgnoredWhenNoPending(t *testing.T) {
	m := &teaModel{
		accordion: NewAccordionModel(),
		msgCh:     make(chan Msg, 1),
	}
	// No pending approval — 'y' should not crash and should be a no-op.
	cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	assert.Nil(t, cmd)
}

// TestApprovalRequestMsg_UpdateFlow verifies that an approvalRequestMsg sets
// pendingApproval and re-arms the waitForApproval command.
func TestApprovalRequestMsg_UpdateFlow(t *testing.T) {
	m := &teaModel{
		accordion:  NewAccordionModel(),
		msgCh:      make(chan Msg, 1),
		approvalCh: make(chan ApprovalRequest, 1),
	}
	req := ApprovalRequest{
		ToolName:   "write",
		Args:       map[string]any{"path": "/tmp/x"},
		ResponseCh: make(chan ApprovalResponse, 1),
	}
	updated, cmd := m.Update(approvalRequestMsg{req: req})
	require.NotNil(t, updated)
	assert.NotNil(t, m.pendingApproval)
	assert.NotNil(t, cmd, "waitForApproval should be re-armed")
}
