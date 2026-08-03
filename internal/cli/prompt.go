package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"

	"github.com/pengjunchen/go-cli/internal/config"
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
	)
	fs.StringVar(&modelFlag, "model", "", "model name to use")
	fs.StringVar(&providerFlag, "provider", "", "provider name to use")
	fs.BoolVar(&verboseFlag, "verbose", false, "enable verbose output")
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
	assembly, err := AssembleAgent(dispatchCtx, rc, providerName, modelName, c.out)
	if err != nil {
		dispatchSpan.SetStatus(tracing.SpanStatusError, err.Error())
		dispatchSpan.End()
		return newExecutionError("prompt: assemble agent", err)
	}
	defer assembly.Cleanup()

	stream, err := assembly.Harness.Submit(dispatchCtx, prompt)
	if err != nil {
		dispatchSpan.SetStatus(tracing.SpanStatusError, err.Error())
		dispatchSpan.End()
		return newExecutionError("prompt: submit", err)
	}

	out := c.out
	streaming := false
	for ev := range stream.Events() {
		if ev.Content == "" || ev.Kind == "done" {
			continue
		}
		if ev.Incremental {
			// Stream tokens as they arrive, without newlines (typewriter
			// effect for terminals and pipes).
			streaming = true
			if _, werr := fmt.Fprint(out, ev.Content); werr != nil {
				dispatchSpan.SetStatus(tracing.SpanStatusError, werr.Error())
				dispatchSpan.End()
				return newExecutionError("prompt: write output", werr)
			}
			continue
		}
		// Complete non-incremental assistant message.
		if streaming {
			// Content has already been streamed token-by-token; close the
			// line with a trailing newline.
			if _, werr := fmt.Fprintln(out); werr != nil {
				dispatchSpan.SetStatus(tracing.SpanStatusError, werr.Error())
				dispatchSpan.End()
				return newExecutionError("prompt: write output", werr)
			}
		} else {
			// Non-streaming response (e.g. provider without Stream support).
			if _, werr := fmt.Fprintf(out, "%s\n", ev.Content); werr != nil {
				dispatchSpan.SetStatus(tracing.SpanStatusError, werr.Error())
				dispatchSpan.End()
				return newExecutionError("prompt: write output", werr)
			}
		}
		streaming = false
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
