package tui

import (
	"context"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApprovalActualKeyCarriesUserSource(t *testing.T) {
	m := &teaModel{accordion: NewAccordionModel(), msgCh: make(chan Msg, 1)}
	response := make(chan ApprovalResponse, 1)
	m.Update(approvalRequestMsg{req: ApprovalRequest{ToolName: "write", ResponseCh: response}})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	r := <-response
	assert.Equal(t, ApprovalDeny, r.Decision)
	assert.Equal(t, "user", r.DecisionSource)
}

func TestApprovalQueueCleanupDoesNotClaimUserSource(t *testing.T) {
	m := &teaModel{accordion: NewAccordionModel(), msgCh: make(chan Msg, 1)}
	response := make(chan ApprovalResponse, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m.Update(approvalRequestMsg{req: ApprovalRequest{ToolName: "write", Context: ctx, ResponseCh: response}})
	r := <-response
	assert.Equal(t, ApprovalDeny, r.Decision)
	assert.Equal(t, "canceled", r.DecisionSource)
}

func TestApprovalQueuedWaitersCanceledByQuitDoNotClaimUserChoices(t *testing.T) {
	m := &teaModel{accordion: NewAccordionModel(), msgCh: make(chan Msg, 1)}
	responses := []chan ApprovalResponse{make(chan ApprovalResponse, 1), make(chan ApprovalResponse, 1)}
	for _, response := range responses {
		m.Update(approvalRequestMsg{req: ApprovalRequest{ToolName: "write", ResponseCh: response}})
	}
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	for _, response := range responses {
		select {
		case r := <-response:
			assert.Equal(t, ApprovalDeny, r.Decision)
			assert.Equal(t, "canceled", r.DecisionSource)
		default:
			require.FailNow(t, "quit must resolve every approval waiter")
		}
	}
}
