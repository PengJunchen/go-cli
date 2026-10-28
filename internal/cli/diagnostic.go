package cli

import (
	"context"
	"io"
	"os"
)

type diagnosticWriterKey struct{}

// WithDiagnosticWriter directs human runtime diagnostics to the invocation's
// error stream, separately from result output. Without it diagnostics use stderr.
func WithDiagnosticWriter(ctx context.Context, writer io.Writer) context.Context {
	if writer == nil {
		return ctx
	}
	return context.WithValue(ctx, diagnosticWriterKey{}, writer)
}

func diagnosticWriter(ctx context.Context) io.Writer {
	if writer, ok := ctx.Value(diagnosticWriterKey{}).(io.Writer); ok && writer != nil {
		return writer
	}
	return os.Stderr
}
