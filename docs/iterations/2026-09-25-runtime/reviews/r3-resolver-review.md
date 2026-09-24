# R3 resolver source review

Follow-up review target: `575dff12f67798e9ea2514566bcfa84f86acc4d7`, atop `1ce909b7310271512489b5b3beea8973067bf7dc` and `bcdac4df5a5e177aa97bad6996f57ab7ac487488`, in an independent detached worktree. This review covers the partial resolver, provider registry, credential scoping, and `internal/config` auth chain. It does not cover later Core/CLI consumer injection and does not mark R3 complete. No product source was changed.

The `1ce909b` follow-up closes the three findings below: redirect callbacks are followed by a URL and Host origin recheck; canonical provider registration and alias collisions are rejected; and model lists are copied at option application and resolver snapshot. `RequireCredential` applies only when the resolved origin equals the spec's declared default origin. The additional `575dff1` follow-up closes the external metadata alias inconsistency. Focused race tests passed for each fix.

## Findings at `bcdac4d`

### P1 — A custom redirect callback could move a scoped request to a foreign origin (closed at `1ce909b`)

`httpClientForResolvedScope` checks the redirect URL at `internal/llm/model_resolver.go:432-438`, then invokes the caller-supplied `CheckRedirect` at `:439-440`. The callback can mutate `request.URL` after the origin check and return nil. `net/http` then dispatches the redirected request with the Authorization header inherited from the same-origin request.

I reproduced this with two local `httptest.Server`s: the first returned a same-origin redirect; an injected callback rewrote the redirect URL to the second server; the second server received `Bearer redirect-callback-canary`. The disposable regression failed its required `secondRequests == 0` assertion (actual count 1) and observed the bearer value. The existing direct cross-origin redirect test at `model_resolver_test.go:122-158` passes but does not exercise callback mutation.

At `1ce909b`, the callback now runs between the initial and final URL/Host origin checks. Callback errors remain errors; a mutated cross-origin request returns `http.ErrUseLastResponse` before dispatch. The new two-server regression asserts the second server receives zero requests and no credential.

### P2 — Case-folded provider registration could shadow a canonical resolver identity (closed at `1ce909b`)

`ProviderRegistry.register` checks duplicates using the raw provider name (`internal/llm/registry.go:78`), but stores provider specs under a lowercased name (`:95-97`). Snapshot publication also lowercases names and assigns into the resolver map (`:109-120`). Registering `EINO` after the default `eino` provider succeeds; `Registry.Get("eino")` still returns the original provider, while `SnapshotResolver().ResolveModel("eino", ...)` resolves through the later `EINO` provider. A disposable regression confirmed this split and resolved the canonical `eino` request to the later provider's configured endpoint.

At `1ce909b`, registration requires canonical lowercase names, checks aliases and IDs case-insensitively, rejects disabled native aliases, and rejects collisions before publishing a resolver snapshot. `Get`, `GetModel`, `GetModelInfo` local lookup, and `ModelResolver` use alias normalization. Current built-in Eino/OpenAI specs do not populate aliases; unknown custom identities still require an explicit endpoint.

### P2 — Model metadata was not part of the claimed immutable resolver snapshot (closed at `1ce909b`)

`WithModels` retains the caller's input slice (`internal/llm/provider.go:112-118`). `Models()` copies that slice when called (`:130-135`), but `providerDefinition` retains a live provider pointer (`internal/llm/registry.go:118`) and `resolveModelSpec` calls `provider.Models()` on each resolution (`internal/llm/model_resolver.go:278-286`). Mutating the original slice after `SnapshotResolver()` therefore changes model metadata returned by the existing resolver snapshot. A disposable regression confirmed that a pre-snapshot `ModelInfo.Description` changed from `before` to `after` in a later resolution.

At `1ce909b`, `WithModels`/`WithNativeModels` copy caller slices and resolver publication snapshots `provider.Models()` into its provider definition. `ModelInfo` contains scalar fields, so the item copy is sufficient. The new regressions cover caller mutation and mutation after snapshot. This was metadata consistency, not protocol or credential authority.

## Follow-up review at `1ce909b` and `575dff1`

### P2 — External model metadata fallback received an alias instead of the canonical provider ID (closed at `575dff1`)

`ProviderRegistry.GetModelInfo` canonicalizes casing and calls `Get`, which maps an alias to its provider (`internal/llm/registry.go:244-256`). When the model is absent from the provider's local list, however, its fallback calls `ModelRegistry.Lookup` with the original normalized alias (`:261-264`), not the canonical provider ID returned by `canonicalProviderNameLocked` (`:182-192`). A disposable test using a model registry keyed by canonical provider reproduced the mismatch: looking up `remote-model` via `PROVIDER-ALIAS` called the registry with `provider-alias` and returned an empty `ModelInfo`.

This was a metadata lookup inconsistency, not a credential or protocol authority bypass; a repository search found no current production callers of `GetModelInfo`. At `575dff1`, `GetModelInfo` maps the input through `canonicalProviderNameLocked` under the registry read lock and uses that canonical ID for both external fallback branches. Its new regression checks uppercase canonical and alias inputs plus a missing metadata result, and verifies both successful external lookups receive the canonical ID.

## Confirmed safeguards and non-blocking note

- The unexported `resolverProviderSpec` method prevents external `ModelProvider` implementations from self-declaring a protocol. The current supported OpenAI-compatible specs are inferred only for the concrete Eino/OpenAI adapters; native Claude/Gemini identities remain disabled before credential lookup or `Build` (`model_resolver.go:17-22, 312-344`; `registry_edges_test.go:47-55`).
- `AuthChain.Resolve` fails closed for provider-only lookup, while `ResolveCredential` accepts only sources with an exact-scope lookup (`internal/config/auth_chain.go:121-155`). Scoped CLI/file/environment tests and resolver canaries passed. No credential appeared in observed error/log paths during this review.
- `ProviderSpec.RequireCredential` is now enforced by `providerRequiresCredential` (`model_resolver.go:362-368`) only when the resolved normalized origin matches `spec.DefaultBaseURL`. The focused probe confirmed a request to an override origin did not inherit the constructor's default-origin key and did not fail for a missing key; the existing default-origin required-credential test remains part of the passing selection.
- The current default identities remain `eino` and `openai`; arbitrary custom names require an explicit compatible endpoint. No native protocol was enabled by model metadata or a provider name.

## Verification

- Scoped resolver, credential, native-gate, cross-origin redirect, endpoint, and custom-provider selection: `GOMAXPROCS=2 rtk go test -race -p 2 ./internal/llm -run '^TestProviderRegistry(RejectsNativeIdentityBeforeBuild|UsesMatchingScopedCredentialAtRealOrigin|ScopeMismatchPrecedesCredentialLookup|RequiresCredentialForOpenAIDefaultOrigin|AllowsConstructorCredentialOnlyOnSameOrigin|RejectsCrossOriginRedirectBeforeSecondDispatch|UnknownAliasNeedsExplicitEndpoint|PreservesExplicitOpenAICompatibleCustomEndpoint|AllowsExplicitCredentiallessCompatibleEndpoint|RejectsInlineCredentialWithMismatchedScope|DoesNotSendProviderDefaultKeyToOverrideOrigin|RejectsUnspecifiedCustomProvider)$' -count=1` — 12 tests passed.
- Scoped AuthChain and legacy-fallback isolation: `GOMAXPROCS=2 rtk go test -race -p 2 ./internal/config -run '^TestAuthChain(ProviderOnlyResolveFailsClosed|ResolveCredentialUsesExactScope|ScopedCLIFlagDoesNotCrossProviderOrOrigin|ScopedAuthFile|ScopedEnvironmentAndLegacyFallbackIsolation)$' -count=1` — 5 tests passed.
- Follow-up scope and collision suite: the exact selection of 14 `internal/llm` regressions (callback URL/Host rewrite, direct foreign redirect, canonical-name/alias collisions, model snapshot copies, credential scope and default-origin requirement) passed under `-race`.
- Alias metadata fix at `575dff1`: `rtk proxy go test -list '^TestProviderRegistryGetModelInfoUsesCanonicalIDForExternalLookup$' ./internal/llm` found the intended test, and `rtk proxy env GOMAXPROCS=2 go test -race -count=1 -p 2 ./internal/llm -run '^TestProviderRegistryGetModelInfoUsesCanonicalIDForExternalLookup$'` passed.
- A disposable override-origin probe passed: `RequireCredential` did not require an override-origin credential and the default-origin constructor key was absent from the resolved request. A disposable same-origin redirect positive control also passed with the scoped Bearer preserved.
- The pre-fix disposable redirect-callback reproducer failed its required zero-dispatch assertion: the foreign server received one request and the canary Bearer credential. It was removed after confirming the regression tests added at `1ce909b` pass.
- The disposable alias metadata fallback probe reproduced the bug at `1ce909b`; the targeted `TestProviderRegistryGetModelInfoUsesCanonicalIDForExternalLookup` passed under race at `575dff1`. It verifies canonical lookup for canonical and alias names and leaves missing metadata unknown.
- The builder's full `internal/llm` and `internal/config` race suite was not repeated. Core/CLI integration and the remaining R3 consumer paths were outside this fixed partial checkpoint.
