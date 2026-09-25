//go:build darwin || linux

package tools

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var sshpassEscapedChildMarker = flag.String("r1-sshpass-escaped-child-marker", "", "test helper marker path")

func TestSSHPassEscapedChildHelper(t *testing.T) {
	if *sshpassEscapedChildMarker == "" {
		return
	}
	if _, err := syscall.Setsid(); err != nil {
		t.Fatalf("detach fake ssh child from sshpass process group: %v", err)
	}
	if err := os.WriteFile(*sshpassEscapedChildMarker+".pid", []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatalf("write escaped child pid: %v", err)
	}
	time.Sleep(600 * time.Millisecond)
	if err := os.WriteFile(*sshpassEscapedChildMarker, []byte("finished"), 0o600); err != nil {
		t.Fatalf("write escaped child marker: %v", err)
	}
}

func TestRemoteBashSSHPassEscapedChildRemainsPending(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "escaped-child-finished")
	testBinary, err := os.Executable()
	require.NoError(t, err)
	dir := t.TempDir()
	sshpassPath := filepath.Join(dir, "sshpass")
	script := fmt.Sprintf("#!/bin/sh\n%s -test.run=%s %s &\nchild=$!\nwhile [ ! -f %s.pid ]; do sleep 0.01; done\nwait \"$child\"\n",
		shellQuoteTestArgument(testBinary),
		shellQuoteTestArgument("^TestSSHPassEscapedChildHelper$"),
		shellQuoteTestArgument("-r1-sshpass-escaped-child-marker="+marker),
		shellQuoteTestArgument(marker))
	require.NoError(t, os.WriteFile(sshpassPath, []byte(script), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	client := NewDefaultSSHClient(SSHConfig{Host: "example.invalid", Password: "secret"})
	tool := NewRemoteBashTool(client, WithRemoteBashTimeout(time.Minute))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type response struct {
		result *ToolResult
		err    error
	}
	finished := make(chan response, 1)
	go func() {
		result, err := tool.Execute(ctx, ToolCall{
			ID:   "call-sshpass-escaped-child",
			Name: "remote_bash",
			Args: map[string]any{"command": "may-continue-after-local-transport-stops"},
		})
		finished <- response{result: result, err: err}
	}()

	require.Eventually(t, func() bool {
		_, statErr := os.Stat(marker + ".pid")
		return statErr == nil
	}, 3*time.Second, 10*time.Millisecond, "fake sshpass child must enter its own session before cancellation")
	escapedPIDBytes, err := os.ReadFile(marker + ".pid")
	require.NoError(t, err)
	escapedPID, err := strconv.Atoi(strings.TrimSpace(string(escapedPIDBytes)))
	require.NoError(t, err)
	cancel()

	var outcome response
	select {
	case outcome = <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("RemoteBash did not return after local process-group cancellation")
	}
	require.ErrorIs(t, outcome.err, context.Canceled)
	require.NotNil(t, outcome.result)
	assert.Equal(t, processContainmentScope, outcome.result.Metadata["local_ssh_process_group_scope"])
	assert.Equal(t, "unknown", outcome.result.Metadata["remote_completion"])
	var pending PendingExecutionError
	require.ErrorAs(t, outcome.err, &pending)
	assert.True(t, pending.OutcomeUnknown())
	select {
	case <-pending.ExecutionDone():
		t.Fatal("the escaped local child and stopped transport cannot prove remote completion")
	default:
	}

	require.Eventually(t, func() bool {
		_, statErr := os.Stat(marker)
		return statErr == nil
	}, 2*time.Second, 10*time.Millisecond, "the child outside sshpass's owned process group continues after cancellation")
	require.Eventually(t, func() bool {
		killErr := syscall.Kill(-escapedPID, 0)
		return errors.Is(killErr, syscall.ESRCH)
	}, 2*time.Second, 10*time.Millisecond, "the finite escaped fixture should exit and leave no process behind")
	select {
	case <-pending.ExecutionDone():
		t.Fatal("a local SSH child exit still cannot provide a remote completion receipt")
	default:
	}
}

func TestNewLocalProcessScopeRejectsJoiningExternalProcessGroup(t *testing.T) {
	cmd := exec.Command("true")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: 1}

	_, err := newLocalProcessScope(cmd)

	require.Error(t, err)
}

func TestPendingLocalExecutionClosesAfterObservedQuiescence(t *testing.T) {
	cmd := exec.Command("sleep", "10")
	scope, err := newLocalProcessScope(cmd)
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	scope.started(cmd)
	reaped := false
	t.Cleanup(func() {
		if !reaped {
			_ = scope.requestKill()
			_ = cmd.Wait()
		}
	})

	leaderDone := make(chan struct{})
	readersDone := make(chan struct{})
	close(readersDone)
	done := observeProcessCompletion(scope.pgid, leaderDone, readersDone)
	err = pendingProcessErrorWithDone(errProcessCleanupUnconfirmed, true, done)
	var pending PendingExecutionError
	require.ErrorAs(t, err, &pending)
	select {
	case <-pending.ExecutionDone():
		t.Fatal("pending local execution closed before the process group stopped")
	case <-time.After(20 * time.Millisecond):
	}

	require.NoError(t, scope.requestKill())
	_ = cmd.Wait()
	reaped = true
	close(leaderDone)
	select {
	case <-pending.ExecutionDone():
	case <-time.After(time.Second):
		t.Fatal("the observer did not release the pending execution after quiescence")
	}
	assert.True(t, pending.OutcomeUnknown())
}

func TestPendingLocalExecutionClosesAfterReaderErrorAndGroupExit(t *testing.T) {
	cmd := exec.Command("sleep", "10")
	scope, err := newLocalProcessScope(cmd)
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	scope.started(cmd)
	reaped := false
	t.Cleanup(func() {
		if !reaped {
			_ = scope.requestKill()
			_ = cmd.Wait()
		}
	})

	leaderDone := make(chan struct{})
	readersDone := make(chan struct{})
	close(leaderDone)
	close(readersDone)
	// A reader error affects output validity, but once the joined reader and
	// owned process group stop it must not keep the resource receipt pending.
	done := observeProcessCompletion(scope.pgid, leaderDone, readersDone)
	var pending PendingExecutionError
	require.ErrorAs(t, pendingProcessErrorWithDone(errProcessCleanupUnconfirmed, true, done), &pending)
	select {
	case <-pending.ExecutionDone():
		t.Fatal("the reader-error pending execution closed while the group was alive")
	case <-time.After(20 * time.Millisecond):
	}

	require.NoError(t, scope.requestKill())
	_ = cmd.Wait()
	reaped = true
	select {
	case <-pending.ExecutionDone():
	case <-time.After(time.Second):
		t.Fatal("the reader error kept the pending execution open after the group stopped")
	}
}

func TestManagedLocalCommandClosesReadDescriptorsAfterEOF(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	managed, err := startManagedLocalCommand(cancel, exec.Command("true"))
	require.NoError(t, err)
	result := managed.collect(ctx, func(reader io.Reader, _ string) (bool, error) {
		_, readErr := io.Copy(io.Discard, reader)
		return readErr == nil, readErr
	})
	require.True(t, result.quiescent)
	require.NoError(t, result.runErr)
	_, stdoutErr := managed.stdout.Stat()
	_, stderrErr := managed.stderr.Stat()
	assert.ErrorIs(t, stdoutErr, os.ErrClosed)
	assert.ErrorIs(t, stderrErr, os.ErrClosed)
}

func TestManagedLocalCommandTracksDelayedReaderJoin(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	managed, err := startManagedLocalCommand(cancel, exec.Command("true"))
	require.NoError(t, err)
	releaseReader := make(chan struct{})
	readerDrained := make(chan struct{}, 2)
	resultCh := make(chan managedLocalResult, 1)
	go func() {
		resultCh <- managed.collect(ctx, func(reader io.Reader, _ string) (bool, error) {
			_, readErr := io.Copy(io.Discard, reader)
			readerDrained <- struct{}{}
			<-releaseReader
			return readErr == nil, readErr
		})
	}()
	defer func() {
		select {
		case <-releaseReader:
		default:
			close(releaseReader)
		}
	}()

	<-readerDrained
	<-readerDrained
	var result managedLocalResult
	select {
	case result = <-resultCh:
	case <-time.After(time.Second):
		t.Fatal("collect did not return after its bounded reader join")
	}
	assert.False(t, result.readersJoined)
	assert.True(t, result.forcedPipeClose)
	assert.False(t, result.containmentProven)
	require.NotNil(t, result.executionDone, "the still-running local reader has an observable completion channel")
	var pending PendingExecutionError
	require.ErrorAs(t, localLifecycleError(errProcessCleanupUnconfirmed, result), &pending)
	assert.True(t, pending.OutcomeUnknown())
	reported := make(chan searchCompletionReport, 1)
	reportPreparedLocalProcessCompletion(withPreparedCompletionReporter(context.Background(), func(proof PreparedCompletionProof, done <-chan struct{}) {
		reported <- searchCompletionReport{proof: proof, done: done}
	}), result)
	select {
	case got := <-reported:
		t.Fatalf("forced pipe close published positive completion proof %q", got.proof)
	default:
	}
	select {
	case <-pending.ExecutionDone():
		t.Fatal("the pending resource was released before the reader joined")
	default:
	}

	close(releaseReader)
	select {
	case <-pending.ExecutionDone():
	case <-time.After(time.Second):
		t.Fatal("the pending resource was not released after reader join and process quiescence")
	}
}

func TestManagedLocalCommandForcedCloseJoinsReaderWithoutProvingContainment(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	managed, err := startManagedLocalCommand(cancel, exec.Command("true"))
	require.NoError(t, err)
	readerStarted := make(chan struct{}, 2)
	releaseReaders := make(chan struct{})
	resultCh := make(chan managedLocalResult, 1)
	go func() {
		resultCh <- managed.collect(ctx, func(reader io.Reader, _ string) (bool, error) {
			readerStarted <- struct{}{}
			<-releaseReaders
			_, readErr := reader.Read(make([]byte, 1))
			return readErr == nil, readErr
		})
	}()
	defer func() {
		select {
		case <-releaseReaders:
		default:
			close(releaseReaders)
		}
	}()
	<-readerStarted
	<-readerStarted

	deadline := time.Now().Add(time.Second)
	for {
		if _, statErr := managed.stdout.Stat(); errors.Is(statErr, os.ErrClosed) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("collect did not force-close the output pipes")
		}
		time.Sleep(time.Millisecond)
	}
	close(releaseReaders)

	var result managedLocalResult
	select {
	case result = <-resultCh:
	case <-time.After(time.Second):
		t.Fatal("collect did not return after the readers joined")
	}
	assert.True(t, result.forcedPipeClose)
	assert.True(t, result.readersJoined)
	assert.ErrorIs(t, result.readerErr, os.ErrClosed)
	assert.True(t, result.quiescent, "the owned process group, leader, and local readers have joined")
	assert.False(t, result.containmentProven, "forced reader closure cannot prove escaped pipe writers stopped")
	assert.Nil(t, result.executionDone, "an escaped writer cannot be represented by a false group-only completion watcher")

	err = managedSearchLifecycleError(context.Background(), "test helper", result)
	var unknown interface{ OutcomeUnknown() bool }
	require.ErrorAs(t, err, &unknown)
	assert.True(t, unknown.OutcomeUnknown())
	assert.False(t, isPendingExecution(err), "joined local readers must not be misreported as an observable live resource")
	assert.False(t, shouldFallbackAfterManagedHelper(context.Background(), err))

	reported := make(chan searchCompletionReport, 1)
	reportPreparedLocalProcessCompletion(withPreparedCompletionReporter(context.Background(), func(proof PreparedCompletionProof, done <-chan struct{}) {
		reported <- searchCompletionReport{proof: proof, done: done}
	}), result)
	select {
	case got := <-reported:
		t.Fatalf("forced reader close published positive completion proof %q", got.proof)
	default:
	}
}
