//go:build darwin || linux

package cli

import (
	"context"
	"io"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	xterm "golang.org/x/term"
)

type ptyLineResult struct {
	line string
	err  error
}

func startPTYReadLine(ctx context.Context, le *DefaultLineEditor) <-chan ptyLineResult {
	result := make(chan ptyLineResult, 1)
	go func() {
		line, err := le.ReadLine(ctx, "> ")
		result <- ptyLineResult{line: line, err: err}
	}()
	return result
}

func awaitPTYState(t *testing.T, slave *os.File, original *xterm.State, wantOriginal bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		state, err := xterm.GetState(int(slave.Fd()))
		if err == nil && reflect.DeepEqual(state, original) == wantOriginal {
			return
		}
		time.Sleep(time.Millisecond)
	}
	state, err := xterm.GetState(int(slave.Fd()))
	require.NoError(t, err)
	require.Equal(t, wantOriginal, reflect.DeepEqual(state, original), "terminal state did not reach requested mode")
}

func assertPTYInputRemains(t *testing.T, slave *os.File, expected int) {
	t.Helper()
	// A master write can return before the line discipline delivers bytes to
	// the slave queue. Observe delivery before checking that no reader steals
	// them; a stolen byte also fails this bounded wait because we never rewrite it.
	deliveryDeadline := time.Now().Add(2 * time.Second)
	for {
		pending, err := pendingPTYInputBytes(slave.Fd())
		require.NoError(t, err)
		require.LessOrEqual(t, pending, expected, "unexpected extra input reached the queue")
		if pending == expected {
			break
		}
		require.True(t, time.Now().Before(deliveryDeadline), "input never reached the next owner's queue")
		time.Sleep(time.Millisecond)
	}
	deadline := time.Now().Add(150 * time.Millisecond)
	for time.Now().Before(deadline) {
		pending, err := pendingPTYInputBytes(slave.Fd())
		require.NoError(t, err)
		require.Equal(t, expected, pending, "input was consumed before the next owner read it")
		time.Sleep(time.Millisecond)
	}
}

func TestReadLineTTYReleasesFirstKeyForNextOwner(t *testing.T) {
	slave, master, cleanup := openPTYWithMaster(t)
	defer cleanup()
	le := NewDefaultLineEditor(slave, io.Discard)
	defer le.Stop()

	original, err := xterm.GetState(int(slave.Fd()))
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result := startPTYReadLine(ctx, le)
	awaitPTYState(t, slave, original, false)
	_, err = master.Write([]byte("\r"))
	require.NoError(t, err)
	first := receivePTYLine(t, result)
	require.NoError(t, first.err)
	require.Empty(t, first.line)
	awaitPTYState(t, slave, original, true)

	rawState, err := setRawMode(slave)
	require.NoError(t, err)
	defer func() { require.NoError(t, restoreMode(slave, rawState)) }()
	_, err = master.Write([]byte{'n'})
	require.NoError(t, err)
	assertPTYInputRemains(t, slave, 1)
	var got [1]byte
	n, err := slave.Read(got[:])
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Equal(t, byte('n'), got[0])
	require.NoError(t, restoreMode(slave, rawState))
	awaitPTYState(t, slave, original, true)
}

func TestReadLineTTYContextCancelReleasesReaderAndRestoresMode(t *testing.T) {
	slave, master, cleanup := openPTYWithMaster(t)
	defer cleanup()
	le := NewDefaultLineEditor(slave, io.Discard)
	defer le.Stop()

	original, err := xterm.GetState(int(slave.Fd()))
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	result := startPTYReadLine(ctx, le)
	awaitPTYState(t, slave, original, false)
	cancel()
	got := receivePTYLine(t, result)
	require.ErrorIs(t, got.err, context.Canceled)
	awaitPTYState(t, slave, original, true)

	rawState, err := setRawMode(slave)
	require.NoError(t, err)
	defer func() { require.NoError(t, restoreMode(slave, rawState)) }()
	_, err = master.Write([]byte{'n'})
	require.NoError(t, err)
	assertPTYInputRemains(t, slave, 1)
	var key [1]byte
	n, err := slave.Read(key[:])
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Equal(t, byte('n'), key[0])
	require.NoError(t, restoreMode(slave, rawState))
	awaitPTYState(t, slave, original, true)
}

func TestReadLineTTYCookedToggleJoinsRawReaderBeforeScanner(t *testing.T) {
	slave, master, cleanup := openPTYWithMaster(t)
	defer cleanup()
	le := NewDefaultLineEditor(slave, io.Discard)
	defer le.Stop()

	original, err := xterm.GetState(int(slave.Fd()))
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result := startPTYReadLine(ctx, le)
	awaitPTYState(t, slave, original, false)
	_, err = master.Write([]byte{0x1c})
	require.NoError(t, err)
	awaitPTYState(t, slave, original, true)
	_, err = master.Write([]byte("cooked\r"))
	require.NoError(t, err)
	got := receivePTYLine(t, result)
	require.NoError(t, got.err)
	require.Equal(t, "cooked", got.line)
	awaitPTYState(t, slave, original, true)
}

func receivePTYLine(t *testing.T, result <-chan ptyLineResult) ptyLineResult {
	t.Helper()
	select {
	case got := <-result:
		return got
	case <-time.After(3 * time.Second):
		t.Fatal("PTY line editor did not return")
		return ptyLineResult{}
	}
}
