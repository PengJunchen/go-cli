package acp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	stdioProcessTermWait = 250 * time.Millisecond
	stdioProcessKillWait = 2 * time.Second
	stdioProcessPoll     = 10 * time.Millisecond
	stdioProcessCallWait = 3 * time.Second
)

var (
	// ErrStdioProcessContainmentUnsupported reports that stdio process-group
	// cleanup is unavailable on the current operating system.
	ErrStdioProcessContainmentUnsupported = errors.New("acp: stdio process containment is unsupported on this operating system")
	// ErrStdioProcessCleanupPending reports that a bounded join could not
	// confirm that the owned process group has exited.
	ErrStdioProcessCleanupPending = errors.New("acp: stdio process group cleanup remains incomplete")
)

// StdioProcessConfig describes a directly invoked stdio child. Command becomes
// argv[0]. ExecutablePath, when set, is the private verified copy used for
// execution; Cleanup removes its materialized source after process and adapter
// I/O cleanup have joined.
type StdioProcessConfig struct {
	Context        context.Context
	Command        string
	ExecutablePath string
	Args           []string
	Cleanup        func() error
}

// StdioProcessOwner owns a child process, its stdio pipes, and its same-group
// descendants. Call Join after Close to reap the direct child and release any
// materialized executable. The process group is deliberately not a sandbox:
// descendants that leave it with setsid or setpgid are outside the containment
// guarantee.
type StdioProcessOwner struct {
	cmd          *exec.Cmd
	stdin        *os.File
	stdout       *os.File
	childStdin   *os.File
	childStdout  *os.File
	cleanup      func() error
	ctx          context.Context
	groupID      int
	started      bool
	startErr     error
	waitErr      error
	shutdownErr  error
	joinErr      error
	groupGone    bool
	closing      bool
	mu           sync.Mutex
	waitDone     chan struct{}
	shutdownDone chan struct{}
	closeOnce    sync.Once
	joinOnce     sync.Once
}

var _ io.Closer = (*StdioProcessOwner)(nil)

// NewStdioProcessOwner prepares a direct child with owned stdin/stdout pipes
// and a new process group. It fails before child creation on unsupported OSes.
func NewStdioProcessOwner(config StdioProcessConfig) (*StdioProcessOwner, error) {
	if strings.TrimSpace(config.Command) == "" {
		return nil, errors.New("acp: stdio command is required")
	}
	if config.Context == nil {
		config.Context = context.Background()
	}
	if err := config.Context.Err(); err != nil {
		return nil, err
	}
	if err := stdioProcessGroupSupported(); err != nil {
		return nil, err
	}

	var cmd *exec.Cmd
	if strings.TrimSpace(config.ExecutablePath) == "" {
		cmd = exec.Command(config.Command, config.Args...)
	} else {
		cmd = &exec.Cmd{
			Path: config.ExecutablePath,
			Args: append([]string{config.Command}, config.Args...),
		}
	}
	if err := configureACPStdioProcessGroup(cmd); err != nil {
		return nil, err
	}

	childStdin, stdin, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("acp: create stdio child input pipe: %w", err)
	}
	stdout, childStdout, err := os.Pipe()
	if err != nil {
		_ = childStdin.Close()
		_ = stdin.Close()
		return nil, fmt.Errorf("acp: create stdio child output pipe: %w", err)
	}
	cmd.Stdin = childStdin
	cmd.Stdout = childStdout

	return &StdioProcessOwner{
		cmd:          cmd,
		stdin:        stdin,
		stdout:       stdout,
		childStdin:   childStdin,
		childStdout:  childStdout,
		cleanup:      config.Cleanup,
		ctx:          config.Context,
		waitDone:     make(chan struct{}),
		shutdownDone: make(chan struct{}),
	}, nil
}

// Reader returns the owned child stdout stream for a StdioAdapter.
func (o *StdioProcessOwner) Reader() io.Reader {
	if o == nil {
		return nil
	}
	return o.stdout
}

// Writer returns the owned child stdin stream for a StdioAdapter.
func (o *StdioProcessOwner) Writer() io.Writer {
	if o == nil {
		return nil
	}
	return o.stdin
}

// Start starts the child and records its exact process-group identity before
// returning. Its Wait runs independently because the owner uses explicit pipe
// files rather than exec.Cmd's StdoutPipe, so Wait cannot close a live adapter
// reader.
func (o *StdioProcessOwner) Start() error {
	if o == nil {
		return errors.New("acp: stdio process owner is nil")
	}
	o.mu.Lock()
	if o.closing {
		o.mu.Unlock()
		return errors.New("acp: stdio process owner is closing")
	}
	if o.started {
		o.mu.Unlock()
		return errors.New("acp: stdio process already started")
	}
	if o.startErr != nil {
		err := o.startErr
		o.mu.Unlock()
		return err
	}
	if err := o.ctx.Err(); err != nil {
		o.startErr = err
		o.mu.Unlock()
		return err
	}
	if err := o.cmd.Start(); err != nil {
		o.startErr = err
		o.mu.Unlock()
		_ = o.closeChildEnds()
		return err
	}
	o.started = true
	o.groupID = o.cmd.Process.Pid
	o.mu.Unlock()

	_ = o.closeChildEnds()
	go func() {
		err := o.cmd.Wait()
		o.mu.Lock()
		o.waitErr = err
		o.mu.Unlock()
		close(o.waitDone)
	}()
	go func() {
		select {
		case <-o.ctx.Done():
			_ = o.Close()
		case <-o.shutdownDone:
		}
	}()
	return nil
}

// Close requests shutdown and waits for bounded TERM/KILL group cleanup. A
// pending error is returned when the group cannot be confirmed absent within
// the bounded join; it never reports success based on direct-child exit alone.
func (o *StdioProcessOwner) Close() error {
	if o == nil {
		return nil
	}
	o.closeOnce.Do(func() {
		o.mu.Lock()
		o.closing = true
		o.mu.Unlock()
		go o.shutdown()
	})
	timer := time.NewTimer(stdioProcessCallWait)
	defer timer.Stop()
	select {
	case <-o.shutdownDone:
		o.mu.Lock()
		err := o.shutdownErr
		o.mu.Unlock()
		return err
	case <-timer.C:
		return ErrStdioProcessCleanupPending
	}
}

// Join finalizes the exact child Wait and materialized executable cleanup. The
// StdioAdapter invokes it only after its reader and writer workers have joined.
func (o *StdioProcessOwner) Join() error {
	if o == nil {
		return nil
	}
	closeErr := o.Close()
	o.joinOnce.Do(func() {
		<-o.shutdownDone
		o.mu.Lock()
		groupGone := o.groupGone
		started := o.started
		shutdownErr := o.shutdownErr
		o.mu.Unlock()
		if !groupGone {
			o.joinErr = errors.Join(closeErr, shutdownErr, ErrStdioProcessCleanupPending)
			return
		}
		if started {
			select {
			case <-o.waitDone:
			case <-time.After(stdioProcessCallWait):
				o.joinErr = errors.Join(closeErr, shutdownErr, ErrStdioProcessCleanupPending)
				return
			}
		}
		_ = o.stdin.Close()
		_ = o.stdout.Close()
		_ = o.closeChildEnds()
		var cleanupErr error
		if o.cleanup != nil {
			cleanupErr = o.cleanup()
		}
		o.mu.Lock()
		waitErr := o.waitErr
		o.mu.Unlock()
		if o.closing && expectedACPStdioShutdown(waitErr) {
			waitErr = nil
		}
		o.joinErr = errors.Join(closeErr, shutdownErr, waitErr, cleanupErr)
	})
	return o.joinErr
}

func (o *StdioProcessOwner) shutdown() {
	o.mu.Lock()
	started := o.started
	groupID := o.groupID
	o.mu.Unlock()
	if !started {
		_ = o.stdin.Close()
		_ = o.stdout.Close()
		_ = o.closeChildEnds()
		o.mu.Lock()
		o.groupGone = true
		o.shutdownErr = nil
		o.mu.Unlock()
		close(o.shutdownDone)
		return
	}

	_ = o.stdin.Close()
	var shutdownErr error
	if err := signalACPStdioProcessGroup(groupID, false); err != nil && !errors.Is(err, os.ErrProcessDone) {
		shutdownErr = errors.Join(shutdownErr, err)
	}
	groupGone, groupErr := waitACPStdioProcessGroupGone(groupID, stdioProcessTermWait)
	shutdownErr = errors.Join(shutdownErr, groupErr)
	if !groupGone {
		if err := signalACPStdioProcessGroup(groupID, true); err != nil && !errors.Is(err, os.ErrProcessDone) {
			shutdownErr = errors.Join(shutdownErr, err)
		}
		groupGone, groupErr = waitACPStdioProcessGroupGone(groupID, stdioProcessKillWait)
		shutdownErr = errors.Join(shutdownErr, groupErr)
	}
	// Once the bounded group check has settled, close this local read end so an
	// escaped descendant that retained a duplicate writer cannot keep the
	// adapter reader (and therefore Join) blocked. This releases only our local
	// reader; process-group absence remains the separate containment result.
	if err := o.stdout.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		shutdownErr = errors.Join(shutdownErr, err)
	}
	if !groupGone {
		shutdownErr = errors.Join(shutdownErr, ErrStdioProcessCleanupPending)
	}
	o.mu.Lock()
	o.groupGone = groupGone
	o.shutdownErr = shutdownErr
	o.mu.Unlock()
	close(o.shutdownDone)
}

func (o *StdioProcessOwner) closeChildEnds() error {
	if o == nil {
		return nil
	}
	var err error
	if o.childStdin != nil {
		err = errors.Join(err, o.childStdin.Close())
	}
	if o.childStdout != nil {
		err = errors.Join(err, o.childStdout.Close())
	}
	return err
}

func waitACPStdioProcessGroupGone(groupID int, timeout time.Duration) (bool, error) {
	if groupID <= 0 {
		return false, errors.New("acp: stdio process group identity is unavailable")
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(stdioProcessPoll)
	defer ticker.Stop()
	for {
		alive, err := acpStdioProcessGroupAlive(groupID)
		if err != nil {
			return false, err
		}
		if !alive {
			return true, nil
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			return false, nil
		}
	}
}
