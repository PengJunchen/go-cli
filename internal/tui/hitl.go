package tui

import "context"

// HITLMessage carries an ask_user question from the CLI into the Bubble Tea
// model. Context is observed so canceled questions can leave the modal queue.
type HITLMessage struct {
	QuestionID string
	Question   string
	Options    []string
	ResponseCh chan HITLResponse
	Context    context.Context
}

// HITLResponse carries the user's answer or a cancellation back to the CLI.
type HITLResponse struct {
	Answer string
	Err    error
}
