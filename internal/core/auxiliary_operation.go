package core

import (
	"context"

	"github.com/pengjunchen/go-cli/internal/llm"
)

// WithAuxiliaryOperations installs independent provider accounting for bounded
// administrative requests. It preserves cancellation and current authorization
// checks, but cannot reuse an agent run identity, owner, or durable commit sink.
func WithAuxiliaryOperations(ctx context.Context, record func(llm.ProviderAttemptResult) error, retryPolicy llm.RetryPolicy) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return llm.WithRunOperationFactory(auxiliaryOperationContext{ctx}, &providerOperationFactory{record: record, retryPolicy: retryPolicy})
}

type auxiliaryOperationContext struct{ context.Context }

func (ctx auxiliaryOperationContext) Value(key any) any {
	switch key.(type) {
	case runIDContextKey, runCommitContextKey, runOwnerContextKey:
		return nil
	default:
		return ctx.Context.Value(key)
	}
}
