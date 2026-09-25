package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

const (
	processTerminateGrace   = 100 * time.Millisecond
	processKillWait         = 300 * time.Millisecond
	processReaderCloseGrace = 150 * time.Millisecond
	processPollInterval     = 5 * time.Millisecond
	processContainmentScope = "process_group"
)

var (
	errProcessGroupRemained      = errors.New("child processes remained after the shell exited")
	errProcessCleanupUnconfirmed = errors.New("process cleanup could not be confirmed")
)

// localProcessScope owns the OS process group started for one local command.
// Quiescence covers the group, leader, and local readers. POSIX process groups
// cannot detect descendants that deliberately escape with setsid/setpgid and
// close inherited pipes; this is not a general process-tree sandbox.
type localProcessScope struct {
	pgid int
}

type managedLocalCommand struct {
	cmd    *exec.Cmd
	scope  *localProcessScope
	stdout *os.File
	stderr *os.File
	cancel context.CancelFunc
}

type managedLocalResult struct {
	runErr          error
	readerErr       error
	readersJoined   bool
	forcedPipeClose bool
	interrupted     bool
	quiescent       bool
	escalated       bool
	cleanupErr      error
	executionDone   <-chan struct{}
}

// startManagedLocalCommand starts a command in a fresh process group and owns
// both pipe ends so leader exit can be observed independently of reader EOF.
func startManagedLocalCommand(cancel context.CancelFunc, cmd *exec.Cmd) (*managedLocalCommand, error) {
	scope, err := newLocalProcessScope(cmd)
	if err != nil {
		return nil, err
	}
	stdoutRead, stdoutWrite, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("tools: create stdout pipe: %w", err)
	}
	stderrRead, stderrWrite, err := os.Pipe()
	if err != nil {
		_ = stdoutRead.Close()
		_ = stdoutWrite.Close()
		return nil, fmt.Errorf("tools: create stderr pipe: %w", err)
	}
	cmd.Stdout = stdoutWrite
	cmd.Stderr = stderrWrite
	if err := cmd.Start(); err != nil {
		_ = stdoutRead.Close()
		_ = stdoutWrite.Close()
		_ = stderrRead.Close()
		_ = stderrWrite.Close()
		return nil, err
	}
	// The parent must close its copies immediately; otherwise EOF would be
	// hidden after all command-group writers have exited.
	_ = stdoutWrite.Close()
	_ = stderrWrite.Close()
	scope.started(cmd)
	return &managedLocalCommand{cmd: cmd, scope: scope, stdout: stdoutRead, stderr: stderrRead, cancel: cancel}, nil
}

// collect drains both pipes concurrently while the owner watches the leader,
// context, and process group. Readers are joined only after bounded cleanup.
func (m *managedLocalCommand) collect(ctx context.Context, read func(io.Reader, string) (bool, error)) managedLocalResult {
	var stdoutDrained, stderrDrained bool
	var stdoutErr, stderrErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		stdoutDrained, stdoutErr = read(m.stdout, "stdout")
		if !stdoutDrained || stdoutErr != nil {
			m.cancel()
		}
	}()
	go func() {
		defer wg.Done()
		stderrDrained, stderrErr = read(m.stderr, "stderr")
		if !stderrDrained || stderrErr != nil {
			m.cancel()
		}
	}()
	readersDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(readersDone)
	}()
	runErrCh := make(chan error, 1)
	leaderDone := make(chan struct{})
	go func() {
		runErrCh <- m.cmd.Wait()
		close(leaderDone)
	}()
	scopeOutcome := m.scope.supervise(ctx, leaderDone, readersDone, func() {
		_ = m.stdout.Close()
		_ = m.stderr.Close()
	})
	joined := waitChannel(readersDone, processReaderCloseGrace)
	// Close both owned read descriptors after normal EOF and after a forced
	// bounded join attempt. os.File does not release its descriptor at EOF.
	_ = m.stdout.Close()
	_ = m.stderr.Close()
	var runErr error
	leaderJoined := waitChannel(leaderDone, processReaderCloseGrace)
	if leaderJoined {
		runErr = <-runErrCh
	}
	var readerErr error
	if joined {
		readerErr = errors.Join(stdoutErr, stderrErr)
	}
	result := managedLocalResult{
		readersJoined:   joined,
		forcedPipeClose: scopeOutcome.forcedPipeClose,
		interrupted:     scopeOutcome.interrupted,
		readerErr:       readerErr,
		runErr:          runErr,
		// Reader success is an output-integrity result, not a resource-lifecycle
		// result. A closed pipe or read error is safe to report once the reader
		// goroutine has joined and the owned process group is gone.
		quiescent:  scopeOutcome.quiescent && leaderJoined && joined,
		escalated:  scopeOutcome.escalated,
		cleanupErr: scopeOutcome.cleanupErr,
	}
	if !leaderJoined {
		result.cleanupErr = errors.Join(result.cleanupErr, errProcessCleanupUnconfirmed)
	}
	if !joined {
		result.cleanupErr = errors.Join(result.cleanupErr, errors.New("output readers did not stop after pipe closure"))
	}
	if !result.quiescent && scopeOutcome.monitorable {
		result.executionDone = observeProcessCompletion(m.scope.pgid, leaderDone, readersDone)
	}
	return result
}

func newLocalProcessScope(cmd *exec.Cmd) (*localProcessScope, error) {
	if err := configureCommandProcessGroup(cmd); err != nil {
		return nil, fmt.Errorf("tools: configure process containment: %w", err)
	}
	return &localProcessScope{}, nil
}

func (s *localProcessScope) started(cmd *exec.Cmd) {
	if cmd.Process != nil {
		// The platform-specific setup creates a process group whose ID is the
		// leader PID. This in-memory ID is never restored from a record.
		s.pgid = cmd.Process.Pid
	}
}

func (s *localProcessScope) requestTerminate() error {
	if s.pgid <= 0 {
		return os.ErrProcessDone
	}
	if err := signalCommandProcessGroup(s.pgid, false); err != nil {
		return err
	}
	return nil
}

func (s *localProcessScope) requestKill() error {
	if s.pgid <= 0 {
		return os.ErrProcessDone
	}
	return signalCommandProcessGroup(s.pgid, true)
}

// cleanupRemaining stops background members after the shell leader has been
// reaped. Quiescence is true only after the OS confirms the group is gone.
func (s *localProcessScope) cleanupRemaining() (quiescent, escalated, monitorable bool, err error) {
	if s.pgid <= 0 {
		return false, false, false, errProcessCleanupUnconfirmed
	}
	alive, err := commandProcessGroupAlive(s.pgid)
	if err != nil {
		return false, false, false, fmt.Errorf("check process group: %w", err)
	}
	monitorable = true
	if !alive {
		return true, false, monitorable, nil
	}

	escalated = true
	if err := s.requestTerminate(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return false, escalated, monitorable, fmt.Errorf("terminate process group: %w", err)
	}
	if waitProcessGroupStopped(s.pgid, processTerminateGrace) {
		return true, escalated, monitorable, nil
	}

	if err := s.requestKill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return false, escalated, monitorable, fmt.Errorf("kill process group: %w", err)
	}
	if waitProcessGroupStopped(s.pgid, processKillWait) {
		return true, escalated, monitorable, nil
	}
	return false, escalated, monitorable, errProcessCleanupUnconfirmed
}

type processScopeOutcome struct {
	quiescent       bool
	escalated       bool
	monitorable     bool
	forcedPipeClose bool
	interrupted     bool
	cleanupErr      error
}

// supervise watches the leader independently of pipe EOF. A normally exiting
// shell can leave descendants alive with redirected output, so leader exit
// always triggers a process-group check before readers are considered done.
func (s *localProcessScope) supervise(ctx context.Context, leaderDone, readersDone <-chan struct{}, closeReaders func()) processScopeOutcome {
	readersDoneCh := readersDone
	var outcome processScopeOutcome
	for {
		select {
		case <-leaderDone:
			outcome.interrupted = ctx.Err() != nil
			outcome.quiescent, outcome.escalated, outcome.monitorable, outcome.cleanupErr = s.cleanupRemaining()
			goto cleanupReaders
		case <-ctx.Done():
			outcome.interrupted = true
			outcome.quiescent, outcome.escalated, outcome.monitorable, outcome.cleanupErr = s.cleanupRemaining()
			goto cleanupReaders
		case <-readersDoneCh:
			readersDoneCh = nil
		}
	}

cleanupReaders:
	if !waitChannel(readersDone, processReaderCloseGrace) {
		closeReaders()
		outcome.forcedPipeClose = true
	}
	return outcome
}

// observeProcessCompletion keeps the in-memory process identity and group
// observation alive after a bounded caller return. It only closes after the
// already-started leader and both readers join and the owned process group is
// positively absent. It never sends a signal after the initial cleanup pass.
func observeProcessCompletion(pgid int, leaderDone, readersDone <-chan struct{}) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		<-leaderDone
		<-readersDone
		for {
			alive, err := commandProcessGroupAlive(pgid)
			if err != nil {
				return
			}
			if !alive {
				close(done)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	return done
}

func waitProcessGroupStopped(pgid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		alive, err := commandProcessGroupAlive(pgid)
		if err == nil && !alive {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(processPollInterval)
	}
}

func waitChannel(ch <-chan struct{}, timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ch:
		return true
	case <-timer.C:
		return false
	}
}

type unknownExecutionError struct {
	cause error
}

func (e *unknownExecutionError) Error() string {
	return fmt.Sprintf("tools: execution outcome unknown: %v", e.cause)
}

func (e *unknownExecutionError) Unwrap() error { return e.cause }

func (*unknownExecutionError) OutcomeUnknown() bool { return true }

func newUnknownExecutionError(cause error) error {
	if cause == nil {
		cause = errors.New("execution completed without a trusted receipt")
	}
	return &unknownExecutionError{cause: cause}
}

type notDispatchedExecutionError struct {
	cause error
}

func (e *notDispatchedExecutionError) Error() string {
	return fmt.Sprintf("tools: execution was not dispatched: %v", e.cause)
}

func (e *notDispatchedExecutionError) Unwrap() error { return e.cause }

func (*notDispatchedExecutionError) ExecutionNotDispatched() bool { return true }

func (*notDispatchedExecutionError) OutcomeUnknown() bool { return false }

func newNotDispatchedExecutionError(cause error) error {
	if cause == nil {
		cause = errors.New("execution did not start")
	}
	return &notDispatchedExecutionError{cause: cause}
}

type pendingExecutionError struct {
	cause   error
	done    <-chan struct{}
	unknown bool
}

func (e *pendingExecutionError) Error() string {
	return fmt.Sprintf("tools: execution resource remains pending: %v", e.cause)
}

func (e *pendingExecutionError) Unwrap() error { return e.cause }

func (e *pendingExecutionError) OutcomeUnknown() bool { return e.unknown }

func (e *pendingExecutionError) ExecutionDone() <-chan struct{} { return e.done }

func newPendingExecutionError(cause error, unknown bool) error {
	if cause == nil {
		cause = errProcessCleanupUnconfirmed
	}
	// This channel intentionally stays open when cleanup cannot be proved.
	return &pendingExecutionError{cause: cause, done: make(chan struct{}), unknown: unknown}
}

func pendingProcessError(cause error, unknown bool) error {
	return newPendingExecutionError(cause, unknown)
}

func pendingProcessErrorWithDone(cause error, unknown bool, done <-chan struct{}) error {
	if done == nil {
		return pendingProcessError(cause, unknown)
	}
	if cause == nil {
		cause = errProcessCleanupUnconfirmed
	}
	return &pendingExecutionError{cause: cause, done: done, unknown: unknown}
}

func localLifecycleError(cause error, result managedLocalResult) error {
	if !result.quiescent || !result.readersJoined || result.cleanupErr != nil {
		return pendingProcessErrorWithDone(errors.Join(cause, result.cleanupErr, result.readerErr), true, result.executionDone)
	}
	return newUnknownExecutionError(cause)
}
