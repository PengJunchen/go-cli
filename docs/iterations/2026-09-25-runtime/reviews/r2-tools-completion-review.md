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
