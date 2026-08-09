package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"

	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/tracing"
)

// Prompt command constants. They live in one place so the scanner does not
// flag them as scattered hardcoded values.
const (
	// promptDefaultProvider is the provider name used when neither the
	// -provider flag nor the loaded configuration specifies one.
	promptDefaultProvider = "eino"
	// promptDefaultModel is the model name used when no model is resolved from
	// flags or configuration.
	promptDefaultModel = "gpt-4o-mini"

	// spanPromptRun is the top-level span emitted for a prompt run.
	spanPromptRun = "prompt.run"
	// spanPromptDispatch is the command dispatch span emitted for a prompt run.
	spanPromptDispatch = "command.dispatch"
	// spanPromptStop is the closedown span emitted after the harness stops.
	spanPromptStop = "harness.stop"
)

// promptCmd implements Command and runs an agent with a natural-language
// prompt. It wires the loaded configuration through an llm provider into the
// core agent harness and streams the resulting events to the output writer.
type promptCmd struct {
	out io.Writer
}

// newPromptCmd creates a prompt command writing to out.
func newPromptCmd(out io.Writer) *promptCmd {
	return &promptCmd{out: out}
}

// Name implements Command.
func (c *promptCmd) Name() string { return "prompt" }

// Synopsis implements Command.
func (c *promptCmd) Synopsis() string { return "Run an agent with a natural-language prompt" }

// Run implements Command. It parses the -model, -provider and -verbose flags,
// resolves the chat model from the loaded *config.Config, wires it through the
// core agent harness, and streams the result to the output writer.
func (c *promptCmd) Run(ctx context.Context, cfg Config, args []string) error {
	fs := flag.NewFlagSet("prompt", flag.ContinueOnError)
	fs.SetOutput(c.out)

	var (
		modelFlag    string
		providerFlag string
		verboseFlag  bool
		outputMode   OutputMode
		approveMode  ApproveMode
		forkFlag     string
	)
	fs.StringVar(&modelFlag, "model", "", "model name to use")
	fs.StringVar(&providerFlag, "provider", "", "provider name to use")
	fs.BoolVar(&verboseFlag, "verbose", false, "enable verbose output")
	fs.Var(&outputMode, "output", "output format: json|stream|text (default text)")
	fs.Var(&approveMode, "approve", "approval mode: auto|deny|ask (default ask)")
	fs.StringVar(&forkFlag, "fork", "", "fork from an existing session id")
	if err := fs.Parse(args); err != nil {
		return newUsageError("prompt: %v", err)
	}

	remaining := fs.Args()
	if len(remaining) == 0 {
		return newUsageError("prompt: missing message argument")
	}
	prompt := remaining[0]

	var rc *config.Config
	if v, ok := cfg.(*config.Config); ok {
		rc = v
	}

	modelName := resolveModelName(modelFlag, rc)
	providerName := resolveProviderName(providerFlag, rc)

	span, spanCtx := tracing.SpanFromContext(ctx, spanPromptRun, tracing.SpanKindInternal)
	span.SetAttributes(
		tracing.Attribute{Key: "command", Value: c.Name()},
		tracing.Attribute{Key: "args", Value: args},
		tracing.Attribute{Key: "version", Value: Version},
	)
	defer func() {
		span.SetStatus(tracing.SpanStatusOK, "")
		span.End()
	}()

	dispatchSpan, dispatchCtx := tracing.SpanFromContext(spanCtx, spanPromptDispatch, tracing.SpanKindInternal)
	dispatchSpan.SetAttributes(
		tracing.Attribute{Key: "command", Value: c.Name()},
		tracing.Attribute{Key: "model", Value: modelName},
		tracing.Attribute{Key: "provider", Value: providerName},
	)

	logger := slog.Default()
	logger.Info("cli_prompt_run",
		"op", "cli.prompt.run",
		"provider", providerName,
		"model", modelName,
		"verbose", verboseFlag,
	)

	// Assemble the full agent runtime with all production wiring (same as
	// interactive: model wrapping, tools, approval gates, retry/cost tracking,
	// output guards, subagent, compaction).
	assembleOpts := []AssembleOption{WithApproveMode(approveMode)}
	if forkFlag != "" {
		// --fork requires a session store to build the tree from.
		assembleOpts = append(assembleOpts, WithSessionPersistence(true))
	}
	assembly, err := AssembleAgent(dispatchCtx, rc, providerName, modelName, c.out,
		assembleOpts...)
	if err != nil {
		dispatchSpan.SetStatus(tracing.SpanStatusError, err.Error())
		dispatchSpan.End()
		return newExecutionError("prompt: assemble agent", err)
	}
	defer assembly.Cleanup()

	// Fork from an existing session: build a SessionTree from the store,
	// zero-copy branch at the requested entry, rebuild context, and inject
	// the resulting history into the agent.
	if forkFlag != "" {
		if err := c.forkSession(dispatchCtx, assembly, forkFlag); err != nil {
			dispatchSpan.SetStatus(tracing.SpanStatusError, err.Error())
			dispatchSpan.End()
			return newExecutionError("prompt: fork session", err)
		}
	}

	stream, err := assembly.Harness.Submit(dispatchCtx, prompt)
	if err != nil {
		dispatchSpan.SetStatus(tracing.SpanStatusError, err.Error())
		dispatchSpan.End()
		return newExecutionError("prompt: submit", err)
	}

	out := c.out
	if err := c.consumeEvents(out, stream.Events(), outputMode); err != nil {
		dispatchSpan.SetStatus(tracing.SpanStatusError, err.Error())
		dispatchSpan.End()
		return newExecutionError("prompt: write output", err)
	}

	result, err := stream.Result()
	dispatchSpan.SetStatus(tracing.SpanStatusOK, "")
	dispatchSpan.End()

	c.emitClosedown(spanCtx, c.Name())

	if err != nil {
		return newExecutionError("prompt", err)
	}
	if result.Content != "" {
		slog.Info("cli_prompt_complete",
			"op", "cli.prompt.complete",
			"result_len", len(result.Content),
		)
	}
	return nil
}

// consumeEvents reads events from the stream and writes them to out according
// to the specified output mode.
func (c *promptCmd) consumeEvents(out io.Writer, events <-chan core.AgentEvent, mode OutputMode) error {
	switch mode {
	case OutputJSON:
		return c.consumeEventsJSON(out, events)
	case OutputStream:
		return c.consumeEventsStream(out, events)
	default:
		return c.consumeEventsText(out, events)
	}
}

// consumeEventsJSON writes each AgentEvent as a newline-delimited JSON object
// (NDJSON), suitable for programmatic consumption in CI/CD pipelines.
func (c *promptCmd) consumeEventsJSON(out io.Writer, events <-chan core.AgentEvent) error {
	enc := json.NewEncoder(out)
	for ev := range events {
		if err := enc.Encode(ev); err != nil {
			return err
		}
	}
	return nil
}

// consumeEventsStream outputs raw content tokens without formatting, suitable
// for piping to other tools. Only incremental message tokens are emitted to
// avoid duplicating content from non-incremental complete messages.
func (c *promptCmd) consumeEventsStream(out io.Writer, events <-chan core.AgentEvent) error {
	for ev := range events {
		if ev.Content == "" || ev.Kind == "done" {
			continue
		}
		if !ev.Incremental && ev.Kind == "message" {
			// Non-incremental complete message: output once with newline.
			if _, err := fmt.Fprintln(out, ev.Content); err != nil {
				return err
			}
			continue
		}
		if _, err := fmt.Fprint(out, ev.Content); err != nil {
			return err
		}
	}
	return nil
}

// consumeEventsText outputs human-readable text. Streams incremental tokens
// and prints complete messages with newlines.
func (c *promptCmd) consumeEventsText(out io.Writer, events <-chan core.AgentEvent) error {
	streaming := false
	for ev := range events {
		if ev.Content == "" || ev.Kind == "done" {
			continue
		}
		if ev.Incremental {
			streaming = true
			if _, err := fmt.Fprint(out, ev.Content); err != nil {
				return err
			}
			continue
		}
		if streaming {
			if _, err := fmt.Fprintln(out); err != nil {
				return err
			}
		} else {
			if _, err := fmt.Fprintf(out, "%s\n", ev.Content); err != nil {
				return err
			}
		}
		streaming = false
	}
	if streaming {
		if _, err := fmt.Fprintln(out); err != nil {
			return err
		}
	}
	return nil
}

// emitClosedown records the closedown span emitted once the harness has
// finished producing events for a prompt run.
func (c *promptCmd) emitClosedown(ctx context.Context, command string) {
	stopSpan, _ := tracing.SpanFromContext(ctx, spanPromptStop, tracing.SpanKindInternal)
	stopSpan.SetAttributes(
		tracing.Attribute{Key: "command", Value: command},
	)
	stopSpan.SetStatus(tracing.SpanStatusOK, "")
	stopSpan.End()
}

// forkSession builds a SessionTree from the assembled store, zero-copy branches
// at the requested session entry, rebuilds the context, and injects the
// resulting history into the agent. This enables headless continuation from any
// point in a previous conversation without affecting the original session.
func (c *promptCmd) forkSession(ctx context.Context, assembly *AgentAssembly, sessionID string) error {
	if assembly.SessionStore == nil {
		return fmt.Errorf("session store unavailable (configure session.store_path)")
	}

	treeBuilder := session.NewDefaultSessionTreeBuilder()
	sessionTree, err := treeBuilder.BuildFromStore(ctx, assembly.SessionStore)
	if err != nil {
		return fmt.Errorf("build session tree: %w", err)
	}
	if sessionTree.CurrentLeaf() == "" {
		return fmt.Errorf("session store is empty, nothing to fork from")
	}

	// Branch zero-copy repoints the leaf at the requested entry.
	if err := sessionTree.Branch(ctx, sessionID); err != nil {
		return fmt.Errorf("fork from session %q: %w", sessionID, err)
	}

	// Rebuild context for the forked branch.
	sc, err := sessionTree.BuildContext(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("rebuild context: %w", err)
	}

	assembly.Agent.SetHistory(session.EntriesToAgentMessages(sc.Messages))
	slog.Info("cli_prompt_fork", "op", "cli.prompt.fork", "session_id", sessionID, "messages", len(sc.Messages))
	return nil
}

// resolveModelName returns the effective model name, preferring the CLI flag
// and falling back to the loaded configuration, then to the default.
func resolveModelName(flagValue string, rc *config.Config) string {
	if flagValue != "" {
		return flagValue
	}
	if rc != nil {
		if rc.Provider.Model != "" {
			return rc.Provider.Model
		}
		if rc.Model.Name != "" {
			return rc.Model.Name
		}
	}
	return promptDefaultModel
}

// resolveProviderName returns the effective provider name, preferring the CLI
// flag and falling back to the loaded configuration, then to the default.
func resolveProviderName(flagValue string, rc *config.Config) string {
	if flagValue != "" {
		return flagValue
	}
	if rc != nil && rc.Provider.Name != "" {
		return rc.Provider.Name
	}
	return promptDefaultProvider
}

var _ Command = (*promptCmd)(nil)
