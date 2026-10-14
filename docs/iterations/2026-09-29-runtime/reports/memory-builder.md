# R8-BLD02 Memory JSONL builder report

## Scope and baseline

This source slice keeps the existing manual `/memory` JSONL behavior selected by the product-capability design. The contract was read from the root checkout's `docs/iterations/2026-09-29-runtime/product-capability-design.md`; that file was an untracked root-checkout input and was not copied or modified in this worktree. Automatic extraction and prompt injection, process-restart acceptance, and CLI/core changes remain outside this slice.

The isolated worktree is `/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/repair-20260929-memory`, branch `codex/repair-20260929-memory`, created from full baseline `182e1bf9271b92e6cbca518e9f0d41408d82bfce`. The root checkout's documentation changes were left untouched.

## Behavior and implementation

- `Add` now rejects serialized JSON rows above 1 MiB (LF excluded), writes the entire JSONL record, and requires `Sync` before publishing to the in-memory map or search index. It records the original file size and attempts `Truncate` plus `Sync` after append or sync failure. If either rollback step fails, it returns `*MemoryStorePoisonedError`, retains the old in-memory state, and rejects later writes without reopening the uncertain file.
- The reader permits up to 1 MiB of JSON plus one LF delimiter. Tests cover serialized rows of 1 MiB−1, exactly 1 MiB, and 1 MiB+1; exact-limit rows reload successfully.
- `Delete` builds a candidate map and index, writes the complete temporary file, syncs it, and closes the current append handle before replacement. Any reported pre-replacement failure keeps the old in-memory state and restores the old append handle when possible. Once replacement succeeds, it commits the new map and index before reopening. Reopen failure keeps the deleted memory deleted in memory and on disk, records an unavailable state, and rejects later writes.
- Closed stores reject `Delete` with `ErrMemoryStoreClosed`; `Close` remains idempotent. The per-store private I/O seam covers temp creation, open, write, sync, truncate, and rename without global mutable hooks.

## RED and GREEN evidence

The neutral per-store I/O seam was added first and the unchanged Memory package passed: `rtk go test ./internal/memory` → **39 passed**.

The behavior tests then compiled and ran against the unfixed implementation. The focused RED command exited 1 with runtime assertions (not missing-symbol compilation failures):

```text
rtk proxy go test ./internal/memory -run 'TestMemoryStore(AppendFailureDoesNotPublishMemory|AppendCompletesShortWrites|AppendFailurePoisonsWhenRollbackIsUncertain|DeleteReopenFailureConsistent|DeleteClosedStoreReturnsError|DeleteSyncsTempBeforeRename|RejectsOversizeRecord)$' -count=1
```

The condensed test result was **1 passed, 11 failed**. Representative failures were: partial append changed the file bytes and lacked rollback sync; a sync failure returned nil; short write left an unreadable row; failed post-rename reopen restored the deleted ID in memory while the replaced file omitted it; closed `Delete` went through the rewrite path and returned an invalid-file error; temp rename occurred without a preceding sync; an exact 1 MiB row failed reload because the scanner limit did not account for LF; and a 1 MiB+1 row was appended.

After the implementation, the focused tests passed: `rtk go test ./internal/memory -run 'TestMemoryStore(AppendFailureDoesNotPublishMemory|AppendCompletesShortWrites|AppendFailurePoisonsWhenRollbackIsUncertain|DeleteReopenFailureConsistent|DeleteClosedStoreReturnsError|DeleteSyncsTempBeforeRename|DeletePreRenameFailurePreservesOldState|RejectsOversizeRecord)$' -count=1` → **18 passed**.

## Final scoped verification

- Focused Memory race cases: `rtk go test -race ./internal/memory -run 'TestMemoryStore(AppendFailureDoesNotPublishMemory|AppendCompletesShortWrites|AppendFailurePoisonsWhenRollbackIsUncertain|DeleteReopenFailureConsistent|DeleteClosedStoreReturnsError|DeleteSyncsTempBeforeRename|DeletePreRenameFailurePreservesOldState|RejectsOversizeRecord)$' -count=1` → **18 passed**.
- Full Memory package race run: `rtk go test -race ./internal/memory -count=1` → **57 passed**.
- Related CLI `/memory` and context dispatch tests: `rtk go test -race ./internal/cli -run 'TestSlashMemory|TestSlashContextDispatch' -count=1` → **17 passed**.
- `rtk go vet ./internal/memory` and `rtk go vet ./internal/cli` → **passed**.
- Serial scoped lint: `rtk golangci-lint run --concurrency=1 ./internal/memory ./internal/cli` → **No issues found**.
- `rtk gofmt -d internal/memory/file_store.go internal/memory/file_store_failure_test.go` and `rtk git diff --check` → **clean**.

These checks cover the Memory package and the selected CLI callers only; no full-repository test or production run was performed. The reload tests close and reopen the JSONL store in-process; they do not claim an actual process-restart acceptance run. `Sync` is used for appended rows and replacement temp files, but no parent-directory sync or power-loss guarantee was added. Windows runtime tests were not run. The old append handle is closed before rename because Go's Windows `syscall.Open` uses read/write sharing without delete sharing ([Go source](https://go.dev/src/syscall/syscall_windows.go#L395)); Go also documents that `os.Rename` is not guaranteed atomic on non-Unix platforms ([Go source](https://go.dev/src/os/file.go#L435)).

## Changed files

- `internal/memory/file_store.go`
- `internal/memory/file_store_failure_test.go`
- `reports/memory-builder.md`
