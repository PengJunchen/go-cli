//go:build !headless

package tui

import (
	"fmt"
	"strings"
)

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
