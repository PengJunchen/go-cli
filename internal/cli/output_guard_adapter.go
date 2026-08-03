package cli

import (
	"context"
	"log/slog"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/production"
)

// outputGuardModel wraps an llm.BaseChatModel with an OutputGuard chain.
// It applies the guard to the Content field of the Generate response,
// replacing it with sanitized text when the guard flags a violation.
// Stream calls pass through unchanged (guards operate on complete text only).
type outputGuardModel struct {
	inner llm.BaseChatModel
	guard production.OutputGuard
}

var _ llm.BaseChatModel = (*outputGuardModel)(nil)

func (m *outputGuardModel) Generate(ctx context.Context, msgs []llm.Message, opts ...llm.Option) (*llm.Message, error) {
	resp, err := m.inner.Generate(ctx, msgs, opts...)
	if err != nil {
		return nil, err
	}
	if resp != nil && resp.Content != "" && m.guard != nil {
		result, gErr := m.guard.Check(ctx, resp.Content)
		if gErr == nil && result != nil {
			if result.Sanitized != "" && result.Sanitized != resp.Content {
				slog.InfoContext(ctx, "output_guard_sanitized",
					"allowed", result.Allowed,
					"severity", string(result.Severity),
					"reason", result.Reason,
				)
				resp.Content = result.Sanitized
			} else if !result.Allowed {
				slog.WarnContext(ctx, "output_guard_blocked",
					"severity", string(result.Severity),
					"reason", result.Reason,
				)
				resp.Content = "[output blocked by safety guard]"
			}
		}
	}
	return resp, nil
}

func (m *outputGuardModel) Stream(ctx context.Context, msgs []llm.Message, opts ...llm.Option) (<-chan llm.MessageChunk, error) {
	return m.inner.Stream(ctx, msgs, opts...)
}

// newModelWrapper creates a core.ModelWrapper that applies ProductionModelWrapper
// (retry + cost tracking) as the inner layer and OutputGuard as the outer layer.
// The wrapper is consumed by core.WithModelWrapper in LoopAgent.
func newModelWrapper(pw *production.ProductionModelWrapper, guard production.OutputGuard) core.ModelWrapper {
	return func(model any) any {
		baseModel, ok := model.(llm.BaseChatModel)
		if !ok {
			return model
		}
		wrapped := pw.WrapModel(baseModel)
		if guard != nil {
			return &outputGuardModel{inner: wrapped, guard: guard}
		}
		return wrapped
	}
}
