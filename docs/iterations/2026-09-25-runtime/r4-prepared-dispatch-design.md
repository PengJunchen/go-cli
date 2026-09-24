# R4 Prepared Dispatch Seam

Status: bounded interface proposal for `R4-EFFECT-01`, ready for independent joint review with the revised R2 contract. It closes the prepared-executor gap raised in `reviews/r2-contract-review.md`, but does not itself approve R2 or implement the seam. Source grounding uses the current `r2-contract.md`, `r1-contract.md`, `r4-policy-design.md`, and the tool/approval/loop path.

## Problem this seam closes

The current `LoopAgent` selects a tool, then calls its registry definition. Approval and its user callback run inside `ApprovalMiddleware.WrapToolCall`; argument normalization/schema validation are executor wrappers; the Plan guard is another wrapper. The call is not represented as a prepared object that R2 can journal before execution. Calling `Execute` to discover the approval outcome can already execute the tool.

For `write` and `edit`, `WithMutationQueue` consumes the call and does not invoke its `next` executor. The current queue handler reconstructs a new `WriteTool` or `EditFileTool`; this loses the exact registered definition, its identity and the prepared authorization snapshot. The prepared object must travel through the queue worker and execute the captured definition there.

`MiddlewareToolRegistry.List` also returns raw definitions, while `Get` returns an Execute wrapper. A caller can execute a value returned by `List` without approval wrappers. Replacing the raw list with protected proxies must preserve the optional interfaces used by the runtime: `Parameterized`, `StreamingBashTool`, `PromptGuideliner`, and tool-search capability. `LoopAgent` currently locates tool search with a concrete `*tools.ToolSearchTool` assertion; that must become a capability-interface check so a proxy does not disable filtering.

## Minimal interface and immutable snapshot

Use one canonical name for the in-memory prepared invocation. The current R2 contract already adopts `PreparedInvocation`; R2 consumes this seam and must not define a second competing interface.

```go
// package tools
type ToolCallPreparer interface {
	PrepareToolCall(context.Context, ToolCall) (PreparedInvocation, error)
}

type PreparedInvocation interface {
	ToolIdentity() TrustedToolIdentity
	EffectSet() TrustedEffectSet
	SchemaDigest() [32]byte
	NormalizedCall() ToolCall
	ArgumentDigest() [32]byte
	Dispatch(context.Context) (*ToolResult, error)
}

// Implemented only when the captured executor supports streamed execution.
type PreparedStreamingInvocation interface {
	PreparedInvocation
	DispatchStreaming(context.Context, StreamSink) (*ToolResult, error)
}
```

The concrete implementation is private to the controlled registry/execution path. It captures one immutable registration snapshot:

- the exact executor definition instance plus registration source, per-name generation, executor fingerprint, and non-secret workspace/endpoint scope identity;
- the effect set minted by the trusted registration boundary, not read from model arguments or an extension's self-declared metadata;
- a defensive schema snapshot and its digest, whether or not the definition currently exposes `Parameterized`;
- the normalized `ToolCall`, deep-copied as a JSON value tree, plus its canonical argument digest;
- the policy mode/generation and Plan Mode active state/generation used for this decision;
- a private, one-use authorization permit created only after validation and the current policy/callback decision.

Snapshot accessors return immutable values or defensive copies. The executor pointer, mutable schema tree, permit state, and raw credentials are never exposed. Executor definitions/configuration remain immutable after registration; replacing a same-name registration advances its generation. Untrusted dynamic registrations default to `unknown` and cannot self-upgrade their effect. Existing schemas may be snapshotted here; adding schemas to every built-in remains R4-BLD-02 work.

## Trusted effect evidence boundary

`EffectSet` is minted from the captured trusted registration and its complete execution path, not inferred from a built-in name, argument label, or a tool's self-reported description. Only a read path whose executable behavior is bounded can receive `workspace_read`; unknown or config-controlled execution remains `unknown` and is fail-closed by Plan/quarantine policy.

- `grep`/ripgrep may be classified `workspace_read` only when its pinned executor constructs fixed arguments that pass the user pattern as a value (`-e`, never as an option), terminate option parsing before paths (`--`), disable config loading (`--no-config`), and removes `RIPGREP_CONFIG_PATH` from the child environment. Keep the effect `unknown` until this is true. Regression tests must show a pattern such as `--pre=sh` and an injected `RIPGREP_CONFIG_PATH` cannot execute a helper or otherwise escape the read operation.
- `find`/`fd` may be classified `workspace_read` only when its pinned executor terminates option parsing with `--` before user-controlled pattern/path values and exposes no user-controlled execution option. Keep it `unknown` until proven; if the utility's parsing cannot be safely bounded, replace that implementation with a pure-Go walk or retain `unknown`. Regression tests must show a leading `--exec` pattern and an executable path cannot invoke a command.
- `git status`, `git diff`, and `git blame` remain `unknown`/composite. Source review confirmed `git status --no-optional-locks` and `git diff --no-ext-diff --no-textconv` can still run a repository gitattributes `filter.clean` helper; argument hardening alone does not make these safe. Do not build a broad Git-config isolation framework in this seam. LSP/server/configuration-driven tools likewise remain `unknown` unless the captured implementation proves they cannot execute or dispatch an external action. Do not infer safety from names such as `read`, `grep`, `find`, or `git status`.

These are prerequisites for granting the corresponding effect; they do not broaden this seam into an OS sandbox. A trusted registration may use a more restrictive implementation or keep the effect unknown. Tests must exercise the real prepared path and assert zero helper/external invocations on blocked or malformed inputs.

`PreparedInvocation` is itself the local one-use dispatch capability. Its private lifecycle is only `prepared → scheduled → committed/finished` for queued work, or `prepared → committed/finished` for direct work; do not add a second runtime or generic job framework. The public `Dispatch` atomically consumes its admission at most once. It takes no new tool name or arguments, so the caller cannot substitute a different request after approval. Reuse returns a typed already-consumed error before any executor side effect. A denied or invalid call returns a typed pre-dispatch error from `PrepareToolCall` and no invocation/permit. An `AlwaysAllow` decision may update its separately scoped cache, but it still produces only a one-call permit for this invocation; Allow Once is never cached.

Keep the streaming path inside the same capability. Do not forward raw `StreamingBashTool.ExecuteStreaming` from a proxy, because that would bypass the prepared permit and frozen policy/identity snapshot. The preparer returns a `PreparedStreamingInvocation` only for a captured streaming executor. `Dispatch` and `DispatchStreaming` share one private consume-once state and are mutually exclusive; both validate the same captured generations and invoke the same captured executor/call. A streaming proxy's compatibility method may prepare and immediately call `DispatchStreaming` exactly once, while R2 prepares before syncing intent and then calls `DispatchStreaming` on the already prepared object with the actual sink. A second prepare, a second dispatch flavor, or a raw optional-interface call cannot run the executor twice.

The captured execution pipeline is the execution stage only. Split out the wrappers whose behavior has already occurred during preparation, including argument normalization/schema validation, approval/Plan decision, and MutationQueue scheduling; retain executor-required validation and tracing that still belong around the actual invocation. A queued worker calls the captured execution stage directly: it must not re-enter approval, re-normalize under a newer schema, or enqueue itself through the same MutationQueue a second time. If a wrapper cannot be safely split or pinned, preparation fails closed rather than invoking an unwrapped or reconstructed definition.

## Execution order and generation fence

The prepared path owns this order for each assistant tool-call group:

1. Resolve each exact registration once and freeze its trusted identity, effects, schema, and generation. Normalize and validate arguments using that same schema snapshot. Reject malformed, missing, or unsupported argument values before approval and before any executor call.
2. Evaluate approval and Plan policy against the same snapshot. If a callback is needed, render the normalized call and a safe effect/target summary from this snapshot. Do not hold registry or policy locks while waiting for the user. `ApprovalAllow` grants this invocation only; `ApprovalAlwaysAllow` additionally updates its declared reusable scope.
3. Retain the returned `PreparedInvocation` in memory. R2 constructs `ActionIntent` from its identity, effect set, scope, and argument digest; it does not persist credentials or duplicate raw arguments. Pre-dispatch denials become known error receipts without an `ActionID`.
4. R2 syncs the applicable session intent batch and workspace `dispatch-ready` marker. The R2 caller invokes no `Dispatch` unless the entire required sync operation returned success. For sequential mode, this happens per call before that call. For parallel mode, every call is prepared first, then the entire approved group and known denial receipts are synced before any group member starts.
5. `Dispatch` (or `DispatchStreaming`) atomically consumes the invocation once. For a direct executor, take the shared fence only long enough to compare the captured registration/approval/Plan generations and commit the dispatch; on mismatch return typed `not_dispatched` without invoking the executor. For a queued mutation, this public method is only one-use admission and scheduling: enqueue the captured job without a final policy fence, because policy may change while it waits. At dequeue the worker checks the original context, then takes the same shared fence, revalidates generations, and commits the dispatch. A stale or canceled queued job is `not_dispatched` and never reaches the executor. Once R2 has reserved its `ActionID`, every such exit closes that exact reservation with a durable `not_dispatched` receipt carrying the same `ActionID`; do not mint a replacement attempt ID or discard the reservation. Release the fence before invoking the executor.
6. Invoke the exact captured executor instance with the exact normalized call. Do not look up the tool by name again. A registration replaced after dispatch commit cannot swap the executor for this call.
7. The caller records a typed receipt before the next provider request. A receipt does not prove a child resource has stopped. R1-03a owns worker/resource tracking and quarantine; only its positive quiescence evidence can authorize the separate `ResolveExecution` journal record.

R2 owns reservation IDs and commit granularity; this seam allocates no journal identity. In sequential mode R2 first commits the complete assistant tool-call row, then handles each call as its own one-action `ActionBatchID`: prepare/approve, reserve its `ActionID`, sync the session intent plus global dispatch-ready marker, dispatch, and sync the receipt (plus execution resolution when required) before preparing the next call. In parallel mode R2 prepares the complete group, reserves all approved calls and records known denials, then syncs one group batch/marker before any executor starts. The R2 `ActionID` is the stable reserved attempt identity: after reservation, every stale fence, cancellation, or queued pre-dispatch exit preserves it in the single `not_dispatched` receipt. This API does not mint, replace, or reinterpret that ID.

The generation fence must be shared by dispatch and every state writer that can affect its decision; a private lock used only inside `Dispatch` is insufficient. Registration replacement, approval mode or reusable-cache policy updates, and Plan state transitions acquire this same narrow fence (or a documented, fixed lock order) while publishing their new generation/state. They do not mutate an already registered definition in place: replacement publishes an immutable new snapshot with a new generation. `AlwaysAllow` cache writes also use the fence; the resulting one-call permit binds the post-update generation. For direct calls, the final comparison and permit consumption happen together under this fence. For queued mutations, the worker performs that same final check at dequeue, after verifying the original context; a generation change while queued therefore prevents execution. Queue admission and enqueueing do not hold the fence. No fence/registry/policy lock is held while an approval callback waits, while the session/index sync runs, while a job waits in the queue, or while the executor runs. A fenced/canceled invocation after R2 allocated its reservation closes with that same `ActionID` and `not_dispatched`; a rejection before reservation has no `ActionID`. The invocation is one-use across ordinary and streaming dispatch. The R2 driver, not an EventStream observer, enforces sync-before-scheduling/dispatch in the production Loop path.

## Mutation queue handoff

The file mutation queue remains a FIFO/per-path scheduling mechanism. It must receive a queue job containing both the existing serializable mutation descriptor (operation/path/payload used for ordering and previews) and the private captured execution stage plus the original run `context.Context`. The public `PreparedInvocation.Dispatch(ctx)` consumes the one-use admission and enqueues this job; it must not enter the same queue recursively. At dequeue, the worker checks cancellation and calls a private `commitAndInvoke(ctx)` path that takes the shared final generation fence and then invokes the captured execution stage directly. It must not call public `Dispatch` again, re-enter `wrappedToolDef.Execute`, re-run approval or normalization, replace the run context with `context.Background`, reconstruct a built-in definition, call `ToolRegistry.Get`, or rediscover an executor by name. If the current queue cannot accept a captured execution job or the wrapper pipeline cannot be split into scheduling and execution stages safely, preparation fails closed rather than falling back to reconstruction.

Keep raw credentials out of the descriptor and audit record. The captured normalized call remains in memory only as long as needed by the queued owner; cancellation and queue shutdown follow the R1 owner contract. A stale/canceled queue job retains the R2-reserved `ActionID` and produces one durable `not_dispatched` receipt. The worker's private path consumes the same invocation state as ordinary dispatch; neither the public nor streaming entrypoint can be used a second time.

## Protected List and optional capabilities

`List` must not hand out executable raw definitions. It returns protected proxy definitions whose `Execute` routes through the same resolve/prepare/dispatch path as `Get`; the proxy contains no authority to call an unwrapped executor. Proxies forward only the optional interfaces actually used by consumers, against the same captured registration snapshot:

- `Parameterized` for model-facing schema construction and existing schema consumers;
- `StreamingBashTool` for the current streamed Bash execution path;
- `PromptGuideliner` for system prompt construction;
- a small `TopToolsProvider` interface for dynamic tool filtering, replacing `*tools.ToolSearchTool` assertions in core.

Search results returned by `TopToolsProvider` must also be protected proxies/descriptors, not raw executable definitions. Do not leave `List` raw as a compatibility workaround. Test that executing a value from `List` cannot bypass approval, that optional capabilities survive the proxy, and that default Assembly still filters tools when its registry exceeds the threshold.

## R2 journal payload boundary

R2 consumes the prepared invocation before it dispatches. For each approved call, its journal intent binds:

- `(SessionID, RunID, ActionBatchID, AssistantTranscriptOrdinal, original ToolCallID)`;
- the stable `ActionID` reserved at intent creation;
- `TrustedToolIdentity` including registration generation and non-secret executor/scope fingerprint;
- `TrustedEffectSet` and `SchemaDigest` from the same snapshot;
- `ArgumentDigest` of the normalized call, never the raw call or credentials.

The canonical transcript uses `TranscriptIndex` for every distinct user, assistant, and tool receipt message; the call/receipt pair uses `(RunID, AssistantTranscriptOrdinal, ToolCallID)`. Do not conflate those coordinates. The per-call `CanonicalRunResult.Transcript` from R1 remains the authoritative transcript; R2 syncs inputs/intents/receipts on the actual Loop path and verifies its terminal delta matches the same transcript indexes and payloads. `AgentEvent` and `EventStream` are projections and are never journal authority.

R2 treats known prepare rejection as a `not_dispatched` receipt with no action reservation. A post-reservation final fence failure/cancellation is a `not_dispatched` receipt carrying the reserved `ActionID`. Once dispatch has committed, a missing terminal receipt is `unknown`. Receipt outcome and resource quiescence remain independent: even `success` or `unknown` does not clear a workspace mutation marker while a child may still run.

R1 consumer errors `ErrCanonicalTranscriptRequired` and `ErrCanonicalTranscriptInvalid` mean the run has no accepted canonical transcript commit. Lifecycle code must not release a claim as transcript-committed on either sentinel. A normal provider error with a validated and appended delta is different: the transcript is committed even though the run outcome is error. The owner tracks these facts separately from worker completion and `PendingExecutionError` resource state. The typed middleware chain's `FailureSynthesisMiddleware` preserves the first `CanonicalRunResult` and error and skips event-to-history reconstruction and an internal second run for recoverable typed errors; the legacy `Run` synthesis and Loop's existing provider retry remain unchanged. A single-use prepared tool permit never instructs that outer middleware to replay a dispatch, and an unknown receipt must not trigger another executor call.

The revised R2 contract now defines `RecordReceipt` as distinct from `ResolveExecution`, separates `ActionID` reservation identity from the assistant-group key, and gives each transcript message its own `TranscriptIndex`. Those fields and per-run coordinates are authoritative in R2; this design does not introduce parallel IDs or receipt semantics. R2 remains unapproved until the independent reviewer rechecks the revised contract and this companion together.

## Ownership and test boundary

| Work | Owner and file boundary |
| --- | --- |
| Trusted registration identity/effects, generation snapshots, preparer, private prepared invocation, executor wrapper/proxy, captured mutation queue job | R4 prepared-dispatch builder: `internal/tools/{tools.go,registry.go,middleware_registry.go,middleware.go,mutation.go,mutation_middleware.go}` and focused tests. Add a new narrow file for types if that keeps `tools.go` stable. Grep/fd and helper-capable Git tools stay unknown until the exact hardening and adversarial tests above pass. |
| Approval decision from the prepared snapshot, one-use permit creation, cache scope, stale-generation rejection | R4 approval builder: `internal/approval/{middleware.go,callback.go}` and tests. It must not hold a lock while the callback waits. |
| Safe approval presentation metadata and cancellation UI | TUI owner: `internal/tui/approval_entry.go`, renderer/model tests. Carry a correlation ID and redacted identity/effect/target summary as display-only data; `ApprovalResponse` remains a decision, never an authority token. |
| Minimal `TopToolsProvider` consumer and default Assembly dynamic-filter regression | After the R1 provider owner hands off `internal/core/loop.go`, the narrowly assigned core integration owner changes the concrete assertion to the capability interface. Until then, R1 retains `loop.go`; R4 must not edit Loop body files. Test with the actual assembled registry and more than the configured threshold. |
| Run commit sink, `ActionIntent`/receipt mapping, sync barriers, canonical transcript coordinates | R2 builder owns its new core sink definitions and session adapter. The actual Loop call-site integration begins only after the R1 `CanonicalLoop` handoff and consumes `PreparedInvocation`; R2 does not reconstruct history from events. |
| Run claim, live worker/resource quiescence, unknown outcome, workspace quarantine release | R1-03a owner; it consumes canonical transcript commit status and R1 execution markers. R4/R2 receipts do not release claims or clear resource markers. |

Required seam tests are: prepared call uses the same executor/schema/effect snapshot; same-name replacement before direct dispatch is rejected without side effects; replacement after dispatch commit cannot change the captured executor; a permit cannot dispatch twice; concurrent registration, approval-cache/mode, and Plan writers share the dispatch fence and produce either a stale rejection or the already-pinned invocation (never a check-then-mutate gap); unknown/dynamic tools remain unknown; grep's `--pre=sh` pattern plus `RIPGREP_CONFIG_PATH` cannot launch a helper before it receives `workspace_read`; fd's leading `--exec` input cannot execute a command before it receives `workspace_read`; verify `git status --no-optional-locks`, `git diff --no-ext-diff --no-textconv`, and `git blame` remain `unknown` when repository-configured `filter.clean` executes; a queued write/edit preserves the original run context and, if its generation changes while queued, is rejected at dequeue with no executor side effect; a successful queued job executes the captured definition without re-entering public `Dispatch`, approval, or the queue; the queue worker does not re-normalize under a new schema; `Dispatch` and `DispatchStreaming` are mutually exclusive and the streaming call receives the caller's sink; a `StreamingBashTool` proxy cannot bypass preparation; `List` proxies cannot bypass wrappers and preserve the four capabilities above; actual Assembly tool-search filtering remains active; parallel preparation starts zero executors before the whole journal batch sync; and R2 receipts use unique transcript coordinates while preserving call-group pairing.

Not in this seam: D-01 Plan-exit product policy, a complete schema/catalog rewrite, new tool behavior, rewriting the R1 Loop implementation, persisting raw arguments, or replacing the session journal/index design. D-01 remains deferred and R0's current `exit_plan_mode` behavior remains unchanged.
