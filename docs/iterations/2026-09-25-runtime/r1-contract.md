# R1 Run ownership contract

Status: approved by independent design review for builder handoff. This contract narrows the audited R1 findings to existing `Harness`, `Agent`, `Loop`, `TurnRunner`, `EventStream`, `RunSlotGuard`, LLM stream and Bash execution paths. The default CLI assembly already creates one `runSlotGuard` and injects that same instance into `Harness` and `TurnRunner`; reuse it. Do not introduce a second workspace lock, a general Runtime framework, a database, a distributed lease, or automatic action replay.

## Required runtime semantics

`Result.Success` and `Turn.Status` are currently the only outcome indicators. Add an outcome dimension and an execution-state dimension without conflating them. The proposed public fields and values are additive to the current structs and preserve existing entry points:

| Outcome | Meaning |
|---|---|
| `success` | Provider protocol completed correctly, all requested tools have known terminal receipts, output checks passed, and the canonical transcript is committed. |
| `error` | The run failed with a known result. If a remote or local side effect may have happened but its receipt is missing, use `unknown`. |
| `cancelled` | Cancellation ended the logical run and no unresolved action outcome remains. A cancel request alone is not this outcome. |
| `unknown` | A side effect may have happened but its terminal receipt is unresolved. A provider parse/read failure with no tool dispatch is `error`; partial text visibility alone does not make an external effect unknown. `unknown` says nothing about whether work has stopped. |

| Execution state | Meaning | May release the shared run claim? |
|---|---|---|
| `running` | The owner is executing the run. | No. |
| `cancel_requested` | Cancellation was recorded and delivered; workers may still be executing. | No. |
| `draining` | The owner is collecting terminal receipts and waiting on registered workers/processes. | No. |
| `cleanup_pending` | The caller's bounded cleanup wait expired. This is a caller observation; the owner still tracks live or unsettled work. | No. |
| `quiescent` | All registered workers/readers/children have stopped and the canonical transcript is committed. | Yes, exactly once. |
| `quarantined` | The owner cannot prove that some worker or child has stopped, or containment failed. | No. A timeout never releases it. |

`cleanup_pending` is set when the caller's bounded cleanup wait expires; it says only that the caller is returning while cleanup continues. It may later become `quiescent` after the owner observes all completions, or `quarantined` if completion cannot be proved. `quarantined` may become `quiescent` only after positive evidence of termination and transcript commit. Otherwise it remains quarantined until process exit or explicit operator recovery; elapsed time is not evidence of termination. Cleanup timeout changes execution state only and can coexist with a known `error` or `cancelled` outcome. No retry, new write run, or workspace mutation may enter while the old owner still holds the claim.

Create one shared per-run owner/helper around the existing `RunClaim` and use it from both `HarnessImpl` and `EinoTurnRunner`; do not implement two cleanup state machines. It registers each launched tool worker before launch, records each worker's terminal notification, owns cancellation/drain deadlines, and serializes terminal outcome exactly once. It also tracks the top-level Agent/Loop callback and canonical transcript commit so an early callback return cannot hide outstanding tool workers or unfinished history. Callers wait only for a configured bounded cleanup interval. The supervisor observes worker completion notifications without an unbounded caller-side `WaitGroup.Wait`. A non-cooperative Go tool that misses the deadline remains tracked/quarantined; do not pretend it stopped or release its claim. The owner releases the exact claim only after every worker callback has returned, every registered pending resource is proven quiescent, and the canonical transcript is committed. An unknown action receipt alone does not keep the claim held once execution and history are quiescent; a pending resource keeps the claim held even when the action receipt is already known. Do not wrap this path in `ExecuteClaimedRun`, whose existing defer releases the claim as soon as its callback returns.

Use immutable value snapshots for `Result` and `Turn`: add `RunID`, `Outcome`, and `ExecutionState`; keep legacy `Success` and `Turn.Status` as compatibility projections. Concretely, add `RunID string`, `Outcome RunOutcome`, and `ExecutionState RunExecutionState` to both structs; add `StatusHandle RunStatusHandle` to `Result` as a separate live observation handle, not mutable backing storage. The caller's returned value fields are never changed by a later supervisor update. Define `RunStatus{RunID string, Outcome RunOutcome, ExecutionState RunExecutionState, PendingWorkers int, EventGapCount uint64}`, `RunStatusHandle.Snapshot() RunStatus`, and optional `RunStatusSource.RunStatusHandle() RunStatusHandle`. `TurnRunner.Get` returns a fresh `Turn`/status snapshot; the asynchronous Harness stream can implement the optional status source without changing `EventStream`. `Result.Success` is true only for `Outcome=success`. A `cleanup_pending` return uses the best known logical `Outcome` (which may be `unknown` only when an action receipt is unresolved); if only known error/cancellation remains, preserve that outcome while execution state is `cleanup_pending`. Later owner-state changes are visible only through `Snapshot`/a fresh `Get`, not by mutating returned values. Use `RunClaim.ID` as `RunID`. Release eligibility depends on quiescence and transcript commit, not on whether the action outcome is known.

Keep `AgentLoop.Run`, `Agent.Run`, `Harness.Submit`, `TurnRunner.RunTurn`, `Cancel`, `Steer`, `FollowUp`, `Get`, and the existing `RunSlotGuard` as entry points. `Cancel` records intent and cancels context but does not claim quiescence. `Turn.Status` remains the compatibility view of logical turn completion; callers must use execution state to know whether resources stopped. Both the direct TurnRunner path and Harness path must exercise the same `runSlotGuard` instance wired by `internal/cli/assemble.go`.

## Canonical history and event projection

The production `AgentImpl` currently stores the submitted user message and the last assistant message, while tool results and steering/follow-up messages can be absent from persistent history. The loop already has typed tool calls/results and correlates parallel results by `ToolCallID`; keep that typed data as the source for the canonical per-run transcript.

Add a small optional `CanonicalLoop` interface with one per-call `RunCanonical` result that contains both the event projection and an immutable typed transcript. The transcript types and interface belong to lane 01. Each transcript is only the current-run delta: the initial submitted user input once, this run's assistant messages/tool calls and receipts, and steering/follow-up inputs consumed during this run. It must not include `priorHistory`. `AgentImpl.Run` snapshots prior history for the model request, but does not append the current user input before calling the loop; after the run returns, it appends the completed delta exactly once under the existing history lock. This avoids duplicate user entries and makes the commit atomic. `LoopAgent.Run` remains the compatibility wrapper and returns the `RunCanonical` event slice; it must not keep a mutable `lastHistory` field. `AgentImpl.Run` type-asserts `CanonicalLoop`, consumes that call's transcript, and commits it under the existing history lock. This avoids a context collector and prevents concurrent calls from reading another run's transcript. Preserve input kind so steering and follow-up do not silently become indistinguishable. Do not reconstruct canonical history from `AgentEvent` or `EventStream`: events are a lossy UI/observer projection. Pair results by ID, never by parallel completion order. If cancellation interrupts a tool with no known receipt, close that call with an explicit interrupted/unknown receipt; never synthesize a success result. Finalize or conservatively close the transcript before releasing the shared run claim, so a later mutation cannot begin against half-written history.

The proposed additive shape is:

```go
type RunOutcome string // success, error, cancelled, unknown
type RunExecutionState string // running, cancel_requested, draining, cleanup_pending, quiescent, quarantined

type RunStatus struct {
    RunID          string
    Outcome        RunOutcome
    ExecutionState RunExecutionState
    PendingWorkers int
    EventGapCount  uint64
}

type RunStatusHandle interface {
    Snapshot() RunStatus
}

type RunStatusSource interface {
    RunStatusHandle() RunStatusHandle
}

// Result and Turn retain their existing fields and gain these value fields.
type Result struct { // existing Message and Success fields are preserved
    Message        string
    Success        bool
    RunID          string
    Outcome        RunOutcome
    ExecutionState RunExecutionState
    StatusHandle   RunStatusHandle // optional live status, separate from this immutable snapshot
}

type Turn struct { // existing fields, including Status, are preserved
    RunID          string
    Outcome        RunOutcome
    ExecutionState RunExecutionState
    // Existing Turn.Status remains the logical-completion compatibility projection.
}

type CanonicalRunResult struct {
    Events     []AgentEvent
    Transcript []TranscriptEntry
}

type CanonicalLoop interface {
    RunCanonical(ctx context.Context, submission Submission, stream ...EventStream) (CanonicalRunResult, error)
}

type TranscriptEntry struct {
    Message AgentMessage
}
```

`RunOutcome` is the string enum `success|error|cancelled|unknown`; `RunExecutionState` is the string enum `running|cancel_requested|draining|cleanup_pending|quiescent|quarantined`. Lane 01 exclusively owns `internal/core/types.go`, including additive `Result.RunID`, `Result.Outcome`, `Result.ExecutionState`, `Result.StatusHandle`, the matching `RunOutcome`/`RunExecutionState` types, and the transcript fields on `AgentMessage`. Lane 03a consumes those fields and owns `Turn`/owner lifecycle fields, not `types.go`. Add optional backward-compatible Go `string` JSON fields to `AgentMessage`: `InputKind` tagged `json:"input_kind,omitempty"` with values `user|steering|follow_up`, and `ReceiptOutcome` tagged `json:"receipt_outcome,omitempty"` with values `success|error|interrupted|unknown`. Preserve and reuse existing `Role`, `Content`, `ContentBlocks`, `Usage`, `ToolCalls`, `ToolCallID`, and `ToolName`. `TranscriptEntry.Message` contains this enriched `AgentMessage`; a tool receipt is always `Role="tool"`, has the original non-empty `ToolCallID` and `ToolName`, and its `ReceiptOutcome` is the authoritative receipt state. `success` means the tool returned success; `error` means a known tool failure; `interrupted` means cancellation/termination is known and no side effect occurred; `unknown` means the tool may have acted but its result cannot be proved. Assistant tool-call messages keep the existing `Role="assistant"` and `ToolCalls` fields; user inputs keep `Role="user"` and carry `InputKind`. This keeps steering/follow-up origin and tool receipt truth when `AgentMessage` is persisted directly. `RunCanonical` returns fresh per-call data and neither Loop nor Agent may mutate its backing slices after return. `LoopAgent.Run` calls it and returns only `Events` for compatibility. `AgentImpl.Run` and TurnRunner's no-Agent branch call `RunCanonical` once when available; they must not call both methods or read a shared last-run field. For fakes that implement only `AgentLoop.Run`, retain source compatibility and the narrow no-tool fallback described below. Lane 03a consumes these values and owns only `Turn`/owner lifecycle fields, not `types.go`.

Lane 03a owns the exact worker-registration seam in `internal/core/run_owner.go`; lane 03a's `parallel_tools.go` uses it:

```go
type runWorkerTracker interface {
    RegisterWorker(toolCallID, toolName string) (done func())
    RegisterPendingResource(toolCallID, toolName string, executionDone <-chan struct{})
}

func withRunWorkerTracker(ctx context.Context, tracker runWorkerTracker) context.Context
func registerRunWorker(ctx context.Context, toolCallID, toolName string) (done func())
```

The per-run owner injects its tracker into the context used by both Harness and TurnRunner. `registerRunWorker` returns a no-op completion callback when no owner is installed, preserving direct unit-test and legacy Loop execution. `parallel_tools.go` registers before each goroutine starts and calls `done` exactly once after the tool function exits. In `internal/tools`, add an `errors.As`-compatible `PendingExecutionError` interface with `OutcomeUnknown() bool`, `ExecutionDone() <-chan struct{}`, and `Unwrap() error`. Concrete wrappers preserve the cause with `Unwrap() error`, allowing `errors.As` to find either this interface or another wrapped marker; core never inspects error strings. `OutcomeUnknown()` classifies the side-effect receipt only. `ExecutionDone()` is the separate resource-quiescence signal: it closes only after the associated process/resource is positively known to have stopped and its readers are joined; it never closes on timeout alone or when quiescence cannot be proved. A known terminal receipt can coexist with a pending resource, and an unknown receipt can coexist with a stopped resource. `executeSingleTool` uses `errors.As` to recognize unknown-outcome markers and pending resources, then calls `RegisterPendingResource` before returning when `ExecutionDone()` is present, for both sequential and parallel tools. A worker's `done()` may then run while its pending-resource registration still keeps the owner claim held. A never-closing resource transitions the owner to quarantined; metadata strings are not evidence. The canonical transcript itself still comes from `RunCanonical`, not from the tracker or events.

The cross-package pending-resource contract is:

```go
// internal/tools
type PendingExecutionError interface {
    error
    OutcomeUnknown() bool
    ExecutionDone() <-chan struct{}
    Unwrap() error
}
```

Core uses `errors.As` to find both this interface and the narrower `interface{ OutcomeUnknown() bool }` marker on an error that has no pending resource. An error that wraps an unknown outcome must implement `Unwrap() error` so `errors.As` reaches the marker. Only `PendingExecutionError` requires `ExecutionDone()`. If `OutcomeUnknown()` is true but no pending resource exists, record `unknown` outcome and release the claim after all workers have returned and the canonical transcript is committed. If `ExecutionDone()` exists and remains open, retain the claim and quarantine after the bounded cleanup wait, even if the effect receipt is known. The `ExecutionDone` channel closes only on positive resource-quiescence evidence; it stays open forever if containment or termination cannot be proved. Do not use a `ToolResult.Metadata` string, command exit code, or timeout elapsed as a substitute.

Keep existing test fakes and callers that implement only `AgentLoop.Run` source-compatible. `AgentImpl` and direct TurnRunner execution may use a narrow legacy fallback for simple runs without tool calls or injected steering/follow-up; preserve the existing user/final-assistant projection for those cases. The fallback must reject or return `unknown` for tool-capable/steered runs whose loop cannot provide typed receipts; it must not claim the canonical-history guarantee from emitted events. The default CLI `LoopAgent` must implement `CanonicalLoop`. TurnRunner's no-Agent fallback also sets `RunOutcome` and execution snapshot from the direct run result instead of only filling legacy `Success`. Malformed/provider streams with no tool dispatch are `error`, not automatically external-effect `unknown`; partial visibility alone does not imply an external side effect, although it still cannot be reported as success.

The EventStream remains bounded and non-authoritative. Surface discard, send timeout, or delivery gaps to the owner/status snapshot with sequence/gap information; a dropped terminal event cannot change the authoritative `Result`. The owner records one authoritative terminal receipt and makes at most one terminal send attempt; a bounded EventStream may drop it, so do not promise caller delivery. `BlockUntilConsumed` is the current default and can block `Send` while no consumer drains the buffer; when the owner's bounded cleanup deadline expires, it must close/cancel the run's EventStream to signal its `done` channel and wake blocked sends. Keep the independent `RunStatusHandle` readable after stream closure. This interruption makes a blocked send return; it does not prove other workers quiescent or release their claim. If the caller receives `cleanup_pending`, later cleanup progress belongs to status snapshots and must not emit a second terminal result. `HarnessImpl.Submit` currently ends its span in the caller before the run goroutine begins; transfer run-span ownership to the per-run owner goroutine so it spans actual execution. End the caller-visible run span exactly once with the observed outcome when the run completes or when bounded return observes `cleanup_pending`; record continued cleanup separately without an unbounded caller wait.

## Provider terminal truth, output visibility, and process cleanup

Provider streams succeed only when the protocol's required terminal marker and clean completion conditions are satisfied. EOF before a required terminal marker, malformed event/JSON, read error, timeout, cancellation, or failed length-continuation returns a non-success outcome. These are known `error` outcomes when no tool side effect is unresolved; partial text already shown does not by itself make the external outcome `unknown`. Continuation errors must not be swallowed into a partial response. Incomplete tool-call arguments must never be dispatched. Use `unknown` only when an action may have happened and its terminal receipt is unresolved.

The optional complete-response guard seam is defined in `internal/core/output_gate.go` and implemented by the outer CLI `outputGuardModel`, preserving the existing `OutputGuard` and `BaseChatModel` interfaces:

```go
type CandidateOutputResult struct {
    Allowed   bool
    Sanitized string
}

type CompleteResponseGuard interface {
    MaxCandidateBytes() int
    CheckCandidate(context.Context, string) (*CandidateOutputResult, error)
}
```

When the model implements `CompleteResponseGuard`, the complete logical candidate across the full Loop continuation chain has a hard `400000` UTF-8-byte `guardedStreamMaxBytes` cap; `MaxCandidateBytes()` returns exactly this cap. Per-provider-stream checks are too early because another `FinishReason=length` continuation can complete the sensitive pattern. Every actual full-response accumulator also has an explicit hard cap no greater than `guardedStreamMaxBytes`, including `overflowRecoveryModel.Stream`'s `contentBuf`/continuation response, core `streamGenerate`'s segment builder, and `generateWithContinuation`'s merged `resp.Content`. The maximum number of continuation attempts and these accumulator sites is fixed and named, so their total retained candidate memory has a documented constant-factor upper bound; do not add an unbounded builder, byte slice, or hidden copy. Use a shared collector where practical, but do not add a per-copy reservation allocator. Empty chunks must not accumulate as unbounded records: aggregate text and enforce a finite `guardedStreamMaxChunks=16384` count across the run. Count text, final tool-call names/IDs/arguments, and other payload fields retained by each accumulator; enforce the 400000-byte cap on the combined logical candidate and fail if any bounded internal accumulator overflows. Do not pass tool arguments to `CheckCandidate` or claim the guard validated them. The core calls `CheckCandidate` once, only after all continuations finish with a valid terminal protocol and final tool calls are fully parsed. Until then reveal no token/delta and dispatch no tool call. A hard-buffer overflow, provider/read/continuation error, nil result, or guard error is a known `error` if no action was dispatched; expose no buffered candidate text and dispatch no tool call. Interpret the existing `GuardResult` contract precisely: whenever `Sanitized` is non-empty, publish exactly `Sanitized` regardless of `Allowed`, never original text; in particular, `Allowed=false` with non-empty `Sanitized` is a usable safe transformation (this preserves `LengthGuard` truncation) and preserves complete structured tool calls for normal approval/schema/Plan Mode review. `Allowed=true` with empty `Sanitized` publishes the original candidate. `Allowed=false` with empty `Sanitized` is a true deny: expose no candidate text and dispatch no tool call. The complete-response guard receives only plain text, not tool arguments. When no `CompleteResponseGuard` is installed, preserve live streaming. The hard cap is new and distinct from CLI `LengthGuard(100000)`'s 100,000-rune policy. The `OutputGuard` interface itself remains unchanged.

R1 also closes the Bash subprocess ownership gap. `StreamingBashTool.ExecuteStreaming` currently waits for pipe-reader goroutines before `cmd.Wait`; `CommandContext` kills only the direct shell, so a descendant that inherited stdout/stderr can keep a reader open past the deadline. A reproduced 100 ms deadline with a 700 ms descendant returned only after about 711 ms. On cancellation, the Bash owner must kill the supported process containment group, reap the direct child, close/drain both pipes, and join the readers within a bounded cleanup budget. Add OS-specific start/kill/join code and tests for each supported GOOS. The guarantee covers descendants that remain within the process group/job assigned to the command. If the platform cannot provide verified containment, or a process escapes that containment, return an explicit unsupported/unknown or quarantined state; never report quiescence based only on the shell exiting. Preserve the R0 combined `MaxOutput` source-byte cap and bounded metadata.

## Three builder tasks, four isolated work lanes

Keep ownership at file boundaries. The typed transcript contract produced by lane 01 is the input to lane 02; lane 03a supplies lifecycle/status snapshots and worker tracking. Integrate those seams in that order before marking any lane complete.

| Lane | Correct behavior and planned red tests | Interface / owned file boundary |
|---|---|---|
| `R1-BLD-01` — provider stream, continuation, output gate | Add `TestStreamReadErrorIsNotComplete`, `TestStreamMalformedEventIsNotComplete`, `TestStreamTimeoutIsNotComplete`, `TestOutputGuardRunsBeforeVisibleDelta`, `TestOutputGuardBuffersAcrossLengthContinuationsBeforeVisibleDelta`, `TestOutputGuardBudgetCoversMiddlewareContinuationBuffers`, `TestContinuationErrorReturnsFailure`, `TestIncompleteToolArgumentsAreNotDispatched`, `TestOutputGuardBufferLimitFailsClosedWithoutVisibleDelta`, and `TestCanonicalLoopTranscriptIsPerCall`. Tests use a local provider fixture plus the actual seven-layer CLI assembly; prove no success marker after stream failure/continuation error, no tool call on partial arguments, no visible delta until the complete continuation chain passes guard checks, and that all named `overflowRecoveryModel` and core accumulators are hard-bounded with a documented fixed number of copies. The guard regression's first segment ends with `FinishReason=length`; subsequent content completes a sensitive pattern across the boundary, and zero deltas are visible before the combined candidate passes. | Preserve `llm.BaseChatModel`, current provider/client APIs, and `AgentLoop.Run`. Own stream protocol/parser files under `internal/llm/`, `internal/cli/output_guard_adapter.go` and its tests, `internal/core/{types.go,output_gate.go,loop.go}` for continuation and `CanonicalLoop.RunCanonical`, and transcript contract tests. Do not edit `agent.go`, `parallel_tools.go`, or lifecycle owner files. |
| `R1-BLD-02` — Agent canonical transcript and subagent delivery | Add `TestCancelClosesToolCallHistory`, `TestAgentHistoryIncludesSteeringAndFollowUp`, `TestAgentHistoryPairsParallelToolResultsByID`, `TestSubagentSendReportsDroppedMessage`, and `TestAssemblyInterceptorsAreIsolated`. Use actual typed transcript records from lane 01; prove each assistant tool call has one matching result/interrupted/unknown receipt and steer/follow-up inputs are retained in consumed order. Never build history from events. | Preserve `Agent.Run` and `Messages()` compatibility. Own `internal/core/agent.go`, a small canonical-history helper/test file, `internal/core/subagent*.go` and a separate new assembly isolation test file. Do not edit `loop.go`, `parallel_tools.go`, `turnrunner.go`, or `harness.go`. This lane integrates after lane 01's optional `CanonicalLoop.RunCanonical` seam lands. |
| `R1-BLD-03a` (part of `R1-BLD-03`; network agent) — core Run owner and projection | Add `TestCancelDoesNotMarkRunQuiescentBeforeToolExit`, `TestUnknownOutcomeRetainsMutationSlot`, `TestEventStreamDropVisibleToRunOwner`, `TestEventStreamBlockedSendUnblocksAtOwnerDeadline`, `TestHarnessSpanEndsAfterRun`, `TestNonCooperativeWorkerQuarantinedAfterDeadline`, `TestResultSnapshotIsImmutableWhileOwnerDrains`, `TestRunClaimReleasesOnlyAfterWorkersAndTranscriptQuiesce`, `TestRunClaimRetainedForPendingToolResource`, `TestUnknownOutcomeWithoutPendingExecutionReleasesAfterQuiescence`, and `TestAssemblySharesRunSlotGuardAcrossHarnessAndTurnRunner`. Include both sequential and parallel pending-tool cases. A barrier-controlled mutation returns within its deadline with the appropriate outcome plus `cleanup_pending`; a second mutation stays rejected, then release the worker/resource barrier and prove the claim is released once. An undrained `BlockUntilConsumed` stream must be closed at the owner deadline to unblock `Send`; the independent status handle remains readable. Exercise real CLI assembly and its one shared guard. | Preserve `RunSlotGuard` and public run entry points; add only the small per-run owner/status handle and optional EventStream status source. The same owner/cleanup helper wraps both Harness and TurnRunner, owns the run span from async launch through run/cleanup observation, and closes/cancels the EventStream at bounded timeout to unblock sends. Own `internal/core/{run_owner.go,run_slot.go,turn.go,turnrunner.go,harness.go,eventstream.go,parallel_tools.go}` and lifecycle tests plus a separate `internal/cli/run_ownership_integration_test.go`; lane 01 is the sole owner of `types.go`. Define `runWorkerTracker`/`RegisterPendingResource` here. Do not edit `loop.go`, `agent.go`, or Bash package files. Core recognizes a narrower `interface{ OutcomeUnknown() bool }` for stopped-but-unknown actions; `PendingExecutionError` with `ExecutionDone()` registers live resources before the tool returns. |
| `R1-BLD-03b` (part of `R1-BLD-03`; Bash agent) — subprocess containment and bounded metadata | Add `TestRemoteBashMetadataRemainsBounded`, `TestStreamingBashCancellationKillsPipeHoldingDescendant`, and `TestStreamingBashCancellationReturnsWithinBudget`. Use a 100 ms command deadline plus a longer-lived child that inherits stdout/stderr; prove the child is terminated, pipe readers join, and return is bounded on each supported OS implementation. A live resource returns `tools.PendingExecutionError` with `ExecutionDone()`; that channel closes only after positive quiescence. If containment/quiescence cannot be proved, keep it open and let the owner quarantine. A stopped action whose external result remains uncertain carries only `OutcomeUnknown() bool` and does not report a live resource. | Preserve `StreamingBashTool` and `ToolResult` contracts. Own `internal/tools/streaming_bash.go`, the `PendingExecutionError` type/interface, new OS-specific process-supervision files, and focused Bash tests. Do not edit `internal/core` lifecycle files. The interface requires `error`, `OutcomeUnknown() bool`, `ExecutionDone() <-chan struct{}`, and `Unwrap() error`. |

For every test, assert the exact test name appears in `go test -list` before invoking `-run`; a compile failure or zero matches is not RED. RED must be a compiled test that fails because the audited implementation violates behavior. GREEN is the same test plus the complete affected package race suite. R1 review checks one authoritative terminal receipt and at most one terminal send attempt (bounded EventStream may drop it; the owner snapshot records the gap), bounded caller return, no slot release while a worker/child may still act, and no transcript gap after true quiescence. Existing fake loops implementing only `AgentLoop.Run` must keep compiling and retain the old simple no-tool projection; if they emit or could have dispatched tool calls, the compatibility fallback cannot report canonical success and returns error/unknown instead.

## R2 seam only

R1 defines identity needed by R2 but does not add a placeholder persistence implementation. R2 may introduce an optional `ActionJournal` at the existing tool-dispatch boundary with `BeginIntent` before dispatch, `RecordReceipt` after known completion, and `RecordUnknown` when the action result cannot be proven. The key includes `SessionID`, `TurnID`, `ToolCallID`, and `OwnerGeneration`; the intent stores tool identity and a payload digest, not secret payloads. On startup R2 reconciles unfinished intents and owned child identities into quarantine. It must not automatically replay them. This seam is a local durable intent/receipt contract, not a new database, distributed lease, or general runtime.
