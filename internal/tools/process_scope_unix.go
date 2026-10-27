//go:build darwin || linux

package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
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

func startManagedLocalBashCommandWithIdentityObserver(ctx context.Context, cancel context.CancelFunc, cmd *exec.Cmd, observer ProcessExecutionIdentityObserver) (*managedLocalCommand, error) {
	gate, err := installBashProcessStartGate(cmd)
	if err != nil {
		return nil, fmt.Errorf("tools: prepare Bash process start gate: %w", err)
	}
	defer gate.closeReadEnd()
	defer gate.abort()

	return startManagedLocalCommandWithStartHook(ctx, cancel, cmd, func(started *exec.Cmd) error {
		if started.Process == nil || started.Process.Pid <= 0 {
			return errors.New("bash process start returned no child identity")
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
		if input, supplied := bashRuntimeInputFromContext(ctx); supplied && input.guard != nil {
			return input.guard(ctx, gate.release)
		}
		return startPreparedEffect(ctx, gate.release)
	})
}

type bashProcessStartGate struct {
	readEnd    *os.File
	writeEnd   *os.File
	restoreEnv []string
	close      sync.Once
}

func installBashProcessStartGate(cmd *exec.Cmd) (*bashProcessStartGate, error) {
	if cmd == nil || cmd.Path == "" || cmd.Env == nil || len(cmd.Args) != 3 || cmd.Args[1] != "-lc" || len(cmd.ExtraFiles) != 0 {
		return nil, errors.New("managed Bash start gate requires the exact local bash -lc invocation")
	}
	trustedBashPath, err := validateTrustedManagedBashPath(cmd.Path)
	if err != nil {
		return nil, fmt.Errorf("managed Bash start gate requires trusted system Bash: %w", err)
	}
	restoreEnv, err := prepareBashRestoreEnvironment(cmd.Env)
	if err != nil {
		return nil, fmt.Errorf("managed Bash start gate cannot safely restore command environment: %w", err)
	}
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create Bash start gate: %w", err)
	}
	fd := 3 + len(cmd.ExtraFiles)
	cmd.ExtraFiles = append(cmd.ExtraFiles, readEnd)
	argv0 := cmd.Args[0]
	command := cmd.Args[2]
	bashPath := trustedBashPath
	// Start the same Bash executable with an empty environment. In particular,
	// this keeps BASH_ENV, exported functions, SHELLOPTS, PS4, and dynamic-loader
	// variables from running code before the durable identity observer returns.
	// The original environment is restored from the private pipe only after the
	// observer releases the gate. The bootstrap receives no caller-controlled
	// environment entries or shell code.
	cmd.Env = []string{}
	cmd.Path = trustedBashPath
	cmd.Args = []string{
		argv0,
		"--noprofile",
		"--norc",
		"-c",
		managedBashGateScript(fd),
		argv0,
		command,
		bashPath,
	}
	return &bashProcessStartGate{readEnd: readEnd, writeEnd: writeEnd, restoreEnv: restoreEnv}, nil
}

func validateTrustedManagedBashPath(path string) (string, error) {
	trustedPath, err := trustedSystemBashPath()
	if err != nil {
		return "", err
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve selected Bash path: %w", err)
	}
	selectedCanonical, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		return "", fmt.Errorf("resolve selected Bash target: %w", err)
	}
	selectedCanonical = filepath.Clean(selectedCanonical)
	if selectedCanonical != trustedPath {
		return "", fmt.Errorf("selected executable %q resolves outside the trusted system Bash", path)
	}

	selectedInfo, err := os.Stat(absPath)
	if err != nil {
		return "", fmt.Errorf("inspect selected Bash: %w", err)
	}
	trustedInfo, err := os.Stat(trustedPath)
	if err != nil {
		return "", fmt.Errorf("inspect trusted system Bash: %w", err)
	}
	if !os.SameFile(selectedInfo, trustedInfo) {
		return "", errors.New("selected executable does not match the trusted system Bash file")
	}
	if err := verifyTrustedSystemExecutable(trustedPath, trustedInfo); err != nil {
		return "", err
	}
	return trustedPath, nil
}

func verifyTrustedSystemExecutable(path string, info os.FileInfo) error {
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return errors.New("trusted system Bash is not a regular executable file")
	}
	if info.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 {
		return errors.New("trusted system Bash has setuid or setgid mode bits")
	}
	if info.Mode().Perm()&0o022 != 0 {
		return errors.New("trusted system Bash is writable by group or other users")
	}
	if err := requireRootOwnedPath(info, path); err != nil {
		return err
	}
	if err := verifyTrustedSystemPathSecurity(path, false); err != nil {
		return err
	}

	for directory := filepath.Dir(path); ; directory = filepath.Dir(directory) {
		directoryInfo, err := os.Lstat(directory)
		if err != nil {
			return fmt.Errorf("inspect trusted Bash path component %q: %w", directory, err)
		}
		if !directoryInfo.IsDir() || directoryInfo.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("trusted Bash path component %q is not a real directory", directory)
		}
		if directoryInfo.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("trusted Bash path component %q is writable by group or other users", directory)
		}
		if err := requireRootOwnedPath(directoryInfo, directory); err != nil {
			return err
		}
		if err := verifyTrustedSystemPathSecurity(directory, true); err != nil {
			return err
		}
		if directory == string(filepath.Separator) {
			break
		}
	}
	return nil
}

func requireRootOwnedPath(info os.FileInfo, path string) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 {
		return fmt.Errorf("trusted Bash path component %q is not owned by root", path)
	}
	return nil
}

func managedBashGateScript(fd int) string {
	// The helper receives opaque NUL-delimited environment entries over fd3.
	// Bash read/export preserve newlines and shell metacharacters without eval.
	// SHELLOPTS and BASHOPTS are reconstructed from validated option names;
	// exported Bash functions are rejected during preflight because importing
	// them after startup would require evaluating their function definitions.
	script := `builtin read -r -d '' gate_token <&__GATE_FD__ || builtin exit 125
[[ $gate_token == go-cli-identity-release ]] || builtin exit 125
while IFS= builtin read -r -d '' gate_entry <&__GATE_FD__; do
	gate_key=${gate_entry%%=*}
	case $gate_key in
	SHELLOPTS)
		gate_options=${gate_entry#*=}
		while [[ -n $gate_options ]]; do
			gate_option=${gate_options%%:*}
			builtin set -o "$gate_option" || builtin exit 125
			if [[ $gate_options == *:* ]]; then
				gate_options=${gate_options#*:}
			else
				break
			fi
		done
		builtin export SHELLOPTS || builtin exit 125
		;;
	BASHOPTS)
		gate_options=${gate_entry#*=}
		while [[ -n $gate_options ]]; do
			gate_option=${gate_options%%:*}
			builtin shopt -s "$gate_option" || builtin exit 125
			if [[ $gate_options == *:* ]]; then
				gate_options=${gate_options#*:}
			else
				break
			fi
		done
		builtin export BASHOPTS || builtin exit 125
		;;
	BASH_FUNC_*)
		builtin exit 125
		;;
	*)
		builtin export "$gate_entry" || builtin exit 125
		;;
	esac
done
exec __GATE_FD__<&-
builtin exec -a "$0" "$2" -lc "$1"`
	return strings.ReplaceAll(script, "__GATE_FD__", strconv.Itoa(fd))
}

func prepareBashRestoreEnvironment(env []string) ([]string, error) {
	lastByKey := make(map[string]string, len(env))
	for _, entry := range env {
		if strings.IndexByte(entry, 0) >= 0 {
			return nil, errors.New("command environment contains a NUL byte")
		}
		key, _, ok := strings.Cut(entry, "=")
		if !ok || !validShellEnvironmentKey(key) && !strings.HasPrefix(key, "BASH_FUNC_") {
			return nil, fmt.Errorf("invalid environment variable name %q", key)
		}
		lastByKey[key] = entry
	}
	keys := make([]string, 0, len(lastByKey))
	for key := range lastByKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	normal := make([]string, 0, len(keys))
	var bashOptions, shellOptions []string
	for _, key := range keys {
		entry := lastByKey[key]
		_, value, _ := strings.Cut(entry, "=")
		if name, isFunction := bashFunctionEnvironmentName(key); isFunction {
			return nil, fmt.Errorf("exported Bash function %q cannot be restored safely by the process start gate", name)
		}
		if strings.HasPrefix(key, "BASH_FUNC_") {
			return nil, fmt.Errorf("invalid exported Bash function variable %q", key)
		}
		if !validShellEnvironmentKey(key) {
			return nil, fmt.Errorf("invalid environment variable name %q", key)
		}
		switch key {
		case "SHELLOPTS":
			if err := validateBashOptionList(value); err != nil {
				return nil, fmt.Errorf("invalid SHELLOPTS: %w", err)
			}
			shellOptions = append(shellOptions, entry)
		case "BASHOPTS":
			if err := validateBashOptionList(value); err != nil {
				return nil, fmt.Errorf("invalid BASHOPTS: %w", err)
			}
			bashOptions = append(bashOptions, entry)
		default:
			if !bashReadonlyEnvironmentKey(key) {
				normal = append(normal, entry)
			}
		}
	}
	// Set regular variables first, then turn on shell options. This ensures
	// values such as PS4 reach the helper before xtrace can expand them. The
	// target Bash receives the resulting exported state.
	ordered := make([]string, 0, len(keys))
	ordered = append(ordered, normal...)
	ordered = append(ordered, bashOptions...)
	ordered = append(ordered, shellOptions...)
	return ordered, nil
}

func bashFunctionEnvironmentName(key string) (string, bool) {
	const prefix = "BASH_FUNC_"
	if !strings.HasPrefix(key, prefix) || !strings.HasSuffix(key, "%%") {
		return "", false
	}
	name := strings.TrimSuffix(strings.TrimPrefix(key, prefix), "%%")
	if !validShellEnvironmentKey(name) {
		return "", false
	}
	return name, true
}

func validShellEnvironmentKey(key string) bool {
	if key == "" || !(key[0] == '_' || key[0] >= 'A' && key[0] <= 'Z' || key[0] >= 'a' && key[0] <= 'z') {
		return false
	}
	for index := 1; index < len(key); index++ {
		value := key[index]
		if !(value == '_' || value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z' || value >= '0' && value <= '9') {
			return false
		}
	}
	return true
}

func validBashOptionName(name string) bool {
	if name == "" || !(name[0] >= 'A' && name[0] <= 'Z' || name[0] >= 'a' && name[0] <= 'z') {
		return false
	}
	for index := 1; index < len(name); index++ {
		value := name[index]
		if !(value == '_' || value == '-' || value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z' || value >= '0' && value <= '9') {
			return false
		}
	}
	return true
}

func validateBashOptionList(value string) error {
	for _, option := range strings.Split(value, ":") {
		if option != "" && !validBashOptionName(option) {
			return fmt.Errorf("unsupported option name %q", option)
		}
	}
	return nil
}

func bashReadonlyEnvironmentKey(key string) bool {
	switch key {
	case "BASH", "BASHOPTS", "BASHPID", "BASH_VERSINFO", "EUID", "PPID", "SHELLOPTS", "UID":
		return true
	default:
		return false
	}
}

func (g *bashProcessStartGate) release() error {
	if g == nil || g.writeEnd == nil {
		return errors.New("bash process start gate is unavailable")
	}
	payload := []byte("go-cli-identity-release\x00")
	for _, entry := range g.restoreEnv {
		payload = append(payload, entry...)
		payload = append(payload, 0)
	}
	var writeErr error
	for len(payload) > 0 {
		var written int
		written, writeErr = g.writeEnd.Write(payload)
		if writeErr != nil {
			break
		}
		if written == 0 {
			writeErr = errors.New("short write to Bash process start gate")
			break
		}
		payload = payload[written:]
	}
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
