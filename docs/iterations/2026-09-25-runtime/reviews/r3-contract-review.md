# R3 provider / compaction contract review

Status: targeted design re-review at `e64f4f40ae3fdc3f84ed2be75df6f429b122ee3a`. The three prior R3 P1 design blockers are closed in the reviewed proposal. This approves the design clarification only: R3 remains a proposal, and builder handoff still depends on R2-BLD implementation plus R2-REV-01 restart-safety acceptance. No product source was changed and no tests were run.

## Reviewed snapshots

| Artifact | SHA256 | Baseline |
|---|---|---|
| `r3-provider-design.md` | `5a22d1118979a7ca0e4886ebf689ea4e6d37690d0edf5ca7d190cb3b62faf886` | `e64f4f40ae3fdc3f84ed2be75df6f429b122ee3a` |
| `r3-compaction-design.md` | `88450af8dd6fcf76e3b7c8644d22ec55def2203ee2762ffe5747b4b4f9f84a66` | `e64f4f40ae3fdc3f84ed2be75df6f429b122ee3a` |
| `r2-contract.md` | `fa8c79024d2f27bc301279f793c2eb39d65b00200e4131e3883e3965bb94acf5` | `e64f4f40ae3fdc3f84ed2be75df6f429b122ee3a` (design approval remains `4f8d1a0d72eebc25fcf06c1fcb897d64bf73d41c`) |
| `r4-prepared-dispatch-design.md` | `9f18a2f1f5ec1f5b5958938b2938c9454387c9622fdce7f6dc3fae756b7e5e2b` | `e64f4f40ae3fdc3f84ed2be75df6f429b122ee3a` |
| source | — | previously inspected checkout `425e17cd3b1c635f1830c5528b28a3da9db6b97e`; this pass rechecks design closure only |

## Focused re-review disposition

| Prior blocker | Disposition at `e64f4f4` |
|---|---|
| Resolver coverage and immutable full requests | Closed. `r3-provider-design.md` now defines one assembly-owned immutable resolver/spec snapshot, requires complete provider-scoped `ModelRequest` values for primary, small/summarizer, selector/switch, cycler, and subagent consumers, bars private registries and implicit primary fallback, and keeps protocol gating before credential lookup. Composer/extensions/model metadata cannot enable a native protocol. Actual assembly-to-HTTP and zero-secret-lookup acceptance is specified. |
| Retry authority, shared attempt budget, continuation, and restart | Closed. The still-running Loop owner receives typed per-attempt facts and is the only retry/fallback decision point. Retry, explicit candidate fallback, and overflow recovery share one operation-wide `MaxAttempts`; length continuation has a separate fixed run cap that cannot reset on retry/provider switch. R2-synced input/projection can be reused only by that live run, while R2 recovery after restart never reissues the old provider request. The compaction proposal carries the same rules into overflow retry. |
| Legacy unscoped credentials | Closed. Global `go-cli/api-key` and provider-free `auth.json` values remain inert and are never dynamically bound to the default or candidate provider. With no valid scoped source, resolution fails closed with an explicit scoped-migration diagnostic; a separately valid scoped source remains usable under normal precedence. No migration wizard or credential-scope inference is introduced. |

The acceptance tests listed below remain implementation/handoff gates; this design re-review does not claim they have run or that R2 restart-safety is complete.

## Blocking findings

### P1 — Resolver must reach every model construction path as a complete request (closed at `e64f4f4`)

The design correctly requires one `ModelRequest`/`ResolvedModel` authority and lists primary, small-model, cycler, selector, subagent, model-list, and composer consumers (`r3-provider-design.md:9,17-26,164-166`). Current production wiring still has independent construction paths: `assembleModel` gives the cycler a fresh Eino-only `ProviderRegistry` (`internal/cli/assemble.go:680-695`); the model selector's builder and switch API carry only a model name (`internal/llm/model_selector.go:14,121-130`, assembled at `assemble.go:748-756`); and subagent assembly creates another fresh registry, while `realSubAgentRunner` passes its model-name override as the registry's provider name (`assemble.go:1190-1195`, `internal/core/real_subagent_runner.go:79-87`). `cycledModel` silently routes build/generation errors back to primary (`internal/llm/cycler.go:246-265`).

Before implementation, specify that those consumers receive the same immutable provider specs/resolver and carry a full provider-scoped request (including endpoint/credential reference) where they can select or override a provider. Define each fallback decision explicitly: unsupported/native identity, unresolved candidate, missing candidate credential, and transient dispatch failure must not collapse into an invisible primary-model switch. The native-disabled gate must run before credential lookup or adapter construction on every entry path; composer, extensions, model metadata, and a custom alias must not enable a native protocol. Acceptance should exercise actual assembly-to-HTTP behavior for switch, cycler, and subagent paths, plus zero resolver-secret lookup and zero requests for disabled native protocols.

### P1 — Safe retry authority and the shared dispatch budget need one owner (closed at `e64f4f4`)

The proposal's replay rules are sound in principle, but the current middleware boundary cannot decide them: `RetryModelMiddleware` sees only `BaseChatModel` errors, while the Loop owns visible deltas/tool dispatch and R2 owns durable acknowledgments. Existing retry, failover, cycler fallback, and overflow recovery each have their own loops/counters (`internal/cli/assemble.go:1099-1124`, `internal/llm/middleware_failover.go:85-133`, `internal/llm/cycler.go:248-289`, `internal/llm/middleware_overflow.go:36-60`). Assign the retry decision to an existing owner boundary or define the narrow typed per-attempt facts crossing it; middleware must not infer safe replay from `Stream()` returning a channel or from an error string.

The documented `MaxAttempts` rule should cover every provider dispatch in that logical operation, including context-overflow compact-and-retry if enabled; no nested retry/failover/overflow layer may reset the count. Keep length continuation as a separate run-scoped cap that does not reset when a continuation itself retries or changes provider. The review tests need a whole-stack dispatch count, not just middleware-local attempts.

Also state the live-run/recovery distinction explicitly. Reusing already-synced user input and compaction projection for a safe retry **within the still-owned run** is consistent with the R2 contract. R2 recovery after a crash following input sync says to restore the incomplete run and never automatically call the provider again (`r2-contract.md:203-204`); R3 must not turn durable input into permission to replay that old run after restart. Any provider-derived assistant/tool-call terminal already published or durably committed bars regeneration, even before the tool executor starts. Only the already-documented input/projection acknowledgments are reusable context.

### P1 — Legacy unscoped credentials need one migration rule before auth-chain wiring (closed at `e64f4f4`)

The `CredentialRef` scope `(canonical ProviderID, protocol, normalized origin)` and primary/small/cycler/subagent rules are internally consistent (`r3-provider-design.md:99-115`). The remaining decision is the legacy single-key mapping: the proposal allows either binding it to the existing default provider or requiring explicit user selection (`r3-provider-design.md:113,165`), but these produce different runtime behavior. Current `KeychainSource.Lookup` ignores provider and uses one `go-cli/api-key`; `auth.json` accepts a provider-free `{"api_key":...}` and returns it for any provider (`internal/config/keychain.go:22-28,88-93`, `internal/config/auth_chain.go:128-150`).

Choose one compatibility rule before enabling these sources in the resolver. At minimum, a legacy value may only be bound to the already-configured provider identity and resolved protocol/origin (or require an explicit migration confirmation when that scope cannot be established); never copy it to candidate providers, infer scope from an adapter's `Name`, or consult it before the disabled-protocol gate. Preserve config/YAML credentials as belonging to their explicit provider block and do not let a later model switch reinterpret a raw key under a different scope.

## Confirmed alignment and implementation gates

- The native-provider gate is aligned with R0: known Claude/Anthropic/Gemini identities remain unavailable regardless of BaseURL/key, while an explicitly named OpenAI-compatible custom endpoint remains supported. The provider proposal correctly bars composer/config metadata from enabling protocols and requires a separate reviewed registration change (`r3-provider-design.md:10,72-96`). Do not relax this boundary.
- Attempt cost truth is correctly separate from action durability and from a hard cost budget. The contract requires per-dispatch `AttemptID`, actual resolved provider/model, unknown usage when absent, and estimated-price provenance; it explicitly leaves PRD-04 hard/soft budget decisions out (`r3-provider-design.md:117-133,152,158`). Implementation must replace the current success-only Generate accounting: `ProductionModelWrapper` records only successful non-streaming Generate usage, does not record streaming usage, and labels cost with configured `modelName` (`internal/production/llm_integration.go:107-155`). Failed dispatched attempts, terminal-error usage, stream-final usage, and actual cycler/fallback identity must be accounted once per `AttemptID`; keep this in reporting, not the R2 session/action ledger.
- The compaction proposal matches the R2 projection and recovery boundary: it preserves the full durable occurrence key `(RunID, AssistantTranscriptOrdinal, ToolCallID)` while locally grouping by a single assistant message's IDs; it keeps `PriorHistoryCount`/`TranscriptCount` in run-start source coordinates; and it explicitly excludes ephemeral `SystemReminder` contributions from durable history/source cursors (`r3-compaction-design.md:9,40,52,65`; `r2-contract.md:117-143`). Keep summaries as fixed user-role untrusted context, not system instructions, and never reconstruct the durable projection from events.
- The default “no hard cost budget” decision and the pending R4 D-01 policy boundary are correctly preserved. No new Runtime, history store, or action replay framework is needed (`r3-provider-design.md:12,171`; `r3-compaction-design.md:3,68`).

## Snapshot/status correction

`r3-provider-design.md:5,177` says the R2 contract is still under revision. The joint review artifact says the design contract was approved at `4f8d1a0d72eebc25fcf06c1fcb897d64bf73d41c`, while explicitly distinguishing that from completed R2 implementation/restart review (`reviews/r2-contract-review.md:3,56`). Update R3's status text to say the R2 **design** is approved and `R2-REV-01` execution remains a gate. The R2 contract's own opening status at `r2-contract.md:3` also still says it is not approved; align that status with its final review rather than letting R3 inherit contradictory authority.

The compaction budget and generation output cap are described separately (`r3-compaction-design.md:29,36`; `r3-provider-design.md:27`); preserve distinct names/types in implementation so `Compaction.MaxTokens` cannot be mistaken for generation `MaxTokens`. This is an implementation guard, not a request to change product defaults.

## Required focused evidence before handoff

1. Resolver canaries through each production consumer above prove provider/protocol/origin and credential headers; negative disabled-native cases prove no secret lookup and zero HTTP requests.
2. Whole-stack tests prove retries plus candidate fallback share the per-operation `MaxAttempts`, continuation has its independent non-resetting run cap, and R2 recovery never replays an acknowledged incomplete run.
3. Attempt accounting proves one record per actual dispatch, including failed and streamed attempts, uses the selected provider/model, and records unknown rather than zero when usage/pricing is absent.
4. R3 compaction preflight tests prove current user/consumed steering/follow-up are retained, complete tool-call groups are atomic, cursors map through R2 source provenance, ephemeral reminders do not advance them, and restart restores the same projection once.

No source changes or tests were performed for this review.
