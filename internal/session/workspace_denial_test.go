package session

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWorkspaceDefiniteNoIORequiresActualCleanPreAppendDenial(t *testing.T) {
	for _, mode := range []string{"clean", "sync-error", "foreign-quarantine-error", "unlock-error", "canceled", "cancel-after-sync", "already-exists", "conflicting-marker", "append-error", "append-sync-error"} {
		t.Run(mode, func(t *testing.T) {
			index := newWorkspaceQuarantineIndex(t.TempDir(), workspaceIndexIO{})
			workspace := "workspace-denial-test"
			require.NoError(t, index.EnsureWorkspace(t.Context(), workspace))
			old := workspaceTestBatch(workspace, "original-session", "original-run", "original-batch", "original-action", 0, "original-call")
			appendMode := mode == "append-error" || mode == "append-sync-error"
			if !appendMode {
				state, err := index.BeginDispatchBatch(t.Context(), old)
				require.NoError(t, err)
				require.Equal(t, WorkspaceDispatchCommittedNow, state)
			}
			next := workspaceTestBatch(workspace, "fresh-session", "fresh-run", "fresh-batch", "fresh-action", 0, "fresh-call")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			injected := errors.New("injected uncertain index I/O")
			switch mode {
			case "sync-error":
				index.io.sync = func(*os.File) error { return injected }
			case "foreign-quarantine-error":
				index.io.sync = func(*os.File) error { return ErrWorkspaceQuarantined }
			case "unlock-error":
				index.io.closeLock = func(file *os.File) error { return errors.Join(closeWorkspaceLock(file), injected) }
			case "canceled":
				cancel()
			case "cancel-after-sync":
				index.io.sync = func(file *os.File) error { err := file.Sync(); cancel(); return err }
			case "already-exists":
				next = old
			case "conflicting-marker":
				next = old
				next.Actions[0].Action.ToolCallID = "other-call"
			case "append-error":
				index.io.write = func(*os.File, []byte) (int, error) { return 0, ErrWorkspaceQuarantined }
			case "append-sync-error":
				calls := 0
				index.io.sync = func(file *os.File) error {
					calls++
					err := file.Sync()
					if calls > 1 {
						return errors.Join(err, ErrWorkspaceQuarantined)
					}
					return err
				}
			}
			result, noIO, err := index.beginDispatchBatch(ctx, next)
			if mode == "clean" {
				require.ErrorIs(t, err, ErrWorkspaceQuarantined)
				require.True(t, noIO)
				require.Equal(t, WorkspaceDispatchNotCommitted, result)
			} else {
				require.False(t, noIO, "generic/borrowed errors, replay, conflict, cancellation and uncertain close must not mint a denial fact")
				if mode == "already-exists" {
					require.NoError(t, err)
					require.Equal(t, WorkspaceDispatchAlreadyExists, result)
				} else {
					require.Error(t, err)
				}
			}
		})
	}
}
