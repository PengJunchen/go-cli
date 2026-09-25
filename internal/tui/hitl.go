package tui

import (
	"context"
	"fmt"
	"strings"
)

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

func renderHITLQuestion(req *HITLMessage, answer string, selected int) string {
	if req == nil {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "[ask_user] %s\n", req.Question) //nolint:errcheck
	if len(req.Options) == 0 {
		fmt.Fprintf(&sb, "  answer: %s█\n  [Enter] submit  [Esc] cancel", answer) //nolint:errcheck
		return sb.String()
	}
	for i, option := range req.Options {
		marker := " "
		if i == selected {
			marker = ">"
		}
		fmt.Fprintf(&sb, "  %s [%d] %s\n", marker, i+1, option) //nolint:errcheck
	}
	fmt.Fprint(&sb, "  [↑/↓] choose  [Enter] submit  [1-9] select  [Esc] cancel") //nolint:errcheck
	return strings.TrimRight(sb.String(), "\n")
}
