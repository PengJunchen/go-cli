# Remaining runtime designs: independent review

**Verdict: REJECT both as builder handoffs pending the API and ownership changes below.** The source-capability/sidecar direction for durable compaction and the AttemptID/usage direction for accounting are appropriate. The drafts state desired behavior, but they do not yet define enough cross-layer contracts for a builder to implement them without inventing ownership or retry semantics.

Reviewed the uncommitted designs against source at HEAD 17ab7b76ef0b7636abff1e5a055de7acf6f6bb31 and the accepted 2026-09-25 runtime handoffs/reviews. This was a read-only design review; no tests were run.

## Durable compaction

### P1 — Source and receipt metadata do not survive the actual runtime conversion path

The design correctly says that each prompt item needs an exact source reference and that unknown/pending tool outcomes must block compaction (durable-compaction-design.md:34-49). The listed owners and API do not complete that contract:

- The loop builds LLM history from Submission.History, but the conversion copies role/content/tool fields without ReceiptOutcome or source provenance (internal/core/loop.go:531-545). Submission and Agent history are plain AgentMessage slices; AgentImpl clones/stores those slices (internal/core/types.go; internal/core/agent.go:136-144,273-280).
- SystemReminderInjector can prepend another AgentMessage to history (internal/core/system_reminder_middleware.go:52-58,94-102). The design mentions this middleware but does not give it a typed way to label that message ephemeral or preserve the aligned sources.
- llm.Message has no receipt or source metadata (internal/llm/llm.go:42-58). The loop’s compaction conversion then creates new AgentMessages without ReceiptOutcome and gives TurnItems only local msg-N IDs (internal/core/loop.go:1050-1085). The reverse conversion applies the user-role untrusted wrapper but drops summary identity/source metadata (1091-1136).
- Consequently, merely assigning ReceiptOutcome in messagesToTurnItems cannot recover an outcome already dropped during submission hydration. A durable unknown receipt can arrive from PublishedView, pass through Agent/Submission, and reach a summarizer without its unknown status.

**Required correction:** define one core-only, one-to-one metadata contract that starts at durable CLI hydration and survives Agent history cloning, Submission construction, middleware insertion, llm.Message conversion, TurnItem conversion, and conversion back after compaction. It must carry both receipt outcome and opaque source provenance. Choose and document one concrete form: either an aligned sidecar that is length/identity validated at every conversion, or runtime-only llm.Message metadata explicitly excluded from provider JSON, plus explicit copy/clone rules. Do not put these fields in provider wire messages, transcript/action rows, or a new event ledger. Give SystemReminderInjector an explicit Ephemeral source marker; do not infer it from role/content.

The sidecar must distinguish:

- prior published messages: exact source entry ID from the sink’s run-start PublishedView;
- current-run messages: exact canonical TranscriptIndex;
- injected/system-only messages: no durable cursor;
- a derived summary: the original source-through cursor it represents;
- tool receipts: their original known success/error/interrupted/unknown outcome.

The source snapshot must be obtained from the same run-bound sink that captured baseView, and durable hydration must verify that the messages and sidecar came from that revision. If a source is missing, duplicated, reordered, or cannot be matched exactly, fail closed before compaction and before the next main-loop provider request. No content equality, length, or compacted-array-index reconstruction is acceptable.

**Required owners:** add internal/core/agent.go, internal/core/types.go (or a dedicated core history-source type), internal/core/system_reminder_middleware.go, and the durable prompt/REPL hydration call sites (internal/cli/durable_cli_session.go and repl_session.go) to the ownership table. The existing sink baseView and CommitCompaction API are good primitives; this is a missing handoff contract, not a reason to add another ledger.

### P1 — The compactor does not return the exact source boundary needed for a durable projection

The durable sink already has the right persistence boundary: RunCommitSink includes CommitCompaction and RunTerminal.Projections; SessionRunCommitSink snapshots PublishedView into baseView and maps a cursor to its actual source entry ID or transcript row (internal/core/run_commit.go:124-132,275-293; internal/session/run_commit.go:64-94,660-670,853-887). The missing link is the result from the core compactor.

The current CompactionProjection contains only ExpectedHistoryRevision, Through, and Summary. The current compaction APIs return item slices/trigger state, not an atomic “new summary plus exact replaced source boundary” result (internal/core/run_commit.go:270-281; internal/compaction/midturn.go; internal/compaction/summary.go:177-223). SummaryCompactor knows the cut internally but does not return it. UnifiedCompactor may degrade between strategies, and LastStrategy is mutable shared state rather than metadata attached to the exact call result. An IsCompaction marker alone is insufficient: a prior summary may remain in the suffix during a later compaction.

**Required correction:** define a typed per-call compaction result before implementation, for example a result containing the output items and either no durable projection or one newly-created summary projection with its raw summary text and exact source-through reference. Source references must travel with the input TurnItems, and SummaryCompactor must derive the result boundary from the exact cut it selected. UnifiedCompactor must return the strategy/result for that invocation rather than requiring callers to inspect shared LastStrategy state. Durable mode must reject placeholder-only Micro or drop-only Truncating results. Core must not guess from item count, content, position, or a newly-seen IsCompaction flag.

For repeated compactions in one run, carry the prior synthetic summary’s original source-through reference forward. A later summary may advance that cursor only through the last original source it actually covers. Add a case where the second cut leaves the earlier synthetic summary in its suffix; either preserve live/restored projection parity exactly or reject before the next main-loop request.

### P1 — The configured CLI summarizer is not wired for the explicit summary strategy

The factory supports a configured summarizer for both unified and summary (internal/compaction/factory.go:47-65). CLI assembly currently injects s.smallModel only for empty, unified, and micro_first, excluding summary (internal/cli/assemble.go:1598-1614). Thus strategy=summary through the assembled CLI path can silently use the factory’s no-op summarizer even though the factory-level API accepts one. A factory-only unit test would miss this.

**Required correction:** include an assembled CLI RED that selects strategy=summary with a configured small model and proves the real small model is used; retain an assembled unified case as well. The summary call is a real provider request. Account it as a separate summary operation with its own actual resolved provider/model identity and AttemptID; do not count it as a main-loop dispatch. For compaction-sync failure assertions, “zero dispatch” means zero main-loop provider requests. A configured summary call may already have occurred after canonical source/input sync and must be recorded separately. A source/input sync failure must prevent the main request; a failed CommitCompaction sync must prevent the next main-loop request.

### P1 — Unknown and pending receipts need a gate before every summarizer path

The proposed loop-side eligibility gate is directionally right, but must operate on metadata that survived the entire conversion path above. Put a shared eligibility check before any compactor strategy or summarizer call, including direct strategy use and Unified degradation. Unknown receipts must cause summarizer-call count zero; missing/open/malformed tool groups must also prevent summary, projection, and the next main-loop dispatch. Preserve known error and interrupted receipt values.

The accepted tool-group review is limited to in-memory group closure; it explicitly does not accept durable projection or provider preflight (r3-compaction-groups-review.md:3-15,30). Keep that algorithm. The user-role UntrustedSummaryContext projection has already been accepted and is implemented on both loop and restore paths (internal/core/loop.go:1091-1103; internal/session/derive.go:368-390); do not reopen the old summary-role finding.

The RED list should include:

- a source-sync failure case asserting no main-loop provider request;
- a CommitCompaction append/sync/refresh failure case asserting no subsequent main-loop request and no success terminal acknowledgement, while separately recording any already-dispatched configured summary request and its actual identity;
- two compactions where the second cut retains the first synthetic summary, checking exact Through values, terminal projection order, and reopen/live parity;
- unknown receipt and malformed/pending group cases asserting zero summarizer calls;
- assembled CLI strategy=summary and unified cases proving the configured summarizer is actually used;
- a SystemReminder insertion case proving it never shifts the source cursor.

The seven-method sink, immutable baseView, real source-ID mapping, and terminal projection equality checks already exist (internal/core/run_commit.go:124-132; internal/session/run_commit.go:92,853-887,954-980). Core still calls terminal with nil projections and currently swaps in an unavailable compactor for durable commits (internal/core/loop.go:425-429,907-920). These are integration gaps, not evidence that the sink needs a second persistence abstraction.

## Attempt accounting and operation retry

### P1 — BeginDispatch is a counter/accounting seam, not Loop retry authorization

The proposed ProviderDispatchAuthority.BeginDispatch allocates an attempt at the HTTP boundary and caps the number of actual calls (attempt-accounting-design.md:31-68). That alone does not make Loop the retry/fallback authority. It does not express who proposed a second candidate, why it is eligible, whether the previous stream has joined, whether partial output/tool dispatch/durable acknowledgement has occurred, or whether ContinuationNoReplay forbids this fallback. A wrapper can still call next after an error and let BeginDispatch count or deny the next request; safety policy remains inside the wrapper path.

That distinction matters in the current source: retry, failover, and overflow middleware each have their own loops (internal/llm/middleware_retry.go:254-320; middleware_failover.go:89-143; middleware_overflow.go:88-160), and ModelCycler directly calls primary after selected-model build/generation/setup failures (internal/llm/cycler.go:246-289). The real request boundaries are client.Do in both supported adapters (internal/llm/provider.go:513; internal/llm/native.go:190). The accepted local retry review expressly excludes an operation-wide budget (r3-retry-safety-review.md:3-5,43-58); the prior production contract says middleware proposes and Loop counts/decides (r3-production-remaining-contract.md:74-76).

**Required correction:** add a concrete typed proposal/authorization contract to the design before handing it to a builder. At minimum define:

    DispatchCandidate {
      resolved candidate handle/identity,
      source/reason: initial | retry | failover | overflow | cycler-primary,
      previous attempt ID (empty only for initial)
    }

    DispatchProposal {
      operation ID,
      candidate,
      previous typed attempt terminal,
      context/cancel state,
      stream/resource joined state,
      candidate/partial output published state,
      tool dispatch started state,
      durable acknowledgement state,
      continuation-no-replay flag
    }

    OperationDispatchAuthority.AuthorizeCandidate(proposal) -> typed permit or typed denial

    ProviderDispatchAuthority.BeginDispatch(permit, actual resolved identity) -> AttemptID/handle

The authority must be Loop-owned. Middleware and cycler may classify failures and propose candidates, but cannot call the next candidate until Loop’s typed authority grants a permit. The provider adapter validates that the actual resolved identity matches the permit and allocates the unique AttemptID immediately before client.Do. Finish returns/stores exactly one immutable result keyed by AttemptID, and that result must be available to Loop before a subsequent proposal. State explicitly how selected-candidate handles are passed so fallback cannot accidentally reuse the initial model identity. Initial request, every retry/failover/overflow recovery/cycler-selected-to-primary transition share one operation cap. Continuation is a new operation under the fixed run-level cap, but ContinuationNoReplay denies every automatic retry/fallback. If the draft chooses an alternate architecture, it must still define the same typed proposal, Loop authorization, adapter fence, and result-return path; do not leave that choice to the builder.

The RED must prove authorization affects real network dispatch, not only BeginDispatch counts: inject a retry-safe terminal with each disqualifying fact (partial publication, tool dispatch, durable acknowledgement, unjoined stream, cancellation, unknown receipt) and assert the unauthorized second httptest request is absent. Also prove a valid retry and a cycler primary proposal are authorized within the same budget and carry distinct actual resolved identities. Preserve the BUILD-1 owner’s stream close/join changes and land this only after that owner completes.

### P1 — Summary-model calls need their own operation but the same attempt provenance

llmModelSummarizer.Summarize directly calls s.model.Generate under TaskTypeSummary (internal/cli/assemble.go:2152-2176). It is outside the normal main-loop Generate/Stream path. The attempt design must explicitly say how a summary call obtains a distinct operation ID, actual resolved identity, AttemptID, terminal result, and reported/unknown usage. It must not consume the main model operation’s MaxAttempts, but it must not disappear from cost/usage accounting. This is especially important when a durable compaction marker sync fails after a summary call: zero main-loop dispatch does not mean zero provider dispatch overall.

The current production wrapper records only successful non-stream responses with non-nil usage, and Stream bypasses accounting (internal/production/llm_integration.go:120-149). Preserve the proposal’s unknown-not-zero rule, but test summary operations at the assembled boundary rather than treating factory wiring or middleware-call count as proof.

### P2 — Clarify pre-dispatch denial versus a dispatched network error

The design says validation/request-construction failures before BeginDispatch have no AttemptID, while any error after admission to client.Do counts as dispatched and may have unknown usage (attempt-accounting-design.md:33-36,66-68). Keep those as distinct typed states. If the term not_sent is retained, define whether it is an operation denial with no AttemptID or a provider attempt; never create a zero-usage attempt for work that did not cross the dispatch fence.

## Handoff decision

Do not start the compaction builder from the current draft. First revise the source/receipt sidecar API and ownership across hydration → Agent/Submission/middleware → llm.Message → TurnItem, define the typed per-call compaction result and boundary, and add the assembled summary-strategy RED. The existing source-capability plus runtime provenance sidecar is sufficient and is not inherently overbuilt; it is incomplete until these transfer and atomic-snapshot rules are explicit.

Do not start attempt-accounting implementation from the current draft. First specify the typed candidate proposal and Loop authorization result described above, how Finish facts return to Loop, and how the summary-model operation is created/accounted. Keep middleware-call counts only as diagnostics; the acceptance oracle is actual httptest request count plus one identity/AttemptID/result per dispatched request.

No tests or full-suite commands were run for this design-only review.
