# Independent review: verification lint cleanup

Artifact: `/Users/pengjunchen/Documents/github/go-cli/docs/iterations/2026-09-25-runtime/reviews/verification-lint-review.md`

Status: **pass** for the behavior reviewed at source commit `9ce80bd9c254aea963357c46237a5daecf20bcf8` (`fix(lint): clear verification source findings`). The detached review worktree was clean. The corresponding integrated change `c776d276ae6bb45c0ddb5e86242c407a53d4a101` has the same stable patch ID as the source commit. This reviewer changed no product files or tests and added only this report; pre-existing and concurrent worktree artifacts were left untouched.

## Source review

`InMemoryApprovalStore.publishUpdate` removes a `for` loop whose branches all returned and which had no retry path. The update and error behavior are unchanged. With a fence, it still releases `s.mu` before calling `DispatchFence`; the publication callback takes `s.mu` while the fence lock is held, preserving the fence-to-store order used by prepared grants. No path holds the store lock while waiting on the fence. The approval generation and sibling-permit regressions remain covered.

`invokeRunCallback` only changes the return tuple order. The recovery defer still replaces a panic with `runCallbackPanicError` and sets `panicked=true`; `runOwner.run` still treats panic as an unknown outcome and quarantines the claim. The persisted `RunOutcomeCancelled` string is unchanged; the new `nolint` comment only documents its wire spelling.

The removed `break` statements were inside `select` receive arms in `LoopAgent.streamGenerateWithBudget`, the CLI output-guard wrapper, loop detection, and overflow recovery. A `break` there exits only the `select`; the unchanged `if !ok { break }` immediately after each `select` exits the receive loop. Closed-input handling and the existing final-chunk/cancellation checks therefore remain in force. New closed-stream regressions exercise the relevant paths.

Session directory descriptors in `syncDirectory`, session listing/recent-session scans, and `openSessionFileNoFollow` are read-only cleanup handles; ignoring their `Close` result does not discard a pending write acknowledgment. The actual directory `Sync`, `ReadDir`, and open errors remain returned. Workspace-index repair uses an `O_RDWR` file: `readWorkspaceIndexFile` now joins `file.Close()` with the operation result, and the production locked-index path joins the repaired file close with lock cleanup. Repair writes still propagate write, truncate, and sync failures. `durable_recovery.go` drops only an unused local accumulator; `plan.resolutions` is still populated and consumed.

Deleting the unused `DefaultExtensionRegistry.commitFrom` wrapper leaves the manager's publication path intact. `runInitOwner` takes `staging.mu`, then `parent.mu`, then the shared commit gate and coordinator lock; it checks cancellation/closing while those locks are held and calls `commitFromLocked`. That function checks conflicts across all registry categories before changing the parent maps, so no partial merge is exposed. User callbacks still run outside those locks. Lifecycle tests cover conflict rollback, close-gate ordering, callback once-only behavior, reverse shutdown, and late-init cleanup.

`errProcessContainmentUnsupported` moved from the common process-scope file into the `!darwin && !linux` implementation file, where its only callers are the unsupported stubs. The supported Darwin/Linux implementation does not depend on that sentinel. The FreeBSD build check below verifies the unsupported build-tag path.

In `model_resolver_test.go`, static canary values are replaced by per-test `T.TempDir`-derived runtime values. Error redaction assertions compare against those values, and request/auth-origin checks remain. The malicious redirect callback still reinjects the canary Authorization header and rewrites the request URL; the test still requires one first-origin request, zero second-origin requests, and no second-origin Authorization. Its gosec suppression is local to that deliberate test action and states the redirect-origin rationale. No security assertion was removed or weakened.

The remaining test-only changes are import grouping, a context-first fixture helper signature with all callers updated, and stream-closure regressions. Review of the complete patch found no removed assertions or broadened security exemptions.

## Independent verification

On Go `go1.26.4 darwin/arm64`, I ran the exact targeted race filter over seven packages. It selected 67 named tests: 8 approval/fence tests, 2 core callback/stream tests, 3 CLI output-guard tests, 21 LLM stream and credential-origin tests, 13 session JSONL/workspace-index tests, 17 extension lifecycle tests, and 3 gate-capture tests. All seven packages passed.

The 6 local process-scope/quiescence tests also passed with `-race`: `TestNewLocalProcessScopeRejectsJoiningExternalProcessGroup`, both `TestPendingLocalExecution...` cases, and the three `TestManagedLocalCommand...` cases. Separately, `GOOS=freebsd GOARCH=amd64 go test -c ./internal/tools` compiled successfully; this is compile evidence for the unsupported stub, not FreeBSD runtime evidence.

`go vet` passed for `internal/approval`, `internal/cli`, `internal/config`, `internal/core`, `internal/extension`, `internal/llm`, `internal/session`, `internal/tools`, and `internal/verify/cmd/gatecapture`. `git diff --check 9ce80bd^ 9ce80bd` was clean.

The Makefile-wide gate was not rerun because another owner is handling that gate. A synthetic file-close failure was not injected; close propagation was verified from the named-return `errors.Join` path and the production locked-index defer. FreeBSD was compile-checked only.

## Disposition

No blocking findings. The reviewed patch preserves the inspected behavior and test intent, and is acceptable for this review scope. This approval covers the fixed lint commit and the listed targeted evidence; it does not replace the separate Makefile-wide gate.
