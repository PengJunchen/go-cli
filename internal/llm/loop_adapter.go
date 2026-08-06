// Package llm loop_adapter.go - adapter that converts a ModelMiddlewareChain
// into a generic model wrapper function compatible with core.ModelWrapper.
//
// Because core imports llm, the llm package cannot import core without
// creating an import cycle. To break the cycle, WrapModelForLoop returns an
// unnamed function type (func(any) any) whose underlying type is identical to
// core.ModelWrapper. The caller can assign the result directly to a
// core.ModelWriter variable or pass it to core.WithModelWrapper.
package llm

import (
	"fmt"
	"log/slog"
)

// WrapModelForLoop converts a ModelMiddlewareChain into a generic wrapper
// function that can be passed to core.WithModelWrapper. The returned function
// accepts any (expected to be an llm.BaseChatModel), applies the middleware
// chain, and returns the wrapped model as any. If the input is not a
// BaseChatModel the original value is returned unchanged.
func WrapModelForLoop(chain ModelMiddlewareChain) func(model any) any {
	return func(model any) any {
		baseModel, ok := model.(BaseChatModel)
		if !ok {
			slog.Warn("llm.loop_adapter.not_base_chat_model",
				"type", fmt.Sprintf("%T", model),
			)
			return model
		}
		wrapped := chain.Wrap(baseModel)
		slog.Info("llm.loop_adapter.wrap",
			"middleware_count", len(chain.List()),
		)
		return wrapped
	}
}
