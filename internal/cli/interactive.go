package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/session"

	tea "github.com/charmbracelet/bubbletea"
)

// Interactive command constants. They live in one place so the scanner does not
// flag them as scattered hardcoded values.
const (
	// spanInteractiveRun is the top-level span for an interactive session.
	spanInteractiveRun = "interactive.run"
	// spanInteractiveTurn is the span for a single turn in the session.
	spanInteractiveTurn = "interactive.turn"
	// spanInteractiveMentionExpand is the span for @-mention file expansion
	// within a single turn.
	spanInteractiveMentionExpand = "interactive.mention_expand"
	// exitCommand is the user input that terminates the interactive session.
	exitCommand = "exit"
)

// interactiveCmd implements Command and runs an interactive multi-turn agent
// session with TUI rendering, MCP tool support, skill execution, and automatic
// session compaction.
type interactiveCmd struct {
	out             io.Writer
	in              io.Reader
	lineEditor      LineEditor
	mentionExpander *MentionExpander
	slashReg        *SlashCommandRegistry
}

// newInteractiveCmd creates an interactive command reading from in and writing
// to out.
func newInteractiveCmd(in io.Reader, out io.Writer) *interactiveCmd {
	return &interactiveCmd{out: out, in: in, slashReg: defaultSlashReg}
}

// Name implements Command.
func (c *interactiveCmd) Name() string { return "interactive" }

// Synopsis implements Command.
func (c *interactiveCmd) Synopsis() string {
	return "Start an interactive multi-turn agent session with TUI"
}

// Run implements Command. It constructs a REPLSession and delegates to it.
// First-run onboarding is handled inside REPLSession.start() after flag
// parsing so that invalid flags produce a UsageError before the wizard runs.
func (c *interactiveCmd) Run(ctx context.Context, cfg Config, args []string) error {
	rs := &REPLSession{
		cmd:             c,
		out:             c.out,
		in:              c.in,
		lineEditor:      c.lineEditor,
		mentionExpander: c.mentionExpander,
		slashReg:        c.slashReg,
	}
	return rs.Run(ctx, cfg, args)
}

// messagesToTurnItems converts core.AgentMessage slice to compaction.TurnItem
// slice for the compaction pipeline.
func messagesToTurnItems(msgs []core.AgentMessage) []compaction.TurnItem {
	items := make([]compaction.TurnItem, len(msgs))
	for i, m := range msgs {
		item := compaction.TurnItem{
			ID:            fmt.Sprintf("msg-%d", i),
			Role:          m.Role,
			ContentBlocks: m.ContentBlocks,
			ToolCalls:     m.ToolCalls,
			ToolCallID:    m.ToolCallID,
			ToolName:      m.ToolName,
		}
		if m.Role == compaction.RoleTool {
			item.ToolResult = m.Content
		} else {
			item.Content = m.Content
		}
		items[i] = item
	}
	return items
}

// turnItemsToMessages converts compaction.TurnItem slice back to
// core.AgentMessage slice.
func turnItemsToMessages(items []compaction.TurnItem) []core.AgentMessage {
	msgs := make([]core.AgentMessage, len(items))
	for i, it := range items {
		msg := core.AgentMessage{
			Role:          it.Role,
			ContentBlocks: it.ContentBlocks,
			ToolCalls:     it.ToolCalls,
			ToolCallID:    it.ToolCallID,
			ToolName:      it.ToolName,
		}
		if it.Role == compaction.RoleTool {
			msg.Content = it.ToolResult
		} else {
			msg.Content = it.Content
			if it.IsCompaction && it.Content == "" {
				msg.Content = "[compacted]"
			}
		}
		msgs[i] = msg
	}
	return msgs
}

// loadSessionHistory reads a JSONL session file and reconstructs the message
// history as []core.AgentMessage. Only user and assistant entries are included.
func loadSessionHistory(path string) ([]core.AgentMessage, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close() //nolint:errcheck

	var messages []core.AgentMessage
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		var entry session.SessionEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		if entry.Type == session.EntryTypeUser || entry.Type == session.EntryTypeAssistant {
			messages = append(messages, core.AgentMessage{
				Role:          string(entry.Type),
				Content:       entry.Content,
				ContentBlocks: entry.ContentBlocks,
				ToolCalls:     entry.ToolCalls,
				ToolCallID:    entry.ToolCallID,
				ToolName:      entry.ToolName,
			})
		}
	}
	return messages, scanner.Err()
}

// emitTokenUsageEvent estimates the total token usage from the agent's message
// history and the accumulated cost from the CostTracker, then sends a
// token_usage event to the stream so the TUI status bar can update.
//
// When the last assistant message carries API-reported Usage (captured during
// streaming), those values are preferred over the local estimation because
// they reflect the actual token consumption billed by the provider. When no
// API usage is available, the function falls back to estimating tokens from
// message content via the configured TokenEstimator.
func emitTokenUsageEvent(stream *core.EventStreamImpl, assembly *AgentAssembly) {
	if assembly.Agent == nil {
		return
	}
	messages := assembly.Agent.Messages()

	var inputTokens, outputTokens int

	// Prefer API-reported usage from the last assistant message that has it.
	// API usage reflects the actual token consumption for the full
	// conversation (input = prompt tokens, output = completion tokens).
	if usage := lastAssistantAPIUsage(messages); usage != nil {
		inputTokens = usage.InputTokens
		outputTokens = usage.OutputTokens
	} else {
		// Fall back to local estimation when the API did not report usage.
		if assembly.Estimator == nil {
			return
		}
		for _, msg := range messages {
			n, _ := assembly.Estimator.Estimate(msg.Content) //nolint:errcheck
			if msg.Role == "assistant" {
				outputTokens += n
			} else {
				inputTokens += n
			}
		}
	}

	cost := 0.0
	if assembly.CostTracker != nil {
		cost = assembly.CostTracker.Total()
	}
	_ = stream.Send(core.AgentEvent{ //nolint:errcheck
		Kind:      "token_usage",
		Timestamp: time.Now(),
		TokenUsage: &core.TokenUsage{
			InputTokens:  inputTokens,
			OutputTokens: outputTokens,
			MaxTokens:    assembly.ContextWindow,
			Cost:         cost,
		},
	})
}

// lastAssistantAPIUsage scans messages from the end and returns the Usage from
// the last assistant message that carries non-nil API-reported usage, or nil
// when none is found.
func lastAssistantAPIUsage(messages []core.AgentMessage) *llm.Usage {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "assistant" && messages[i].Usage != nil {
			return messages[i].Usage
		}
	}
	return nil
}

var _ Command = (*interactiveCmd)(nil)

// cliHITLEmitter implements core.HITLQuestionEmitter for the interactive CLI.
// It prints the question to the output writer and reads the answer from stdin.
//
// When program is non-nil (TUI mode), Emit routes the question through the
// bubbletea program via program.Send(HITLMessage{...}) instead of writing
// directly to stdout. Writing to stdout while bubbletea owns the terminal
// corrupts the display, so the program path must be used whenever a TUI is
// running. When program is nil (plain CLI mode), Emit falls back to direct
// stdout output.
type cliHITLEmitter struct {
	out     io.Writer
	mu      sync.RWMutex
	program *tea.Program
}

// SetProgram sets the bubbletea program used to route HITL questions through
// the TUI. It is safe for concurrent use with Emit.
func (e *cliHITLEmitter) SetProgram(p *tea.Program) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.program = p
}

// HITLMessage is a bubbletea message carrying a HITL question to the TUI. The
// TUI renders the question, collects the user's answer, and sends a single
// HITLResponse on ResponseCh so the emitter can deliver it to the agent.
type HITLMessage struct {
	QuestionID string
	Question   string
	Options    []string
	ResponseCh chan HITLResponse
}

// HITLResponse is the user's answer to a HITLMessage, sent back by the TUI.
type HITLResponse struct {
	Answer string
	Err    error
}

func (e *cliHITLEmitter) Emit(ctx context.Context, event core.HITLQuestionEvent) error {
	// TUI mode: route the question through the bubbletea program so it renders
	// inside the TUI instead of corrupting stdout.
	e.mu.RLock()
	prog := e.program
	e.mu.RUnlock()
	if prog != nil {
		respCh := make(chan HITLResponse, 1)
		prog.Send(HITLMessage{
			QuestionID: event.QuestionID,
			Question:   event.Question,
			Options:    event.Options,
			ResponseCh: respCh,
		})
		select {
		case resp := <-respCh:
			if resp.Err != nil {
				return resp.Err
			}
			select {
			case event.ResponseCh <- core.HITLAnswer{QuestionID: event.QuestionID, Answer: resp.Answer}:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	// Plain CLI mode: write directly to stdout and read from stdin.
	fmt.Fprintf(e.out, "\n[ask_user] %s\n", event.Question) //nolint:errcheck
	for i, opt := range event.Options {
		fmt.Fprintf(e.out, "  %d. %s\n", i+1, opt) //nolint:errcheck
	}
	fmt.Fprint(e.out, "> ") //nolint:errcheck

	line, err := readLine(os.Stdin)
	if err != nil {
		return err
	}
	answer := strings.TrimSpace(line)

	select {
	case event.ResponseCh <- core.HITLAnswer{QuestionID: event.QuestionID, Answer: answer}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// readLine reads a single line from r using a fresh bufio.Reader so it does
// not compete with the REPL scanner's internal buffer.
func readLine(r io.Reader) (string, error) {
	br := bufio.NewReader(r)
	return br.ReadString('\n')
}
