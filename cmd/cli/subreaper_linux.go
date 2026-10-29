//go:build linux

package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

const subreaperBootstrapTimeout = 5 * time.Second

func standaloneSubreaperDispatch() (int, bool) {
	private, err := readSubreaperPrivateEnvironment(os.Environ())
	if err != nil {
		return subreaperFailure(err), true
	}
	enabled, err := parseStandaloneSubreaperMode(os.Getenv(subreaperEnvironment))
	if err != nil {
		return subreaperFailure(err), true
	}
	if enabled || len(private) != 0 {
		// Go's environment map discards duplicate keys. Inspect only our
		// private keys from the initial kernel environment as well, without
		// logging or forwarding the raw environment. Default-off never reads
		// /proc here. Private metadata changed by init code also fails closed.
		data, err := os.ReadFile("/proc/self/environ")
		if err != nil {
			return subreaperFailure(fmt.Errorf("inspect initial private bootstrap environment: %w", err)), true
		}
		initial, err := readSubreaperPrivateEnvironment(strings.Split(string(data), "\x00"))
		if err != nil {
			return subreaperFailure(err), true
		}
		if len(initial) != len(private) {
			return subreaperFailure(errors.New("private bootstrap environment changed before dispatch")), true
		}
		for key, value := range initial {
			if private[key] != value {
				return subreaperFailure(errors.New("private bootstrap environment changed before dispatch")), true
			}
		}
	}
	if len(private) != 0 {
		if !enabled {
			return subreaperFailure(errors.New("inner bootstrap requires explicit opt-in")), true
		}
		if err := enterSubreaperInner(private); err != nil {
			return subreaperFailure(err), true
		}
		return 0, false
	}
	if !enabled {
		return 0, false
	}
	for _, file := range []*os.File{os.Stdin, os.Stdout, os.Stderr} {
		if term.IsTerminal(int(file.Fd())) {
			return subreaperFailure(errors.New("opt-in supervisor does not support TTY stdio")), true
		}
	}
	code, err := runSubreaperOuter()
	if err != nil {
		return subreaperFailure(err), true
	}
	return code, true
}

func readSubreaperIdentity(pid int) (subreaperProcessIdentity, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return subreaperProcessIdentity{}, fmt.Errorf("read process identity %d: %w", pid, err)
	}
	return parseSubreaperProcStat(string(data))
}

func readSubreaperChildren(procRoot string) ([]int, error) {
	threads, err := os.ReadDir(filepath.Join(procRoot, "self", "task"))
	if err != nil {
		return nil, fmt.Errorf("enumerate supervisor threads: %w", err)
	}
	if len(threads) == 0 {
		return nil, errors.New("supervisor thread inventory is empty")
	}
	seen := make(map[int]bool)
	for _, thread := range threads {
		if _, err := strconv.Atoi(thread.Name()); err != nil {
			return nil, errors.New("unexpected supervisor thread entry")
		}
		data, err := os.ReadFile(filepath.Join(procRoot, "self", "task", thread.Name(), "children"))
		if err != nil {
			return nil, fmt.Errorf("read supervisor thread children: %w", err)
		}
		for _, field := range strings.Fields(string(data)) {
			pid, err := strconv.Atoi(field)
			if err != nil || pid <= 0 {
				return nil, errors.New("malformed supervisor child PID")
			}
			seen[pid] = true
		}
	}
	children := make([]int, 0, len(seen))
	for pid := range seen {
		children = append(children, pid)
	}
	return children, nil
}

func checkSubreaperInheritedFDs() error {
	// Keep the enumeration descriptor open until fcntl has checked every row.
	// Otherwise its own number could look like a disappearing inherited FD.
	directory, err := os.Open("/proc/self/fd")
	if err != nil {
		return fmt.Errorf("open descriptor inventory: %w", err)
	}
	defer func() { _ = directory.Close() }()
	names, err := directory.Readdirnames(-1)
	if err != nil {
		return fmt.Errorf("read descriptor inventory: %w", err)
	}
	for _, name := range names {
		fd, err := strconv.Atoi(name)
		if err != nil {
			return errors.New("malformed descriptor inventory")
		}
		if fd <= 2 {
			continue
		}
		flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
		if err != nil {
			return fmt.Errorf("inspect inherited descriptor %d: %w", fd, err)
		}
		if flags&unix.FD_CLOEXEC == 0 {
			return fmt.Errorf("unsupported inherited descriptor %d: opt-in requires native execution without emulation-injected or user extra descriptors", fd)
		}
	}
	return nil
}

func subreaperExecutableIdentity(path string) (string, error) {
	var stat unix.Stat_t
	if err := unix.Stat(path, &stat); err != nil {
		return "", fmt.Errorf("inspect executable inode: %w", err)
	}
	return fmt.Sprintf("%d:%d", stat.Dev, stat.Ino), nil
}

func sendSubreaperPacket(connection net.Conn, phase, nonce string, pid int) error {
	data, err := json.Marshal(subreaperPacket{Phase: phase, Nonce: nonce, PID: pid})
	if err != nil {
		return fmt.Errorf("encode bootstrap packet: %w", err)
	}
	if _, err := connection.Write(data); err != nil {
		return fmt.Errorf("send bootstrap packet: %w", err)
	}
	return nil
}

func receiveSubreaperPacket(connection net.Conn, phase, nonce string, pid int) error {
	data := make([]byte, 4097)
	n, err := connection.Read(data)
	if err != nil {
		return fmt.Errorf("receive bootstrap packet: %w", err)
	}
	if n == 0 || n > 4096 {
		return errors.New("invalid bootstrap packet size")
	}
	return validateSubreaperPacket(data[:n], phase, nonce, pid)
}

func subreaperSocket(file *os.File) (net.Conn, error) {
	if file == nil {
		return nil, errors.New("missing private bootstrap descriptor")
	}
	kind, err := unix.GetsockoptInt(int(file.Fd()), unix.SOL_SOCKET, unix.SO_TYPE)
	if err != nil || kind != unix.SOCK_SEQPACKET {
		return nil, errors.New("private bootstrap descriptor is not a seqpacket socket")
	}
	connection, err := net.FileConn(file)
	if err != nil {
		return nil, fmt.Errorf("open private bootstrap connection: %w", err)
	}
	if err := connection.SetDeadline(time.Now().Add(subreaperBootstrapTimeout)); err != nil {
		_ = connection.Close()
		return nil, fmt.Errorf("set bootstrap deadline: %w", err)
	}
	return connection, nil
}

func checkSubreaperNoQueuedPacket(connection net.Conn) error {
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok {
		return errors.New("unexpected private connection type")
	}
	raw, err := unixConnection.SyscallConn()
	if err != nil {
		return fmt.Errorf("inspect private connection: %w", err)
	}
	var receiveErr error
	err = raw.Control(func(fd uintptr) {
		buffer := make([]byte, 1)
		_, _, receiveErr = unix.Recvfrom(int(fd), buffer, unix.MSG_PEEK|unix.MSG_DONTWAIT)
	})
	if err != nil {
		return fmt.Errorf("inspect queued bootstrap data: %w", err)
	}
	if !errors.Is(receiveErr, unix.EAGAIN) && !errors.Is(receiveErr, unix.EWOULDBLOCK) {
		return errors.New("duplicate bootstrap packet or closed private channel")
	}
	return nil
}

func enterSubreaperInner(private map[string]string) error {
	file := os.NewFile(3, "private-subreeper-bootstrap")
	if file == nil {
		return errors.New("missing private bootstrap FD")
	}
	defer func() { _ = file.Close() }()
	credentials, err := unix.GetsockoptUcred(3, unix.SOL_SOCKET, unix.SO_PEERCRED)
	if err != nil || int(credentials.Pid) != os.Getppid() {
		return errors.New("bootstrap socket creator is not the current parent")
	}
	parent, err := readSubreaperIdentity(os.Getppid())
	if err != nil {
		return err
	}
	if private["PARENT"] != fmt.Sprintf("%d:%d", parent.pid, parent.started) {
		return errors.New("bootstrap parent identity changed")
	}
	executable, err := subreaperExecutableIdentity("/proc/self/exe")
	if err != nil {
		return err
	}
	parentExecutable, err := subreaperExecutableIdentity(fmt.Sprintf("/proc/%d/exe", parent.pid))
	if err != nil || parentExecutable != executable || private["EXE"] != executable {
		return errors.New("bootstrap executable identity mismatch")
	}
	connection, err := subreaperSocket(file)
	if err != nil {
		return err
	}
	defer func() { _ = connection.Close() }()
	if err := receiveSubreaperPacket(connection, "challenge", private["NONCE"], parent.pid); err != nil {
		return err
	}
	if err := sendSubreaperPacket(connection, "ready", private["NONCE"], os.Getpid()); err != nil {
		return err
	}
	if err := receiveSubreaperPacket(connection, "release", private["NONCE"], parent.pid); err != nil {
		return err
	}
	if err := checkSubreaperNoQueuedPacket(connection); err != nil {
		return err
	}
	if os.Getppid() != parent.pid {
		return errors.New("supervisor died during inner bootstrap")
	}
	for key := range private {
		if err := os.Unsetenv(subreaperPrivatePrefix + key); err != nil {
			return fmt.Errorf("consume private environment: %w", err)
		}
	}
	return sendSubreaperPacket(connection, "ack", private["NONCE"], os.Getpid())
}

func runSubreaperOuter() (int, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	children, err := readSubreaperChildren("/proc")
	if err != nil {
		return 0, err
	}
	if len(children) != 0 {
		return 0, errors.New("preexisting children prevent isolated supervisor startup")
	}
	if err := checkSubreaperInheritedFDs(); err != nil {
		return 0, err
	}
	parent, err := readSubreaperIdentity(os.Getpid())
	if err != nil {
		return 0, err
	}
	executable, err := subreaperExecutableIdentity("/proc/self/exe")
	if err != nil {
		return 0, err
	}
	selfFD, err := unix.PidfdOpen(os.Getpid(), 0)
	if err != nil {
		return 0, fmt.Errorf("pidfd capability unavailable: %w", err)
	}
	probeErr := unix.PidfdSendSignal(selfFD, 0, nil, 0)
	_ = unix.Close(selfFD)
	if probeErr != nil {
		return 0, fmt.Errorf("pidfd signal capability unavailable: %w", probeErr)
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return 0, fmt.Errorf("enable outer subreaper: %w", err)
	}
	var value int32
	err = unix.Prctl(unix.PR_GET_CHILD_SUBREAPER, uintptr(unsafe.Pointer(&value)), 0, 0, 0)
	runtime.KeepAlive(&value)
	if err != nil || value != 1 {
		return 0, errors.New("outer subreaper readback failed")
	}
	return spawnSubreaperInner(parent, executable)
}

func spawnSubreaperInner(parent subreaperProcessIdentity, executable string) (int, error) {
	sockets, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return 0, fmt.Errorf("create bootstrap socketpair: %w", err)
	}
	outerFile, innerFile := os.NewFile(uintptr(sockets[0]), "outer-bootstrap"), os.NewFile(uintptr(sockets[1]), "inner-bootstrap")
	defer func() { _ = outerFile.Close(); _ = innerFile.Close() }()
	connection, err := subreaperSocket(outerFile)
	_ = outerFile.Close() // FileConn owns its CLOEXEC duplicate; EOF must not be held open.
	if err != nil {
		return 0, err
	}
	defer func() { _ = connection.Close() }()
	nonceBytes := make([]byte, 32)
	if _, err := rand.Read(nonceBytes); err != nil {
		return 0, fmt.Errorf("create bootstrap nonce: %w", err)
	}
	nonce := hex.EncodeToString(nonceBytes)
	command := exec.Command("/proc/self/exe", os.Args[1:]...)
	command.Args[0] = os.Args[0]
	command.Env = append(stripSubreaperPrivateEnvironment(os.Environ()),
		subreaperPrivatePrefix+"ROLE=inner", subreaperPrivatePrefix+"NONCE="+nonce,
		subreaperPrivatePrefix+"PARENT="+fmt.Sprintf("%d:%d", parent.pid, parent.started), subreaperPrivatePrefix+"EXE="+executable)
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	command.ExtraFiles = []*os.File{innerFile}
	// This descriptor is returned atomically by fork/clone. Never reconstruct
	// the actual CLI's signaling identity by opening its numeric PID later.
	// os.Process owns a separate duplicate for its sole Cmd.Wait consumer.
	atomicFD := -1
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGTERM, PidFD: &atomicFD}
	signals := make(chan os.Signal, 16)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)
	if err := command.Start(); err != nil {
		return 0, fmt.Errorf("start actual CLI: %w", err)
	}
	if atomicFD >= 0 {
		defer func() { _ = unix.Close(atomicFD) }()
	}
	_ = innerFile.Close()
	// Cmd.Wait is the sole consumer of the actual CLI status on every path.
	waited := make(chan error, 1)
	var actualExited atomic.Bool
	go func() {
		err := command.Wait()
		actualExited.Store(true)
		waited <- err
	}()
	actual, identityErr := readSubreaperIdentity(command.Process.Pid)
	pidFD, fdErr := subreaperActualDescriptor(command)
	if fdErr != nil || identityErr != nil {
		return 0, failSubreaperBootstrap(connection, waited, errors.Join(identityErr, fdErr))
	}
	// os.StartProcess's synchronous runtime capability probes have already
	// joined. No reaper runs until the sole active child is the actual CLI.
	children, inventoryErr := readSubreaperChildren("/proc")
	if inventoryErr != nil || len(children) != 1 || children[0] != actual.pid || actual.parent != os.Getpid() {
		return 0, failSubreaperBootstrap(connection, waited, errors.Join(inventoryErr, errors.New("actual CLI is not the sole active direct child at bootstrap")))
	}
	bootstrapErr := bootstrapSubreaperActual(connection, nonce, parent.pid, actual.pid, pidFD, signals)
	_ = connection.Close()
	if bootstrapErr != nil {
		return 0, failSubreaperBootstrap(connection, waited, bootstrapErr)
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	var supervisorErr error
	for {
		select {
		case waitErr := <-waited:
			// No numeric PID blacklist remains after Wait; adopted identities
			// that later reuse its number are independently checked.
			drainErr := drainSubreaperChildren()
			if supervisorErr != nil || drainErr != nil {
				return 0, errors.Join(supervisorErr, drainErr)
			}
			return subreaperActualExitCode(waitErr), nil
		case received := <-signals:
			if actualExited.Load() {
				continue
			}
			if sig, ok := received.(syscall.Signal); ok {
				if err := unix.PidfdSendSignal(pidFD, unix.Signal(sig), nil, 0); err != nil && !errors.Is(err, unix.ESRCH) {
					supervisorErr = errors.Join(supervisorErr, fmt.Errorf("forward actual CLI signal: %w", err))
				}
			}
		case <-ticker.C:
			if err := reapSubreaperZombies(&actual); err != nil {
				supervisorErr = errors.Join(supervisorErr, err)
			}
		}
	}
}

func subreaperActualDescriptor(command *exec.Cmd) (int, error) {
	if command.SysProcAttr == nil || command.SysProcAttr.PidFD == nil || *command.SysProcAttr.PidFD < 0 {
		return -1, errors.New("atomic actual CLI pidfd is unavailable; numeric PID fallback is forbidden")
	}
	return *command.SysProcAttr.PidFD, nil
}

// Rejected/unconfirmed bootstrap has no signaling capability. In particular,
// ECHILD or an identity mismatch must never turn a newly opened replacement
// descriptor into a kill target. Closing the private channel asks the trusted
// bootstrap to fail; inability to join is an explicit nonzero supervisor exit.
func failSubreaperBootstrap(connection net.Conn, waited <-chan error, cause error) error {
	_ = connection.Close()
	select {
	case <-waited:
		return cause
	case <-time.After(subreaperBootstrapTimeout):
		return errors.Join(cause, errors.New("private actual CLI did not join after bootstrap EOF; supervisor exits nonzero with parent-death cleanup required"))
	}
}

func bootstrapSubreaperActual(connection net.Conn, nonce string, parentPID, actualPID, pidFD int, signals <-chan os.Signal) error {
	var info unix.Siginfo
	if err := unix.Waitid(unix.P_PIDFD, pidFD, &info, unix.WEXITED|unix.WNOWAIT|unix.WNOHANG, nil); err != nil {
		return fmt.Errorf("non-consuming actual CLI waitid capability probe: %w", err)
	}
	if err := sendSubreaperPacket(connection, "challenge", nonce, parentPID); err != nil {
		return err
	}
	if err := receiveSubreaperPacket(connection, "ready", nonce, actualPID); err != nil {
		return err
	}
	select {
	case received := <-signals:
		if sig, ok := received.(syscall.Signal); ok {
			_ = unix.PidfdSendSignal(pidFD, unix.Signal(sig), nil, 0)
		}
		return errors.New("supervisor interrupted before actual CLI release")
	default:
	}
	if err := sendSubreaperPacket(connection, "release", nonce, parentPID); err != nil {
		return err
	}
	return receiveSubreaperPacket(connection, "ack", nonce, actualPID)
}

func subreaperActualExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		if status, ok := exitError.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return 128 + int(status.Signal())
		}
		if exitError.ExitCode() > 0 {
			return exitError.ExitCode()
		}
	}
	return 125
}

func sameSubreaperIdentity(left, right subreaperProcessIdentity) bool {
	return left.pid == right.pid && left.started == right.started
}

type subreaperChildOperations struct {
	read  func(int) (subreaperProcessIdentity, error)
	open  func(int) (int, error)
	close func(int) error
	wait  func(int) error
}

func nativeSubreaperChildOperations() subreaperChildOperations {
	return subreaperChildOperations{
		read:  readSubreaperIdentity,
		open:  func(pid int) (int, error) { return unix.PidfdOpen(pid, 0) },
		close: unix.Close,
		wait: func(fd int) error {
			var info unix.Siginfo
			return unix.Waitid(unix.P_PIDFD, fd, &info, unix.WEXITED|unix.WNOHANG, nil)
		},
	}
}

// bindSubreaperChild verifies both sides of pidfd_open. Its returned descriptor
// is never used for another numeric PID, and the caller must close it.
func bindSubreaperChild(expected subreaperProcessIdentity) (int, bool, error) {
	return bindSubreaperChildWithOperations(expected, os.Getpid(), nativeSubreaperChildOperations())
}

func bindSubreaperChildWithOperations(expected subreaperProcessIdentity, parentPID int, operations subreaperChildOperations) (int, bool, error) {
	fd, err := operations.open(expected.pid)
	if errors.Is(err, unix.ESRCH) {
		_, observeErr := operations.read(expected.pid)
		if errors.Is(observeErr, os.ErrNotExist) {
			return -1, false, nil
		}
	}
	if err != nil {
		return -1, false, fmt.Errorf("bind adopted child identity: %w", err)
	}
	current, err := operations.read(expected.pid)
	if errors.Is(err, os.ErrNotExist) {
		_ = operations.close(fd)
		return -1, false, nil
	}
	if err != nil || !sameSubreaperIdentity(expected, current) || current.parent != parentPID || (expected.state == 'Z' && current.state != 'Z') {
		_ = operations.close(fd)
		return -1, false, errors.New("adopted child identity changed while binding")
	}
	return fd, true, nil
}

func reapSubreaperZombieWithOperations(identity subreaperProcessIdentity, parentPID int, operations subreaperChildOperations) error {
	if identity.parent != parentPID || identity.state != 'Z' {
		return errors.New("only adopted zombie identities can be precisely reaped")
	}
	fd, exists, err := bindSubreaperChildWithOperations(identity, parentPID, operations)
	if err != nil || !exists {
		return err
	}
	waitErr := operations.wait(fd)
	_ = operations.close(fd)
	current, observeErr := operations.read(identity.pid)
	if errors.Is(observeErr, os.ErrNotExist) || (observeErr == nil && !sameSubreaperIdentity(identity, current)) {
		return nil // absence of this bound identity, not waitid's zero, is evidence
	}
	if waitErr != nil {
		return fmt.Errorf("reap adopted child: %w", waitErr)
	}
	if observeErr != nil {
		return fmt.Errorf("confirm adopted child absence: %w", observeErr)
	}
	return errors.New("adopted zombie remains after precise wait")
}

func reapSubreaperZombies(actual *subreaperProcessIdentity) error {
	children, err := readSubreaperChildren("/proc")
	if err != nil {
		return err
	}
	for _, pid := range children {
		identity, err := readSubreaperIdentity(pid)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if actual != nil && sameSubreaperIdentity(identity, *actual) {
			continue
		}
		if identity.parent != os.Getpid() || identity.state != 'Z' {
			continue
		}
		if err := reapSubreaperZombieWithOperations(identity, os.Getpid(), nativeSubreaperChildOperations()); err != nil {
			return err
		}
	}
	return nil
}

func drainSubreaperChildren() error {
	var anomaly error
	for _, phase := range []struct {
		signal unix.Signal
		wait   time.Duration
	}{{unix.SIGTERM, 100 * time.Millisecond}, {unix.SIGKILL, 300 * time.Millisecond}} {
		deadline := time.Now().Add(phase.wait)
		signaled := make(map[subreaperProcessIdentity]bool)
		for {
			if err := reapSubreaperZombies(nil); err != nil {
				return errors.Join(anomaly, err)
			}
			children, err := readSubreaperChildren("/proc")
			if err != nil {
				return errors.Join(anomaly, err)
			}
			if len(children) == 0 {
				return anomaly
			}
			for _, pid := range children {
				identity, err := readSubreaperIdentity(pid)
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				if err != nil {
					return errors.Join(anomaly, err)
				}
				if identity.parent != os.Getpid() || identity.state == 'Z' || signaled[identity] {
					continue
				}
				if anomaly == nil {
					anomaly = errors.New("actual CLI exited with live adopted children; supervisor performed abnormal cleanup")
				}
				fd, exists, err := bindSubreaperChild(identity)
				if err != nil {
					return errors.Join(anomaly, err)
				}
				if exists {
					err = unix.PidfdSendSignal(fd, phase.signal, nil, 0)
					_ = unix.Close(fd)
					if err != nil && !errors.Is(err, unix.ESRCH) {
						return errors.Join(anomaly, fmt.Errorf("signal live adopted child: %w", err))
					}
					signaled[identity] = true
				}
			}
			if time.Now().After(deadline) {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	return errors.Join(anomaly, errors.New("supervisor cleanup deadline ended with adopted children remaining"))
}
