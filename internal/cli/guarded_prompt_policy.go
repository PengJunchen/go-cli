package cli

import (
	"context"

	"github.com/pengjunchen/go-cli/internal/core"
)

const cliSensitiveDataPromptPolicy = "For sensitive data tasks, preserve authorized artifacts and omit personal values or secrets from assistant text. Report safe paths, aggregate results, actual checks, and unresolved work."

type defaultCLIPromptBuilder struct {
	inner core.SystemPromptBuilder
}

func (b *defaultCLIPromptBuilder) Build(ctx context.Context, options core.SystemPromptOptions) string {
	if options.CustomPrompt != "" {
		return b.inner.Build(ctx, options)
	}
	if options.AppendPrompt != "" {
		options.AppendPrompt = cliSensitiveDataPromptPolicy + "\n" + options.AppendPrompt
	} else {
		options.AppendPrompt = cliSensitiveDataPromptPolicy
	}
	return b.inner.Build(ctx, options)
}

func newCLIDefaultPromptBuilder(guarded bool) core.SystemPromptBuilder {
	var builder core.SystemPromptBuilder = core.NewDefaultSystemPromptBuilder()
	if guarded {
		builder = &defaultCLIPromptBuilder{inner: builder}
	}
	return builder
}
