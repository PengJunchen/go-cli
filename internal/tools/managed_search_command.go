package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
)

// runManagedSearchCommand drains stdout and stderr while the existing local
// process-scope runner owns the helper group and both pipe readers.
func runManagedSearchCommand(ctx context.Context, cmd *exec.Cmd) ([]byte, managedLocalResult, error) {
	var lifecycle managedLocalResult
	if ctx == nil {
		return nil, lifecycle, errors.New("tools: nil search command context")
	}
	if err := ctx.Err(); err != nil {
		return nil, lifecycle, newNotDispatchedExecutionError(err)
	}

	execCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	managed, err := startManagedLocalCommand(cancel, cmd)
	if err != nil {
		return nil, lifecycle, err
	}

	var stdout bytes.Buffer
	lifecycle = managed.collect(execCtx, func(reader io.Reader, stream string) (bool, error) {
		if stream == "stderr" {
			_, readErr := io.Copy(io.Discard, reader)
			return readErr == nil, readErr
		}
		_, readErr := io.Copy(&stdout, reader)
		return readErr == nil, readErr
	})
	return append([]byte(nil), stdout.Bytes()...), lifecycle, nil
}

// managedSearchLifecycleError reports interruption or unconfirmed process
// cleanup without classifying an ordinary helper exit status as a lifecycle
// failure. Callers may use a completed ordinary helper error for the existing
// fallback, but must stop for a pending local resource or unproved containment.
func managedSearchLifecycleError(ctx context.Context, label string, lifecycle managedLocalResult) error {
	if lifecycle.forcedPipeClose {
		cause := errors.Join(fmt.Errorf("%s: %w", label, errProcessContainmentUnconfirmed), lifecycle.runErr, lifecycle.cleanupErr, lifecycle.readerErr)
		if lifecycle.executionDone != nil {
			return pendingProcessErrorWithDone(cause, true, lifecycle.executionDone)
		}
		return newUnknownExecutionError(cause)
	}
	if lifecycle.interrupted {
		cause := error(context.Canceled)
		if ctx != nil && ctx.Err() != nil {
			cause = ctx.Err()
		}
		return localLifecycleError(errors.Join(fmt.Errorf("%s interrupted: %w", label, cause), lifecycle.runErr, lifecycle.readerErr), lifecycle)
	}
	if !lifecycle.quiescent || !lifecycle.readersJoined || lifecycle.cleanupErr != nil {
		cause := errors.Join(errProcessCleanupUnconfirmed, lifecycle.runErr, lifecycle.cleanupErr, lifecycle.readerErr)
		return localLifecycleError(fmt.Errorf("%s: %w", label, cause), lifecycle)
	}
	if lifecycle.escalated {
		return newUnknownExecutionError(errors.Join(fmt.Errorf("%s: %w", label, errProcessGroupRemained), lifecycle.runErr))
	}
	if lifecycle.readerErr != nil {
		return newUnknownExecutionError(fmt.Errorf("%s output read failed: %w", label, lifecycle.readerErr))
	}
	return nil
}

func isPendingExecution(err error) bool {
	var pending PendingExecutionError
	return errors.As(err, &pending)
}

func shouldFallbackAfterManagedHelper(ctx context.Context, err error) bool {
	if err == nil {
		return true
	}
	if ctx == nil {
		return false
	}
	if errors.Is(err, errProcessContainmentUnconfirmed) {
		return false
	}
	if isPendingExecution(err) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return ctx.Err() == nil
}
