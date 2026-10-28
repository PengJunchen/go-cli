package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
)

// runOwnedCommand joins each command's leader, owned process group and output
// readers. Its caller can compose several commands synchronously, but must stop
// immediately on an unknown or pending result. It never reports an individual
// process as completion of the enclosing multi-command tool action.
func runOwnedCommand(ctx context.Context, cmd *exec.Cmd, maxOutput int) (string, managedLocalResult, error) {
	var lifecycle managedLocalResult
	if ctx == nil {
		return "", lifecycle, errors.New("tools: owned command requires context")
	}
	if err := ctx.Err(); err != nil {
		return "", lifecycle, newNotDispatchedExecutionError(err)
	}
	if !ownedCommandPlatformSupported() {
		return runUnmanagedCommand(ctx, cmd, maxOutput)
	}
	execCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	managed, err := startManagedLocalCommandWithStartHook(execCtx, cancel, cmd, nil)
	if err != nil {
		return "", lifecycle, newNotDispatchedExecutionError(err)
	}
	capture := newBoundedOutputCapture(maxOutput)
	lifecycle = managed.collect(execCtx, func(reader io.Reader, stream string) (bool, error) {
		_, readErr := io.Copy(capture.writer(stream), reader)
		return readErr == nil, readErr
	})
	_, _, output, _ := capture.snapshot("\n[output truncated]")
	if lifecycle.forcedPipeClose || !lifecycle.containmentProven || !lifecycle.readersJoined || lifecycle.cleanupErr != nil || lifecycle.readerErr != nil {
		cause := errors.Join(errProcessContainmentUnconfirmed, lifecycle.runErr, lifecycle.cleanupErr, lifecycle.readerErr, execCtx.Err())
		if lifecycle.executionDone != nil {
			return output, lifecycle, pendingProcessErrorWithDone(cause, true, lifecycle.executionDone)
		}
		return output, lifecycle, newUnknownExecutionError(cause)
	}
	if lifecycle.interrupted || execCtx.Err() != nil {
		return output, lifecycle, errors.Join(lifecycle.runErr, execCtx.Err())
	}
	if lifecycle.escalated {
		return output, lifecycle, newUnknownExecutionError(errors.Join(errProcessGroupRemained, lifecycle.runErr))
	}
	if lifecycle.runErr != nil {
		return output, lifecycle, fmt.Errorf("command failed: %w", lifecycle.runErr)
	}
	return output, lifecycle, nil
}

func ownedCommandPlatformSupported() bool { return runtime.GOOS == "darwin" || runtime.GOOS == "linux" }

// runUnmanagedCommand preserves ordinary execution on platforms without the
// reviewed process-group owner. Registrations on those platforms have no
// completion proof, so durable preflight rejects them before reaching here.
func runUnmanagedCommand(ctx context.Context, command *exec.Cmd, maxOutput int) (string, managedLocalResult, error) {
	cmd := exec.CommandContext(ctx, command.Path, command.Args[1:]...)
	cmd.Dir, cmd.Env, cmd.Stdin = command.Dir, command.Env, command.Stdin
	capture := newBoundedOutputCapture(maxOutput)
	cmd.Stdout, cmd.Stderr = capture.writer("stdout"), capture.writer("stderr")
	if err := startPreparedEffect(ctx, cmd.Start); err != nil {
		return "", managedLocalResult{ordinaryPipelineJoined: true}, newNotDispatchedExecutionError(err)
	}
	// Waiting and draining run outside the short final authority guard.
	err := cmd.Wait()
	_, _, output, _ := capture.snapshot("\n[output truncated]")
	return output, managedLocalResult{ordinaryPipelineJoined: ctx.Err() == nil}, errors.Join(err, ctx.Err())
}
