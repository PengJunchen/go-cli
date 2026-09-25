# R2 CLI durable session manager review

Reviewed fixed snapshot: `198fdbe30bd2e06689578ffbb84e10827adbc607` (`internal/cli/durable_session_manager.go` and `durable_session_manager_test.go` only).

## Findings — changes required

1. **P1: Close is neither deadline-bounded nor immediately effective while a selection transition is in progress.** `prepareNew`, `selectResume`, and `reopenPoisoned` hold `manager.selection` across preparation/recovery and, for replacement, the old-session lease drain. `requestClose(ctx)` first waits on the same ordinary `sync.Mutex` and sets `manager.closing` only after acquiring it (`durable_session_manager.go:121-149, 256-273, 286-289, 340-362`). A canceled/deadline context cannot interrupt `Mutex.Lock`; until the selector releases the mutex, `acquireRunLease` can continue admitting work because `closing` is still false.

   **Reproducer:** on this fixed tree, a temporary package-internal test was injected with `go test -overlay` (no repository test/source file was changed). It prepared A, held an A lease, gated `PrepareResume(B)`, let `selectResume(B)` publish B and block draining A, then called `requestClose` with a 30 ms deadline. The reproducer observed the deadline expire while `requestClose` remained blocked, and a subsequent `acquireRunLease` succeeded on B. The command below **passes because it asserts this current bug**; it is not evidence of a fix:

   ```text
   rtk env GOMAXPROCS=2 go test -overlay=/tmp/r2_manager_review_overlay.json -p 2 -race -count=1 -run '^TestReviewRequestCloseDeadlineWhileSelectionDrainsOldLease$' -v ./internal/cli
   ```

   Close intent should be recorded under the short state mutex before waiting for transition serialization, so new leases fail immediately. Transition serialization also needs a context-aware gate (or an equivalent state-machine approach), so an expired close request cannot wait indefinitely. An in-flight prepare must recheck the close state before publishing; if closing has started, it must close the unpublished prepared handle and retain/report any cleanup failure rather than leak it.

2. **P1: A selection error can mean the new selection already took effect, but the method result does not say so.** `installPreparedSession` sets `manager.current = slot` before waiting for old leases/closing the old handle, then returns the old handle's pending or close error (`durable_session_manager.go:340-362`). The existing `TestDurableSessionManagerSwapDrainsOldLeaseOnRequesterPath` demonstrates the observable contract: `selectResume(B)` returns `ErrDurableSessionClosePending`, while a subsequent lease succeeds on B (`durable_session_manager_test.go:144-164`). A caller that treats any returned error as “selection failed; A is still selected” can misreport state or take the wrong cleanup path. Preserve the intended publish-before-drain order, but expose the outcome explicitly, for example with a typed selection-applied/old-cleanup error or a result that says B is selected while A cleanup remains pending. Do not roll back a target after it has been published or leased.

## Verified behavior

- Workspace identity is derived only after `Getwd`, `Abs`, `Clean`, and `EvalSymlinks` succeed; the digest is domain-separated and there is no fallback root (`durable_session_manager.go:38-64`).
- A same-ID/same-branch resume reuses the held handle, and a failed different-ID preparation leaves the old selection available. Identity, active branch, and poison checks run before a lease is issued.
- Leases pin the exact prepared handle and physical ID/branch. `CloseRunCommit` is idempotent and only returns the borrow; close-requested handles reject leases once `closing`/`closeReq` is set.
- A lease timeout leaves the retired handle tracked for a later close request. `PreparedSession.Close` errors are returned and retained; the manager does not call `Close` a second time after it has recorded the close result. Poisoned state requires reopening the exact physical ID before another lease.

## Validation

I enumerated and independently ran these 10 fixed-snapshot tests with `-race`; they all passed:

1. `TestResolveWorkspaceIDUsesCanonicalSymlinkTarget`
2. `TestResolveWorkspaceIDFailsClosedOnGetwdAndCanonicalizationErrors`
3. `TestDurableSessionManagerSameIDSameBranchDoesNotPrepareResume`
4. `TestDurableSessionManagerRejectsUnsupportedBranchWithoutReopeningSameFile`
5. `TestDurableSessionManagerFailedDifferentSessionPreparationKeepsCurrent`
6. `TestDurableSessionManagerRejectsNonActiveBranchOnNewPhysicalSession`
7. `TestDurableSessionManagerSwapDrainsOldLeaseOnRequesterPath`
8. `TestDurableSessionManagerCloseRequestRejectsNewLeasesAndReturnsCloseError`
9. `TestDurableSessionManagerWaitsForLeasesBeforeClosingOnRequesterPath`
10. `TestDurableSessionManagerPoisonRequiresExactReopenBeforeNextLease`

The target command was `rtk env GOMAXPROCS=2 go test -p 2 -race -count=1 -run '^(TestResolveWorkspaceIDUsesCanonicalSymlinkTarget|TestResolveWorkspaceIDFailsClosedOnGetwdAndCanonicalizationErrors|TestDurableSessionManagerSameIDSameBranchDoesNotPrepareResume|TestDurableSessionManagerRejectsUnsupportedBranchWithoutReopeningSameFile|TestDurableSessionManagerFailedDifferentSessionPreparationKeepsCurrent|TestDurableSessionManagerRejectsNonActiveBranchOnNewPhysicalSession|TestDurableSessionManagerSwapDrainsOldLeaseOnRequesterPath|TestDurableSessionManagerCloseRequestRejectsNewLeasesAndReturnsCloseError|TestDurableSessionManagerWaitsForLeasesBeforeClosingOnRequesterPath|TestDurableSessionManagerPoisonRequiresExactReopenBeforeNextLease)$' ./internal/cli`; it passed. The additional overlay reproducer above passed as a **bug reproduction**, not a repair test. No full CLI suite was repeated.

This review covers only the manager seam at the fixed commit. It does not establish prompt/REPL wiring, command shutdown propagation, or restart-safety acceptance.

**Verdict: reject `198fdbe30bd2e06689578ffbb84e10827adbc607` pending the two findings above.**

## Fixed follow-up review — `2fcd1bb67d7ffb32b6aaec94011750bf8ce9f340`

I reviewed the detached, clean snapshot at `2fcd1bb67d7ffb32b6aaec94011750bf8ce9f340`; the product delta from `198fdbe30bd2e06689578ffbb84e10827adbc607` is limited to `internal/cli/durable_session_manager.go` and its tests. Both original findings are closed:

- The ordinary selection mutex is replaced by a context-aware transition gate. `requestClose` records `closing` under the short state mutex before waiting for the gate, so new leases are rejected immediately and gate waits honor the caller deadline. Run-lease acquisition pins the handle before taking `Snapshot`, releases the manager mutex during that call, and rechecks closing/current/identity before issuing the lease. A close request therefore cannot close the handle during a blocked snapshot, and the temporary pin is released on every result path.
- A prepared target that returns after close admission is not published. It is first retained in the manager's retired set, then closed on the preparing transition path; close failures remain observable and retained. This also covers reopen paths that lose their slot while preparing.
- If the new selection was published but old-handle cleanup fails, callers now receive `*SelectionAppliedCleanupError` with the selected physical session and branch plus an unwrap-able cause. Failed preparation remains an ordinary error. The regression checks both typed result and that a subsequent lease observes the selected target.

The context deadline bounds waiting to enter a transition and waiting for leases. `PreparedSession.Close` has no context-taking API, so the manager cannot interrupt a `Close` call that has already started; the implementation documents this limitation and retains the handle/result rather than claiming a hard deadline. I found no new blocker in the changed paths.

I enumerated the workspace-identity and manager tests, then ran all 14 against the fixed snapshot with race detection; they passed:

```text
rtk env GOMAXPROCS=2 go test -p 2 -race -count=1 -run '^(TestResolveWorkspaceIDUsesCanonicalSymlinkTarget|TestResolveWorkspaceIDFailsClosedOnGetwdAndCanonicalizationErrors|TestDurableSessionManager.*)$' ./internal/cli
ok github.com/pengjunchen/go-cli/internal/cli 1.170s
```

**Delta verdict: accept `2fcd1bb67d7ffb32b6aaec94011750bf8ce9f340` for the manager seam; this does not approve prompt/REPL wiring or command-shutdown integration.**
