# Joint R2 / R4 Contract Review

Status: **approved for design** at `4f8d1a0d72eebc25fcf06c1fcb897d64bf73d41c`. The final joint contract closes the trusted-read path-scope and durable-resolution recovery gaps while preserving fail-closed behavior. This approves the contract/design only; it does not establish that R2/R4 code, their implementation gates, or R1 integration are complete.

## Reviewed snapshots

| Artifact | SHA256 | Snapshot status |
|---|---|---|
| `r2-contract.md` | `31ebbafc8a220e0f9c5b9f1851294a4645cae91c37e1aee21cc93a316a98b2dd` | committed at `34bf1d3ae9eeae9cc766ea1657305f65445794ed` |
| `r4-prepared-dispatch-design.md` | `e44a53c1d8eee9a6c86a5663952c4492a5a66d4afe3af0898c3bfb69e4bff32f` | committed at `34bf1d3ae9eeae9cc766ea1657305f65445794ed` |
| `execution-tasks.json` | `c005e8d4733534271ddc4f343cd496ccf174bafceefda42426bdd74e59ee64ca` | committed at `34bf1d3ae9eeae9cc766ea1657305f65445794ed` |
| `r2-source-feasibility.md` | `a13438df31cde88d01c0ef702922e6c7fbfdf929125c4b3bb1228355a415c1d2` | working-tree snapshot, not part of 34bf1d3; its stated source baseline is `ba92e45` |

Final targeted re-review at `4f8d1a0d72eebc25fcf06c1fcb897d64bf73d41c` confirms:

| Artifact | SHA256 | Snapshot status |
|---|---|---|
| `r2-contract.md` | `0a47e523ae11689e995e4fddfb19135eb328a40082e16a6aa5b2bd6de37d007c` | committed at final review commit |
| `r4-prepared-dispatch-design.md` | `3dff4e89b91bc0aa170638020d2d16cde0f9c5899208d97c3a94cbe07b7a6724` | committed at final review commit |
| `execution-tasks.json` | `8242e3713fb76901db42128d3a4fcef3f5556656ea4e8928fd0964bca52dd1f3` | worktree-only at re-review; not included in 4f8d1a0 |

`rtk git diff --check 4f8d1a0^ 4f8d1a0 -- r2-contract.md r4-prepared-dispatch-design.md` returned no whitespace errors. The final review is limited to those committed documents plus the targeted R2/R4 acceptance rows visible in the stated execution-task worktree snapshot.

The working tree contains unrelated R0/R1 evidence and design changes. I did not edit them. Source checks below are against the checked-out `codex/runtime-stabilization` tree at 34bf1d3; the older source-feasibility baseline is advisory only.

## Findings

### Closed: keep unscoped default readers out of `workspace_read`

The source does not currently enforce workspace path scope for the default read-only built-ins. `RegisterDefaults` creates `NewReadTool()` without a path whitelist (`internal/tools/builtins.go:143-150`); CLI assembly calls it without `WithRegisteredPathWhitelist` (`internal/cli/assemble.go:873-876`). `ReadTool.resolvePath` delegates to `resolveWithinWorkdir`, which accepts absolute paths unchanged (`internal/tools/read.go:206-209`, `internal/tools/write.go:66-90`). The registered grep/find/ls instances also receive no whitelist in the default assembly, and their path validator accepts absolute paths when the whitelist is empty (`internal/tools/builtins.go:125-150`, `internal/tools/write.go:107-138`).

The final R2 contract now makes path scope a separate required property of `workspace_read`, states that unscoped default `Read`/`Grep`/`Find`/`Ls` remain `unknown`, and gates promotion on R4-BLD-02. The final R4 design assigns the assembly-to-constructor-to-resolver path-scope proof to R4-BLD-02 and requires absolute-path and symlink-escape tests. This closes the design blocker without granting current unscoped instances read authority or expanding the effect framework.

### Closed: replay a durable session resolution after a global-index write failure

The final contract adds `ValidatedExecutionResolution` and `RecoveryAuthority.ApplyDurableResolutions`. Under the session lock, the loader validates the checkpoint's `EntryID`/`Seq`, complete action identity, and typed evidence before handing it to the R2-BLD-02 index adapter. The adapter holds only the global index lock, verifies the full action reference, appends and syncs the identical resolution idempotently, treats an already-identical resolution as a no-op, and fails closed on mismatch or sync error. A missing dispatch-ready marker is a no-op only for validated `never_dispatched` evidence paired with its durable matching receipt; other missing markers remain inconsistent and quarantined. Recovery then inspects marker state and does not replay tools or providers. The R2 acceptance rows add the between-sync crash test, exactly-once assertion, conflict case, and zero-replay assertion. This closes the recovery gap without adding a general recovery framework.


## Earlier findings closed in the frozen snapshot

- **Prepared executor seam:** R4 defines `ToolCallPreparer.PrepareToolCall` and one immutable `PreparedInvocation`; R2 consumes that API rather than inventing a second one. Both pin the exact executor, normalized arguments, identity/effects/schema, policy and Plan generations. Queue workers receive the captured invocation and do not rediscover/reconstruct a tool (`r4-prepared-dispatch-design.md:17-48,64-86`; `r2-contract.md:89-91`).
- **Receipt versus resource resolution:** `RecordReceipt` only commits the transcript receipt. Only a separately evidenced `ResolveExecution` can clear the global marker; a local HTTP/MCP return after a lost remote response is expressly insufficient (`r2-contract.md:68,87,95,111`).
- **Transcript coordinates:** each input and receipt has its own `TranscriptIndex`; assistant-group matching stays `(RunID, AssistantTranscriptOrdinal, ToolCallID)` (`r2-contract.md:24-30,68-69,91`).
- **Action reservation:** `ActionID` is reserved before intent append and denotes at most one executor invocation. Denial before reservation has none; a post-reservation fence/cancel keeps the same ID in a durable `not_dispatched` receipt (`r2-contract.md:26,30,68,89,95`; `r4-prepared-dispatch-design.md:72-80,109-112`).

## Additional seam checks

- **Queue dequeue and mutable policy state:** R4 requires the final direct/dequeue fence to be shared with registration replacement, approval mode/cache writes, and Plan transitions. Enqueue and executor work happen outside the fence; dequeue validates the original context and the same generations before committing. The required tests cover concurrent registry, approval-cache/mode, and Plan writers, and generation changes while queued (`r4-prepared-dispatch-design.md:74,80-86,128`). The design is adequate provided implementation routes every decision-affecting writer through that same fence; do not keep a private dispatch-only mutex.
- **Ordinary and streaming calls:** `Dispatch` and `DispatchStreaming` share one consume-once state and are mutually exclusive; the streaming proxy cannot expose a raw bypass (`r4-prepared-dispatch-design.md:60-64,80,128`).
- **Lost remote response:** receipt success/unknown and local HTTP completion do not establish remote quiescence; a trusted protocol terminal or operator reconciliation is required to clear workspace quarantine (`r2-contract.md:68,87,176,187-189`; R2-BLD-02 includes `TestUnknownRemoteReceiptWithoutTerminalEvidenceKeepsQuarantine`).
- **Branch clone and recovery:** branch Clone shares the immutable prefix without rewriting entries, action IDs, or provider call IDs; recovery has a typed authority lookup and fails closed if it is absent/unverifiable. Unsupported `flock` is explicitly an error, not a no-op (`r2-contract.md:36,146-178,200`). Reuse `core.CloneAgentMessages` only when R1's handoff is present; otherwise defer dependent code instead of duplicating a clone helper (`r2-contract.md:167,196,200`). The checked tree currently has no exported `CloneAgentMessages`; the contract correctly makes its reuse conditional and gates R2 on the R1 handoff.
- **Compaction coordinates:** cursors are based on original prior-history plus canonical run delta, and ephemeral `SystemReminder` prompt contributions are excluded from source counts through explicit provenance/separation rather than content matching (`r2-contract.md:134-140`).
- **Grep/fd read grants:** current source does not prove a read-only grant. `grepWithRipgrep` passes the regexp as a bare argument, inherits `RIPGREP_CONFIG_PATH`, and does not use `--no-config` (`internal/tools/grep.go:202-215`). `findWithFd` places the model pattern before the path without `--` (`internal/tools/find.go:169-200`). The frozen contract correctly keeps these effects unknown until argv/config controls and adversarial no-helper tests pass (`r2-contract.md:32,113`; `r4-prepared-dispatch-design.md:54-58,128`; `execution-tasks.json` R4-BLD-01 verification). No implementation evidence currently supports granting `workspace_read` to those paths.
- **Git read effects:** current `DefaultGitTool` inherits environment/repository config (`internal/tools/git.go:675-683`) and invokes status/diff/blame without helper isolation (`internal/tools/git.go:192-230,377-394`). In a disposable temp repository I reproduced a configured `.gitattributes` `filter.clean` helper for `git diff --no-ext-diff --no-textconv`, `git blame`, and status during the racy-stat content check (`core.trustctime=false`, same-size content, mtime aligned to index). Therefore these remain `unknown`, as the design requires. One acceptance example spells `git status --no-optional-locks`; that order is rejected by the installed Git. Use the global-option form `git --no-optional-locks status --porcelain` (the current Go tool uses `git status --porcelain`) so the regression proves helper execution rather than passing on an invalid command. This is a test-command correction, not a reason to relax the unknown classification.

## Final decision

The four original R2 findings and the later path-scope/recovery blockers are closed in the reviewed commit. Queue/dequeue fencing, one-use ordinary/streaming dispatch, remote-loss quarantine, shared-prefix Clone/RecoveryAuthority, unsupported flock, and compaction cursor provenance remain coherent at design level. Builders must preserve the documented gates: unscoped readers and unknown/config-controlled effects stay fail-closed; a receipt or local HTTP return cannot clear a global marker; recovery applies only a validated durable resolution and syncs before exposing a ready view.

No product tests were run. The Git helper observation used a throwaway temporary repository and did not modify the checkout.
