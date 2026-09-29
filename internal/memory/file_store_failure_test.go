package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMemoryStoreAppendFailureDoesNotPublishMemory(t *testing.T) {
	for _, mode := range []string{"partial-write-error", "sync-failure"} {
		t.Run(mode, func(t *testing.T) {
			s, path := newTestStore(t)
			ctx := context.Background()
			require.NoError(t, s.Add(ctx, Memory{ID: "existing", Content: "existing"}))
			before, err := os.ReadFile(path)
			require.NoError(t, err)

			writeErr := errors.New("injected append failure")
			originalWrite := s.fileIO.write
			originalSync := s.fileIO.sync
			syncCalls := 0
			if mode == "partial-write-error" {
				s.fileIO.write = func(file *os.File, data []byte) (int, error) {
					if file != s.file {
						return originalWrite(file, data)
					}
					n, err := originalWrite(file, data[:len(data)/2])
					if err != nil {
						return n, err
					}
					return n, writeErr
				}
			}
			s.fileIO.sync = func(file *os.File) error {
				syncCalls++
				if mode == "sync-failure" && syncCalls == 1 {
					return writeErr
				}
				return originalSync(file)
			}

			addErr := s.Add(ctx, Memory{ID: "candidate", Content: "uniquecandidateword"})
			require.ErrorIs(t, addErr, writeErr)
			_, err = s.Get(ctx, "candidate")
			assert.ErrorIs(t, err, ErrMemoryNotFound)
			results, err := s.Search(ctx, "uniquecandidateword", 10)
			require.NoError(t, err)
			assert.Empty(t, results)
			list, err := s.List(ctx)
			require.NoError(t, err)
			assert.Equal(t, []string{"existing"}, memoryIDs(list))
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.True(t, bytes.Equal(before, after), "failed append must restore the original file bytes")
			assert.GreaterOrEqual(t, syncCalls, 1, "rollback must sync the restored file")

			s.fileIO.write = originalWrite
			s.fileIO.sync = originalSync
			require.NoError(t, s.Add(ctx, Memory{ID: "afterfailure", Content: "valid append"}))
			require.NoError(t, s.Close())
			reloaded, err := NewFileMemoryStore(path)
			require.NoError(t, err)
			t.Cleanup(func() { _ = reloaded.Close() })
			loaded, err := reloaded.List(ctx)
			require.NoError(t, err)
			assert.Equal(t, []string{"afterfailure", "existing"}, sortedMemoryIDs(loaded))
		})
	}
}

func TestMemoryStoreAppendCompletesShortWrites(t *testing.T) {
	s, path := newTestStore(t)
	ctx := context.Background()
	originalWrite := s.fileIO.write
	writeCalls := 0
	s.fileIO.write = func(file *os.File, data []byte) (int, error) {
		if file != s.file {
			return originalWrite(file, data)
		}
		writeCalls++
		if writeCalls == 1 {
			return originalWrite(file, data[:len(data)/2])
		}
		return originalWrite(file, data)
	}

	require.NoError(t, s.Add(ctx, Memory{ID: "short", Content: "complete record"}))
	assert.Equal(t, 2, writeCalls, "the append must retry the unwritten suffix")
	require.NoError(t, s.Close())

	reloaded, err := NewFileMemoryStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = reloaded.Close() })
	got, err := reloaded.Get(ctx, "short")
	require.NoError(t, err)
	assert.Equal(t, "complete record", got.Content)
}

func TestMemoryStoreAppendFailurePoisonsWhenRollbackIsUncertain(t *testing.T) {
	for _, mode := range []string{"truncate-failure", "rollback-sync-failure"} {
		t.Run(mode, func(t *testing.T) {
			s, _ := newTestStore(t)
			ctx := context.Background()
			require.NoError(t, s.Add(ctx, Memory{ID: "existing", Content: "existing"}))
			appendErr := errors.New("injected append failure")
			rollbackErr := errors.New("injected rollback failure")
			originalWrite := s.fileIO.write
			s.fileIO.write = func(file *os.File, data []byte) (int, error) {
				if file != s.file {
					return originalWrite(file, data)
				}
				n, err := originalWrite(file, data[:len(data)/2])
				if err != nil {
					return n, err
				}
				return n, appendErr
			}
			if mode == "truncate-failure" {
				s.fileIO.truncate = func(_ *os.File, _ int64) error { return rollbackErr }
			} else {
				s.fileIO.sync = func(*os.File) error { return rollbackErr }
			}
			originalOpen := s.fileIO.openFile
			openCalls := 0
			s.fileIO.openFile = func(name string, flag int, perm os.FileMode) (*os.File, error) {
				openCalls++
				return originalOpen(name, flag, perm)
			}

			err := s.Add(ctx, Memory{ID: "candidate", Content: "not published"})
			var poisoned *MemoryStorePoisonedError
			require.ErrorAs(t, err, &poisoned)
			assert.ErrorIs(t, err, ErrMemoryStorePoisoned)
			assert.ErrorIs(t, err, appendErr)
			assert.ErrorIs(t, err, rollbackErr)
			assert.Zero(t, openCalls, "an unknown rollback must not be followed by an assumed-safe reopen")

			_, err = s.Get(ctx, "candidate")
			assert.ErrorIs(t, err, ErrMemoryNotFound)
			list, err := s.List(ctx)
			require.NoError(t, err)
			assert.Equal(t, []string{"existing"}, memoryIDs(list))
			var secondPoison *MemoryStorePoisonedError
			err = s.Add(ctx, Memory{ID: "later", Content: "must not append"})
			assert.ErrorAs(t, err, &secondPoison)
		})
	}
}

func TestMemoryStoreDeleteReopenFailureConsistent(t *testing.T) {
	s, path := newTestStore(t)
	ctx := context.Background()
	require.NoError(t, s.Add(ctx, Memory{ID: "drop", Content: "uniquedropword"}))
	require.NoError(t, s.Add(ctx, Memory{ID: "keep", Content: "keep"}))
	openErr := errors.New("injected reopen failure")
	s.fileIO.openFile = func(string, int, os.FileMode) (*os.File, error) {
		return nil, openErr
	}

	deleteErr := s.Delete(ctx, "drop")
	assert.ErrorIs(t, deleteErr, ErrMemoryStoreUnavailable)
	assert.ErrorIs(t, deleteErr, openErr)
	_, getErr := s.Get(ctx, "drop")
	assert.ErrorIs(t, getErr, ErrMemoryNotFound)
	list, err := s.List(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"keep"}, memoryIDs(list))
	results, err := s.Search(ctx, "uniquedropword", 10)
	require.NoError(t, err)
	assert.Empty(t, results)
	assert.ErrorIs(t, s.Add(ctx, Memory{ID: "later", Content: "blocked"}), ErrMemoryStoreUnavailable)

	require.NoError(t, s.Close())
	reloaded, err := NewFileMemoryStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = reloaded.Close() })
	loaded, err := reloaded.List(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"keep"}, memoryIDs(loaded))
}

func TestMemoryStoreDeleteClosedStoreReturnsError(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	require.NoError(t, s.Add(ctx, Memory{ID: "present", Content: "present"}))
	require.NoError(t, s.Close())

	var deleteErr error
	assert.NotPanics(t, func() { deleteErr = s.Delete(ctx, "present") })
	assert.ErrorIs(t, deleteErr, ErrMemoryStoreClosed)
	_, err := s.Get(ctx, "present")
	assert.NoError(t, err, "a rejected delete must preserve memory state")
}

func TestMemoryStoreDeleteSyncsTempBeforeRename(t *testing.T) {
	s, path := newTestStore(t)
	ctx := context.Background()
	require.NoError(t, s.Add(ctx, Memory{ID: "drop", Content: "drop"}))
	require.NoError(t, s.Add(ctx, Memory{ID: "keep", Content: "keep"}))
	originalWrite := s.fileIO.write
	originalSync := s.fileIO.sync
	originalRename := s.fileIO.rename
	tempSynced := false
	tempWriteCalls := 0
	s.fileIO.write = func(file *os.File, data []byte) (int, error) {
		if file == s.file {
			return originalWrite(file, data)
		}
		if tempWriteCalls == 0 {
			tempWriteCalls++
			return originalWrite(file, data[:len(data)/2])
		}
		return originalWrite(file, data)
	}
	s.fileIO.sync = func(file *os.File) error {
		if file != s.file {
			tempSynced = true
		}
		return originalSync(file)
	}
	s.fileIO.rename = func(oldPath, newPath string) error {
		if !tempSynced {
			return errors.New("temp file renamed before sync")
		}
		return originalRename(oldPath, newPath)
	}

	require.NoError(t, s.Delete(ctx, "drop"))
	require.NoError(t, s.Close())
	reloaded, err := NewFileMemoryStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = reloaded.Close() })
	loaded, err := reloaded.List(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"keep"}, memoryIDs(loaded))
}

func TestMemoryStoreDeletePreRenameFailurePreservesOldState(t *testing.T) {
	for _, mode := range []string{"temp-write", "temp-sync", "rename"} {
		t.Run(mode, func(t *testing.T) {
			s, path := newTestStore(t)
			ctx := context.Background()
			require.NoError(t, s.Add(ctx, Memory{ID: "drop", Content: "drop"}))
			require.NoError(t, s.Add(ctx, Memory{ID: "keep", Content: "keep"}))
			before, err := os.ReadFile(path)
			require.NoError(t, err)
			injectedErr := errors.New("injected pre-rename failure")
			originalIO := s.fileIO
			switch mode {
			case "temp-write":
				s.fileIO.write = func(file *os.File, data []byte) (int, error) {
					if file == s.file {
						return originalIO.write(file, data)
					}
					n, writeErr := originalIO.write(file, data[:len(data)/2])
					if writeErr != nil {
						return n, writeErr
					}
					return n, injectedErr
				}
			case "temp-sync":
				s.fileIO.sync = func(file *os.File) error {
					if file != s.file {
						return injectedErr
					}
					return originalIO.sync(file)
				}
			case "rename":
				s.fileIO.rename = func(string, string) error { return injectedErr }
			}

			deleteErr := s.Delete(ctx, "drop")
			assert.ErrorIs(t, deleteErr, injectedErr)
			_, getErr := s.Get(ctx, "drop")
			assert.NoError(t, getErr)
			list, err := s.List(ctx)
			require.NoError(t, err)
			assert.Equal(t, []string{"drop", "keep"}, sortedMemoryIDs(list))
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.True(t, bytes.Equal(before, after), "pre-rename failure must leave the old file intact")

			s.fileIO = originalIO
			require.NoError(t, s.Add(ctx, Memory{ID: "later", Content: "still writable"}))
		})
	}
}

func TestMemoryStoreRejectsOversizeRecord(t *testing.T) {
	const maxRecordBytes = 1 << 20
	for _, tc := range []struct {
		name string
		size int
	}{
		{name: "one-byte-below", size: maxRecordBytes - 1},
		{name: "exact-limit", size: maxRecordBytes},
		{name: "one-byte-over", size: maxRecordBytes + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, path := newTestStore(t)
			mem := memoryWithSerializedSize(t, tc.size)
			err := s.Add(context.Background(), mem)
			if tc.size <= maxRecordBytes {
				require.NoError(t, err)
				contents, readErr := os.ReadFile(path)
				require.NoError(t, readErr)
				assert.Len(t, contents, tc.size+1)
				assert.Equal(t, byte('\n'), contents[len(contents)-1])
				require.NoError(t, s.Close())
				reloaded, openErr := NewFileMemoryStore(path)
				require.NoError(t, openErr)
				t.Cleanup(func() { _ = reloaded.Close() })
				_, getErr := reloaded.Get(context.Background(), "sized")
				assert.NoError(t, getErr, "the row limit is measured excluding LF")
				return
			}

			assert.ErrorIs(t, err, ErrMemoryRecordTooLarge)
			contents, readErr := os.ReadFile(path)
			require.NoError(t, readErr)
			assert.Empty(t, contents, "an oversized row must not be appended")
			require.NoError(t, s.Close())
			encoded, marshalErr := json.Marshal(mem)
			require.NoError(t, marshalErr)
			require.NoError(t, os.WriteFile(path, append(encoded, '\n'), 0o600))
			_, openErr := NewFileMemoryStore(path)
			assert.ErrorIs(t, openErr, ErrMemoryRecordTooLarge)
		})
	}
}

func memoryWithSerializedSize(t *testing.T, size int) Memory {
	t.Helper()
	mem := Memory{
		ID:        "sized",
		CreatedAt: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	base, err := json.Marshal(mem)
	require.NoError(t, err)
	require.GreaterOrEqual(t, size, len(base))
	mem.Content = strings.Repeat("x", size-len(base))
	encoded, err := json.Marshal(mem)
	require.NoError(t, err)
	require.Len(t, encoded, size)
	return mem
}

func memoryIDs(memories []Memory) []string {
	ids := make([]string, len(memories))
	for i, mem := range memories {
		ids[i] = mem.ID
	}
	return ids
}

func sortedMemoryIDs(memories []Memory) []string {
	ids := memoryIDs(memories)
	for i := range ids {
		for j := i + 1; j < len(ids); j++ {
			if ids[j] < ids[i] {
				ids[i], ids[j] = ids[j], ids[i]
			}
		}
	}
	return ids
}
