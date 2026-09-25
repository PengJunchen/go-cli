# R6 scoped-auth handoff design review

**Decision: approved for implementation after the handoff clarification.** The revised contract in `r6-auth-persistence-handoff.md` at `f12386c06fb90a462900b049ee2a4e9800936bcf` closes the auth-scope and keychain-failure gaps found in the initial handoff. This approval is limited to the scoped onboarding/auth-persistence slice; it does not approve or claim completion of the broader R6 entrypoint, session, trust, output, ACP, MCP, or Web work.

## Reviewed revisions and authority

- Main branch design authority: `b966f1c24a618c5afee98c535b7b3683e34c0a20` (`r6-entrypoint-design.md`).
- Source snapshot audited by the handoff: `b1d5cc92b0835fba5021f12ada0926e72cd9ba71`; the detached review checkout was at `41c7c6d5bb2588be3e3da96edf6b67499c9ebd0c`. The handoff clarification commit `f12386c06fb90a462900b049ee2a4e9800936bcf` changes documentation only.
- Review scope: onboarding persistence, exact `AuthScope` derivation and lookup, scoped keychain/file precedence, provider override isolation, and no-HOME behavior.

## Source-established findings

`REPLSession.start` parses interactive flags before onboarding, and `s.providerName` is resolved from the CLI override or loaded config. `newCLIModelResolver` binds an inline config key only when the configured provider identity matches the effective provider, then obtains the canonical provider/protocol/origin from `ModelResolver.CredentialRefFor`. That resolver normalizes provider identity and endpoint origin using the same path later used for model resolution. This is the correct source of the onboarding scope; deriving an origin separately from `Provider.Name` would be unsafe. The built-in `eino` provider supplies the OpenAI-compatible protocol and default OpenAI endpoint.

`AuthChain.ResolveCredential` uses exact scope and continues only for `ErrAuthNotFound`; any other source error stops resolution. The default source order is CLI-scoped key, keychain, auth file, scoped environment, and models file. The handoff now identifies the mismatch at `KeychainSource.GetScoped`: it currently converts every `security find-generic-password` execution error into `ErrAuthNotFound`. The revised contract separates a disabled/unavailable backend, a positively classified missing scoped item, and an actual access/operation/parse error. That distinction is necessary for the existing chain to fail closed before prompting.

The current onboarding code has two related hazards which the revised contract addresses. Its skip condition is `cfg.Provider.APIKey != ""`, even when a selected provider override has a different scope. Its input helper mutates `cfg.Provider.APIKey`; an override's newly entered key could therefore be attached to the original configured provider later. The revised contract makes the exact resolved scope the sole skip criterion, passes the newly entered key separately to scoped persistence, preserves any pre-existing inline key, and recomputes scope after final first-run provider selection/config mutation.

On HOME handling, scoped `authFilePath` and `modelsConfigPath` return no default path when HOME is empty, while the legacy `configFilePath` helper can construct a project-relative path. The scoped path must not call that legacy helper. The revised acceptance case plants a canary at CWD `.config/go-cli/auth.json` and proves no-HOME execution neither reads nor writes it.

## Approval conditions captured in the revised handoff

The handoff now requires the implementation and its tests to:

- Treat an unavailable keychain backend as disabled, only classify a proven missing exact item as `ErrAuthNotFound`, and propagate other `security` failures as source errors. Tests must demonstrate miss fallthrough and access/operation failures that stop before a prompt.
- Use the scoped keychain as the writer when available. If `SetScoped` fails, report a save error instead of claiming file fallback succeeded, because an old item at the same scope may still win by source precedence. File persistence is the fallback when the backend is unavailable.
- Exercise both onboarding paths, exact provider/protocol/origin binding, existing matching-scope skip, provider/origin mismatch, inline-key/CLI-override isolation, preservation of the original config key, and scope recomputation after first-run selection.
- Launch the absolute real CLI binary with isolated HOME, XDG config, working directory, and trace/output directories. On macOS, use a temporary `security` executable as the only PATH entry so keychain behavior is a controlled external-command simulation; explicitly do not describe it as OS-Keychain verification and do not add a product test-bypass variable.
- Seed the shim with an old exact-scope key, make `add -U` fail while `find` still returns the old key, and verify the command fails without claiming setup complete or a successful fallback. Also verify the HTTP peer receives the secret only in Authorization at the configured origin.

These conditions directly cover the identified stale-key shadow case and prevent a passing file-write assertion from masking that the higher-priority keychain still returns an older key.

## Verification evidence and limits

This was a source/design review only. No tests, runtime probes, OS Keychain operations, or CLI subprocess journeys were executed. Source and existing test names inspected include `AuthChain.ResolveCredential` / `TestAuthChainResolveCredentialUsesExactScope`, `TestLookupScopedModelsJSONUsesNoProjectRelativePathWithoutHome`, `TestLookupScopedModelsJSONReadAndParseFailuresAreSourceErrors`, `TestSmallModelOverrideDoesNotInheritDifferentProviderConfig`, `TestAssembledProviderFlagCanonicalAliasKeepsProviderConfig`, and the existing onboarding tests. These existing tests do not establish the newly specified scoped keychain error classification, stale-item write failure, or real-binary onboarding journey; those remain implementation acceptance tests, not review results.

No product source was changed. The reviewed source establishes that the resolver and scoped auth-chain seams are available; only the implementation tests can establish the new persistence behavior. This review makes no claim that native Claude/Gemini, OAuth, Assembly lifecycle, or the broader R6 gates are supported.
