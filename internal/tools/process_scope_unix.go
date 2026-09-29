//go:build darwin || linux

package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"syscall"
)

func configureCommandProcessGroup(cmd *exec.Cmd) error {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		return nil
	}
	if cmd.SysProcAttr.Pgid != 0 {
		return errors.New("command process group must be created for this execution")
	}
	cmd.SysProcAttr.Setpgid = true
	return nil
}

func signalCommandProcessGroup(pgid int, force bool) error {
	signal := syscall.SIGTERM
	if force {
		signal = syscall.SIGKILL
	}
	if err := syscall.Kill(-pgid, signal); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	return nil
}

func commandProcessGroupAlive(pgid int) (bool, error) {
	err := syscall.Kill(-pgid, 0)
	if err == nil || errors.Is(err, syscall.EPERM) {
		return true, nil
	}
	if errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	return true, err
}

func startManagedLocalBashCommandWithIdentityObserver(cancel context.CancelFunc, cmd *exec.Cmd, ctx context.Context, observer ProcessExecutionIdentityObserver) (*managedLocalCommand, error) {
	gate, err := installBashProcessStartGate(cmd)
	if err != nil {
		return nil, fmt.Errorf("tools: prepare Bash process start gate: %w", err)
	}
	defer gate.closeReadEnd()
	defer gate.abort()

	return startManagedLocalCommandWithStartHook(cancel, cmd, func(started *exec.Cmd) error {
		if started.Process == nil || started.Process.Pid <= 0 {
			return errors.New("Bash process start returned no child identity")
		}
		identity, err := captureManagedProcessIdentity(started.Process.Pid, started.Process.Pid)
		if err != nil {
			return fmt.Errorf("capture exact Bash process identity: %w", err)
		}
		if err := observer(ctx, identity); err != nil {
			return fmt.Errorf("persist exact Bash process identity: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("prepared Bash canceled before start-gate release: %w", err)
		}
		return gate.release()
	})
}

type bashProcessStartGate struct {
	readEnd  *os.File
	writeEnd *os.File
	close    sync.Once
}

func installBashProcessStartGate(cmd *exec.Cmd) (*bashProcessStartGate, error) {
	if cmd == nil || cmd.Path == "" || len(cmd.Args) != 3 || cmd.Args[1] != "-lc" || len(cmd.ExtraFiles) != 0 {
		return nil, errors.New("managed Bash start gate requires the exact local bash -lc invocation")
	}
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create Bash start gate: %w", err)
	}
	fd := 3 + len(cmd.ExtraFiles)
	cmd.ExtraFiles = append(cmd.ExtraFiles, readEnd)
	command := cmd.Args[2]
	cmd.Args = []string{
		cmd.Args[0], "--noprofile", "--norc", "-c",
		fmt.Sprintf("IFS= read -r _ <&%d || exit 125; exec %d<&-; exec \"$2\" -lc \"$1\"", fd, fd),
		"go-cli-managed-bash", command, cmd.Path,
	}
	return &bashProcessStartGate{readEnd: readEnd, writeEnd: writeEnd}, nil
}

func (g *bashProcessStartGate) release() error {
	if g == nil || g.writeEnd == nil {
		return errors.New("Bash process start gate is unavailable")
	}
	_, writeErr := g.writeEnd.Write([]byte("release\n"))
	closeErr := g.closeWriteEnd()
	if writeErr != nil || closeErr != nil {
		return errors.Join(writeErr, closeErr)
	}
	return nil
}

func (g *bashProcessStartGate) abort() {
	if g == nil {
		return
	}
	_ = g.closeWriteEnd()
}

func (g *bashProcessStartGate) closeReadEnd() {
	if g != nil && g.readEnd != nil {
		_ = g.readEnd.Close()
	}
}

func (g *bashProcessStartGate) closeWriteEnd() error {
	if g == nil || g.writeEnd == nil {
		return nil
	}
	var err error
	g.close.Do(func() { err = g.writeEnd.Close() })
	return err
}

func captureManagedProcessIdentity(pid, pgid int) (ProcessExecutionIdentity, error) {
	if pid <= 0 || pgid <= 0 {
		return ProcessExecutionIdentity{}, errors.New("process and process-group IDs must be positive")
	}
	hostGeneration, err := currentHostGeneration()
	if err != nil {
		return ProcessExecutionIdentity{}, err
	}
	startToken, observedPGID, err := processStartToken(pid)
	if err != nil {
		return ProcessExecutionIdentity{}, err
	}
	if observedPGID != pgid {
		return ProcessExecutionIdentity{}, fmt.Errorf("process %d is in group %d, expected %d", pid, observedPGID, pgid)
	}
	ownerPID := os.Getpid()
	ownerStartToken, _, err := processStartToken(ownerPID)
	if err != nil {
		return ProcessExecutionIdentity{}, fmt.Errorf("inspect owner process identity: %w", err)
	}
	return ProcessExecutionIdentity{
		Platform:               runtime.GOOS,
		HostGeneration:         hostGeneration,
		LeaderPID:              pid,
		ProcessStartToken:      startToken,
		ProcessGroupID:         pgid,
		OwnerPID:               ownerPID,
		OwnerProcessStartToken: ownerStartToken,
	}, nil
}
