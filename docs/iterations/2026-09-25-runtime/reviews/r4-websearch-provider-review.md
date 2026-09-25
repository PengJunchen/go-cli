# R4 WebSearch provider review

Reviewed commit: `167b9c7c89ecce4ac31aaad9205d2f9114e034e7` (`codex/r4-websearch-fail-closed`)

Verdict: **REJECT pending fixes**. The implementation correctly fails closed when no usable provider is configured, but the Brave path has a credential-format bug and a cross-origin redirect leak. The config validator also accepts unknown WebSearch provider names.

## Findings

### P1 — Brave API key can be forwarded to a cross-origin redirect

`internal/tools/web_search_provider.go` constructs an empty `http.Client` by default and calls `Do` with `X-Subscription-Token` set. No redirect policy is installed. Go's documented client behavior forwards all initial headers except a small set of special sensitive headers; `X-Subscription-Token` is not one of them. The default client follows redirects, so a compromised or misconfigured search endpoint can return a cross-origin redirect and receive the configured key.

I verified the behavior with a controlled local HTTP probe using a synthetic key and a redirect between `127.0.0.1` and `localhost` (distinct origins). The target received `X-Subscription-Token: synthetic-review-key`. No real credential was used.

Required fix: use a per-request cloned client with a redirect policy that rejects a cross-origin target before issuing the redirected request (or otherwise guarantees the token is removed). Do not mutate a shared client. Add a local redirect regression proving the second origin receives no token, and preserve any injected client's transport/timeout settings.

Sources: [Go `net/http.Client` redirect behavior](https://pkg.go.dev/net/http#Client) documents that ordinary headers are forwarded and lists the specially filtered headers; [Brave authentication guide](https://api-dashboard.search.brave.com/documentation/guides/authentication) identifies the API key as confidential.

### P2 — Brave authentication header incorrectly prefixes the key with `Bearer `

`internal/tools/web_search_provider.go` sets `X-Subscription-Token` to `"Bearer " + p.apiKey`. Brave's official format is the raw API key: `X-Subscription-Token: YOUR_API_KEY`. Current assertions in `internal/tools/web_search_provider_test.go`, `internal/cli/assemble_test.go`, and `tests/e2e_20260802/17_phase19_wiring_e2e_test.go` encode the same incorrect prefix, so the existing local tests pass while production Brave requests are malformed.

Required fix: send the raw key and update the provider, assembly, and E2E request-shape assertions to verify the exact raw value. No live provider key is needed.

Source: [Brave authentication guide](https://api-dashboard.search.brave.com/documentation/guides/authentication).

### P2 — Unknown provider names pass config validation

`internal/config/loader.go` invokes the configured validator, but `DefaultValidator.Validate` in `internal/config/validator.go` has no WebSearch provider check. An unknown value such as `web_search.provider: not-a-provider` therefore loads successfully; `assembleExtraTools` only logs a warning and installs an unavailable tool. Invocation still fails closed with `ErrWebSearchProviderUnavailable` and does not fabricate results, which is good runtime behavior, but it does not reject a typo at the config boundary.

Required fix: define and validate the supported configured values (including the documented empty/default and explicit `mock` semantics), and add validator/loader tests showing a typo is rejected while supported values remain valid. Keep the existing invocation-level fail-closed tests.

## Behaviors that passed review

- An unset provider, explicit mock provider, and an unknown provider do not return fabricated search results; the assembled tool returns `ErrWebSearchProviderUnavailable` and makes no request.
- Brave without an API key remains unavailable before network I/O, and the provider identity remains `brave`.
- Fetch and Brave remain wired through the protected assembled tool registry. The Phase 19 config-driven test exercises that assembled tool path.
- The change explicitly injects the mock provider in the prepared-policy test; it does not weaken approval checks or replace the protected registry path.

## Validation evidence

All commands below ran in the reviewed worktree and passed with exit code 0:

- `rtk go test -p 2 -race -count=1 -run '^Test(WebSearch|MockSearchProvider|FetchSearchProvider|BraveSearchProvider)' ./internal/tools`
- `rtk go test -p 2 -race -count=1 -run '^TestAssembleAgent_WebSearch' ./internal/cli`
- `rtk go test -tags e2e -p 2 -race -count=1 -run '^TestET_Phase19_AC4_WebSearchProviderConfigDriven$' ./tests/e2e_20260802`
- `rtk git diff --check HEAD^ HEAD`

These tests validate local request wiring and fail-closed behavior; they do not validate a live Brave account or endpoint. The redirect finding was established separately by the synthetic-key local probe described above.

## Independent review of 80db3f92d48034c9cc062a9d34ac43dd905cb5a6

**Target chain:** `167b9c7c89ecce4ac31aaad9205d2f9114e034e7` + `80db3f92d48034c9cc062a9d34ac43dd905cb5a6`, reviewed in clean worktree `codex/r4-websearch-auth-redirect-fix`.  
**Verdict:** **REJECT pending one P1**. The raw Brave header format, unknown-provider validation, default-unavailable behavior, explicit mock injection, missing-key fail-closed path, Fetch path, and normal redirect cases are covered and pass. The redirect guard can still be bypassed by a caller-supplied `CheckRedirect` callback that mutates its arguments, forwarding the API token cross-origin.

### P1 — Mutable caller redirect callback can bypass the credential origin guard

`braveSearchClientWithRedirectPolicy` checks `via[0].URL` against the pending `req.URL`, then invokes the injected client's original `CheckRedirect` and returns its result (`internal/tools/web_search_provider.go:284-299`). Both callback arguments are mutable. If the caller callback rewrites `req.URL` to another host and returns nil, `http.Client` dispatches that rewritten request after the guard has already passed. On later hops, the callback can instead mutate `via[0].URL`; the guard then compares against that altered URL rather than the original Brave endpoint.

Two temporary RoundTripper probes reproduced token forwarding using only `synthetic-review-key`:

- A same-origin redirect callback changed the pending request URL to `https://attacker.example/exfil`. The transport observed `hosts=[search.example attacker.example]` and the second request still carried `X-Subscription-Token`.
- A callback changed `via[0].URL` after the first same-origin redirect; the next response redirected to `attacker.example`. The transport observed `hosts=[search.example search.example attacker.example]` and again received the token.

The guard should capture the original request origin as an immutable tuple before `Do`, use that tuple for prechecks, run the caller callback to preserve its deny/`ErrUseLastResponse` behavior, then validate the final pending `req.URL` against the captured origin before allowing the redirect. Do not use callback-mutable `via[0].URL` as the security baseline. Add regressions for both request-URL and `via[0].URL` mutation; the client copy should remain unmodified. These probes establish a credential leak in the injected-client path, not an issue with ordinary nil or non-mutating callback behavior.

### Confirmed behavior and validation

- Brave now sends the raw API key in `X-Subscription-Token`, and the assembly and Phase 19 assertions expect that exact value.
- Redirect checks compare scheme, normalized host, and effective port; ordinary cross-host, cross-port, and cross-scheme redirects are rejected before a second transport dispatch. Same-origin redirects including scheme/host case and explicit default-port normalization pass. The default ten-request limit and caller redirect rejection are preserved; tests confirm the injected client is not mutated.
- The redirect error assertion checks the synthetic key is absent. HTTP status errors contain the status only; response bodies are not added to the error.
- Config tests include a file-backed `NewLoader().WithFile(path).Load(...)` rejection for an unknown provider plus accepted empty, `mock`, `fetch`, and `brave` values. Assemble/E2E tests confirm default and configured `mock` stay unavailable, explicit provider injection remains available, Brave with no key issues zero HTTP requests, and Fetch still selects its provider.
- Listed targets before execution. Race tests passed: `internal/tools` Brave/Fetch/WebSearch/Mock providers; config validator and loader; CLI WebSearch assembly; and `TestET_Phase19_AC4_WebSearchProviderConfigDriven` under the `e2e` tag. `git diff --check 167b9c7c89ecce4ac31aaad9205d2f9114e034e7 80db3f92d48034c9cc062a9d34ac43dd905cb5a6` passed.
- Both callback-mutation probes were temporary and removed. No real key or external network was used; the review worktree is clean at `80db3f92d48034c9cc062a9d34ac43dd905cb5a6`.
