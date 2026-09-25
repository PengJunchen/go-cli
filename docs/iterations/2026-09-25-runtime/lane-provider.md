# Provider resolution lane

Status: implementation complete; isolated commit recorded below.

Task: `R0-BLD-02` / `LLM-01`  
Worktree: `/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/provider`  
Branch: `codex/runtime-r0-provider`  
Contract: [r0-contract.md](/Users/pengjunchen/Documents/github/go-cli/docs/iterations/2026-09-25-runtime/r0-contract.md), Provider resolution row.

## Behavior

The actual CLI factory now resolves model construction, model listing, and the small-model path through one provider resolver. Native identities `claude`, `anthropic`, and `gemini` fail before a model or HTTP request is created, even when a `BaseURL` is present. The error explains that native protocol support is unavailable and tells users to use a custom provider name only when the configured endpoint speaks OpenAI-compatible protocol.

An explicit `BaseURL` on a non-native provider name continues to select the existing Eino/OpenAI-compatible implementation. The canary test builds the model through `buildModel`, sends a real `Generate` request to the configured `httptest` endpoint, and checks its path, model, and bearer credential. A key without a `BaseURL` is accepted only for the explicit `eino`/`openai` identities; an unknown alias fails rather than inheriting the default OpenAI origin. An unconfigured unknown name still fails registry lookup.

This closes CLI routing safety only. It does not enable or claim native Claude/Gemini support; the native tool-call contract remains deferred to R3.

## Evidence

| Check | Command | Exit | Evidence |
|---|---|---:|---|
| Baseline red | `rtk proxy env GOMAXPROCS=2 go test -p 2 ./internal/cli -run '^(TestProviderCredentialNeverSentToWrongOrigin|TestProviderCustomOpenAICompatibleAliasUsesConfiguredOrigin|TestProviderOpenAIIdentityAllowsAPIKeyOnlyConfiguration|TestCLIProviderListingFailsClosedForNativeProvider)$' -count=1` | 1 (expected) | [provider-red.log](evidence/provider-red.log) |
| Test inventory | `rtk proxy env GOMAXPROCS=2 go test -p 2 ./internal/cli -run '^(TestProvider|TestCLIProvider)' -list .` | 0 | [provider-test-list.log](evidence/provider-test-list.log) |
| Targeted green | `rtk proxy env GOMAXPROCS=2 go test -p 2 ./internal/cli -run '^(TestProviderCredentialNeverSentToWrongOrigin|TestProviderCustomOpenAICompatibleAliasUsesConfiguredOrigin|TestProviderOpenAIIdentityAllowsAPIKeyOnlyConfiguration|TestCLIProviderListingFailsClosedForNativeProvider)$' -count=1` | 0 | [provider-green.log](evidence/provider-green.log) |
| Affected package race | `rtk proxy env GOMAXPROCS=2 go test -p 2 -race -count=1 ./internal/cli` | 0 | [provider-race.log](evidence/provider-race.log) |

The red run reached the old CLI factory and recorded the canary as `Authorization: Bearer ...` at the local endpoint for native identities. It also confirmed that native or unknown names with a key and no endpoint were accepted into the Eino factory. The green run verifies rejection before any request and preserves the explicit custom-compatible endpoint path. No full repository `make verify` was run in this lane.

## Commit

Code: `fe0666f4aea1a7219314060add27cf9c50dc72ed` (`fix(cli): fail closed on unsupported provider protocols`).

## R0 follow-up: small-model credential scope

The small-model factory now receives both the effective primary provider identity and the effective small-provider identity. It inherits the primary endpoint and key only for the same identity. If the small model explicitly selects a different endpoint, it may reuse the primary key only when normalized scheme, host, and effective port match; URL paths do not change origin. A different provider must supply its own endpoint or key, and receives no primary fallback. An explicit small endpoint without a small key remains allowed as an unauthenticated OpenAI-compatible endpoint.

| Check | Command | Exit | Evidence |
|---|---|---:|---|
| Baseline red | `rtk proxy env GOMAXPROCS=2 go test -p 2 ./internal/cli -run '^TestSmallModel' -count=1` | 1 (expected) | [provider-small-red.log](evidence/provider-small-red.log) |
| Test inventory | `rtk proxy env GOMAXPROCS=2 go test -p 2 ./internal/cli -list '^TestSmallModel'` | 0 | [provider-small-test-list.log](evidence/provider-small-test-list.log) |
| Targeted green | `rtk proxy env GOMAXPROCS=2 go test -p 2 ./internal/cli -run '^TestSmallModel' -count=1` | 0 | [provider-small-green.log](evidence/provider-small-green.log) |
| Affected package race | `rtk proxy env GOMAXPROCS=2 go test -p 2 -race -count=1 ./internal/cli` | 0 | [provider-small-race.log](evidence/provider-small-race.log) |

The baseline red log records both failures through real `buildSmallModel` → `Generate` → `httptest` requests: a changed provider received the primary credential at the primary URL, and a changed origin received the primary credential at the small URL. That red run covered the five request-path cases; the sixth, helper-level default-origin normalization case was added afterward and is covered by the green and race runs. The tests also cover default same-provider reuse, same-origin URL-path changes, and routing the small model's own credential only to its configured endpoint.
