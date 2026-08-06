// Package llm middleware_chain.go — ModelMiddlewareChain builder and registry.
//
// This file defines the ModelMiddlewareChain interface and its concrete
// DefaultModelMiddlewareChain implementation. The chain assembles multiple
// ModelMiddleware layers around a base BaseChatModel using the onion model:
// the first registered middleware becomes the outermost layer (called first).
package llm

import (
	"fmt"
	"log/slog"
	"sync"
)

// ModelMiddleware wraps a BaseChatModel to intercept and augment LLM calls.
// It mirrors core.ModelMiddleware but is declared locally to avoid an import
// cycle (core already depends on llm).
type ModelMiddleware interface {
	// Name returns the middleware identifier.
	Name() string
	// WrapModel returns a wrapped view of the given chat model.
	WrapModel(next BaseChatModel) BaseChatModel
}

// Priority constants defining the standard middleware layering order. Higher
// values are more outermost (executed first). The chain keeps middlewares
// sorted by descending priority so that failover runs before retry, retry
// before timeout, and so on.
const (
	// PriorityFailover is the outermost layer — try alternative models first.
	PriorityFailover = 100
	// PriorityRetry retries transient failures.
	PriorityRetry = 90
	// PriorityTimeout enforces time limits.
	PriorityTimeout = 80
	// PrioritySanitize cleans cross-model content.
	PrioritySanitize = 70
	// PriorityLoopDetection detects repetition.
	PriorityLoopDetection = 60
	// PriorityValidate validates the response.
	PriorityValidate = 50
	// PriorityOverflow is the innermost layer — recover from context overflow.
	PriorityOverflow = 40
)

// ModelMiddlewareChain assembles multiple ModelMiddleware layers around a base
// BaseChatModel in a defined order. The first registered middleware becomes the
// outermost layer (called first).
type ModelMiddlewareChain interface {
	// Wrap applies all registered middlewares to the given base model.
	// Middlewares are applied in registration order so that the first
	// registered middleware wraps all subsequent ones (outermost).
	Wrap(model BaseChatModel) BaseChatModel
	// Register adds a middleware to the chain.
	Register(mw ModelMiddleware) error
	// List returns the registered middlewares in order.
	List() []ModelMiddleware
}

// DefaultModelMiddlewareChain is the concrete, thread-safe implementation of
// ModelMiddlewareChain. Middlewares are stored in registration (or priority)
// order; Wrap applies them so the first stored middleware is the outermost
// wrapper. All methods are safe for concurrent use.
type DefaultModelMiddlewareChain struct {
	mu          sync.RWMutex
	middlewares []ModelMiddleware
	priorities  map[string]int
	names       map[string]bool
}

// Compile-time assertion that DefaultModelMiddlewareChain satisfies
// ModelMiddlewareChain.
var _ ModelMiddlewareChain = (*DefaultModelMiddlewareChain)(nil)

// NewModelMiddlewareChain creates an empty DefaultModelMiddlewareChain.
func NewModelMiddlewareChain() *DefaultModelMiddlewareChain {
	return &DefaultModelMiddlewareChain{
		priorities: map[string]int{},
		names:      map[string]bool{},
	}
}

// Wrap applies all registered middlewares to model. Middlewares are applied in
// reverse registration order so the first registered middleware becomes the
// outermost layer (its code runs first on the way in and last on the way out).
func (c *DefaultModelMiddlewareChain) Wrap(model BaseChatModel) BaseChatModel {
	c.mu.RLock()
	defer c.mu.RUnlock()
	slog.Debug("llm_middleware_chain.wrap", "count", len(c.middlewares))
	wrapped := model
	for i := len(c.middlewares) - 1; i >= 0; i-- {
		mw := c.middlewares[i]
		slog.Debug("llm_middleware_chain.apply", "middleware", mw.Name(), "index", i)
		wrapped = mw.WrapModel(wrapped)
	}
	return wrapped
}

// Register adds mw to the end of the chain. It returns an error if mw is nil
// or a middleware with the same name is already registered.
func (c *DefaultModelMiddlewareChain) Register(mw ModelMiddleware) error {
	if mw == nil {
		return fmt.Errorf("llm: cannot register a nil ModelMiddleware")
	}
	name := mw.Name()

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.names[name] {
		return fmt.Errorf("llm: middleware already registered: %s", name)
	}
	c.middlewares = append(c.middlewares, mw)
	c.names[name] = true
	c.priorities[name] = 0
	slog.Debug("llm_middleware_chain.register", "name", name, "count", len(c.middlewares))
	return nil
}

// RegisterWithPriority inserts mw at the position determined by priority.
// Higher priority values are placed earlier in the chain (more outermost).
// Middlewares with equal priority keep stable insertion order. It returns an
// error if mw is nil or a middleware with the same name is already registered.
func (c *DefaultModelMiddlewareChain) RegisterWithPriority(mw ModelMiddleware, priority int) error {
	if mw == nil {
		return fmt.Errorf("llm: cannot register a nil ModelMiddleware")
	}
	name := mw.Name()

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.names[name] {
		return fmt.Errorf("llm: middleware already registered: %s", name)
	}
	// Find the first index whose stored priority is strictly less than the
	// new priority; insert before it so higher-priority middlewares stay
	// outermost and equal-priority entries remain in insertion order.
	pos := len(c.middlewares)
	for i, existing := range c.middlewares {
		if c.priorities[existing.Name()] < priority {
			pos = i
			break
		}
	}
	c.middlewares = append(c.middlewares, nil)
	copy(c.middlewares[pos+1:], c.middlewares[pos:])
	c.middlewares[pos] = mw
	c.names[name] = true
	c.priorities[name] = priority
	slog.Debug("llm_middleware_chain.register_with_priority",
		"name", name, "priority", priority, "position", pos, "count", len(c.middlewares))
	return nil
}

// List returns the registered middlewares in chain order. The returned slice
// is a copy; mutating it does not affect the chain.
func (c *DefaultModelMiddlewareChain) List() []ModelMiddleware {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]ModelMiddleware, 0, len(c.middlewares))
	out = append(out, c.middlewares...)
	return out
}

// NewStandardMiddlewareChain creates a chain with the standard middleware
// ordering: failover -> retry -> timeout -> sanitize -> loopdetection ->
// validate. Middlewares with zero values (nil) are skipped. Duplicate names
// are also skipped with a debug log.
func NewStandardMiddlewareChain(mws ...ModelMiddleware) *DefaultModelMiddlewareChain {
	chain := NewModelMiddlewareChain()
	for _, mw := range mws {
		if mw == nil {
			slog.Debug("llm_standard_chain.skip_nil")
			continue
		}
		if err := chain.Register(mw); err != nil {
			slog.Debug("llm_standard_chain.register_failed", "name", mw.Name(), "err", err)
			continue
		}
	}
	return chain
}
