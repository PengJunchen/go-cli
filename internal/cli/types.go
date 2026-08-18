package cli

import "fmt"

// OutputMode controls how the prompt command formats agent events on stdout.
type OutputMode int

const (
	// OutputText outputs human-readable text (default). Streams incremental
	// tokens and prints complete messages with newlines.
	OutputText OutputMode = iota
	// OutputJSON outputs each AgentEvent as a newline-delimited JSON object
	// (NDJSON), suitable for programmatic consumption in CI/CD pipelines.
	OutputJSON
	// OutputStream outputs raw content tokens without formatting, suitable
	// for piping to other tools.
	OutputStream
)

// String returns the string representation of the output mode.
func (m OutputMode) String() string {
	switch m {
	case OutputJSON:
		return "json"
	case OutputStream:
		return "stream"
	default:
		return "text"
	}
}

// Set parses a string flag value into an OutputMode. Implements flag.Value.
func (m *OutputMode) Set(s string) error {
	switch s {
	case "json":
		*m = OutputJSON
	case "stream":
		*m = OutputStream
	case "text", "":
		*m = OutputText
	default:
		return fmt.Errorf("invalid output mode %q (want: json|stream|text)", s)
	}
	return nil
}

// ApproveMode controls how the prompt command handles tool approval in
// headless mode.
type ApproveMode int

const (
	// ApproveAsk prompts for approval on each tool call (default). In
	// non-interactive mode, this auto-denies.
	ApproveAsk ApproveMode = iota
	// ApproveAuto automatically approves all tool calls without prompting.
	ApproveAuto
	// ApproveDeny automatically denies all tool calls.
	ApproveDeny
)

// String returns the string representation of the approve mode.
func (m ApproveMode) String() string {
	switch m {
	case ApproveAuto:
		return "auto"
	case ApproveDeny:
		return "deny"
	default:
		return "ask"
	}
}

// Set parses a string flag value into an ApproveMode. Implements flag.Value.
func (m *ApproveMode) Set(s string) error {
	switch s {
	case "auto":
		*m = ApproveAuto
	case "deny":
		*m = ApproveDeny
	case "ask", "":
		*m = ApproveAsk
	default:
		return fmt.Errorf("invalid approve mode %q (want: auto|deny|ask)", s)
	}
	return nil
}
