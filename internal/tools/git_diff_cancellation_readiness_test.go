//go:build darwin || linux

package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type gitCancelReadinessClock struct {
	now  func() time.Time
	wait func(time.Duration, <-chan error) (error, bool)
}

type gitCancelPIDRead func() ([]byte, os.FileMode, error)

type gitCancelFakeClock struct {
	now   time.Time
	waits int
}

func (f *gitCancelFakeClock) clock() gitCancelReadinessClock {
	return gitCancelReadinessClock{
		now: func() time.Time { return f.now },
		wait: func(d time.Duration, returned <-chan error) (error, bool) {
			f.waits++
			select {
			case err := <-returned:
				return err, true
			default:
			}
			f.now = f.now.Add(d)
			return nil, false
		},
	}
}

func TestGitCancelReadinessPublishedBeforeDeadline(t *testing.T) {
	start := time.Unix(10, 0)
	t.Run("pid_file", func(t *testing.T) {
		clock := &gitCancelFakeClock{now: start}
		returned := make(chan error, 1)
		calls := 0
		leader, child, err := awaitGitCancelPIDs(start.Add(time.Second), returned, clock.clock(), func() ([]byte, os.FileMode, error) {
			calls++
			if calls == 1 {
				return nil, 0, os.ErrNotExist
			}
			return []byte("41 42\n"), 0o600, nil
		})
		require.NoError(t, err)
		assert.Equal(t, 41, leader)
		assert.Equal(t, 42, child)
		assert.Equal(t, 2, calls)
		assert.Equal(t, 1, clock.waits)
		assert.Equal(t, 5*time.Millisecond, clock.now.Sub(start))
		want := errors.New("later cancellation result")
		returned <- want
		assert.Same(t, want, <-returned)
	})
	t.Run("done_attachment", func(t *testing.T) {
		clock := &gitCancelFakeClock{now: start}
		returned := make(chan error, 1)
		attached := make(chan struct{})
		calls := 0
		done, err := awaitGitCancelDone(start.Add(time.Second), returned, clock.clock(), func() <-chan struct{} {
			calls++
			if calls == 1 {
				return nil
			}
			return attached
		})
		require.NoError(t, err)
		assert.Equal(t, (<-chan struct{})(attached), done)
		assert.Equal(t, 1, clock.waits)
	})
}

func TestGitCancelReadinessEarlyDispatchHasPriority(t *testing.T) {
	start := time.Unix(20, 0)
	deadline := start.Add(time.Second)
	sentinel := errors.New("early dispatch failure")
	for _, tc := range []struct {
		name string
		err  error
	}{{"error", sentinel}, {"nil", nil}} {
		t.Run(tc.name, func(t *testing.T) {
			clock := &gitCancelFakeClock{now: start}
			returned := make(chan error, 1)
			_, _, err := awaitGitCancelPIDs(deadline, returned, clock.clock(), func() ([]byte, os.FileMode, error) {
				returned <- tc.err
				clock.now = deadline
				return []byte("41 42\n"), 0o600, nil
			})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "before cancellation")
			if tc.err != nil {
				assert.ErrorIs(t, err, sentinel)
			}
			assert.NotErrorIs(t, err, context.DeadlineExceeded)
		})
	}
	t.Run("already_returned_at_deadline", func(t *testing.T) {
		clock := &gitCancelFakeClock{now: deadline}
		returned := make(chan error, 1)
		returned <- sentinel
		reads := 0
		_, _, err := awaitGitCancelPIDs(deadline, returned, clock.clock(), func() ([]byte, os.FileMode, error) {
			reads++
			return []byte("41 42"), 0o600, nil
		})
		assert.ErrorIs(t, err, sentinel)
		assert.Zero(t, reads)
	})
	t.Run("done_attachment", func(t *testing.T) {
		clock := &gitCancelFakeClock{now: start}
		returned := make(chan error, 1)
		attached := make(chan struct{})
		_, err := awaitGitCancelDone(deadline, returned, clock.clock(), func() <-chan struct{} {
			returned <- sentinel
			return attached
		})
		assert.ErrorIs(t, err, sentinel)
	})
}

func TestGitCancelReadinessAbsoluteDeadline(t *testing.T) {
	start := time.Unix(30, 0)
	deadline := start.Add(time.Second)
	for _, tc := range []struct {
		name    string
		readEnd time.Time
		ok      bool
	}{{"before", deadline.Add(-time.Nanosecond), true}, {"exact", deadline, false}, {"across", deadline.Add(time.Nanosecond), false}} {
		t.Run(tc.name, func(t *testing.T) {
			clock := &gitCancelFakeClock{now: start}
			returned := make(chan error, 1)
			_, _, err := awaitGitCancelPIDs(deadline, returned, clock.clock(), func() ([]byte, os.FileMode, error) {
				clock.now = tc.readEnd
				return []byte("41 42"), 0o600, nil
			})
			if tc.ok {
				assert.NoError(t, err)
			} else {
				assert.ErrorIs(t, err, context.DeadlineExceeded)
			}
			assert.Zero(t, clock.waits)
		})
	}
	t.Run("missing_file_at_deadline_keeps_cause", func(t *testing.T) {
		clock := &gitCancelFakeClock{now: start}
		returned := make(chan error, 1)
		_, _, err := awaitGitCancelPIDs(start.Add(5*time.Millisecond), returned, clock.clock(), func() ([]byte, os.FileMode, error) { return nil, 0, os.ErrNotExist })
		assert.ErrorIs(t, err, context.DeadlineExceeded)
		assert.ErrorIs(t, err, os.ErrNotExist)
	})
}

func TestGitCancelReadinessReadAndParseErrors(t *testing.T) {
	start := time.Unix(40, 0)
	sentinel := errors.New("cannot read PID file")
	for _, tc := range []struct {
		name    string
		body    string
		mode    os.FileMode
		readErr error
		want    string
		numErr  bool
	}{
		{"read", "", 0, sentinel, "cannot read PID file", false},
		{"directory", "41 42", os.ModeDir | 0o700, nil, "regular", false},
		{"field_count", "41", 0o600, nil, "2", false},
		{"leader_parse", "x 42", 0o600, nil, "leader", true},
		{"child_parse", "41 x", 0o600, nil, "child", true},
		{"leader_nonpositive", "0 42", 0o600, nil, "positive", false},
		{"child_nonpositive", "41 -1", 0o600, nil, "positive", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := &gitCancelFakeClock{now: start}
			_, _, err := awaitGitCancelPIDs(start.Add(time.Second), make(chan error, 1), clock.clock(), func() ([]byte, os.FileMode, error) { return []byte(tc.body), tc.mode, tc.readErr })
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
			if tc.readErr != nil {
				assert.ErrorIs(t, err, tc.readErr)
			}
			if tc.numErr {
				var parseErr *strconv.NumError
				assert.ErrorAs(t, err, &parseErr)
			}
			assert.Zero(t, clock.waits)
		})
	}
}

func gitCancelSystemClock() gitCancelReadinessClock {
	return gitCancelReadinessClock{
		now: time.Now,
		wait: func(delay time.Duration, returned <-chan error) (error, bool) {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case err := <-returned:
				return err, true
			case <-timer.C:
				return nil, false
			}
		},
	}
}

func gitCancelUnexpectedReturn(err error) error {
	if err == nil {
		return errors.New("prepared dispatch returned before cancellation without an error")
	}
	return fmt.Errorf("prepared dispatch returned before cancellation: %w", err)
}

func gitCancelCheckReturned(returned <-chan error) error {
	select {
	case err := <-returned:
		return gitCancelUnexpectedReturn(err)
	default:
		return nil
	}
}

// waitGitCancelReadiness is specific to this fixture: the dispatch result is
// consumed only when it proves an early return, never on the success path.
func waitGitCancelReadiness(deadline time.Time, returned <-chan error, clock gitCancelReadinessClock, phase string, check func() (bool, error)) error {
	last := fmt.Errorf("%s not yet observed", phase)
	deadlineError := func() error {
		return fmt.Errorf("%s readiness: %w; last observation: %w", phase, context.DeadlineExceeded, last)
	}
	for {
		if err := gitCancelCheckReturned(returned); err != nil {
			return err
		}
		if !clock.now().Before(deadline) {
			return deadlineError()
		}
		ready, err := check()
		if returnedErr := gitCancelCheckReturned(returned); returnedErr != nil {
			return returnedErr
		}
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("%s readiness: %w", phase, err)
			}
			last = err
		}
		if !clock.now().Before(deadline) {
			return deadlineError()
		}
		if ready {
			return nil
		}
		delay := deadline.Sub(clock.now())
		if delay <= 0 {
			continue
		}
		if delay > 5*time.Millisecond {
			delay = 5 * time.Millisecond
		}
		if dispatchErr, returnedEarly := clock.wait(delay, returned); returnedEarly {
			return gitCancelUnexpectedReturn(dispatchErr)
		}
	}
}

func awaitGitCancelPIDs(deadline time.Time, returned <-chan error, clock gitCancelReadinessClock, read gitCancelPIDRead) (int, int, error) {
	leader, child := 0, 0
	err := waitGitCancelReadiness(deadline, returned, clock, "fake Git PID file", func() (bool, error) {
		data, mode, readErr := read()
		if readErr != nil {
			return false, fmt.Errorf("read PID file: %w", readErr)
		}
		if !mode.IsRegular() {
			return false, fmt.Errorf("PID file is not regular: %s", mode)
		}
		fields := strings.Fields(string(data))
		if len(fields) != 2 {
			return false, fmt.Errorf("PID file requires 2 fields, got %d", len(fields))
		}
		parsedLeader, parseErr := strconv.Atoi(fields[0])
		if parseErr != nil {
			return false, fmt.Errorf("parse leader PID: %w", parseErr)
		}
		parsedChild, parseErr := strconv.Atoi(fields[1])
		if parseErr != nil {
			return false, fmt.Errorf("parse child PID: %w", parseErr)
		}
		if parsedLeader <= 0 || parsedChild <= 0 {
			return false, errors.New("leader and child PIDs must be positive")
		}
		leader, child = parsedLeader, parsedChild
		return true, nil
	})
	if err != nil {
		return 0, 0, err
	}
	return leader, child, nil
}

func awaitGitCancelDone(deadline time.Time, returned <-chan error, clock gitCancelReadinessClock, getDone func() <-chan struct{}) (<-chan struct{}, error) {
	var done <-chan struct{}
	err := waitGitCancelReadiness(deadline, returned, clock, "queue Done attachment", func() (bool, error) {
		done = getDone()
		return done != nil, nil
	})
	if err != nil {
		return nil, err
	}
	return done, nil
}

func readGitCancelPIDFile(path string) ([]byte, os.FileMode, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, fmt.Errorf("open PID file: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, 0, fmt.Errorf("stat PID file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, info.Mode(), nil
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, info.Mode(), fmt.Errorf("read PID file: %w", err)
	}
	return data, info.Mode(), nil
}
