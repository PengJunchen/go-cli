# R2 managed Grep/Find helper review

## Verdict

The fixed implementation correctly moves `rg`, `fd`, and system `find` onto the managed local process-group runner. Normal helper failure can fall back only after the group/readers have joined; cancellation and a pending cleanup stop the fallback chain. Two gaps remain before accepting the handoff's full completion-proof claim: a forced pipe close can hide a still-running escaped writer, and `RegisterDefaults` does not wire Grep/Find to the prepared completion reporter.

## Reviewed snapshot and validation

- Source: `2bf4c0971847fa2782b738a1a51033477ce3ec37`, parent `84324fe7a55ea852583f5082fe5d76ac0dd0608a`, in a clean detached worktree at `/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/r2-managed-search-review`.
- Design: the managed-helper completion handoff at `b5d9f99427f460be3c4eb9da9396b2e963837ee2` and its resource-completion amendment at `a52952b`; the handoff's code baseline predates the reviewed implementation.
- Runtime: Darwin arm64. The Unix lifecycle tests ran here; no Linux runtime claim is made.
- Exact test listing found `TestManagedSearchFallbackStopsForPendingOrCanceledHelper`, `TestGrepJoinsFailedHelperDescendantBeforeFallback`, and `TestFindJoinsFDFailedHelperDescendantBeforeSystemFindFallback`.
- `rtk env GOMAXPROCS=2 go test -race -p 2 ./internal/tools -run '^Test(ManagedSearchFallbackStopsForPendingOrCanceledHelper|GrepJoinsFailedHelperDescendantBeforeFallback|FindJoinsFDFailedHelperDescendantBeforeSystemFindFallback)$' -count=1` passed.
- Exact lifecycle/completion test listing found the pending-quiescence, descriptor-close, delayed-reader, forced-reader-close, sticky-invalidation, and Bash wrapper-join cases. Their targeted race run passed.
- `rtk env GOMAXPROCS=2 go test -race -p 2 ./internal/tools` passed (`40.533s`).
- A temporary, untracked Darwin probe was removed after use. Its fake `rg` started a `setsid` child that retained the inherited output pipe. Grep returned through its pure-Go fallback while `kill(pid, 0)` still succeeded for that child, and the completion reporter had already received a closed `owned_process_group_joined` channel. The probe's cleanup sent SIGKILL; a follow-up `ps` found no remaining process.

## Findings

### R2-MH-3 [P1] Forced pipe close can report completion while an escaped writer is alive

`managedLocalCommand.collect` records `forcedPipeClose`, but computes `quiescent` from group absence, leader join, and reader-goroutine join without considering that the owner had to close the read descriptors before EOF (`internal/tools/process_scope.go:125-160`). `managedSearchLifecycleError` then turns the resulting reader error into an ordinary unknown error; `shouldFallbackAfterManagedHelper` permits fallback for that error (`internal/tools/managed_search_command.go:56-84`). Finally, `reportPreparedLocalProcessCompletion` treats `quiescent` alone as a completed group signal (`internal/tools/prepared_completion.go:263-275`).

The temporary probe reproduced this path on the reviewed snapshot: a helper spawned a child in a new session, the child retained stdout, and the helper leader exited. The group check saw the original group absent; the reader was forcibly closed and joined; Grep ran the Go fallback and reported a closed group-completion channel while the escaped child remained alive. The positive proof is narrowly named for the original process group, so this does not show that group-membership checking is wrong. It does show that the source's forced-close path erases observable evidence of a writer outside that group and then permits continuation. The handoff requires an unjoined/unmonitorable helper to stop fallback and withhold proof. Resolve the contract explicitly: either treat forced close before EOF as unproved containment and stop/fail closed, or narrow the accepted guarantee and tests so they do not imply all started-helper resources have joined. The existing `TestManagedLocalCommandForcedCloseReaderErrorIsQuiescent` currently asserts the broad interpretation and should be considered alongside this case.

### R2-MH-4 [P2] Default prepared Grep/Find calls never install the reporter

`RegisterDefaults` creates Grep and Find definitions, but `registerDefaultTool` only uses special trusted registrations for Write/Edit and Bash; Grep/Find go through ordinary `reg.Register` (`internal/tools/builtins.go:205-216`). Ordinary `registerDefinition` assigns an empty completion proof (`internal/tools/trusted_registry.go:284-288`). `preparedInvocation.commitAndInvoke` adds the private completion reporter only when registration metadata says `CompletionOwnedProcessGroupJoined` (`internal/tools/prepared.go:722-735`). Therefore `reportPreparedLocalProcessCompletion` is inert for the actual default prepared Grep/Find path. The lifecycle tests inject `withPreparedCompletionReporter` directly, so they validate the local callback but not a real prepared invocation's signal.

If this handoff includes action-scoped proof for default Grep/Find, add a narrowly reviewed first-party registration and a real `RegisterDefaults` → `PrepareToolCall` → `Dispatch` regression. If this slice only promises local process cleanup, record the prepared-proof wiring as deferred and do not claim that default calls emit that proof. Ordinary/custom registrations correctly remain without process-group authority.

## Confirmed behavior and limits

- The new runner uses plain `exec.Command`, not `exec.CommandContext`; cancellation is supervised through the derived context and the owned process group, so there is no competing `CommandContext` goroutine that kills only the leader.
- `configureCommandProcessGroup` requires a fresh group and rejects a nonzero caller-provided `Pgid`. `collect` drains stdout and stderr concurrently, joins the leader/readers, and closes owned read descriptors.
- Grep preserves its `rg` arguments and `RIPGREP_CONFIG_PATH` filtering, treats exit 1 as no matches, and only runs the Go fallback after a completed ordinary helper failure. Find preserves `fd` → `find` → Go, accepts `fd` exit 1 as empty, and retains final sorting/capping.
- A missing executable or failed `Start` has `started=false`; the caller may use the existing fallback because no child was started. Unsupported platforms reject process-group setup before `Start`; the pure-Go fallback can still run, but it publishes no helper completion report.
- The containment is process-group scoped, not a process-tree sandbox. It cannot prove escaped `setsid`/`setpgid` work stopped, and this review does not establish binary provenance for `rg`, `fd`, or `find` resolved through `PATH`. Custom registrations receive no prepared proof by default.
- The full package race test passed on Darwin. No Linux runtime evidence or unsupported-OS runtime test was run in this review.

## Review boundary

No product source was changed. The detached review worktree was clean after removing the temporary probe; this report is an untracked addition in the shared main worktree and was not staged.
