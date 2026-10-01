package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/production"
	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/tui"

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

// ErrInteractiveRemoteModeUnsupported indicates that interactive remote mode
// has no prompt-and-dispatch protocol in the current CLI.
var ErrInteractiveRemoteModeUnsupported = errors.New("cli: interactive remote mode is unsupported")

// UnsupportedInteractiveRemoteModeError identifies an unsupported TUI mode.
type UnsupportedInteractiveRemoteModeError struct {
	Mode string
}

// Error implements error.
func (e *UnsupportedInteractiveRemoteModeError) Error() string {
	return fmt.Sprintf("interactive: tui.mode %q is unsupported; set tui.mode to local", e.Mode)
}

// Unwrap identifies the unsupported interactive capability.
func (e *UnsupportedInteractiveRemoteModeError) Unwrap() error {
	return ErrInteractiveRemoteModeUnsupported
}

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
	if rc, ok := cfg.(*config.Config); ok && rc.TUI.Mode == "remote" {
		return &UnsupportedInteractiveRemoteModeError{Mode: rc.TUI.Mode}
	}

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
	cloned := core.CloneAgentMessages(msgs)
	items := make([]compaction.TurnItem, len(cloned))
	for i, m := range cloned {
		item := compaction.TurnItem{
			ID:             fmt.Sprintf("msg-%d", i),
			Role:           m.Role,
			Usage:          m.Usage,
			ContentBlocks:  m.ContentBlocks,
			ToolCalls:      m.ToolCalls,
			ToolCallID:     m.ToolCallID,
			ToolName:       m.ToolName,
			InputKind:      m.InputKind,
			ReceiptOutcome: m.ReceiptOutcome,
			Runtime:        m.Runtime,
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
			Role:           it.Role,
			Usage:          it.Usage,
			ContentBlocks:  it.ContentBlocks,
			ToolCalls:      it.ToolCalls,
			ToolCallID:     it.ToolCallID,
			ToolName:       it.ToolName,
			InputKind:      it.InputKind,
			ReceiptOutcome: it.ReceiptOutcome,
			Runtime:        it.Runtime,
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
	return core.CloneAgentMessages(msgs)
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
	var unknownUsageAttempts, unknownCostAttempts int
	var attemptUsageAvailable bool
	usageSource := core.TokenUsageSourceUnknown
	providerModels := make(map[core.TokenUsageProviderModel]struct{})
	costSources := make(map[core.TokenUsageCostSource]struct{})
	priceSources := make(map[core.TokenUsagePriceSource]struct{})
	cost := 0.0
	var costSummary production.AttemptCostSummary
	if assembly.CostTracker != nil {
		attempts, summary := assembly.CostTracker.AttemptCostSnapshot()
		costSummary = summary
		if summary.Attempts > 0 {
			attemptUsageAvailable = true
			inputTokens = summary.KnownInputTokens
			outputTokens = summary.KnownOutputTokens
			unknownUsageAttempts = summary.UnknownUsageAttempts
			unknownCostAttempts = summary.UnknownCostAttempts
			if summary.UnknownUsageAttempts < summary.Attempts {
				usageSource = core.TokenUsageSourceReported
			}
			for _, attempt := range attempts {
				providerModels[core.TokenUsageProviderModel{
					ProviderID: attempt.Result.Identity.ProviderID,
					ModelID:    attempt.Result.Identity.ModelID,
				}] = struct{}{}
				costSources[tokenUsageCostSource(attempt.Status)] = struct{}{}
				priceSources[tokenUsagePriceSource(attempt.PriceSource)] = struct{}{}
			}
		}
		if summary.LegacyCalls > 0 {
			unknownUsageAttempts += summary.LegacyCalls
			costSources[core.TokenUsageCostEstimated] = struct{}{}
			priceSources[tokenUsagePriceSource(summary.LegacyPriceSource)] = struct{}{}
		}
	}

	// Prefer API-reported usage from the last assistant message that has it.
	// API usage reflects the actual token consumption for the full
	// conversation (input = prompt tokens, output = completion tokens).
	if !attemptUsageAvailable {
		if usage := lastAssistantAPIUsage(messages); usage != nil {
			inputTokens = usage.InputTokens
			outputTokens = usage.OutputTokens
			usageSource = core.TokenUsageSourceReported
		} else {
			// Fall back to local estimation when neither the operation ledger nor
			// the latest assistant response reported authoritative usage.
			if assembly.Estimator != nil {
				usageSource = core.TokenUsageSourceEstimated
				for _, msg := range messages {
					n, _ := assembly.Estimator.Estimate(msg.Content) //nolint:errcheck
					if msg.Role == "assistant" {
						outputTokens += n
					} else {
						inputTokens += n
					}
				}
			}
		}
	}

	if assembly.CostTracker != nil {
		cost = costSummary.CombinedKnownCostSubtotal
	}
	if len(costSources) == 0 {
		costSources[core.TokenUsageCostUnknown] = struct{}{}
	}
	contextWindow := assembly.ContextWindow
	if assembly.ModelSelector != nil {
		contextWindow = assembly.ModelSelector.ActiveModelSnapshot().ContextWindow
	}
	providerModelList := make([]core.TokenUsageProviderModel, 0, len(providerModels))
	for providerModel := range providerModels {
		providerModelList = append(providerModelList, providerModel)
	}
	sort.Slice(providerModelList, func(i, j int) bool {
		if providerModelList[i].ProviderID != providerModelList[j].ProviderID {
			return providerModelList[i].ProviderID < providerModelList[j].ProviderID
		}
		return providerModelList[i].ModelID < providerModelList[j].ModelID
	})
	_ = stream.Send(core.AgentEvent{ //nolint:errcheck
		Kind:      "token_usage",
		Timestamp: time.Now(),
		TokenUsage: &core.TokenUsage{
			InputTokens:          inputTokens,
			OutputTokens:         outputTokens,
			MaxTokens:            contextWindow,
			Cost:                 cost,
			UnknownUsageAttempts: unknownUsageAttempts,
			UnknownCostAttempts:  unknownCostAttempts,
			UsageSource:          usageSource,
			CostSources:          sortedTokenUsageCostSources(costSources),
			PriceSources:         sortedTokenUsagePriceSources(priceSources),
			ProviderModels:       providerModelList,
		},
	})
}

func tokenUsageCostSource(status production.AttemptCostStatus) core.TokenUsageCostSource {
	switch status {
	case production.AttemptCostProviderReported:
		return core.TokenUsageCostReported
	case production.AttemptCostEstimated:
		return core.TokenUsageCostEstimated
	default:
		return core.TokenUsageCostUnknown
	}
}

func tokenUsagePriceSource(source production.AttemptCostPriceSource) core.TokenUsagePriceSource {
	switch source {
	case production.AttemptCostFromProvider:
		return core.TokenUsagePriceProvider
	case production.AttemptCostFromDefaultTier:
		return core.TokenUsagePriceDefaultTier
	case production.AttemptCostFromConfiguredTier:
		return core.TokenUsagePriceConfiguredTier
	default:
		return core.TokenUsagePriceUnknown
	}
}

func sortedTokenUsageCostSources(sources map[core.TokenUsageCostSource]struct{}) []core.TokenUsageCostSource {
	result := make([]core.TokenUsageCostSource, 0, len(sources))
	for source := range sources {
		result = append(result, source)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

func sortedTokenUsagePriceSources(sources map[core.TokenUsagePriceSource]struct{}) []core.TokenUsagePriceSource {
	result := make([]core.TokenUsagePriceSource, 0, len(sources))
	for source := range sources {
		result = append(result, source)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
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

// HITLMessage is retained as an alias for compatibility with cli callers.
type HITLMessage = tui.HITLMessage

// HITLResponse is retained as an alias for compatibility with cli callers.
type HITLResponse = tui.HITLResponse

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
			Context:    ctx,
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
