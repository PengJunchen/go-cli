package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRenderApprovalRequest_NoDiff verifies the basic approval entry rendering
// without a diff preview: it contains the request details, truthful scope
// status, and the once/reject option bar.
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
	assert.Contains(t, out, "[n] reject")
	assert.Contains(t, out, "scope: unknown")
	assert.NotContains(t, out, "[a] always")
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
	assert.Contains(t, out, "[d] diff")
}

// TestRenderApprovalRequest_NilRequest returns empty string for nil.
func TestRenderApprovalRequest_NilRequest(t *testing.T) {
	assert.Empty(t, renderApprovalRequest(nil))
}

func receiveApprovalResponse(t *testing.T, ch <-chan ApprovalResponse) ApprovalResponse {
	t.Helper()
	select {
	case response := <-ch:
		return response
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for approval response")
		return ApprovalResponse{}
	}
}

func receiveHITLResponse(t *testing.T, ch <-chan HITLResponse) HITLResponse {
	t.Helper()
	select {
	case response := <-ch:
		return response
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for HITL response")
		return HITLResponse{}
	}
}

// TestTeaModel_ApprovalEntryDisplayed verifies that when an approval request
// arrives, it becomes the active interaction and View() shows the approval UI.
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
	m.Update(approvalRequestMsg{req: req})
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
	}
	m.Update(approvalRequestMsg{req: ApprovalRequest{ToolName: "bash", Args: map[string]any{"command": "ls"}, ResponseCh: respCh}})
	m.View() // The initial details page has been displayed to the user.
	cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	assert.Nil(t, cmd)
	assert.Nil(t, m.activeInteraction)
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
	}
	m.Update(approvalRequestMsg{req: ApprovalRequest{ToolName: "bash", Args: map[string]any{"command": "rm -rf /"}, ResponseCh: respCh}})
	cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	assert.Nil(t, cmd)
	assert.Nil(t, m.activeInteraction)
	select {
	case resp := <-respCh:
		assert.Equal(t, ApprovalDeny, resp.Decision)
	case <-time.After(time.Second):
		t.Fatal("did not receive approval response")
	}
}

// TestTeaModel_ApprovalKeyAIsIgnoredWithoutScope verifies that an unscoped
// persistent-approval key cannot resolve a request.
func TestTeaModel_ApprovalKeyAIsIgnoredWithoutScope(t *testing.T) {
	respCh := make(chan ApprovalResponse, 1)
	m := &teaModel{
		accordion: NewAccordionModel(),
		msgCh:     make(chan Msg, 1),
	}
	m.Update(approvalRequestMsg{req: ApprovalRequest{ToolName: "git", Args: map[string]any{"command": "status"}, ResponseCh: respCh}})
	cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	assert.Nil(t, cmd)
	assert.NotNil(t, m.activeInteraction)
	select {
	case resp := <-respCh:
		t.Fatalf("unscoped persistent approval unexpectedly responded: %+v", resp)
	default:
	}
	m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	assert.Equal(t, ApprovalDeny, receiveApprovalResponse(t, respCh).Decision)
}

// TestTeaModel_ApprovalDiffPreview verifies the diff preview is rendered in the
// approval entry view.
func TestTeaModel_ApprovalDiffPreview(t *testing.T) {
	m := &teaModel{
		accordion: NewAccordionModel(),
		msgCh:     make(chan Msg, 1),
	}
	m.Update(approvalRequestMsg{req: ApprovalRequest{ToolName: "edit", Args: map[string]any{"file_path": "main.go"}, DiffPreview: "+ added line", ResponseCh: make(chan ApprovalResponse, 1)}})
	view := m.View()
	assert.Contains(t, view, "diff:")
	assert.Contains(t, view, "+ added line")
}

func TestTeaModel_ApprovalDiffToggle(t *testing.T) {
	m := &teaModel{accordion: NewAccordionModel(), msgCh: make(chan Msg, 1)}
	m.Update(approvalRequestMsg{req: ApprovalRequest{
		ToolName:    "edit",
		DiffPreview: "+ added line",
		ResponseCh:  make(chan ApprovalResponse, 1),
	}})
	assert.Contains(t, m.View(), "+ added line")
	assert.Contains(t, m.View(), "[d] diff")

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	assert.NotContains(t, m.View(), "+ added line")
	assert.Contains(t, m.View(), "[d] show diff")

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	assert.Contains(t, m.View(), "+ added line")
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
	require.NotNil(t, m.activeInteraction)
	assert.NotNil(t, m.activeInteraction.approval)
	assert.NotNil(t, cmd, "waitForApproval should be re-armed")
}

func TestTUIApprovalDisplaysAuthoritativeRequestID(t *testing.T) {
	m := &teaModel{accordion: NewAccordionModel(), msgCh: make(chan Msg, 1)}
	m.Update(approvalRequestMsg{req: ApprovalRequest{
		RequestID:  "source-request-42",
		ToolName:   "write",
		ResponseCh: make(chan ApprovalResponse, 1),
	}})

	view := m.View()
	assert.Contains(t, view, "source-request-42")
	assert.NotContains(t, view, "local interaction #")
}

func TestTUIApprovalModalFitsSmallTerminalAndSanitizesUntrustedDetails(t *testing.T) {
	content := "before\x1b[2Jafter\x1b]52;c;Zm9v\a\rend"
	m := &teaModel{accordion: NewAccordionModel(), msgCh: make(chan Msg, 1)}
	m.Update(tea.WindowSizeMsg{Width: 20, Height: 4})
	m.Update(approvalRequestMsg{req: ApprovalRequest{
		RequestID:  "r42",
		ToolName:   "write",
		Args:       map[string]any{"content": content},
		ResponseCh: make(chan ApprovalResponse, 1),
	}})

	view := m.View()
	lines := strings.Split(view, "\n")
	require.LessOrEqual(t, len(lines), 4, "approval modal must fit terminal height")
	for _, line := range lines {
		require.LessOrEqual(t, cellWidth(line), 20, "approval row must fit terminal width: %q", line)
	}
	require.Contains(t, view, "r42")
	require.Contains(t, view, "scope")
	require.Contains(t, lastLine(view), "[n]", "the bounded decision row keeps rejection visible")
	require.NotContains(t, view, "\x1b[2J")
	require.NotContains(t, view, "\x1b]52;c;Zm9v")
	require.NotContains(t, view, "\a")
	require.NotContains(t, view, "\r")
	assert.Equal(t, content, m.activeInteraction.approval.Args["content"], "rendering must preserve source arguments")

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	assert.NotNil(t, m.activeInteraction, "details that cannot fit must not be approvable until reviewed")
}

func TestTUIApprovalVisibleDetailsSanitizeTerminalControls(t *testing.T) {
	content := "cmd:before\x1b[2Jafter\r\x1b]52;c;Zm9v\a"
	styled := "red\x1b[31mplain"
	diff := "+safe\x1b[2Jdanger"
	m := &teaModel{accordion: NewAccordionModel(), msgCh: make(chan Msg, 1)}
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.Update(approvalRequestMsg{req: ApprovalRequest{
		RequestID:   "id\x1b[2J",
		ToolName:    "tool\x1b[2J",
		Args:        map[string]any{"command": content, "format": styled},
		DiffPreview: diff,
		ResponseCh:  make(chan ApprovalResponse, 1),
	}})

	view := m.View()
	require.Contains(t, view, "tool\\x1b[2J")
	require.Contains(t, view, "request: id\\x1b[2J")
	require.Contains(t, view, "cmd:before\\x1b[2Jafter\\r\\x1b]52;c;Zm9v\\a")
	require.Contains(t, view, "format: red\\x1b[31mplain")
	require.Contains(t, view, "+safe\\x1b[2Jdanger")
	for _, control := range []string{"\x1b[2J", "\x1b]52;c;Zm9v", "\x1b[31m", "\a", "\r"} {
		assert.NotContains(t, view, control)
	}
	assert.Equal(t, content, m.activeInteraction.approval.Args["command"], "display sanitizing must not mutate original arguments")
	assert.Equal(t, styled, m.activeInteraction.approval.Args["format"], "display sanitizing must not mutate original arguments")
	assert.Equal(t, diff, m.activeInteraction.approval.DiffPreview, "display sanitizing must not mutate original diff")

	clean := &teaModel{accordion: NewAccordionModel(), msgCh: make(chan Msg, 1)}
	clean.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	clean.Update(approvalRequestMsg{req: ApprovalRequest{
		RequestID:   "id",
		ToolName:    "tool",
		Args:        map[string]any{"command": "cmd:beforeafter", "format": "redplain"},
		DiffPreview: "+safedanger",
		ResponseCh:  make(chan ApprovalResponse, 1),
	}})
	assert.NotEqual(t, clean.View(), view, "control-bearing input must not render identically to cleaned input")
}

func TestSanitizeApprovalTextEscapesControlsAndInvalidUTF8WithoutLoss(t *testing.T) {
	input := string([]byte{'A', 0x00, '\t', '\r', 0x85, 0xff, 'B'}) + "\n" + "C\u0085D"
	assert.Equal(t, "A\\x00\\t\\r\\x85\\xffB\nC\\u0085D", sanitizeApprovalText(input))
}

func TestTUIApprovalEscapedLongDetailsMustBeReviewedAfterResizeAndDiffChange(t *testing.T) {
	content := strings.Repeat("\x1b[2J", 100)
	diff := strings.Repeat("+diff\x1b[2J", 50)
	toolName := strings.Repeat("write-", 15) + "\x1b[2J"
	requestID := strings.Repeat("req-", 20) + "\x1b[2J"
	response := make(chan ApprovalResponse, 1)
	m := &teaModel{accordion: NewAccordionModel(), msgCh: make(chan Msg, 1)}
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 10})
	m.Update(approvalRequestMsg{req: ApprovalRequest{
		ToolName:    toolName,
		RequestID:   requestID,
		Args:        map[string]any{"command": content},
		DiffPreview: diff,
		ResponseCh:  response,
	}})

	view := m.View()
	require.Contains(t, view, "view truncated")
	require.Contains(t, view, `\x1b[2J`)
	allDetails := make([]string, 0)
	walkApprovalDetailLines(m.activeInteraction.approval, m.activeInteraction.showDiff, approvalDetailWidth(m.width), func(line string) {
		allDetails = append(allDetails, line)
	})
	completeDetailText := strings.ReplaceAll(strings.Join(allDetails, "\n"), "\n", "")
	require.Contains(t, completeDetailText, strings.Repeat("write-", 15)+`\x1b[2J`, "the clipped fixed header has a complete scrollable name row")
	require.Contains(t, completeDetailText, strings.Repeat("req-", 20)+`\x1b[2J`, "the clipped identity has a complete scrollable request ID row")
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	assert.NotNil(t, m.activeInteraction, "escaped bytes must count toward the reviewable wrapped detail lines")
	m.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	assert.NotNil(t, m.activeInteraction, "scrolling without rendering the page must not count as review")
	m.View()

	for i := 0; i < 1000 && !strings.Contains(lastLine(m.View()), "[y] once"); i++ {
		m.Update(tea.KeyMsg{Type: tea.KeyPgDown})
		m.View()
	}
	require.Contains(t, lastLine(m.View()), "[y] once", "the complete escaped preview becomes approvable after every wrapped row was displayed")

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	require.Contains(t, lastLine(m.View()), "[y] review details", "toggling the diff resets review coverage")
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	assert.NotNil(t, m.activeInteraction, "toggling the diff resets review coverage")

	m.Update(tea.WindowSizeMsg{Width: 20, Height: 4})
	m.View()
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	assert.NotNil(t, m.activeInteraction, "resizing to a view that hides details resets review coverage")
	assert.Equal(t, content, m.activeInteraction.approval.Args["command"])
	select {
	case got := <-response:
		t.Fatalf("unreviewed escaped details unexpectedly produced a decision: %+v", got)
	default:
	}
}

func TestTUIConcurrentApprovalRequestsRemainResolvable(t *testing.T) {
	firstResponse := make(chan ApprovalResponse, 1)
	secondResponse := make(chan ApprovalResponse, 1)
	m := &teaModel{
		accordion:  NewAccordionModel(),
		msgCh:      make(chan Msg, 1),
		approvalCh: make(chan ApprovalRequest, 2),
	}
	first := ApprovalRequest{ToolName: "first-tool", ResponseCh: firstResponse}
	second := ApprovalRequest{ToolName: "second-tool", ResponseCh: secondResponse}

	// The second concurrent request arrives first; response ownership remains
	// tied to its channel while the earlier logical request waits behind it.
	m.Update(approvalRequestMsg{req: second})
	m.Update(approvalRequestMsg{req: first})
	require.Contains(t, m.View(), "second-tool", "the first-arriving approval must remain active")
	assert.NotContains(t, m.View(), "first-tool", "later approvals must wait in arrival order")
	require.Empty(t, m.activeInteraction.approval.RequestID, "the TUI must not invent an authoritative request ID")
	assert.Contains(t, m.View(), "local interaction #1")

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	select {
	case response := <-secondResponse:
		assert.Equal(t, ApprovalAllow, response.Decision)
	default:
		t.Fatal("first-arriving approval did not receive its own response")
	}
	require.Contains(t, m.View(), "first-tool", "resolving the first approval must show the next one")
	require.Empty(t, m.activeInteraction.approval.RequestID, "the TUI must not invent an authoritative request ID")

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	select {
	case response := <-firstResponse:
		assert.Equal(t, ApprovalAllow, response.Decision)
	default:
		t.Fatal("queued approval did not receive its own response")
	}
}

func TestTUIAskUserMessageReachesHandler(t *testing.T) {
	responseCh := make(chan HITLResponse, 1)
	m := &teaModel{accordion: NewAccordionModel(), msgCh: make(chan Msg, 1)}
	msg := HITLMessage{
		QuestionID: "q-answer",
		Question:   "Which environment?",
		Options:    []string{"test", "live"},
		ResponseCh: responseCh,
	}

	updated, _ := m.Update(msg)
	require.Same(t, m, updated)
	assert.Contains(t, m.View(), "Which environment?")
	assert.Contains(t, m.View(), "[2] live")
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'2'}})
	select {
	case response := <-responseCh:
		assert.Equal(t, "live", response.Answer)
		assert.NoError(t, response.Err)
	default:
		t.Fatal("HITL response was not delivered to the request callback")
	}
	assert.Nil(t, m.activeInteraction)
}

func TestTUIAskUserFreeTextReachesHandler(t *testing.T) {
	responseCh := make(chan HITLResponse, 1)
	m := &teaModel{accordion: NewAccordionModel(), msgCh: make(chan Msg, 1)}
	m.Update(HITLMessage{QuestionID: "q-text", Question: "What should I name it?", ResponseCh: responseCh})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("draft")})
	m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	assert.Equal(t, "draf", receiveHITLResponse(t, responseCh).Answer)
	assert.Nil(t, m.activeInteraction)
}

func TestAskUserCancelUnblocksWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	responseCh := make(chan HITLResponse, 1)
	m := &teaModel{accordion: NewAccordionModel(), msgCh: make(chan Msg, 1)}
	msg := HITLMessage{QuestionID: "q-cancel", Question: "Cancel me", ResponseCh: responseCh, Context: ctx}
	_, cancelCmd := m.Update(msg)
	require.NotNil(t, cancelCmd)

	canceled := make(chan tea.Msg, 1)
	go func() { canceled <- cancelCmd() }()
	cancel()
	select {
	case got := <-canceled:
		m.Update(got)
	case <-time.After(time.Second):
		t.Fatal("context cancellation did not wake the TUI waiter")
	}
	select {
	case response := <-responseCh:
		assert.ErrorIs(t, response.Err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("canceled HITL request did not receive a cancellation response")
	}
	assert.Nil(t, m.activeInteraction)
}

func TestTUIApprovalAndHITLModalsFollowArrivalOrder(t *testing.T) {
	for _, first := range []string{"approval", "hitl"} {
		t.Run(first+" first", func(t *testing.T) {
			approvalResponse := make(chan ApprovalResponse, 1)
			hitlResponse := make(chan HITLResponse, 1)
			m := &teaModel{
				accordion:  NewAccordionModel(),
				msgCh:      make(chan Msg, 1),
				approvalCh: make(chan ApprovalRequest, 2),
			}
			approval := ApprovalRequest{ToolName: "write", ResponseCh: approvalResponse}
			hitl := HITLMessage{QuestionID: "ordered", Question: "Where?", Options: []string{"left", "right"}, ResponseCh: hitlResponse}
			if first == "approval" {
				m.Update(approvalRequestMsg{req: approval})
				m.Update(hitl)
				require.Contains(t, m.View(), "write")
				assert.NotContains(t, m.View(), "Where?")
				m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
				assert.Equal(t, ApprovalAllow, receiveApprovalResponse(t, approvalResponse).Decision)
				require.Contains(t, m.View(), "Where?")
				m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'2'}})
				assert.Equal(t, "right", receiveHITLResponse(t, hitlResponse).Answer)
				return
			}

			m.Update(hitl)
			m.Update(approvalRequestMsg{req: approval})
			require.Contains(t, m.View(), "Where?")
			assert.NotContains(t, m.View(), "write")
			m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'2'}})
			assert.Equal(t, "right", receiveHITLResponse(t, hitlResponse).Answer)
			require.Contains(t, m.View(), "write")
			m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
			assert.Equal(t, ApprovalDeny, receiveApprovalResponse(t, approvalResponse).Decision)
		})
	}
}

func TestTUICancelResolvesEveryQueuedInteraction(t *testing.T) {
	approvalResponse := make(chan ApprovalResponse, 1)
	hitlResponse := make(chan HITLResponse, 1)
	m := &teaModel{accordion: NewAccordionModel(), msgCh: make(chan Msg, 1)}
	m.Update(approvalRequestMsg{req: ApprovalRequest{ToolName: "write", ResponseCh: approvalResponse}})
	m.Update(HITLMessage{QuestionID: "queued", Question: "Pending?", ResponseCh: hitlResponse})
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	require.NotNil(t, cmd)
	assert.Nil(t, m.activeInteraction)
	assert.Empty(t, m.queuedInteractions)
	assert.Equal(t, ApprovalDeny, receiveApprovalResponse(t, approvalResponse).Decision)
	assert.ErrorIs(t, receiveHITLResponse(t, hitlResponse).Err, context.Canceled)
}

func TestTUIClosedApprovalChannelResolvesQueuedRequests(t *testing.T) {
	approvalCh := make(chan ApprovalRequest)
	close(approvalCh)
	approvalResponse := make(chan ApprovalResponse, 1)
	hitlResponse := make(chan HITLResponse, 1)
	m := &teaModel{
		accordion:  NewAccordionModel(),
		msgCh:      make(chan Msg, 1),
		approvalCh: approvalCh,
	}
	m.Update(approvalRequestMsg{req: ApprovalRequest{ToolName: "write", ResponseCh: approvalResponse}})
	m.Update(HITLMessage{QuestionID: "queued", Question: "Pending?", ResponseCh: hitlResponse})

	closedMsg := waitForApproval(approvalCh, nil)()
	_, cmd := m.Update(closedMsg)
	assert.Nil(t, cmd, "closing the approval source must not quit the live TUI")
	require.NotNil(t, m.activeInteraction)
	assert.NotNil(t, m.activeInteraction.hitl)
	assert.Empty(t, m.queuedInteractions)
	assert.Equal(t, ApprovalDeny, receiveApprovalResponse(t, approvalResponse).Decision)
	assert.Contains(t, m.View(), "Pending?")
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("answer")})
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	assert.Equal(t, "answer", receiveHITLResponse(t, hitlResponse).Answer)
}

func TestTUIQueuedApprovalCancellationKeepsOtherRequestResolvable(t *testing.T) {
	activeResponse := make(chan ApprovalResponse, 1)
	queuedResponse := make(chan ApprovalResponse, 1)
	queuedCtx, cancelQueued := context.WithCancel(context.Background())
	defer cancelQueued()
	m := &teaModel{accordion: NewAccordionModel(), msgCh: make(chan Msg, 1)}

	m.Update(approvalRequestMsg{req: ApprovalRequest{RequestID: "duplicate", ToolName: "active", ResponseCh: activeResponse}})
	m.Update(approvalRequestMsg{req: ApprovalRequest{
		RequestID:  "duplicate",
		ToolName:   "queued",
		ResponseCh: queuedResponse,
		Context:    queuedCtx,
	}})

	m.mu.Lock()
	queued := m.queuedInteractions[0]
	m.mu.Unlock()
	watcher := waitForInteractionCancel(queuedCtx, queued.token, queued.finished, nil)
	cancelQueued()
	canceledMsg := watcher()
	m.Update(canceledMsg)

	require.Contains(t, m.View(), "active")
	assert.NotContains(t, m.View(), "queued")
	assert.Empty(t, m.queuedInteractions)
	assert.Equal(t, ApprovalDeny, receiveApprovalResponse(t, queuedResponse).Decision)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	assert.Equal(t, ApprovalAllow, receiveApprovalResponse(t, activeResponse).Decision)
}

func TestTUIApprovalDetailsScrollKeepsDecisionRowVisible(t *testing.T) {
	lines := make([]string, 24)
	for i := range lines {
		lines[i] = fmt.Sprintf("detail-line-%02d", i)
	}
	diffLines := make([]string, 16)
	for i := range diffLines {
		diffLines[i] = fmt.Sprintf("+diff-line-%02d", i)
	}
	content := strings.Join(lines, "\n")
	response := make(chan ApprovalResponse, 1)
	m := &teaModel{accordion: NewAccordionModel(), msgCh: make(chan Msg, 1)}
	m.Update(tea.WindowSizeMsg{Width: 60, Height: 10})
	m.Update(approvalRequestMsg{req: ApprovalRequest{
		ToolName:    "write",
		Args:        map[string]any{"content": content, "path": "/tmp/large.txt"},
		DiffPreview: strings.Join(diffLines, "\n"),
		ResponseCh:  response,
	}})

	view := m.View()
	assert.Contains(t, view, "scope: unknown")
	assert.Contains(t, view, "view truncated")
	assert.Contains(t, view, "detail-line-00")
	assert.NotContains(t, view, "detail-line-23")
	assert.Contains(t, lastLine(view), "[y] review details", "decision row stays fixed and locked until details are reviewed")
	assert.LessOrEqual(t, strings.Count(view, "\n")+1, 10, "the modal stays within the configured height")

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	assert.NotNil(t, m.activeInteraction, "approval must wait until every detail row has been displayed")
	select {
	case got := <-response:
		t.Fatalf("unreviewed details unexpectedly produced a decision: %+v", got)
	default:
	}

	m.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	assert.Contains(t, m.View(), "detail-line-03", "page down scrolls the bounded details area")

	sawLastArg := false
	sawLastDiff := false
	for i := 0; i < 96 && !sawLastDiff; i++ {
		m.Update(tea.KeyMsg{Type: tea.KeyDown})
		page := m.View()
		sawLastArg = sawLastArg || strings.Contains(page, "detail-line-23")
		sawLastDiff = strings.Contains(page, "+diff-line-15")
	}
	view = m.View()
	assert.True(t, sawLastArg, "the complete request remains inspectable by scrolling")
	assert.True(t, sawLastDiff, "the complete diff remains inspectable by scrolling")
	assert.Contains(t, lastLine(view), "[y] once", "scrolling does not move the decision row")
	assert.Equal(t, content, m.activeInteraction.approval.Args["content"], "rendering must not alter the captured arguments")
	for i := 0; i < 12; i++ {
		m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	}
	assert.Contains(t, m.View(), "detail-line-00", "page up can return to the beginning of the same details")

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	assert.Equal(t, ApprovalAllow, receiveApprovalResponse(t, response).Decision)
}

func TestTUIApprovalWithoutKnownReusableScopeHidesAlways(t *testing.T) {
	response := make(chan ApprovalResponse, 1)
	m := &teaModel{accordion: NewAccordionModel(), msgCh: make(chan Msg, 1)}
	m.Update(approvalRequestMsg{req: ApprovalRequest{ToolName: "bash", ResponseCh: response}})

	view := m.View()
	assert.Contains(t, view, "scope: unknown")
	assert.NotContains(t, view, "[a] always")
	assert.Empty(t, m.activeInteraction.approval.RequestID, "the TUI must not invent an authoritative request ID")
	assert.Contains(t, view, "local interaction #1")

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	assert.NotNil(t, m.activeInteraction, "unscoped persistent approval must not resolve the request")
	select {
	case got := <-response:
		t.Fatalf("unscoped always key unexpectedly responded: %+v", got)
	default:
	}

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	assert.Equal(t, ApprovalAllow, receiveApprovalResponse(t, response).Decision)
}

func TestTUIApprovalQueueCancellationKeepsRemainingFIFOOrder(t *testing.T) {
	activeResponse := make(chan ApprovalResponse, 1)
	canceledResponse := make(chan ApprovalResponse, 1)
	lastResponse := make(chan ApprovalResponse, 1)
	canceledCtx, cancelCanceled := context.WithCancel(context.Background())
	defer cancelCanceled()
	m := &teaModel{accordion: NewAccordionModel(), msgCh: make(chan Msg, 1)}
	m.Update(approvalRequestMsg{req: ApprovalRequest{ToolName: "active", ResponseCh: activeResponse}})
	m.Update(approvalRequestMsg{req: ApprovalRequest{ToolName: "canceled", ResponseCh: canceledResponse, Context: canceledCtx}})
	m.Update(approvalRequestMsg{req: ApprovalRequest{ToolName: "last", ResponseCh: lastResponse}})

	m.mu.Lock()
	canceled := m.queuedInteractions[0]
	m.mu.Unlock()
	watcher := waitForInteractionCancel(canceledCtx, canceled.token, canceled.finished, nil)
	cancelCanceled()
	m.Update(watcher())

	assert.Contains(t, m.View(), "active")
	assert.NotContains(t, m.View(), "canceled")
	assert.NotContains(t, m.View(), "last")
	assert.Len(t, m.queuedInteractions, 1)
	assert.Equal(t, ApprovalDeny, receiveApprovalResponse(t, canceledResponse).Decision)

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	assert.Equal(t, ApprovalAllow, receiveApprovalResponse(t, activeResponse).Decision)
	assert.Contains(t, m.View(), "last", "the remaining request should become active after the first")
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	assert.Equal(t, ApprovalDeny, receiveApprovalResponse(t, lastResponse).Decision)
}

func lastLine(view string) string {
	lines := strings.Split(strings.TrimRight(view, "\n"), "\n")
	if len(lines) == 0 {
		return ""
	}
	return lines[len(lines)-1]
}

func TestCanceledApprovalCannotBeApprovedBeforeCancelMessage(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	response := make(chan ApprovalResponse, 1)
	m := &teaModel{accordion: NewAccordionModel(), msgCh: make(chan Msg, 1)}
	m.Update(approvalRequestMsg{req: ApprovalRequest{
		ToolName:   "write",
		ResponseCh: response,
		Context:    ctx,
	}})

	cancel()
	// Model the user key being handled before the queued context-cancel message.
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})

	assert.Equal(t, ApprovalDeny, receiveApprovalResponse(t, response).Decision)
	assert.Nil(t, m.activeInteraction)
}

func TestAnsweredInteractionsStopContextWatchers(t *testing.T) {
	t.Run("approval", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		m := &teaModel{accordion: NewAccordionModel(), msgCh: make(chan Msg, 1)}
		response := make(chan ApprovalResponse, 1)
		m.Update(approvalRequestMsg{req: ApprovalRequest{ToolName: "bash", ResponseCh: response, Context: ctx}})
		m.View() // The initial approval page has been displayed before input.

		m.mu.Lock()
		interaction := *m.activeInteraction
		m.mu.Unlock()
		watcher := waitForInteractionCancel(ctx, interaction.token, interaction.finished, nil)
		watcherReturned := make(chan tea.Msg, 1)
		go func() { watcherReturned <- watcher() }()

		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
		assert.Equal(t, ApprovalAllow, receiveApprovalResponse(t, response).Decision)
		select {
		case msg := <-watcherReturned:
			assert.Nil(t, msg)
		case <-time.After(time.Second):
			t.Fatal("answered approval left its context watcher blocked")
		}
	})

	t.Run("ask_user", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		response := make(chan HITLResponse, 1)
		m := &teaModel{accordion: NewAccordionModel(), msgCh: make(chan Msg, 1)}
		_, watcher := m.Update(HITLMessage{
			QuestionID: "answered",
			Question:   "Continue?",
			Options:    []string{"yes"},
			ResponseCh: response,
			Context:    ctx,
		})
		watcherReturned := make(chan tea.Msg, 1)
		go func() { watcherReturned <- watcher() }()
		m.Update(tea.KeyMsg{Type: tea.KeyEnter})
		assert.Equal(t, "yes", receiveHITLResponse(t, response).Answer)
		select {
		case msg := <-watcherReturned:
			assert.Nil(t, msg)
		case <-time.After(time.Second):
			t.Fatal("answered ask_user left its context watcher blocked")
		}
	})
}
