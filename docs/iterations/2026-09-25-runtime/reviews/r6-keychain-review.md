# R6 keychain error classification review

## Verdict

**Not approved at `ad1f02000c3f0a6dc08528e05cc97965d17bcc2c`.** Scoped command failures now fail closed on the tested macOS path, but this commit has a Linux test failure and an invalid-scope classification gap when the backend is disabled. Both need a small delta before acceptance.

## Fixed snapshot and scope

- Reviewed commit: `ad1f02000c3f0a6dc08528e05cc97965d17bcc2c`, checked out in a clean detached review worktree.
- Design authority: `docs/iterations/2026-09-25-runtime/r6-auth-persistence-handoff.md`, especially its three-outcome contract (line 19) and fake `security` subprocess requirement (line 33).
- Reviewed only `internal/config/keychain.go`, `internal/config/keychain_test.go`, and the existing scoped `AuthChain` behavior they depend on. No product source was changed. Temporary probes were removed.

## Findings

### R6-KC-1 [P2] The “empty success output” test fails on Linux

`TestKeychainExecClassifiesOnlyConfirmedMissingItem` explicitly runs on both Darwin and Linux (`keychain_test.go:262-264`). Its final subtest constructs `NewKeychainSource()` and calls `GetScoped` (`keychain_test.go:290-295`). On Linux, `KeychainSource.Available()` returns false because `runtime.GOOS != "darwin"` (`keychain.go:63-71`); `GetScoped` therefore returns `ErrAuthNotFound` before the shim is invoked, while the subtest asserts that the error is *not* `ErrAuthNotFound`. The failure is statically established from the cross-platform guard and implementation; this review host is Darwin, so I did not claim a Linux execution. Inject an available fake source for this subtest, or split/platform-limit it while retaining an explicit empty-success source-error assertion.

### R6-KC-2 [P2] An unavailable backend masks an invalid scope

`GetScoped` checks `Available()` before calling `scopedCredentialAccount` (`keychain.go:120-128`). If the backend is disabled and the input scope is invalid, the method returns `ErrAuthNotFound` instead of the invalid-scope source error. A temporary probe using `availableFunc: false` and `AuthScope{}` observed `ErrAuthNotFound=true` (`auth: key not found in source`). The existing `TestKeychainSource_GetScopedInvalidScopeIsSourceError` only uses an available fake backend, so it misses this order. Validate/normalize the requested scope before deciding that an unavailable backend can be skipped. The normal `AuthChain.ResolveCredential` currently normalizes the scope before calling sources, so this finding concerns the exported `GetScoped` boundary and its promised error classification.

## Source-established behavior

`keychainExec` captures stderr and classifies an item miss only when the executable name is `security`, the process exited with `*exec.ExitError`, and stderr contains the canonical missing-item message (`keychain.go:42-59`). `GetScoped` translates only that private sentinel to `ErrAuthNotFound`; other runner errors are wrapped as source errors, and empty successful output is a source error (`keychain.go:120-140`). `AuthChain.ResolveCredential` continues only for `ErrAuthNotFound`; any other source error stops resolution (`auth_chain.go:143-160`). On non-Darwin systems or when the `security` executable is absent, `Available()` disables the backend.

The legacy unscoped `Get` method still maps all runner errors to `ErrAuthNotFound`; the scoped resolver uses `LookupScoped`/`GetScoped`, and the approved handoff keeps legacy reads out of the scoped fallback path. I did not treat that unchanged legacy behavior as a defect in this scoped-classification commit.

## Validation evidence

`go test -list` enumerated the keychain and auth-chain cases before execution. The targeted race run was:

```text
rtk proxy go test -race ./internal/config -run '^Test(Keychain|AuthChain|NewAuthChain)' -count=1
```

It passed 29 matching tests on Darwin. The target includes `TestKeychainSource_GetScopedAccessFailureIsNotNotFound`, `TestKeychainSource_GetScopedInvalidScopeIsSourceError`, `TestKeychainSource_GetScopedFailureStopsAuthChainFallback`, `TestKeychainSource_GetScopedConfirmedMissingItemFallsThrough`, `TestKeychainSource_GetScopedUnavailableBackendFallsThrough`, and `TestKeychainExecClassifiesOnlyConfirmedMissingItem`.

The full config-package race run also passed:

```text
rtk proxy go test -race ./internal/config -count=1
```

It completed in 1.083s on the review host. The shell-shim test runs a temporary executable named `security` from a temporary directory placed alone on `PATH`; its missing-item, denied-with-secret-on-stderr, and empty-output cases do not use the user's keychain. Other keychain unit tests use injected `run` functions. `TestKeychainSource_Available` only checks `LookPath` and does not invoke the OS command. I confirmed the focused auth-chain tests replace sources before resolution, so they do not reach the real keychain.

The added denied-command case places a canary secret in shim stderr and asserts it is absent from the returned error. The production runner captures but does not append stderr to its error, and scoped resolver logs contain source/provider/protocol/found metadata rather than credential values.

## Limits

The tests ran on macOS only; Linux behavior was reviewed from source and the test guard, not executed on a Linux host. The fake `security` shim verifies command classification, not OS Keychain integration, prompts, authorization, or localized diagnostics. This review did not exercise the onboarding subprocess, scoped-file persistence, or a real provider request. The two findings above keep this fixed snapshot unapproved pending a targeted delta review.

## Follow-up review at `778eec5`

The bounded fix maps source `ad1→29e66a1` and follow-up `ba01→778eec5`. Root's independent source review accepted the two classification corrections; this supersedes the rejected verdict above only for those findings and does not accept the broader R6 task.

- R6-KC-1 is fixed by making the empty-success shim test explicitly inject an available backend, so it reaches the command-output classification on every platform. The source correction is cross-platform, but the fixed tests were run only on macOS; Linux execution remains unverified.
- R6-KC-2 is fixed by validating the scope before checking backend availability. The new regression verifies that an unavailable backend does not turn an invalid scope into `ErrAuthNotFound` and does not invoke the `security` runner.

At the fixed main snapshot, `keychain_test.go` contains 17 top-level `TestKeychain` functions. The enumerated `^TestKeychain` race selection passed in 1.030s on macOS:

```text
go test -race -p 2 -count=1 ./internal/config -run '^TestKeychain'
```

This accepts only the scoped keychain error-classification slice. It does not verify Linux runtime behavior, actual OS Keychain access, or the remaining R6 entrypoint and release gates.
