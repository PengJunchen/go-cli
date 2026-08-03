package core

// ModelWrapper wraps a base chat model with additional behavior (e.g.
// middleware). Since core cannot export llm.BaseChatModel in the type
// signature without creating a hard dependency, the wrapper accepts and
// returns any, and the LoopAgent type-asserts the result back to
// llm.BaseChatModel before use.
//
// This indirection lets the llm package provide an adapter
// (llm.WrapModelForLoop) that converts a ModelMiddlewareChain into a
// ModelWrapper without an import cycle.
type ModelWrapper func(model any) any

// WithModelWrapper sets a function that wraps the LLM model before use.
// The wrapper is applied once at the start of Run, before the first LLM
// call. If the wrapper returns a non-nil value that satisfies
// llm.BaseChatModel, the wrapped model replaces the base model for the
// duration of that Run.
func WithModelWrapper(wrapper ModelWrapper) LoopOption {
	return func(c *loopConfig) { c.modelWrapper = wrapper }
}
