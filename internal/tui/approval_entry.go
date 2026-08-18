package tui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// ApprovalDecision is the user's verdict on a pending tool-call approval.
type ApprovalDecision int

const (
	// ApprovalDeny rejects the tool call.
	ApprovalDeny ApprovalDecision = iota
	// ApprovalAllow allows the tool call once.
	ApprovalAllow
	// ApprovalAlwaysAllow allows the tool call and persists the decision.
	ApprovalAlwaysAllow
)

// ApprovalRequest is sent from the approval middleware to the TUI for
// interactive resolution. The TUI renders it as a special accordion entry and
// delivers the user's decision through ResponseCh.
type ApprovalRequest struct {
	// ToolName is the name of the tool requesting approval.
	ToolName string
	// Args are the tool call arguments, displayed as key=value pairs.
	Args map[string]any
	// DiffPreview carries a unified diff for edit/write tools; empty for others.
	DiffPreview string
	// ResponseCh receives the user's decision. It is buffered (1) so the TUI
	// can send the response without blocking even if the middleware has moved on.
	ResponseCh chan ApprovalResponse
}

// ApprovalResponse carries the user's decision back to the middleware.
type ApprovalResponse struct {
	Decision ApprovalDecision
	// Pattern is a suggested glob for "always allow" (e.g. "git *"). Empty when
	// the decision is not ApprovalAlwaysAllow.
	Pattern string
}

// approvalHeaderStyle styles the "[approval]" prefix in warning yellow.
var approvalHeaderStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#FFC000")).Bold(true)

// approvalToolStyle styles the tool name in cyan.
var approvalToolStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#00D9FF"))

// approvalDiffAddStyle styles added diff lines in green.
var approvalDiffAddStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#04E762"))

// approvalDiffDelStyle styles deleted diff lines in red.
var approvalDiffDelStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#FF5C5C"))

// renderApprovalRequest produces the terminal output for a pending approval
// entry. It shows the tool name, arguments, an optional diff preview, and the
// keyboard shortcuts (y/a/n/d).
func renderApprovalRequest(req *ApprovalRequest) string {
	if req == nil {
		return ""
	}
	var sb strings.Builder

	// Header: [approval] tool_name
	sb.WriteString(approvalHeaderStyle.Render("[approval]"))
	sb.WriteString(" ")
	sb.WriteString(approvalToolStyle.Render(req.ToolName))
	sb.WriteString("\n")

	// Args (sorted for deterministic output).
	if len(req.Args) > 0 {
		sb.WriteString("  args:\n")
		keys := make([]string, 0, len(req.Args))
		for k := range req.Args {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&sb, "    %s: %v\n", k, req.Args[k]) //nolint:errcheck
		}
	}

	// Diff preview (if available).
	if req.DiffPreview != "" {
		sb.WriteString("  diff:\n")
		for _, line := range strings.Split(req.DiffPreview, "\n") {
			styled := line
			switch {
			case strings.HasPrefix(line, "+"):
				styled = approvalDiffAddStyle.Render(line)
			case strings.HasPrefix(line, "-"):
				styled = approvalDiffDelStyle.Render(line)
			}
			fmt.Fprintf(&sb, "    %s\n", styled) //nolint:errcheck
		}
	}

	// Options bar.
	opts := "  [y] once  [a] always  [n] reject"
	if req.DiffPreview != "" {
		opts += "  [d] toggle diff"
	}
	fmt.Fprintf(&sb, "%s\n", opts) //nolint:errcheck

	return strings.TrimRight(sb.String(), "\n")
}
