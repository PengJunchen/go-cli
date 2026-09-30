# R3 first-request provider-envelope preflight handoff

Status: bounded designer handoff for the separate provider-envelope follow-up in `r3-acceptance-audit.md`. This document does not close R3-DES-01, authorize a provider/protocol, change product policy, or modify the durable current-submission repair. The R3 provider and compaction documents remain design contracts; R2 implementation and restart-safety review remain gates.

## Boundary and source evidence

The current Loop checks durable or non-durable message size at `loop.go:698-726`, then applies dynamic tool filtering at `728-746`, and dispatches the resulting messages/options at `1384-1387` ([`loop.go`](../../../internal/core/loop.go#L698)). The preflight receives only messages, estimator and the compaction ceiling. It cannot account for the tool schemas and provider envelope that the model will receive. The selector's `ModelInfo` has `InputTokenLimit`, `ContextWindow` and `MaxOutputTokens`, but these limits are not attached to the request at this boundary ([`llm.go`](../../../internal/llm/llm.go#L319), [`model_selector.go`](../../../internal/llm/model_selector.go#L848)).

R3 compaction already defines the budget order and its unknown fallback: direct `InputTokenLimit` takes precedence; otherwise derive an input limit from `ContextWindow` less the output reserve; cap by the configured compaction ceiling; apply the existing 20% safety margin; subtract the final tool-schema and protocol-framing estimates. Missing model limits permit the local ceiling as an explicitly unknown provider limit, not a claim of provider fit. A known direct input limit does not receive a second output deduction. The compaction ceiling remains distinct from generation `MaxTokens` ([`r3-compaction-design.md`](../2026-09-25-runtime/r3-compaction-design.md#L21)).

R3 provider design requires complete provider-scoped model identity and one resolver/catalog authority; it also keeps unknown model metadata explicit and defines sampling precedence as per-call explicit value, model setting, provider setting, then adapter default. The contract review says compaction budgeting and generation output caps must remain separate ([`r3-provider-design.md`](../2026-09-25-runtime/r3-provider-design.md#L22), [`r3-provider-design.md`](../2026-09-25-runtime/r3-provider-design.md#L27), [`r3-provider-design.md`](../2026-09-25-runtime/r3-provider-design.md#L39), [`r3-contract-review.md`](../2026-09-25-runtime/reviews/r3-contract-review.md#L58)). `ResolvedModel` already pairs its resolved candidate with a `ModelSpec`, but the selected candidate's metadata is not carried through the Loop preflight boundary. A cycler or post-switch model must not borrow the primary model's limits.

## Request admission contract

For the bounded main-request path, preflight the finalized logical request plan consumed by the provider adapter. This is not proof of exact wire tokenization or provider acceptance: only terms established by an estimator bound to the active adapter may be estimated. The selected candidate identity, its own model metadata, final messages, and generation options form one immutable request plan. Dynamic tool filtering is materialized once; the same frozen tool definitions and generation values feed both the estimate and dispatch. No option is rebuilt, tool list re-filtered, candidate re-selected, or output cap changed after admission without creating a new plan and re-running preflight.

Follow the existing compaction-design order for this boundary: consume steering/follow-up, materialize the final filtered tools/options, estimate the request, compact if needed, re-estimate the changed messages with the same candidate/options, then dispatch. If a change in messages requires a different tool set, create a new plan and re-estimate that exact set before dispatch.

The smallest useful seam is a request-plan capability at the model/Loop boundary. It may be internal; this task does not require a new runtime or a redesign of selector policy:

```go
type RequestPlan struct {
    Candidate ResolvedCandidateHandle // opaque resolver-issued dispatch target
    Model     ModelSpec               // from this same candidate; Known=false stays unknown
    Messages  []Message               // final provider projection
    Options   GenerationOptions       // materialized once after tool filtering
}

type RequestEnvelopeEstimator interface {
    EstimateRequestEnvelope(context.Context, RequestPlan) (RequestEnvelopeEstimate, error)
}
```

This snapshot matters because `llm.Option` is a function that mutates `GenerationOptions`, and `WithTools` captures a slice ([`llm.go`](../../../internal/llm/llm.go#L391), [`llm.go`](../../../internal/llm/llm.go#L411)). Apply options in their declared order once after filtering, then defensively freeze the option values and schema data. The estimator and provider dispatch must consume that same materialized snapshot; do not independently apply the original closures on the two paths.

The plan is ephemeral and contains no credential material. `Candidate` and `Model` must come from one `ResolvedModel` (or equivalent resolver-issued snapshot); the plan's target is also the target consumed by dispatch. A model wrapper that selects a cycler candidate only inside `Stream` must expose a side-effect-free selection/preparation hook so preflight can pin that candidate. If it cannot, the request cannot claim a candidate-specific fit and must stop before `Stream`; it must never substitute primary metadata. Candidate changes on switch, cycle, or an already-authorized later attempt invalidate the plan and require preflight for the newly selected candidate. This preserves current retry/fallback authority and does not authorize a new retry.

`RequestEnvelopeEstimate` should report disjoint message, final tool-schema, protocol-framing and multimodal terms, each with a known/unknown state and reason. The enabled protocol adapter owns its framing estimate; unsupported protocols remain disabled under R0. Use the existing estimator for message and JSON-marshaled final schema structures, while keeping framing overhead distinct so terms are not counted twice. A custom model, opaque custom option, or protocol adapter without a matching estimator cannot be marked fit-known from `ModelInfo` alone; keep its envelope state unknown and stop before dispatch unless a configured conservative bound covers each required term. An estimator error or canceled context is a pre-dispatch failure, never a zero-cost estimate.

## Budget resolution

Let `C` be the configured compaction ceiling (`Compaction.MaxTokens`, default 8000), `I` the active candidate's positive `InputTokenLimit`, `W` its positive `ContextWindow`, `O` the effective output reserve, and `E` the sum of estimated final tool schemas plus protocol framing. The message budget follows the existing R3 compaction proposal:

1. If `I > 0`, `raw = min(C, I)`; do not subtract `O` again because `I` is already an input limit.
2. Otherwise, if `W > 0` and `O` is known, `raw = min(C, W - O)`. Resolve `O` from the effective generation cap after R3 sampling precedence. Clamp a positive requested cap to the active model's positive `MaxOutputTokens` before storing it in the plan; use the same clamped value for estimation and the provider body. When the request has no explicit/configured output cap, use the known `MaxOutputTokens` as the reserve. Preserve option presence: an explicit `MaxTokens=0` is not “unset” and must not be replaced with metadata. If the effective cap/reserve cannot be established, the derived input limit remains unknown.
3. If no input limit can be derived because candidate limit metadata or the output reserve needed for `W` is missing, use `C` only as the existing local fallback and mark provider-limit status `unknown`. Do not treat an absent reserve as zero, or turn a ceiling or estimate into evidence that the provider will accept the request.
4. For the chosen local `raw` value (including `C` when marked provider-limit `unknown`), `safe = floor(raw * 0.80)`, preserving the existing MidTurnCompact safety margin. Then `messageBudget = safe - E`. Compact against this message budget. If `W - O <= 0`, mandatory retained messages, final schemas, or framing still exceed it, or a term required for admission is unknown without a configured conservative bound, return a typed pre-dispatch error and make zero `Stream` calls.

The effective output cap/reserve is separate from `C`: `C` is never sent as generation `MaxTokens`. The 20% margin covers ordinary text tokenization/serialization estimation error only; it is not an image-size or provider multimodal bound. A known direct `InputTokenLimit` still uses the 20% margin and schema/framing deductions, but not an output-reserve deduction.

For images or other multimodal blocks, preserve an explicit `unknown` estimate if provider accounting, dimensions/detail, or model support cannot be established. A configured conservative per-item reserve may be used while keeping the provider-fit status unknown. Without such a bound, do not declare the request to fit. Do not retain the fixed 85-token image heuristic as a safe upper bound. Logs and traces may report estimated terms and their `estimated`/`unknown` state, never provider-reported usage. Do not emit a `fit=true` or equivalent verified-provider-fit signal: even a fully estimated request is not provider acceptance evidence, and opaque custom models/options/protocols remain unknown.

This reuses the current compaction fallback without inventing a hard provider guarantee: missing model-limit metadata may still use the local compaction ceiling, with an `unknown` status. By contrast, inability to identify the active candidate or estimate a required final schema/protocol-envelope term is not a fit result; stop before provider dispatch. Unknown values are never silently replaced with zero.

## TDD acceptance matrix

Use deterministic local provider fixtures and recording estimators. The decisive integration case should exercise the real default Assembly/Loop request path, with a candidate `InputTokenLimit` and final filtered schemas/framing sized so messages alone fit but leave no budget for the full envelope (for example, `messageTokens <= safe` while `messageBudget <= 0`). Assert a typed pre-dispatch error and zero provider `Stream` calls. Add a neighboring case where compaction can reduce the retained request enough to fit, and allow dispatch only after re-estimating the compacted request under the same candidate and frozen options.

| Case | Required assertion |
|---|---|
| Filtered options are the admitted options | A dynamic tool filter returns a smaller schema set; estimate receives those exact definitions and the provider fixture receives the same canonical definitions. No post-estimate filtering or rebuild occurs. |
| No-tools boundary | An empty final tools list costs zero schema tokens while protocol framing remains separately accounted for. |
| Limit precedence and ceiling | Positive `InputTokenLimit` wins over `ContextWindow`; direct input limit does not subtract output reserve; `C` caps either known limit; the 0.80 safety factor is applied once. |
| Context fallback and output cap | With no `InputTokenLimit`, derive from `ContextWindow - O`; prove per-call/model/provider/adapter cap precedence, clamp requested `MaxTokens` to known `MaxOutputTokens`, assert the same effective cap is estimated and sent, and preserve explicit zero as present. |
| Switch/cycler identity | Select or switch to a candidate with limits different from primary; assert its candidate ID, provider/model identity and its own `ModelSpec` remain paired through preflight and dispatch. An unknown selected candidate remains unknown and never borrows primary limits. |
| Missing model metadata | Use the configured compaction ceiling as the documented local fallback, assert the provider-limit result remains `unknown`, and assert no test or log claims guaranteed provider fit. |
| Estimator failure/cancellation | A schema/framing estimator error or pre-dispatch cancellation returns before `Stream`; no zero-token fallback or retry is manufactured. |
| Multimodal unknown | Unknown image accounting is visible as unknown; a configured conservative reserve can be used without changing that status; with no bound, preflight rejects before `Stream`. |
| Opaque model/options/protocol | Without a matching estimator, a custom model or opaque option/protocol remains unknown, never emits a fit-known trace/log state, and fails before `Stream` unless conservative bounds cover every required term. |

The durable current-submission identity/projection repair remains its own builder task and its acceptance cases must not be folded into this matrix. The summarized conversation, subagent and summary-model policy, continuation semantics, retry/breaker behavior, and broad selector/resolver redesign are also outside this handoff. The request-plan candidate pin only makes preflight and dispatch refer to the same already-selected target.

## Stop conditions and external decisions

Stop before provider dispatch if the active dispatch candidate cannot be pinned, a switch/cycle cannot supply matching metadata, a required final schema or enabled-protocol framing estimate errors or is unsupported, or a cancellation occurs during estimation/compaction. Return a typed reason and keep all unmeasured terms explicit; do not dispatch under an assumed capacity.

No external decision is needed to implement this bounded estimate/admission contract. PRD-04 cost-budget policy remains an external product decision and is not a context-window hard cap. Native provider/version enablement remains disabled pending the existing R0 wire-fixture and owner-approval gates. This handoff does not change either decision.
