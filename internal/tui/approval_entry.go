package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
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
	// RequestID is an optional identity supplied by the request source. The
	// TUI leaves it empty when the caller has no authoritative ID; it uses a
	// separately labeled local interaction token for queue cancellation.
	RequestID string
	// Context cancels this request if the approval waiter is no longer active.
	Context context.Context
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

// renderApprovalRequest produces the first page of a pending approval's
// bounded detail view and its fixed decision row.
func renderApprovalRequest(req *ApprovalRequest) string {
	return renderApprovalRequestDisplay(req, true)
}

func renderApprovalRequestDisplay(req *ApprovalRequest, showDiff bool) string {
	view, _ := renderApprovalRequestPage(req, showDiff, 0, 0, 80, defaultApprovalDetailRows+approvalModalFixedRows, defaultApprovalDetailRows, 0, true)
	return view
}

const (
	defaultApprovalDetailRows = 8
	maxApprovalDetailRows     = 20
	approvalModalFixedRows    = 5
)

func approvalDetailViewportRows(height int) int {
	if height <= 0 {
		return defaultApprovalDetailRows
	}
	rows := height - approvalModalFixedRows
	if rows < 0 {
		return 0
	}
	if rows > maxApprovalDetailRows {
		return maxApprovalDetailRows
	}
	return rows
}

func approvalDetailWidth(width int) int {
	if width <= 0 {
		return 80
	}
	if width <= 2 {
		return 1
	}
	return width - 2
}

func sanitizeApprovalText(text string) string {
	return ansi.Strip(safeTerminalText(text))
}

func approvalSingleLine(text string) string {
	return strings.ReplaceAll(sanitizeApprovalText(text), "\n", " ")
}

func walkApprovalDetailLines(req *ApprovalRequest, showDiff bool, width int, visit func(string)) int {
	if req == nil {
		return 0
	}
	lineCount := 0
	addLine := func(line string) {
		for _, wrapped := range wrapApprovalLine(line, width) {
			if visit != nil {
				visit(wrapped)
			}
			lineCount++
		}
	}
	if len(req.Args) == 0 {
		addLine("  (no arguments)")
	} else {
		addLine("  args:")
		keys := make([]string, 0, len(req.Args))
		for key := range req.Args {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			value, ok := req.Args[key].(string)
			if !ok {
				value = fmt.Sprint(req.Args[key])
			}
			key = approvalSingleLine(key)
			value = sanitizeApprovalText(value)
			first := true
			walkApprovalTextLines(value, func(valueLine string) {
				prefix := "      "
				if first {
					prefix = fmt.Sprintf("    %s: ", key)
					first = false
				}
				addLine(prefix + valueLine)
			})
		}
	}

	if req.DiffPreview != "" && showDiff {
		addLine("  diff:")
		walkApprovalTextLines(sanitizeApprovalText(req.DiffPreview), func(line string) {
			wrapped := wrapApprovalLine("    "+line, width)
			for _, visibleLine := range wrapped {
				switch {
				case strings.HasPrefix(line, "+"):
					visibleLine = approvalDiffAddStyle.Render(visibleLine)
				case strings.HasPrefix(line, "-"):
					visibleLine = approvalDiffDelStyle.Render(visibleLine)
				}
				if visit != nil {
					visit(visibleLine)
				}
				lineCount++
			}
		})
	} else if req.DiffPreview != "" {
		addLine("  diff hidden (press d to show)")
	}
	return lineCount
}

func walkApprovalTextLines(text string, visit func(string)) {
	for {
		index := strings.IndexByte(text, '\n')
		if index < 0 {
			visit(text)
			return
		}
		visit(text[:index])
		text = text[index+1:]
	}
}

func wrapApprovalLine(line string, width int) []string {
	if width <= 0 {
		return []string{sanitizeApprovalText(line)}
	}
	parts := strings.Split(wrapCellText(line, width), "\n")
	for i := range parts {
		parts[i] = strings.TrimRight(parts[i], " ")
	}
	return parts
}

func renderApprovalRequestPage(req *ApprovalRequest, showDiff bool, localToken uint64, queued int, width, height, detailRows, offset int, reviewed bool) (string, int) {
	if req == nil {
		return "", 0
	}
	if width <= 0 {
		width = 80
	}
	if height > 0 && height < approvalModalFixedRows {
		return renderCompactApprovalRequestPage(req, localToken, queued, width, height), 0
	}
	if detailRows < 0 {
		detailRows = 0
	}
	detailWidth := approvalDetailWidth(width)
	contentLines := walkApprovalDetailLines(req, showDiff, detailWidth, nil)
	total := contentLines
	if contentLines == 0 {
		total = 1
	}
	maxOffset := total - detailRows
	if maxOffset < 0 {
		maxOffset = 0
	}
	if offset < 0 {
		offset = 0
	}
	if offset > maxOffset {
		offset = maxOffset
	}
	end := offset + detailRows
	if end > total {
		end = total
	}
	visibleLines := make([]string, 0, max(0, end-offset))
	if contentLines == 0 {
		visibleLines = append(visibleLines, "  (no details)")
	} else {
		lineIndex := 0
		walkApprovalDetailLines(req, showDiff, detailWidth, func(line string) {
			if lineIndex >= offset && lineIndex < end {
				visibleLines = append(visibleLines, line)
			}
			lineIndex++
		})
	}
	var sb strings.Builder

	// Header and provenance stay visible while only the bounded details region scrolls.
	header := approvalHeaderStyle.Render("[approval]") + " " + approvalToolStyle.Render(approvalSingleLine(req.ToolName))
	sb.WriteString(clipCells(header, width))
	sb.WriteString("\n")
	identity := approvalIdentityLine(req, localToken)
	if queued > 0 {
		identity += fmt.Sprintf(" · %d waiting", queued)
	}
	sb.WriteString(clipCells(identity, width))
	sb.WriteString("\n")
	sb.WriteString(clipCells("  scope: unknown; once only; reuse unavailable", width))
	sb.WriteString("\n")
	var status strings.Builder
	if detailRows == 0 {
		status.WriteString("  details hidden · resize to review")
	} else if total > detailRows {
		fmt.Fprintf(&status, "  view truncated · details %d-%d/%d", offset+1, end, total) //nolint:errcheck
		status.WriteString(" · ↑/↓ PgUp/PgDn")
	} else {
		fmt.Fprintf(&status, "  details %d-%d/%d", offset+1, end, total) //nolint:errcheck
	}
	sb.WriteString(clipCells(status.String(), width))
	sb.WriteString("\n")

	for _, line := range visibleLines {
		sb.WriteString(line)
		sb.WriteString("\n")
	}

	// Unknown scope intentionally hides the persistent-allow key. A clipped
	// request cannot be approved until every details row has appeared in a view.
	decision := approvalDecisionLine(reviewed, width)
	if width >= 32 && req.DiffPreview != "" {
		if showDiff {
			decision += " [d] diff"
		} else {
			decision += " [d] show diff"
		}
	}
	sb.WriteString(clipCells(decision, width))

	return strings.TrimRight(sb.String(), "\n"), offset
}

func approvalIdentityLine(req *ApprovalRequest, localToken uint64) string {
	if id := approvalSingleLine(req.RequestID); id != "" {
		return "  request: " + id
	}
	if localToken == 0 {
		return "  local interaction (sequence unavailable)"
	}
	return fmt.Sprintf("  local interaction #%d", localToken)
}

func approvalDecisionLine(reviewed bool, width int) string {
	if !reviewed {
		if width < 16 {
			return "y:next n:no"
		}
		if width < 30 {
			return "[y] locked [n] no"
		}
		return "  [y] review details [n] reject"
	}
	if width < 16 {
		return "y:once n:no"
	}
	if width < 22 {
		return "[y] once [n] no"
	}
	return "  [y] once [n] reject [Esc] cancel"
}

func renderCompactApprovalRequestPage(req *ApprovalRequest, localToken uint64, queued, width, height int) string {
	lines := []string{
		clipCells(approvalHeaderStyle.Render("[approval]")+" "+approvalToolStyle.Render(approvalSingleLine(req.ToolName)), width),
		clipCells(approvalIdentityLine(req, localToken), width),
		clipCells("scope unknown · resize to review details", width),
		clipCells(approvalDecisionLine(false, width), width),
	}
	if queued > 0 && height > 4 {
		lines = append(lines, clipCells(fmt.Sprintf("%d waiting", queued), width))
	}
	if height < len(lines) {
		switch height {
		case 1:
			return lines[0]
		case 2:
			return strings.Join([]string{lines[0], lines[3]}, "\n")
		default:
			return strings.Join([]string{lines[0], lines[2], lines[3]}, "\n")
		}
	}
	return strings.Join(lines[:height], "\n")
}
