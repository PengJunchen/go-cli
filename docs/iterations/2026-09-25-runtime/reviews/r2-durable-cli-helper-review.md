# R2 Durable CLI Session Helper Review

## Decision

**ACCEPTED for the helper slice at `9c92ac55458fafd2e41ef3ef4ee2fe098d358cba` only.** This does not accept CLI wiring, a durable normal turn, or close R2.

The fixed commit changes only `internal/cli/durable_cli_session.go` and `internal/cli/durable_cli_session_test.go` (148 and 150 added lines). I reviewed it in the independent detached worktree `/Users/pengjunchen/Documents/github/go-cli-r2-durable-cli-helper-review-9c92ac5`; the worktree was clean at the fixed hash before review tests and after removing temporary review-only tests.

## Evidence and findings

- New sessions select the physical ID returned by the prepared handle, while resume asks the manager to reopen the exact physical ID and optional exact active branch. Tests create two real sessions, resume one by its physical ID and `main` branch, reject an unsupported branch, then reopen the session to prove the rejected selection released its prepared handle.
- `resolveWorkspaceID(os.Getwd)` is called once in the helper constructor. The resulting workspace ID is used to initialize the quarantine index and manager; the preparer receives that same index, and the run sink receives the workspace ID from the manager lease. Helper tests isolate `HOME` and `XDG_CONFIG_HOME` under `t.TempDir()` before creating the real workspace index. The package's parallel tests do not read this authority.
- `snapshotSelected` borrows the selected manager handle, takes and clones its `PreparedSessionSnapshot`, and checks physical ID and active branch. A temporary review-only test appended a real user row through that handle and passed the returned snapshot to `BuildFromSnapshot`; the tree restored the snapshot leaf. This confirms the handoff can stay on one prepared handle without reopening the store.
- `bindRun` attaches the sink and physical session/workspace binding. The core run owner captures the sink's lease capability and returns it after the run resources join. A temporary adversarial test also confirmed the edge before owner creation: if `Harness.Submit` fails its run-slot claim, manager close stays pending until the caller invokes `sink.CloseRunCommit()`. This is an integration gate for future CLI wiring; the caller must return the sink when `Submit` errors before an owner takes responsibility. The helper does not submit work, so I do not treat that caller obligation as a defect in this slice.
- Sink-construction failure explicitly returns the borrowed lease. A close request that is still waiting on leases retains the manager reference; the tests prove the physical lock remains held while close is pending and becomes resumable after the sink releases its lease.
- One bounded follow-up risk remains on the constructor error path: failed selection calls `requestClose(context.WithoutCancel(ctx))`, and `PreparedSession.Close` has no context-aware cancellation. Cleanup can therefore block beyond the original request's deadline. This is a close-owner/budget concern for later lifecycle wiring; I found no evidence in this slice that a real JSONL close loses a still-live retired handle.

## Commands and results

All commands ran from the independent detached worktree unless noted.

```text
rtk run -c 'go test ./internal/cli -list "Test(DurableCLISession|NewDurableCLISession)"'
TestDurableCLISessionCreatesPhysicalSessionAndBindsRunLease
TestDurableCLISessionResumeUsesExactPhysicalIDAndBranch
TestDurableCLISessionReleasesLeaseWhenSinkConstructionFails
TestNewDurableCLISessionRejectsMissingSelectionInputs
exit 0

rtk go test -race ./internal/cli -run 'Test(DurableCLISession|NewDurableCLISession)'
Go test: 4 passed in 1 packages
exit 0

rtk go test -race ./internal/cli
Go test: 680 passed in 1 packages
exit 0

rtk go vet ./internal/cli
no findings; exit 0

rtk git diff --check 9c92ac5^ 9c92ac5 -- internal/cli/durable_cli_session.go internal/cli/durable_cli_session_test.go
no findings; exit 0

rtk go test -race ./internal/cli -run '^TestReviewDurableCLISnapshotFeedsSnapshotTreeBuilder$'
Go test: 1 passed in 1 packages
exit 0

rtk go test -race ./internal/cli -run '^TestReviewHarnessClaimFailureLeavesBorrowToCaller$'
Go test: 1 passed in 1 packages
exit 0
```

The last two tests were temporary, review-only tests in the detached worktree; the file was removed after they passed and is not part of the reviewed delta.

## Dependency check

`d4d656f9b2d546988db54c680010ca2f2cbb37ac` is an ancestor of the fixed commit. The exact `bbfebb20884aa70eae64e4b381ad5e775fe88d5f` hash is not an ancestor, but its `internal/cli/line_editor_pty_handoff_test.go` contents match the corresponding file at `373493f` (`git diff --quiet bbfebb 373493f -- internal/cli/line_editor_pty_handoff_test.go` exited 0); `373493f` is in the fixed commit's ancestry. The helper commit is based on `373493f`.
