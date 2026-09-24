# R3 resolver source review

Review target: fixed partial-resolver commit `bcdac4df5a5e177aa97bad6996f57ab7ac487488` in a detached worktree. This review covers the resolver, provider registry, credential scoping, and `internal/config` auth chain in that snapshot. It does not cover later Core/CLI consumer injection and does not mark R3 complete. No product source was changed.

## Findings

### P1 — A custom redirect callback can move a scoped request to a foreign origin

`httpClientForResolvedScope` checks the redirect URL at `internal/llm/model_resolver.go:432-438`, then invokes the caller-supplied `CheckRedirect` at `:439-440`. The callback can mutate `request.URL` after the origin check and return nil. `net/http` then dispatches the redirected request with the Authorization header inherited from the same-origin request.

I reproduced this with two local `httptest.Server`s: the first returned a same-origin redirect; an injected callback rewrote the redirect URL to the second server; the second server received `Bearer redirect-callback-canary`. The disposable regression failed its required `secondRequests == 0` assertion (actual count 1) and observed the bearer value. The existing direct cross-origin redirect test at `model_resolver_test.go:122-158` passes but does not exercise callback mutation.

Run the caller callback before the final origin check, then validate the resulting URL immediately before allowing dispatch. Preserve callback errors and `http.ErrUseLastResponse`. Keep a regression that asserts the second server receives zero requests and no credential.

### P2 — Case-folded provider registration can shadow a canonical resolver identity

`ProviderRegistry.register` checks duplicates using the raw provider name (`internal/llm/registry.go:78`), but stores provider specs under a lowercased name (`:95-97`). Snapshot publication also lowercases names and assigns into the resolver map (`:109-120`). Registering `EINO` after the default `eino` provider succeeds; `Registry.Get("eino")` still returns the original provider, while `SnapshotResolver().ResolveModel("eino", ...)` resolves through the later `EINO` provider. A disposable regression confirmed this split and resolved the canonical `eino` request to the later provider's configured endpoint.

Normalize provider IDs and aliases before duplicate checks and reject collisions before publishing a resolver snapshot. Apply the same canonical comparison to aliases. Current built-in Eino/OpenAI specs do not populate aliases, and this finding is specifically the case-variant collision; unknown custom identities still require an explicit endpoint.

### P2 — Model metadata is not part of the claimed immutable resolver snapshot

`WithModels` retains the caller's input slice (`internal/llm/provider.go:112-118`). `Models()` copies that slice when called (`:130-135`), but `providerDefinition` retains a live provider pointer (`internal/llm/registry.go:118`) and `resolveModelSpec` calls `provider.Models()` on each resolution (`internal/llm/model_resolver.go:278-286`). Mutating the original slice after `SnapshotResolver()` therefore changes model metadata returned by the existing resolver snapshot. A disposable regression confirmed that a pre-snapshot `ModelInfo.Description` changed from `before` to `after` in a later resolution.

Copy the input slice in `WithModels` and/or snapshot model metadata into `providerDefinition` when the registry publishes a resolver. `ModelInfo` currently contains only scalar fields, so a shallow item copy is sufficient. This affects metadata consistency, not protocol or credential authority.

## Confirmed safeguards and non-blocking note

- The unexported `resolverProviderSpec` method prevents external `ModelProvider` implementations from self-declaring a protocol. The current supported OpenAI-compatible specs are inferred only for the concrete Eino/OpenAI adapters; native Claude/Gemini identities remain disabled before credential lookup or `Build` (`model_resolver.go:17-22, 312-344`; `registry_edges_test.go:47-55`).
- `AuthChain.Resolve` fails closed for provider-only lookup, while `ResolveCredential` accepts only sources with an exact-scope lookup (`internal/config/auth_chain.go:121-155`). Scoped CLI/file/environment tests and resolver canaries passed. No credential appeared in observed error/log paths during this review.
- `ProviderSpec.RequireCredential` is populated by the current concrete adapter inference (`model_resolver.go:318-336`) but never read; resolution recomputes the same default-provider/default-origin predicate at `:232`. For today's supported adapters this is redundant metadata, not a current credential bypass. Remove it or make it authoritative before adding another in-package provider spec, so future policy does not diverge.
- The current default identities remain `eino` and `openai`; arbitrary custom names require an explicit compatible endpoint. No native protocol was enabled by model metadata or a provider name.

## Verification

- Scoped resolver, credential, native-gate, cross-origin redirect, endpoint, and custom-provider selection: `GOMAXPROCS=2 rtk go test -race -p 2 ./internal/llm -run '^TestProviderRegistry(RejectsNativeIdentityBeforeBuild|UsesMatchingScopedCredentialAtRealOrigin|ScopeMismatchPrecedesCredentialLookup|RequiresCredentialForOpenAIDefaultOrigin|AllowsConstructorCredentialOnlyOnSameOrigin|RejectsCrossOriginRedirectBeforeSecondDispatch|UnknownAliasNeedsExplicitEndpoint|PreservesExplicitOpenAICompatibleCustomEndpoint|AllowsExplicitCredentiallessCompatibleEndpoint|RejectsInlineCredentialWithMismatchedScope|DoesNotSendProviderDefaultKeyToOverrideOrigin|RejectsUnspecifiedCustomProvider)$' -count=1` — 12 tests passed.
- Scoped AuthChain and legacy-fallback isolation: `GOMAXPROCS=2 rtk go test -race -p 2 ./internal/config -run '^TestAuthChain(ProviderOnlyResolveFailsClosed|ResolveCredentialUsesExactScope|ScopedCLIFlagDoesNotCrossProviderOrOrigin|ScopedAuthFile|ScopedEnvironmentAndLegacyFallbackIsolation)$' -count=1` — 5 tests passed.
- Two disposable resolver regressions confirmed the case-collision and mutable-model-metadata findings; the temporary test file was removed.
- The redirect-callback mutation reproducer was also temporary and removed. Its expected zero-dispatch assertion failed: the foreign server received one request and the canary Bearer credential.
- The builder's full `internal/llm` and `internal/config` race suite was not repeated. Core/CLI integration and the remaining R3 consumer paths were outside this fixed partial checkpoint.
