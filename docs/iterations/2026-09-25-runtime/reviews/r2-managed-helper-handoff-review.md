# R2 managed-helper completion handoff review

## Verdict

**Conditionally accepted as a design-only handoff; not ready to implement or claim queued proof completion.** The fallback, Git preview, queue, and output-budget descriptions match the source baseline. Two lifecycle prerequisites need to be explicit: the separate queued-proof race fix must be accepted first, and a pending process-group channel must be the completion signal for that same action.

## Fixed snapshot and scope

- Reviewed document commit: `b5d9f99427f460be3c4eb9da9396b2e963837ee2`, in a clean detached worktree.
- The handoff identifies integrated source baseline `b3af60e6ba48faae0423ae711e8612a4e14ccf53` and explicitly says `PreparedInvocation.CompletionSignal()` is absent there.
- Compared the handoff to the named `internal/tools`, assembly, R4 prepared-dispatch, and R2 local-quiescence source/design contracts. No source was changed and no tests were run for this docs-only review.
- The separate tools-proof review at `1386d02ebd9d7880de43a5ffb1d0229ea5737222` records an open P1 and an ephemeral reproducer; that fix is not established by this handoff commit.

## Source-grounded behavior

The described helper routing and fallbacks are correct for the reviewed baseline:

- Grep probes `rg`, passes `--no-config --line-number -n`, optional `--glob=...`, `-e pattern -- path`, filters `RIPGREP_CONFIG_PATH`, and treats exit 1 as no matches. Other helper errors fall back to the existing Go walker. A pending/unjoined helper must stop that fallback chain.
- Find attempts `fd`, then system `find`, then Go. `fd` exit 1 is a valid empty result; other `fd` errors can advance to `find`; a completed `find` error can advance to Go. Final results are sorted and capped after collection.
- `GitDiffGenerator` is the real assembled default: assembly builds it around the selected `DefaultGitTool` and unified fallback, registers it for Write/Edit, and also injects it into the default mutation queue. The Git path uses `ensureRepo`, read-only flags/environment filtering, and `git diff --no-ext-diff --no-textconv`; nonempty successful Git output wins, while no-repo/error/empty output uses the existing unified fallback. The proposed managed proof covers only this exact first-party path; unknown custom generators remain opaque.
- The existing process runner owns stdout/stderr readers, the process leader, and process-group cleanup; its `executionDone` channel closes only after leader/readers join and positive group absence. The source documents Darwin/Linux support and the deliberate limits around escaped descendants. The handoff correctly reuses this seam and preserves a pending/unmonitorable result instead of falling through while a helper may still run.

The output-budget claims are also accurate. Grep's default 1,000-match and 1 MiB result caps are applied after `cmd.Output()` or after the Go walk has accumulated matches; ripgrep parsing has a 1 MiB scanner-token limit. Find materializes/sorts helper output before applying its 1,000-result cap. `DefaultGitTool.runReadOnly` uses unbounded `CombinedOutput`; the unified diff fallback has a line limit, but the Git diff route does not. These limits are result limits, not peak-memory bounds. The handoff correctly forbids claiming a new hard memory budget and requires drain-after-retain-cap plus differential output tests before introducing truncation.

## Findings

### R2-MH-1 [P1] Gate implementation on the queued-proof race fix

The handoff says the completion API is absent and assumes a separately owned tools-proof slice will provide it, but it does not name the known P1 prerequisite from the `1386d02` review. That finding covers both late queue attachment resurrecting proof after executor panic and `completedProof` returning a stale snapshot after invalidation. The proposed `queued_job_joined` proof and the handoff's statement that the exact accepted job remains attached to the same owner depend on proof publication being linearizable against invalidation. The parent design's generic dependency on a future API is not enough to ensure this specific defect is fixed first.

Add an explicit prerequisite that no consumer may use or persist prepared queued-completion proof until the tools-proof P1 is fixed and independently accepted. Record the actual accepted source hash when it exists; until then use an explicit pending gate and do not invent a hash or describe the proof slice as complete. Keep the P1's stale-positive regression coverage in that separate tools-proof task.

### R2-MH-2 [P2] Bind pending helper cleanup to the action completion signal

The handoff separately requires (a) returning the exact `PendingExecutionError.ExecutionDone()` channel when a helper is not yet quiescent and (b) having Core register the prepared invocation's `CompletionSignal().Done()` for durable proof. It does not state that, for this single active helper, the prepared signal must remain open and resolve to `owned_process_group_joined` only when that same process `executionDone` channel closes. If the prepared signal were finalized merely because the synchronous tool returned its pending error, R1 could continue waiting on its resource channel while R2 saw a closed or missing prepared proof; the resource wait would not produce the action-scoped persisted evidence required for local resolution.

Specify this as a channel-identity/order contract: for an unjoined managed helper, the action's prepared completion remains pending on that exact positive process-group completion; no later fallback starts. For queued Write/Edit, the existing per-job completion stays open until that same nested channel closes and the handler returns. Unknown/unmonitorable cleanup produces no proof. Because helper fallbacks are serial and a pending helper stops the chain, this needs no new generic resource list or watcher; a focused regression can assert both signals remain open before group release and resolve only after leader, readers, and group join.

## Design assessment

The design otherwise preserves the required runtime behavior: external helpers remain the default where currently selected; no silent pure-Go mode is introduced; fallback continues only after an attempt is joined; GitDiffGenerator and its queue/approval-preview injection remain intact; unknown/custom routes receive no first-party proof; an accepted queued job remains owned after the caller stops waiting; and completion evidence does not grant workspace read permission, alter effects, validate paths, or turn an error/unknown receipt into success.

The handoff appropriately avoids an independent process watcher or generic resource aggregate. It also correctly distinguishes dispatch commit from completion, reserves `queued_job_joined` for the exact accepted job, retains queue FIFO, and treats escaped process-group descendants as outside this proof rather than overstating containment.

## Validation limits and acceptance boundary

This review made no implementation/test claims. The listed acceptance scenarios are coherent with the current helper and queue seams, but they remain proposed tests. In particular, no run at `1386d02` was re-executed here, and the earlier P1 reproducer is described in its review as ephemeral rather than a maintained regression test.

Accept the handoff's route, fallback, Git, process-scope, queue, and output-budget design. Keep implementation approval conditional on (1) independent acceptance of the queued-proof late-attach/stale-snapshot fix and (2) explicit action-signal binding for delayed process-group completion. This review does not close R2, integrate a Core consumer, or authorize source edits.
