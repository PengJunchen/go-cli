# R6 scoped-auth onboarding review

## Verdict

**Not approved at `ae89cd94613ff99919c321b3d9016427b0f4954b`.** The scoped credential persistence path is substantially implemented, but the onboarding model selection can report a model that is not effective, and an existing integration test now reaches the host macOS Keychain instead of an isolated fixture.

## Fixed snapshot and scope

- Reviewed commits: `6c8021f4aeedcc16783f92d7f92c247701d7b682` and `ae89cd94613ff99919c321b3d9016427b0f4954b`; review worktree was clean at `ae89cd9` after removing a temporary local model-selection probe.
- Design authority: `docs/iterations/2026-09-25-runtime/r6-auth-persistence-handoff.md` and `r6-entrypoint-design.md`.
- Reviewed onboarding scope resolution and persistence, scoped auth/keychain behavior, private sidecar locking, phase-48 coverage, and the real-binary CLI test. No product source changes were retained in the review worktree.

## Findings

### R6-AUTH-1 [P2] The wizard can claim a model selection that is not effective

`promptModel` only writes the selected model when `cfg.Provider.Model` and `cfg.Model.Name` are empty, but it always prints `Model set to: <chosen>` (`internal/cli/onboarding.go:305-344`). The environment loader can populate `Provider.Model` from `GO_CLI_PROVIDER_MODEL` (`internal/config/loader.go:226`), and `resolveModelName` prefers `Provider.Model` over `Model.Name` when no explicit `--model` flag is supplied (`internal/cli/prompt.go:313-323`).

I reproduced this with the real CLI binary, isolated HOME/CWD, a fake `security` executable, no existing config, and `GO_CLI_PROVIDER_MODEL=gpt-4o-mini`. The wizard listed its fallback choices and the input selected `gpt-4o`; output said `Model set to: gpt-4o`, while saved `provider.model` remained `gpt-4o-mini` and the effective startup model was `gpt-4o-mini`. The selected choice therefore does not take effect, despite the success message. Preserve explicit `--model` precedence, and either apply the wizard choice over an environment-provided default or state clearly that the environment value remains effective. The reviewed snapshot does not contain a regression test or fix for this case.

### R6-AUTH-2 [P2] An existing integration test invokes the host Keychain

The full affected-package race run fails `TestE2E_Phase49_OnboardingAutoTrigger` at `tests/e2e_phase49_test.go:1015`. Its existing-config path now calls `defaultSaveAPIKey`, which uses `KeychainSource.SetScoped` whenever the macOS backend is available (`internal/cli/onboarding.go:538-548`). The test has no fake `security` shim or unavailable-keychain override; its comment that the path avoids the OS Keychain is stale. On this host the attempted `security add-generic-password` returned exit status 154, so onboarding returned an error. I did not inspect or alter the host Keychain state. Isolate the test with an injected fake/unavailable backend or a temporary `security` shim and assert it never reaches the host command.

## Source review and evidence

The save and lookup paths otherwise follow the intended exact-scope shape: onboarding recomputes the scope after wizard choices (`internal/cli/onboarding.go:198-209`), derives it through the resolver's `CredentialRefFor` path (`internal/cli/assemble.go:1717-1726`), keeps prompted keys separate from `cfg.Provider.APIKey`, and serializes config without the key. An available keychain write failure is returned directly rather than falling through to a shadowing auth file (`internal/cli/onboarding.go:538-548`). Scoped file upserts validate the whole current document, preserve other valid scopes, and replace the matching scope under a stable sidecar lock (`internal/config/auth_chain.go:337-417`, `internal/privatefile/lock.go:16-49`). The lock checks cancellation, validates a private regular sidecar, and intentionally keeps the lock inode after release. Darwin/Linux use `flock`; other platforms fail closed.

Relevant maintained tests include `TestRunOnboardingProviderOverrideKeepsInlineKeyOnConfiguredScope`, `TestDefaultSaveAPIKeyDoesNotFallbackAfterAvailableKeychainFailure`, `TestUpsertScopedCredentialFileConcurrentProcessesPreserveBothScopes`, `TestWithExclusiveLockWaitIsCancellable`, `TestET_Phase48_Onboarding_YAMLEscaping`, and `TestAuthPersistenceRealCLIOnboardingThenRestartedPrompt`. The provider-override unit test uses `setupOnboardingTest`, which replaces the scope callback with a synthetic test scope; its separate resolver assertions do not prove that the newly saved override key used the production resolver's canonical origin. The real-binary test covers an existing-config save followed by a restarted `prompt` request to a local HTTP server, but that second command does not exercise interactive onboarding-skip behavior. These remain coverage limits against the handoff's stronger override/restart journey.

## Validation evidence

The targeted real-binary restart test passed with race detection:

```text
rtk env GOMAXPROCS=2 go test -race -p 2 ./cmd/cli -run '^TestAuthPersistenceRealCLIOnboardingThenRestartedPrompt$' -count=1
ok github.com/pengjunchen/go-cli/cmd/cli
```

The full affected-package run did **not** pass:

```text
rtk env GOMAXPROCS=2 go test -race -p 2 ./internal/privatefile ./internal/config ./internal/cli ./cmd/cli ./tests -count=1
```

`internal/privatefile`, `internal/config`, and `internal/cli` passed. `cmd/cli` also reported `TestRunSuccessNoArgs` failing because the reviewed branch predates `785f33a`, which isolates that legacy bare-interactive fixture; this is a known baseline fixture issue, not attributed to the auth diff. The `tests` package failed only `TestE2E_Phase49_OnboardingAutoTrigger`, described above. A focused race rerun of that test reproduced the `security add-generic-password` error.

The model mismatch was reproduced with a local binary and disposable HOME/CWD, but the reproduction was not retained as a source test. No actual OS Keychain contents were read back or changed as part of the review.

## Limits

This is a bounded implementation review, not a complete R6 release sign-off. In particular, the committed real-binary journey does not yet prove a second interactive startup skips onboarding from the persisted exact scope, nor a real `--provider=eino` override with an OpenAI inline key and production-derived canonical endpoint scope. The two findings above require a delta before accepting this snapshot.
