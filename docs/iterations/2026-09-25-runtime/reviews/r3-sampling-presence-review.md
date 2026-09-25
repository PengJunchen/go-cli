# R3 sampling presence implementation review

**Status:** Approve the sampling-presence slice at `e14f704333227ba5a00c87ba900a30c75852bf7e` for integration. The reviewed chain is `0aa23794fb852c25f9988bd75b61fcc223f266fd` → `eab5917e76b69252bba44a071b47e41709fb20a5` → `6ffa737a30db74086e18575b21d0a7037c81d034` → `bd5cb4a730c094ccb303ca2ac2defbef69c75e76` → `e14f704333227ba5a00c87ba900a30c75852bf7e`. This approves config-to-active-OpenAI-compatible sampling behavior only; it does not close other R3 consumers or enable native protocols. The reviewer made no product-source changes.

## Scope and evidence

Reviewed the fixed snapshots in detached worktrees. Source coverage included `internal/config/types.go`, `loader.go`, `yaml_loader.go`, `validator.go`; `internal/llm/llm.go`, `model_resolver.go`, `registry.go`, `provider.go`; and `internal/cli/assemble.go` plus the sampling-presence and assembled HTTP regression tests.

The presence model is additive and preserves existing Go scalar fields. `Config` records whether source presence is authoritative, while provider/model/small-model sections each retain per-field set bits (`internal/config/types.go:38-88,90-167`). Loaded configurations distinguish absent from explicit zero. For older code-built values, non-zero scalars remain explicit; callers use the exported `SetTemperature`/`SetMaxTokens` methods to express an explicit zero. Accessors return scalar snapshots, so no mutable pointer or slice alias is introduced.

The loader keeps its source priority and merges sampling presence separately from the generic non-zero reflection overlay (`internal/config/loader.go:96-165,217-232,304-365`). The built-in 4096 values remain unset defaults (`:281-302`); assembly treats 4096 as the final max-token fallback, rather than as a model/provider override. Per-call pointer options override model settings, then matching provider settings; temperature remains omitted when unset (`internal/cli/assemble.go:1657-1725`, `internal/llm/provider.go:449-455,534-565`). Resolver compatibility keeps legacy non-zero request values set (`internal/llm/model_resolver.go:248-269`). The active HTTP DTO uses optional pointers, so a configured zero is serialized as numeric zero and an unset value remains absent.

The actual `httptest` assembly coverage verifies provider `0.8/512` when model sampling is unset, model and per-call explicit zero, the 4096 fallback, small-model explicit zero, and no sampling inheritance from a different provider (`internal/cli/assembly_sampling_precedence_test.go:21-220,231-335`). Small-model assembly reads its own values and only borrows provider sampling for a matching provider identity (`internal/cli/assemble.go:1675-1703`). This rechecks the sampling interaction; provider identity/credential behavior was accepted separately in `r3-consumer-review.md`.

## Closed findings

### SAMP-01 — JSON `null` was treated as an explicit zero (fixed at `eab5917`)

At `0aa2379`, `encoding/json` decoded `null` into scalar temperature/token fields as zero, while presence recording treated key existence as an explicit setting. A real Loader probe showed `provider.max_tokens: null` overriding the built-in 4096 fallback to zero; YAML rejected the equivalent typed value. This made JSON and YAML disagree and allowed an invalid config to reach request construction.

`eab5917` added JSON sampling-field inspection before typed decode and rejects `null` for all six provider/model/small-model temperature and max-token keys. The final helper is at `internal/config/yaml_loader.go:66-100,115-183`. Permanent tests cover all six fields and confirm Loader returns no config on a null override (`internal/config/sampling_presence_test.go:75-97`). A real assembled JSON explicit-zero request remains numeric zero (`internal/cli/assembly_sampling_precedence_test.go:62-94`).

### SAMP-02 — Small-model sampling bounds were not validated (fixed at `6ffa737`)

Before `6ffa737`, `DefaultValidator` checked provider/model temperature in `[0,2]` and non-negative max tokens, but did not check the corresponding small-model values. Invalid small-model settings could therefore be accepted by `Loader` and fail only after model dispatch.

The fix applies the same range rules to small-model sampling (`internal/config/validator.go:52-74`). Tests reject temperatures below zero and above two and negative max tokens, allow explicit zero, and verify the real small-model HTTP body carries zero (`internal/config/sampling_presence_test.go:195-228`; `internal/cli/assembly_sampling_precedence_test.go:188-228,275-335`). The invalid-configuration assembly fixture verifies Loader rejects before any provider request.

### SAMP-03 — NaN passed the range checks (fixed at `bd5cb4a`)

The YAML scalar parser accepts values parsed by `strconv.ParseFloat` (`internal/config/yaml_loader.go:579-603`). The old range expression `value < 0 || value > 2` accepts NaN because both comparisons are false. A real `Loader.Load` probe at `6ffa737` accepted `temperature: NaN` independently in provider, model, and small-model sections. Such a value cannot be JSON-encoded in the provider request.

`bd5cb4a` centralizes the predicate as finite and within range, using `math.IsNaN` and `math.IsInf` for all three sections (`internal/config/validator.go:35-44,57-74`). Tests cover YAML NaN and positive/negative infinity for all three sections, code-built configs, and finite zero (`internal/config/sampling_validation_test.go:14-78`).

### SAMP-04 — JSON case-folded tags bypassed presence and null checks (fixed at `e14f704`)

At `6ffa737`, the typed JSON decoder accepted case-insensitive spellings such as `Provider` and `TEMPERATURE`, but presence/null detection looked up only exact lowercase map keys. A real Loader probe showed a previously accepted non-zero value in `{"provider":{"TEMPERATURE":0.8,"MAX_TOKENS":512}}` being discarded in favor of unset/4096 defaults; `TEMPERATURE:null` also bypassed the null guard.

`e14f704` now inspects the actual tagged section/field names with case-fold matching, applies presence only after typed decoding succeeds, and rejects ambiguous case-fold-equivalent keys before choosing a map value (`internal/config/yaml_loader.go:104-233`). Tests cover folded section names, folded temperature and underscore-preserving `max_tokens` spellings, explicit zero/non-zero values, all six folded null combinations, and duplicate folded section/field forms (`internal/config/sampling_presence_test.go:99-193`). An unmatched `MaxTokens` spelling is not promoted to the `max_tokens` tag (`:133-147`). YAML parsing is unchanged. The implementation deliberately does not infer aliases beyond the existing tagged JSON names.

## Validation

Test names were listed before running the corresponding selections. The original sampling matrix at `0aa2379` passed under race (`-count=3`) across config, LLM, and CLI: config JSON/YAML presence, absent fields, env-zero precedence, merge and programmatic compatibility; resolver and HTTP DTO presence; and assembled primary/small/per-call/fallback cases.

On the fixed chain, the reviewer independently ran these targeted race selections at `-count=3`:

- At `6ffa737`, config null/Loader, small-model bounds, and explicit-zero tests passed; CLI JSON explicit-zero, invalid-small-before-dispatch, and small-model explicit-zero HTTP tests passed.
- At `bd5cb4a`, `TestLoaderRejectsNonFiniteSamplingTemperatures`, `TestLoaderAllowsFiniteSamplingTemperatureBoundaries`, and `TestValidatorRejectsCodeBuiltNonFiniteSamplingTemperatures` passed in `./internal/config`.
- At `e14f704`, `TestLoaderPreservesCaseFoldedJSONSamplingPresence`, `TestLoaderDoesNotPromoteUnmatchedJSONSamplingFieldNames`, `TestLoaderRejectsCaseFoldedJSONNullSampling`, and `TestLoaderRejectsAmbiguousCaseFoldedJSONSamplingKeys` passed in `./internal/config`.

`git diff --check` was clean for the reviewed fix deltas. This reviewer did not rerun full config/LLM/CLI packages on the final snapshot; builder-reported full package results are not counted as independent review evidence.

## Boundaries and remaining configuration notes

- The existing environment schema reads sampling values from `PROVIDER_*` and `MODEL_*` variables (`internal/config/loader.go:217-232`). It has no `SMALL_MODEL_TEMPERATURE` or `SMALL_MODEL_MAX_TOKENS` keys. Small-model sampling works from JSON/YAML and programmatic layers. The handoff does not define new small-model env keys, so this is recorded as an existing capability boundary rather than an unrequested schema expansion.
- `parseFloatEnvSet` and `parseIntEnvSet` still treat malformed environment values as unset (`internal/config/loader.go:492-525`), following existing loader behavior. Thus a malformed higher-priority env value can silently fall through to a lower source; tightening env diagnostics is separate from zero-presence handling.
- Distinct case-fold-equivalent JSON keys are rejected, but repeated identical JSON key text is still handled by the standard decoder's existing last-value behavior; generic duplicate-key policy was not expanded in this slice.
- The R3 handoff explicitly keeps Claude/Gemini native protocol paths disabled. The reviewed wire assertions cover the active OpenAI-compatible adapter only; they do not establish native-protocol sampling support.

## Decision

Accept the sampling-presence implementation at `e14f704` for integration. The tested path now preserves absent versus explicit-zero values through provider/model file-and-env merge, small-model file/programmatic configuration, resolver, primary and small-model assembly, per-call override, and active HTTP serialization; validation rejects null, invalid small-model bounds, and non-finite temperatures. This is a narrow implementation acceptance, not full R3 completion.
