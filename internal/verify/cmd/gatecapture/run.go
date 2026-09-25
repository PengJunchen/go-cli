// Package gatecapture runs gate subprocesses while placing a hard bound on
// captured output and on pipe cleanup after a child exits or is canceled.
package gatecapture

import (
	"errors"
	"os/exec"
	"sync"
	"time"
)

const DefaultWaitDelay = 500 * time.Millisecond

var (
	ErrOutputLimitExceeded       = errors.New("gatecapture: output exceeded configured limit")
	ErrProcessGroupUnsupported   = errors.New("gatecapture: owned process groups are unsupported on this operating system")
	ErrProcessAlreadyStarted     = errors.New("gatecapture: command was already started")
	ErrForeignProcessGroup       = errors.New("gatecapture: command requests a process group not owned by this run")
	ErrProcessGroupConfiguration = errors.New("gatecapture: command process-group settings conflict with owned group")
	ErrResidualDescendants       = errors.New("gatecapture: command left descendants in its owned process group")
	ErrProcessCleanupUnconfirmed = errors.New("gatecapture: cleanup of the owned process group could not be confirmed")
)

type processGroup interface {
	terminate() error
	cleanup() (residual bool, err error)
}

type Limits struct {
	StdoutBytes int
	StderrBytes int
	WaitDelay   time.Duration
}

type Result struct {
	Stdout         []byte
	Stderr         []byte
	StdoutExceeded bool
	StderrExceeded bool
}

func Run(cmd *exec.Cmd, limits Limits) (Result, error) {
	if cmd == nil {
		return Result{}, errors.New("gatecapture: nil command")
	}
	if limits.StdoutBytes <= 0 {
		return Result{}, errors.New("gatecapture: stdout capture limit must be positive")
	}
	if limits.StderrBytes <= 0 {
		return Result{}, errors.New("gatecapture: stderr capture limit must be positive")
	}
	if limits.WaitDelay <= 0 {
		limits.WaitDelay = DefaultWaitDelay
	}
	group, err := newOwnedProcessGroup(cmd)
	if err != nil {
		return Result{}, err
	}

	var terminateOnce sync.Once
	var terminateErr error
	terminateGroup := func() error {
		terminateOnce.Do(func() {
			terminateErr = group.terminate()
		})
		return terminateErr
	}
	stdout := newBoundedBuffer(limits.StdoutBytes, func() { _ = terminateGroup() })
	stderr := newBoundedBuffer(limits.StderrBytes, func() { _ = terminateGroup() })
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.WaitDelay = limits.WaitDelay
	// CommandContext's default callback kills only the command process. This
	// command is now in a fresh group owned by this invocation, so cancellation
	// must target that group instead.
	cmd.Cancel = terminateGroup

	err = cmd.Run()
	residual, cleanupErr := group.cleanup()
	if residual {
		err = errors.Join(err, ErrResidualDescendants)
	}
	if cleanupErr != nil {
		err = errors.Join(err, cleanupErr)
	}
	result := Result{}
	result.Stdout, result.StdoutExceeded = stdout.snapshot()
	result.Stderr, result.StderrExceeded = stderr.snapshot()
	if result.StdoutExceeded || result.StderrExceeded {
		return result, errors.Join(err, ErrOutputLimitExceeded)
	}
	return result, err
}

type boundedBuffer struct {
	mu         sync.Mutex
	data       []byte
	limit      int
	exceeded   bool
	onExceeded func()
}

func newBoundedBuffer(limit int, onExceeded func()) *boundedBuffer {
	initialCapacity := limit
	if initialCapacity > 32<<10 {
		initialCapacity = 32 << 10
	}
	return &boundedBuffer{data: make([]byte, 0, initialCapacity), limit: limit, onExceeded: onExceeded}
}

func (buffer *boundedBuffer) Write(data []byte) (int, error) {
	buffer.mu.Lock()
	remaining := buffer.limit - len(buffer.data)
	if remaining > len(data) {
		remaining = len(data)
	}
	if remaining > 0 {
		buffer.growLocked(len(buffer.data) + remaining)
		buffer.data = append(buffer.data, data[:remaining]...)
	}
	newOverflow := remaining < len(data) && !buffer.exceeded
	if newOverflow {
		buffer.exceeded = true
	}
	onExceeded := buffer.onExceeded
	buffer.mu.Unlock()

	if newOverflow && onExceeded != nil {
		onExceeded()
	}
	// Continue draining the pipe after the cap is reached. The caller receives
	// the overflow flag after Run returns and rejects the incomplete evidence.
	return len(data), nil
}

func (buffer *boundedBuffer) growLocked(needed int) {
	if needed <= cap(buffer.data) {
		return
	}
	newCapacity := cap(buffer.data)
	if newCapacity == 0 {
		newCapacity = 1
	}
	for newCapacity < needed {
		if newCapacity > buffer.limit/2 {
			newCapacity = buffer.limit
			break
		}
		newCapacity *= 2
	}
	data := make([]byte, len(buffer.data), newCapacity)
	copy(data, buffer.data)
	buffer.data = data
}

func (buffer *boundedBuffer) snapshot() ([]byte, bool) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return append([]byte(nil), buffer.data...), buffer.exceeded
}
