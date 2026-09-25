# R4 WebSearch provider final review

**Reviewed chain:** `167b9c7c89ecce4ac31aaad9205d2f9114e034e7` → `80db3f92d48034c9cc062a9d34ac43dd905cb5a6` → `e4443709f16a18e47fd8dfeadfcc31b42239aa8c`  
**Final source worktree:** `/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/r4-websearch-auth-redirect-fix`  
**Verdict:** **ACCEPT** at `e4443709f16a18e47fd8dfeadfcc31b42239aa8c`.

This final review covers the complete provider-fail-closed, Brave-auth, and redirect-policy chain. It retains the earlier findings documented in `r4-websearch-provider-review.md` and supersedes that report's pending redirect finding after verifying the final commit.

## Review findings

- **Provider selection fails closed.** `NewWebSearchTool()` leaves its provider unset and returns `ErrWebSearchProviderUnavailable` on a valid query. Assembly leaves empty and explicit `mock` configuration unavailable; a missing Brave key returns the same typed error before network I/O. Config validation accepts only `""`, `mock`, `fetch`, and `brave`; an unknown value is rejected. See `internal/tools/web_search.go:11-70`, `internal/config/validator.go:52,102-104`, and `internal/cli/assemble.go:1329-1346`.
- **Brave uses the raw API key header.** The initial request sets `X-Subscription-Token` to the key without the erroneous `Bearer ` prefix (`internal/tools/web_search_provider.go:228-259`).
- **The redirect origin is pinned before `Client.Do`.** `Search` captures `initialOrigin` from the initial request URL before dispatch. The redirect wrapper compares every proposed redirect to that immutable origin before invoking the caller policy, then checks `req.URL` again after the callback. Origin comparison normalizes scheme and host case and the effective port (`internal/tools/web_search_provider.go:264-343`). A relative/path redirect on the same origin remains allowed; cross-host, cross-port, and cross-scheme redirects are rejected before the next transport dispatch.
- **The prior mutable-callback bypass is closed.** One regression mutates the pending `req.URL` to an attacker host and proves the post-callback check blocks dispatch. A second mutates `via[0].URL` during a same-origin hop, then attempts a cross-origin hop; the precheck uses the captured origin, rejects that hop, and does not call the caller callback again (`internal/tools/web_search_provider_test.go:228-323`).
- **The injected client is preserved.** The policy is installed on a shallow copy of the supplied `http.Client`; the original transport, timeout, and redirect callback remain unchanged. The wrapper preserves the caller callback's rejection behavior and preserves the default ten-redirect limit when no callback was configured (`internal/tools/web_search_provider.go:316-339`; tests at `internal/tools/web_search_provider_test.go:324-378`).
- **The credential is not forwarded or exposed by redirect errors.** Cross-origin cases use only `synthetic-review-key`, assert there is no second transport dispatch, and check that the returned error omits the key. The redirect response body is closed. No live credentials or external requests were used.

I found no remaining blocker in the requested provider or redirect scope. The `WithBraveSearchURL` constructor option can deliberately point a caller-created provider at a custom endpoint; production assembly uses the fixed HTTPS Brave endpoint and does not expose this option through configuration. The guard pins redirects relative to the selected initial endpoint, as intended.

## Exact tests listed and run

The four `go test -list` queries enumerated the following tests before the race run.

`internal/tools`:

```text
TestWebSearchMockWarning
TestWebSearchNoWarningForNonMockProvider
TestMockSearchProviderReturns3Results
TestMockSearchProviderName
TestFetchSearchProviderParsesHTML
TestFetchSearchProviderName
TestFetchSearchProviderServerError
TestFetchSearchProviderTimeout
TestFetchSearchProviderMaxResults
TestBraveSearchProviderParsesJSON
TestBraveSearchProviderBlocksCrossOriginRedirectBeforeDispatch
TestBraveSearchProviderAllowsSameOriginRedirectWithNormalizedOrigin
TestBraveSearchProviderRedirectPolicyRejectsCallerCrossOriginMutation
TestBraveSearchProviderRedirectPolicyUsesImmutableInitialOrigin
TestBraveSearchProviderPreservesDefaultRedirectLimit
TestBraveSearchProviderPreservesCallerRedirectPolicyWithoutMutatingClient
TestBraveSearchProviderName
TestBraveSearchProviderMissingKey
TestBraveSearchProviderServerError
TestWebSearchToolWithMockProviderNoPanic
TestWebSearchToolWithFetchProviderNoPanic
TestWebSearchToolWithBraveProviderNoPanic
TestWebSearchName
TestWebSearchDescription
TestWebSearchMissingProviderFailsClosed
TestWebSearchReturnsResults
TestWebSearchMissingQuery
TestWebSearchEmptyQuery
TestWebSearchContainsURLs
TestWebSearchDeterministic
```

`internal/config`: `TestDefaultValidator_WebSearchProviderValues`, `TestLoad_RejectsUnknownWebSearchProvider`.  
`internal/cli`: `TestAssembleAgent_WebSearchWithoutRealProviderFailsClosed`, `TestAssembleAgent_WebSearchFetchProvider`, `TestAssembleAgent_WebSearchBraveProvider`, `TestAssembleAgent_WebSearchBraveWithoutAPIKeyFailsClosed`.  
`tests/e2e_20260802`: `TestET_Phase19_AC4_WebSearchProviderConfigDriven`.

The corrected targeted race command passed in all four packages:

```text
rtk env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb go test -tags=e2e -race -p 2 -count=1 -run '^(Test(WebSearch|MockSearchProvider|FetchSearchProvider|BraveSearchProvider).*|Test(DefaultValidator_WebSearchProviderValues|Load_RejectsUnknownWebSearchProvider)|TestAssembleAgent_WebSearch.*|TestET_Phase19_AC4_WebSearchProviderConfigDriven)$' ./internal/tools ./internal/config ./internal/cli ./tests/e2e_20260802
```

Results: `internal/tools` 1.576s, `internal/config` 1.027s, `internal/cli` 1.158s, and `tests/e2e_20260802` 1.112s; all passed.

Targeted vet and full-chain whitespace checks also passed:

```text
rtk env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb go vet -tags=e2e ./internal/tools ./internal/config ./internal/cli ./tests/e2e_20260802
rtk git diff --check 167b9c7^..e4443709f16a18e47fd8dfeadfcc31b42239aa8c
```

These tests exercise local providers and redirect transports. They do not call a live Brave account or validate its external service availability.
