# Private ACP HTTP serve startup report

**Baseline:** `bda13f2b3162b4425c88b615d1690a5dcae4ef08`  
**Branch:** `fix/r4-acp-http-serve`

## Finding and change

`serveCmd.Run` wrote a generated bearer token to its destination before `HTTPServer.Start` attempted to bind. An occupied address therefore returned a startup error after publishing a new credential, and an existing token file could be replaced by a credential for a server that never started.

The command now waits for `HTTPServer.Start` to succeed before writing the generated token file. A bind failure leaves an absent token path absent and preserves any prior file. A token-file write failure closes the Assembly-owned server before returning, and the listening success message remains after both server start and token-file persistence.

The regression test first failed on the baseline implementation in both cases: the absent-path case found a generated token, and the existing-path case found that the old token had been replaced. Token values are omitted from this report. The fixed test also releases the occupied socket and confirms that the failed command left no listener behind.

## Unsupported remote interaction

The existing `--approve ask` preflight rejects before Assembly, token-file mutation, or listener startup. Added coverage confirms the unsupported `--tui` flag is rejected at argument parsing, before Assembly or token-file mutation, while preserving a prior token file.

## Validation

- `rtk env GOMAXPROCS=2 GOFLAGS='-p=2 -trimpath' go test -race -count=5 ./internal/cli -run '^(TestServe(BindFailureDoesNotPublishTokenOrLeaveListener|TokenFileFailureDoesNotStartListener|RemoteTUIRejectedBeforeAssemblyOrTokenWrite|ApproveAskRejectedBeforeTokenWriteOrListener|AssemblyFailureDoesNotWriteTokenOrStartListener)|TestServeStartsAssembledRuntimeWithExplicitDenyOnLoopback)$'` — passed.
- `rtk env GOMAXPROCS=2 GOFLAGS='-p=2 -trimpath' go test -race -count=1 ./internal/cli -run '^TestServe'` — passed.
- `rtk env GOMAXPROCS=2 GOFLAGS='-p=2 -trimpath' go test -race -count=1 ./internal/acp` — passed. This includes HTTP authentication, opaque session and alias isolation, event routing, and bounded dispatch admission tests.
- `rtk env GOMAXPROCS=2 GOFLAGS='-p=2 -trimpath' go vet ./internal/cli ./internal/acp` — passed.
- `rtk git diff --check` — passed.

## Limits

This is a serve startup transaction slice. It does not claim the final-verification plan's real-binary `connect → send → stream/events → disconnect` journey or multi-session agent dispatch through the assembled production provider. `TestServeStartsAssembledRuntimeWithExplicitDenyOnLoopback` verifies that the actual Assembly-backed command starts; ACP package tests separately exercise the HTTP route, identity, and admission contracts. No official ACP or gRPC interoperability is claimed. This report does not mark R4 or R0–R8 complete.
