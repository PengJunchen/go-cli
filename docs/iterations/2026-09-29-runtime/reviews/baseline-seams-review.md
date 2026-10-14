# Baseline seams review: Assembly, scoped read, and transcript DTO

Date: 2026-09-29  
Reviewed baseline: `17ab7b76ef0b7636abff1e5a055de7acf6f6bb31`  
Review worktree: `/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/repair-20260929-seams-review` (detached)  
Scope: Assembly commits `7646b10..cbc9971`, scoped-read `a3fb465`, and DTO/adapter `52a918b` / `42e294d`.

## Decision

No P1 was found in these three source slices. R2 CLI wiring can proceed. The R7 local Assembly shutdown path now provides the guard and ACP admission fences, waits for local producers, and retains dependencies on unknown completion. R4 scoped-read metadata remains tied to each concrete reader's captured whitelist. R5 projects exact durable source facts into a detached value DTO. The DTO remains a standalone projection; this review does not claim prompt/REPL hydration, TUI update integration, or PTY coverage.

One non-blocking R7 P2 remains: `Close` fences the run guard and owned ACP server synchronously, but invokes `requestStopSources` only after starting the asynchronous close-owner goroutine. Until that owner runs, `assemblyMCPResources.Reload` can still pass its `closing` check. Downstream cleanup waits for the stop/join sequence, so this is not a release-before-join finding and does not block R2; it is a small mismatch with the handoff's close-at-initiation request-source fence. Consider publishing the MCP `closing` bit synchronously, then invoking potentially blocking `RequestStop` callbacks on the owner.

## R7 Assembly close

**Conclusion: no P1; proceed with R2 while preserving the close hook boundary.**

- `assemblyCloseOwner.Close` cancels the Assembly lifetime and calls `DefaultRunSlotGuard.CloseAdmission` before it starts any potentially blocking close callback (`internal/cli/assembly_lifecycle.go:58-120`). The guard serializes claim publication, close, and final release under one mutex; its stable idle signal includes a registered wrapper lease (`internal/core/run_slot.go:143-180,190-225,275-348`). A claim accepted before the fence remains active until the real owner and wrapper release; later claims wake and fail closed.
- When an ACP server is registered, `Close` synchronously seals its admission before starting the close owner. `Start` and sealing serialize under the server mutex. The generation tracks accepted HTTP requests and detached dispatches; the close owner waits for `Serve`, session cleanup, requests, dispatches, and `Start` completion before publishing `closeDone` (`internal/acp/http_server.go:294-388,391-481,484-497,735-766`). Assembly consumes the server's exact pending completion channel before continuing teardown (`internal/cli/assembly_lifecycle.go:224-255`). This closes the server start/admission race in the reviewed local path.
- Assembly waits for server completion and the shared run-slot idle signal before selector detach. It then joins hot reloaders and `PluginManager` shutdown through `WaitShutdown`, closes dependent clients, runs cleanup, flushes/closes the legacy session store, joins memory extraction, and only then closes memory/event resources and disposes models (`internal/cli/assembly_lifecycle.go:502-620`). A missing completion seam, unknown completion, or cleanup panic leaves the owner pending and prevents later dependency teardown.
- The previous REPL flush-ticker gap is closed in this baseline: `stopFlushTicker` signals and waits on the actual `flushExited` channel, and the Assembly close hook invokes it before resource teardown (`internal/cli/repl_session.go:302-315,489-548`).
- R2 still owns the durable-session integration. `durableCLISession` is explicitly not connected to prompt/REPL lifecycle in this snapshot (`internal/cli/durable_cli_session.go:15-20`); its `requestClose` must be wired through the existing post-owner-quiescence `beforeSessionClose` hook or an equivalent Assembly-owned step. The existing run lease remains returned by the run-commit sink, before physical manager close.

### P2 — MCP reload admission closes on the asynchronous owner

The synchronous portion of `Close` fences the run guard and ACP server, then starts `o.start` in a goroutine (`internal/cli/assembly_lifecycle.go:85-120`). That goroutine calls `requestStopSources` (`:156-187`); only then does `requestStopReloaders` set `assemblyMCPResources.closing` (`:493-500,382-399`). `Reload` admits work while that bit is false (`:401-418`). Therefore a concurrent reload can be admitted after `Close` begins but before the close owner gets scheduled. The close owner does not release the reloader's dependencies before its later wait/join, so the observed gap permits late work but does not show premature teardown. This is a follow-up to make the request-source fence linearize at close initiation.

The detached run-slot and HTTP tests exercise claim/close races, blocked-claim wakeup, accepted-run drain, HTTP start/seal, and joined dispatch workers. They passed in the target race selection below. The known command-root/exporter close ordering issue remains outside this local Assembly verdict and is not used as a rejection here.

## R4 scoped workspace read

**Conclusion: accepted for the reviewed registration/identity slice; no finding.**

- `NewPathWhitelist` builds a fresh cleaned slice and records invalid construction separately. `IsAllowed` denies every target for an invalid non-empty scope, while an intentionally empty whitelist preserves the prior unrestricted direct-reader behavior (`internal/tools/bash_sandbox.go:87-140`). These remain application path checks with the documented symlink check/use race, not an OS sandbox.
- The private registrar accepts only exact `*ReadTool`, `*LSTool`, `*GrepTool`, or `*FindTool` values and reads the whitelist captured in that reader (`internal/tools/trusted_registry.go:293-334`). A non-empty valid scope sorts/deduplicates copied canonical roots and hashes a versioned, length-framed encoding (`:337-359`). It does not derive authority from a caller-supplied name or a parallel config copy.
- Empty/invalid scopes give Read/Ls built-in provenance, unknown effect, zero scope, and preserve `inline_pipeline_joined`; Grep/Find remain ordinary unknown registrations with no completion proof. A valid non-empty scope alone mints `workspace_read`. The fallback for registries without the new private capability preserves Read/Ls' prior completion-only registration (`internal/tools/builtins.go:205-226`). Middleware and deferred wrappers forward the same private capability and fence.
- Source tests cover canonical root order/duplicates, changed scope, replacement invalidation, and same-name ordinary definitions. The affected package passed the targeted race selection. These results do not enlarge the reviewed path checks into a stronger filesystem isolation claim.

## R5 canonical transcript DTO and adapter

**Conclusion: accepted as a standalone DTO/adapter slice; no finding.**

- DTO row identity is `(PhysicalSessionID, selected BranchID, actual EntryID)`. `Clone` copies nested canonical message values via `core.CloneAgentMessages` and independently copies compaction, occurrence, call, and journal fact slices (`internal/tui/transcript_projection.go:15-93,106-139`). `Validate` rejects mismatched/repeated keys, invalid row kinds/roles, inconsistent compaction facts, invalid tool occurrence facts, duplicate durable source IDs, invalid enums, and partial action reservation identities (`:143-212`).
- The adapter first clones the prepared snapshot, validates view identity, rejects missing/duplicate durable IDs, builds the active-leaf path, and checks physical-session membership through `BuildFromSnapshot` (`internal/cli/transcript_projection.go:75-134`; `internal/session/tree_builder.go:190-215,250-285`). Each published message is paired by position with its exact `SourceEntryID`, must be on that selected path, and must exactly match the source projection. Compaction keeps a typed row with the valid provider-facing user role (`:136-198,269-305`).
- Journal rows attach only by the exact run / assistant ordinal / tool-call tuple, selected-path membership, and matching tool name. The DTO retains the actual journal `EntryID`, `ActionBatchID`, and persisted `ActionID`; ambiguous duplicate assistant occurrences and contradictory tool names fail the whole projection. Journal-only rows do not synthesize unpublished transcript rows (`:200-239`). Run observation copies the supplied owner's status handle and reports a same-snapshot terminal row as an observed fact only; it does not synthesize acknowledgement or saved state (`:35-73`).
- The targeted tests include malformed source/message role and tool-receipt identity, compaction typing, repeated ToolCallID across run/ordinal, sibling and incomplete joins, open journal groups, defensive nested copies, and owner-failure versus terminal-row observation. They passed in the target race selection. This is not evidence for Bubble Tea replacement reducers, default CLI persistence, or PTY/restart behavior; those remain later R2/R5 integration gates.

## Validation performed

Test names were listed before execution with `go test -list` in the review worktree. The selected race run covered Assembly close, prepared Plan/scoped readers, transcript projection/observation, DefaultRunSlotGuard close races, ACP server close/start, scoped-read identity/replacement, and DTO clone/validation:

```text
rtk env GOMAXPROCS=2 go test -race -p 2 -count=1 -run '^Test(AssemblyClose|AssemblyDoesNotTrustCustomClient|CloseAssemblyForCommand|AssemblePreparedPlanAllowsScopedReaders|TranscriptProjection|TranscriptRunObservation|TranscriptOpenJournal|DefaultRunSlotGuardClose|DefaultRunSlotGuardTryClaimRunRacesClose|HarnessAndTurnRunnerRejectAfterAdmissionFenceBeforeCallbacks|HTTPServerStopJoinsDetachedDispatchBeforeRestart|HTTPServerSealAdmissionBeforeStartPreventsLateStart|HTTPServerConcurrentStartIsIdempotent|HTTPServerStopJoinsRegisteredRequestHandler|RegisterDefaultsMintsScopedWorkspaceReadForConcreteReaders|ScopedWorkspaceReadScopeUsesCanonicalWhitelistRoots|ScopedReaderReplacementChangesScopeAndStalesPriorInvocation|OrdinarySameNameReaderRegistrationCannotKeepScopedReadMetadata|TrustedScopedWorkspaceReadRejectsSameNameCustomDefinition|CanonicalTranscriptSnapshot)' ./internal/cli ./internal/core ./internal/acp ./internal/tools ./internal/tui
```

Result: `internal/cli`, `internal/core`, `internal/acp`, `internal/tools`, and `internal/tui` all passed. `rtk go vet ./internal/cli ./internal/core ./internal/acp ./internal/tools ./internal/tui` passed. No product-source changes or temporary probes were made in the review worktree; no full `make verify`, live CLI, PTY, or end-to-end UI validation was run.
