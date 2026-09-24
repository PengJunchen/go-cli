package tui

import (
	"context"
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
	assert.Contains(t, out, "[d] hide diff")
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

// TestTeaModel_ApprovalKeyA verifies that pressing 'a' sends ApprovalAlwaysAllow.
func TestTeaModel_ApprovalKeyA(t *testing.T) {
	respCh := make(chan ApprovalResponse, 1)
	m := &teaModel{
		accordion: NewAccordionModel(),
		msgCh:     make(chan Msg, 1),
	}
	m.Update(approvalRequestMsg{req: ApprovalRequest{ToolName: "git", Args: map[string]any{"command": "status"}, ResponseCh: respCh}})
	cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	assert.Nil(t, cmd)
	assert.Nil(t, m.activeInteraction)
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
	assert.Contains(t, m.View(), "[d] hide diff")

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
	require.Equal(t, "approval-1", m.activeInteraction.approval.RequestID)

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	select {
	case response := <-secondResponse:
		assert.Equal(t, ApprovalAllow, response.Decision)
	default:
		t.Fatal("first-arriving approval did not receive its own response")
	}
	require.Contains(t, m.View(), "first-tool", "resolving the first approval must show the next one")
	require.Equal(t, "approval-2", m.activeInteraction.approval.RequestID)

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	select {
	case response := <-firstResponse:
		assert.Equal(t, ApprovalAlwaysAllow, response.Decision)
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
			m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
			assert.Equal(t, ApprovalAlwaysAllow, receiveApprovalResponse(t, approvalResponse).Decision)
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
	watcher := waitForInteractionCancel(queued.token, queuedCtx, queued.finished, nil)
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

		m.mu.Lock()
		interaction := *m.activeInteraction
		m.mu.Unlock()
		watcher := waitForInteractionCancel(interaction.token, ctx, interaction.finished, nil)
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
