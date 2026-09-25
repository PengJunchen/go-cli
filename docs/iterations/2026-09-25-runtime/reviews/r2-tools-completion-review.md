# R2 tools completion proof review

Reviewed source commit `1386d02ebd9d7880de43a5ffb1d0229ea5737222` in a detached worktree. This review is limited to the approved local quiescence proof contract and the changed `internal/tools` implementation/tests. It does not assess the unconnected `runOwner` consumer or claim that the broader R2 lifecycle is closed.

## Finding

**P1 — accepted mutation can regain a completion proof after executor panic.** The prepared mutation path first hands the job to the worker and receives its `done` channel from `q.enqueue`, then attaches that channel and `CompletionQueuedJobJoined` to the invocation (`internal/tools/mutation.go:179-189`). The worker recovers executor panics, invalidates the invocation proof, and closes the job channel (`internal/tools/mutation.go:276-289`). Since attachment can occur after worker dispatch, this interleaving is possible:

1. The worker starts the accepted job and the prepared executor panics.
2. `invalidateProof` clears the current proof while no queue `done` channel has yet been attached.
3. The worker closes the queue `done` channel.
4. The enqueueing goroutine resumes and attaches `CompletionQueuedJobJoined` to the already-closed channel.

`attach` currently accepts that late attachment and assigns the proof (`internal/tools/prepared_completion.go:149-190`); invalidation only clears the current proof and records no sticky invalidated state (`internal/tools/prepared_completion.go:206-213`). `completedProof` then sees a closed channel and returns the resurrected proof. A false positive here can tell a future quiescence consumer that execution joined even though the executor pipeline panicked.

An ephemeral direct regression probe performed `invalidateProof`, closed the job channel, then attached the queued-job proof. It failed against this commit as expected: `completedProof()` returned `queued_job_joined`. The probe file was removed after the run; it is not a maintained test.

There is a second race in the same proof state that must be addressed with this fix: `completedProof` snapshots `done` and `proof` under `RLock`, releases the lock, then selects on the channel (`internal/tools/prepared_completion.go:87-103`). If invalidation clears the canonical proof and closes `done` after the snapshot but before that select, the caller returns the stale non-empty snapshot. Sticky invalidation alone does not prevent this read-after-invalidation. Make the closed-channel observation and proof read linearizable with invalidation (for example, synchronize the nonblocking close check with the state lock, or re-read the canonical proof under lock after observing closure).

Recommended correction: record invalidation monotonically; allow a late queue attachment to preserve the accepted job's completion channel for the owner's wait/handoff, but never restore a proof once invalidated; and make `completedProof` observe only the canonical proof associated with a completed channel in a way ordered against invalidation. This must preserve the owner's pre-dispatch handle and continuous handoff contract: do not drop the accepted job's `done` signal merely because proof eligibility was revoked.

## Validation

- `rtk env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb go test -race -p 2 -count=1 ./internal/tools` passed at the reviewed commit.
- The ephemeral late-attach regression failed with the stale positive proof described above.
- The review worktree was left clean at the reviewed commit; no source changes or commits were made.

## Scope limits

The `internal/tools` race suite and the direct state probe cover this source slice. This review does not validate process sandboxing, OS-level containment, a run-owner timeout consumer, the default CLI Bash completion chain, or full R2/R5/R8 phase closure.

## Follow-up review: proof invalidation fix

Reviewed follow-up commit `5d6fcdd6e9ee9345180cd07fa089f6ff3fc010f6`, whose parent is the reviewed `1386d02`. The fix is accepted for the finding above. It changes only `internal/tools/prepared_completion.go` and `internal/tools/prepared_completion_test.go`.

The invalidation bit is now sticky. Late queue attachment still stores the accepted job's `done` channel for owner waiting, but does not restore a proof after invalidation (`internal/tools/prepared_completion.go:203-206`). Failed inline/process completion and queue panic all mark invalidation before the corresponding owned completion signal can be observed as complete (`internal/tools/prepared_completion.go:144-161`, `223-230`). `completedProof` now delegates its initial snapshot to a helper that rechecks invalidation, channel identity, and the canonical proof while holding the read lock across the nonblocking close check (`internal/tools/prepared_completion.go:88-112`). This orders proof observation against invalidation and closes both reported races while preserving the done handle.

The two new regression tests are meaningful RED tests. To account for the extracted `proofFromCompletedSnapshot` helper not existing on the old source, I temporarily restored the pre-fix snapshot/close-check behavior in that helper, unconditional proof assignment in late `attach`, and non-sticky `invalidateProof` in the review checkout. Both tests compiled and failed on assertions with `queued_job_joined`; this was a behavioral RED check, not a compile failure. I then restored the checkout to `5d6fcdd` and confirmed it was clean.

At the fixed commit, `rtk env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb go test -race -p 2 -count=1 ./internal/tools` passed (`41.440s`). No further blocker was found in this two-file follow-up scope. This acceptance covers the proof-state repair only; it does not change the original scope limits above.
