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
	cleanup() (residual bool, err error)
}

type processGroupCleanupOwner struct {
	group    processGroup
	mu       sync.Mutex
	done     chan struct{}
	residual bool
	err      error
}

func (owner *processGroupCleanupOwner) startLocked() {
	if owner.done != nil {
		return
	}
	done := make(chan struct{})
	owner.done = done
	go func() {
		residual, err := owner.group.cleanup()
		owner.mu.Lock()
		owner.residual = residual
		owner.err = err
		close(done)
		owner.mu.Unlock()
	}()
}

func (owner *processGroupCleanupOwner) request() {
	owner.mu.Lock()
	owner.startLocked()
	owner.mu.Unlock()
}

func (owner *processGroupCleanupOwner) wait() error {
	owner.request()
	owner.mu.Lock()
	done := owner.done
	owner.mu.Unlock()
	<-done
	owner.mu.Lock()
	defer owner.mu.Unlock()
	return owner.err
}

func (owner *processGroupCleanupOwner) finish() (startedBeforeFinish bool, residual bool, err error) {
	owner.mu.Lock()
	startedBeforeFinish = owner.done != nil
	owner.startLocked()
	done := owner.done
	owner.mu.Unlock()
	<-done
	owner.mu.Lock()
	defer owner.mu.Unlock()
	return startedBeforeFinish, owner.residual, owner.err
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
	cleanupOwner := &processGroupCleanupOwner{group: group}
	stdout := newBoundedBuffer(limits.StdoutBytes, cleanupOwner.request)
	stderr := newBoundedBuffer(limits.StderrBytes, cleanupOwner.request)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.WaitDelay = limits.WaitDelay
	// CommandContext's default callback kills only the command process. Replace
	// it when present so context cancellation shares the same bounded owner as
	// output overflow and post-command cleanup. Plain exec.Command values must
	// retain a nil Cancel callback for os/exec compatibility.
	if cmd.Cancel != nil {
		cmd.Cancel = cleanupOwner.wait
	}

	err = cmd.Run()
	cleanupStartedBeforeFinish, residual, cleanupErr := cleanupOwner.finish()
	if residual && !cleanupStartedBeforeFinish {
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
