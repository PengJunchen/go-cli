package tools

import (
	"context"
	"fmt"
	"time"
)

const maxBashTimeoutMilliseconds = int64((1<<63 - 1) / time.Millisecond)

// executionTimeout resolves a per-call limit without changing the host policy.
func (t *BashTool) executionTimeout(ctx context.Context, command string, args map[string]any) (time.Duration, error) {
	timeout := t.Timeout
	if t.TimeoutTier {
		timeout = classifyCommand(command)
	}
	timeout = runtimeBashTimeout(ctx, timeout)
	value, present := args["timeout_ms"]
	if !present {
		return timeout, nil
	}
	number, valid := schemaRational(value)
	if !valid || !number.IsInt() || number.Sign() <= 0 || !number.Num().IsInt64() || number.Num().Int64() > maxBashTimeoutMilliseconds {
		return 0, fmt.Errorf("bash: timeout_ms must be an integer between 1 and %d", maxBashTimeoutMilliseconds)
	}
	requested := time.Duration(number.Num().Int64()) * time.Millisecond
	if requested < timeout {
		timeout = requested
	}
	return timeout, nil
}
