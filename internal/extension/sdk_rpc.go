package extension

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	api "github.com/pengjunchen/go-cli/pkg/extensionapi/v1"
)

// ErrRPCOverloaded rejects work rather than growing an unbounded request queue.
var ErrRPCOverloaded = errors.New("extension: RPC inflight budget exhausted")

// RPCOutcomeUnknownError marks a disconnect/deadline after dispatch as non-replayable.
type RPCOutcomeUnknownError struct{ Cause error }

// Error returns a safe outcome category without request or environment data.
func (e *RPCOutcomeUnknownError) Error() string {
	return "extension: RPC execution outcome unknown: " + e.Cause.Error()
}

// Unwrap preserves the cancellation or transport failure.
func (e *RPCOutcomeUnknownError) Unwrap() error { return e.Cause }

// OutcomeUnknown prevents callers treating a failed connection as a rollback.
func (e *RPCOutcomeUnknownError) OutcomeUnknown() bool { return true }

// RPCProcessConfig is an explicitly trusted local .rpc.json process descriptor.
// Env is the complete child environment, never an addition to os.Environ.
type RPCProcessConfig struct {
	Command        []string          `json:"command"`
	Cwd            string            `json:"cwd,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	ArtifactSHA256 string            `json:"artifact_sha256"`
	MaxInflight    int               `json:"max_inflight,omitempty"`
	TimeoutMillis  int               `json:"timeout_millis,omitempty"`
}

type sdkPluginLoader struct{ fallback PluginLoader }

var _ PluginLoader = (*sdkPluginLoader)(nil)

// NewSDKPluginLoader adds bounded stdio JSONL processes to an existing native loader.
func NewSDKPluginLoader(fallback PluginLoader) PluginLoader {
	if fallback == nil {
		fallback = NewDefaultPluginLoader()
	}
	return &sdkPluginLoader{fallback: fallback}
}

// Name returns the loader identity.
func (l *sdkPluginLoader) Name() string { return "sdk-jsonl+" + l.fallback.Name() }

// Load initializes .rpc.json descriptors and delegates other sources unchanged.
func (l *sdkPluginLoader) Load(ctx context.Context, path string) ([]Extension, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !strings.HasSuffix(strings.ToLower(path), ".rpc.json") {
		return l.fallback.Load(ctx, path)
	}
	file, err := openRPCArtifact(path)
	if err != nil {
		return nil, fmt.Errorf("extension: open RPC descriptor: %w", err)
	}
	defer func() { _ = file.Close() }()
	var config RPCProcessConfig
	decoder := json.NewDecoder(&rpcArtifactReader{ctx: ctx, reader: io.LimitReader(file, api.MaxFrameBytes)})
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&config); err != nil {
		return nil, fmt.Errorf("extension: decode RPC descriptor: %w", err)
	}
	var trailing any
	if err = decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("extension: trailing RPC descriptor content")
	}
	if config.Cwd == "" {
		config.Cwd = filepath.Dir(path)
	}
	process, manifest, err := startRPCProcess(ctx, config)
	if err != nil {
		return nil, err
	}
	return []Extension{&sdkExtension{manifest: manifest, caller: process}}, nil
}

type rpcReply struct {
	response api.Response
	err      error
}
type rpcProcess struct {
	mu      sync.Mutex
	writeMu sync.Mutex
	command *exec.Cmd
	input   io.WriteCloser
	pending map[uint64]chan rpcReply
	next    uint64
	closed  bool
	failure error
	permits chan struct{}
	timeout time.Duration
	done    chan struct{}
}

func startRPCProcess(ctx context.Context, config RPCProcessConfig) (*rpcProcess, api.Manifest, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, api.Manifest{}, err
	}
	if len(config.Command) == 0 || !filepath.IsAbs(config.Command[0]) {
		return nil, api.Manifest{}, errors.New("extension: RPC command must be an absolute executable path")
	}
	if config.ArtifactSHA256 == "" {
		return nil, api.Manifest{}, errors.New("extension: RPC artifact_sha256 is required (integrity, not authentication)")
	}
	artifactDir, err := os.MkdirTemp("", "go-cli-extension-")
	if err != nil {
		return nil, api.Manifest{}, fmt.Errorf("extension: create RPC artifact scope: %w", err)
	}
	artifactTransferred := false
	defer func() {
		if !artifactTransferred {
			_ = os.RemoveAll(artifactDir)
		}
	}()
	artifactName := "worker"
	if runtime.GOOS == "windows" {
		artifactName += ".exe"
	}
	artifactPath := filepath.Join(artifactDir, artifactName)
	file, err := openRPCArtifact(config.Command[0])
	if err != nil {
		return nil, api.Manifest{}, fmt.Errorf("extension: open RPC artifact: %w", err)
	}
	captured, err := os.OpenFile(artifactPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0500)
	if err != nil {
		_ = file.Close()
		return nil, api.Manifest{}, fmt.Errorf("extension: capture RPC artifact: %w", err)
	}
	hash := sha256.New()
	const maxArtifactBytes = 128 << 20
	size, copyErr := io.Copy(io.MultiWriter(captured, hash), &rpcArtifactReader{ctx: ctx, reader: io.LimitReader(file, maxArtifactBytes+1)})
	if err = errors.Join(copyErr, captured.Close(), file.Close()); err != nil {
		return nil, api.Manifest{}, fmt.Errorf("extension: capture RPC executable: %w", err)
	}
	if size > maxArtifactBytes {
		return nil, api.Manifest{}, errors.New("extension: RPC executable exceeds 128 MiB artifact budget")
	}
	if !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), config.ArtifactSHA256) {
		return nil, api.Manifest{}, errors.New("extension: RPC artifact digest mismatch")
	}
	if config.MaxInflight == 0 {
		config.MaxInflight = 8
	}
	if config.MaxInflight < 1 || config.MaxInflight > 64 {
		return nil, api.Manifest{}, errors.New("extension: RPC max_inflight must be 1..64")
	}
	if config.TimeoutMillis == 0 {
		config.TimeoutMillis = 30000
	}
	if config.TimeoutMillis < 1 || config.TimeoutMillis > 300000 {
		return nil, api.Manifest{}, errors.New("extension: RPC timeout_millis must be 1..300000")
	}
	command := exec.Command(artifactPath, config.Command[1:]...)
	command.Dir = config.Cwd
	command.Env = []string{}
	for key, value := range config.Env {
		if key == "" || strings.ContainsAny(key, "=\x00") || strings.ContainsRune(value, '\x00') {
			return nil, api.Manifest{}, errors.New("extension: invalid RPC environment")
		}
		command.Env = append(command.Env, key+"="+value)
	}
	command.Stderr = io.Discard // plugin stderr may contain secrets; no implicit host logging
	configureRPCProcess(command)
	input, err := command.StdinPipe()
	if err != nil {
		return nil, api.Manifest{}, fmt.Errorf("extension: RPC stdin: %w", err)
	}
	output, err := command.StdoutPipe()
	if err != nil {
		_ = input.Close()
		return nil, api.Manifest{}, fmt.Errorf("extension: RPC stdout: %w", err)
	}
	if err = command.Start(); err != nil {
		_ = input.Close()
		_ = output.Close()
		return nil, api.Manifest{}, fmt.Errorf("extension: start RPC worker: %w", err)
	}
	artifactTransferred = true
	process := &rpcProcess{command: command, input: input, pending: make(map[uint64]chan rpcReply), permits: make(chan struct{}, config.MaxInflight), timeout: time.Duration(config.TimeoutMillis) * time.Millisecond, done: make(chan struct{})}
	go process.read(output)
	go func() {
		err := command.Wait()
		if err == nil {
			err = io.EOF
		}
		process.fail(err)
		_ = os.RemoveAll(artifactDir)
		close(process.done)
	}()
	var manifest api.Manifest
	if err = process.call(ctx, "initialize", struct {
		Version string `json:"version"`
	}{api.Version}, &manifest); err == nil {
		err = manifest.Validate()
	}
	if err == nil {
		for _, capability := range manifest.Capabilities {
			if capability == "skills" {
				err = fmt.Errorf("extension: RPC capability %q is not negotiated by this host", capability)
				break
			}
		}
	}
	if err != nil {
		closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = process.close(closeCtx)
		return nil, api.Manifest{}, err
	}
	return process, manifest, nil
}

func (p *rpcProcess) read(output io.ReadCloser) {
	defer func() { _ = output.Close() }()
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 4096), api.MaxFrameBytes)
	for scanner.Scan() {
		var response api.Response
		if err := json.Unmarshal(scanner.Bytes(), &response); err != nil {
			p.fail(errors.New("extension: malformed RPC response"))
			return
		}
		p.mu.Lock()
		waiting, known := p.pending[response.ID]
		if known {
			delete(p.pending, response.ID)
		}
		p.mu.Unlock()
		if !known {
			p.fail(errors.New("extension: unknown or duplicate RPC response id"))
			return
		}
		waiting <- rpcReply{response: response}
	}
	err := scanner.Err()
	if err == nil {
		err = io.EOF
	}
	p.fail(err)
}

func (p *rpcProcess) fail(err error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.failure = err
	pending := p.pending
	p.pending = make(map[uint64]chan rpcReply)
	p.mu.Unlock()
	for _, waiting := range pending {
		waiting <- rpcReply{err: err}
	}
	_ = p.input.Close()
	terminateRPCProcess(p.command)
}

func (p *rpcProcess) call(ctx context.Context, method string, params any, result any) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case p.permits <- struct{}{}:
		defer func() { <-p.permits }()
	default:
		return ErrRPCOverloaded
	}
	callCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	data, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("extension: encode RPC request: %w", err)
	}
	p.mu.Lock()
	if p.closed {
		failure := p.failure
		p.mu.Unlock()
		return fmt.Errorf("extension: RPC worker unavailable: %w", failure)
	}
	p.next++
	id := p.next
	waiting := make(chan rpcReply, 1)
	p.pending[id] = waiting
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.pending, id); p.mu.Unlock() }()
	deadline, _ := callCtx.Deadline()
	frame, err := json.Marshal(api.Request{ID: id, Method: method, Params: data, DeadlineUnixNano: deadline.UnixNano()})
	if err != nil {
		return err
	}
	if len(frame)+1 > api.MaxFrameBytes {
		return errors.New("extension: RPC request exceeds frame budget")
	}
	stop := context.AfterFunc(callCtx, func() { p.fail(callCtx.Err()) })
	defer stop()
	p.writeMu.Lock()
	_, err = p.input.Write(append(frame, '\n'))
	p.writeMu.Unlock()
	if err != nil {
		p.fail(err)
		return &RPCOutcomeUnknownError{Cause: err}
	}
	select {
	case reply := <-waiting:
		if reply.err != nil {
			return &RPCOutcomeUnknownError{Cause: reply.err}
		}
		if err = callCtx.Err(); err != nil {
			return &RPCOutcomeUnknownError{Cause: err}
		}
		if reply.response.Error != nil {
			if reply.response.Error.Code == "callback_panicked" || reply.response.Error.Code == "outcome_unknown" {
				return &RPCOutcomeUnknownError{Cause: errors.New("extension: remote callback outcome unknown")}
			}
			return fmt.Errorf("extension: RPC %s failed (%s)", method, reply.response.Error.Code)
		}
		if result == nil {
			return nil
		}
		if err = json.Unmarshal(reply.response.Result, result); err != nil {
			p.fail(errors.New("extension: invalid RPC result shape"))
			return &RPCOutcomeUnknownError{Cause: fmt.Errorf("extension: decode RPC result: %w", err)}
		}
		return nil
	case <-callCtx.Done():
		p.fail(callCtx.Err())
		return &RPCOutcomeUnknownError{Cause: callCtx.Err()}
	}
}

func (p *rpcProcess) close(ctx context.Context) error {
	p.fail(errors.New("extension: RPC worker closed"))
	select {
	case <-p.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("extension: RPC worker exit pending: %w", ctx.Err())
	}
}

// Regular-file checks and a nonblocking/no-follow open reject device/FIFO
// artifacts without leaving a candidate builder stuck waiting for a writer.
func openRPCArtifact(path string) (*os.File, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, errors.New("extension: RPC artifact must be a regular file")
	}
	file, err := openRPCArtifactFile(path)
	if err != nil {
		return nil, err
	}
	after, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !after.Mode().IsRegular() {
		_ = file.Close()
		return nil, errors.New("extension: RPC artifact changed to a non-regular file")
	}
	return file, nil
}

type rpcArtifactReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *rpcArtifactReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}
