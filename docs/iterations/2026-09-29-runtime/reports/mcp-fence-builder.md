# MCP reload admission fence Builder report

## Baseline and scope

- Base: `17ab7b76ef0b7636abff1e5a055de7acf6f6bb31`.
- Worktree/branch: `repair-20260929-mcp-fence` / `codex/repair-20260929-mcp-fence`.
- Scope: synchronously fence MCP reload admission at Assembly close start, while keeping reloader stop requests and dependency cleanup in the existing asynchronous, single-owner sequence.

## Change

`assemblyCloseOwner` now retains a private typed reference to its `assemblyMCPResources`. Its `Close` owner publishes `closing=true` under the same mutex checked by `Reload`, before launching the asynchronous owner. Production wiring supplies the reference from `newAssembleState`. The new deterministic barrier test holds both an admitted reload and the asynchronous stop callback: post-close reload admission is rejected while Close is pending, repeated Close shares one stop/cleanup owner, and client cleanup stays behind the admitted reload's actual join signal. `RequestStop` remains outside the synchronous fence and outside the resource mutex.

## TDD and verification

- RED: before implementation, the new test compiled and failed at the post-Close `Reload` assertion because `mcp.ErrHotReloadPending` was absent while the asynchronous stop callback was held.
- `rtk run go test ./internal/cli -list AssemblyCloseFencesMCPReload`: listed the new test.
- `rtk go test ./internal/cli -run '^TestAssemblyCloseFencesMCPReloadBeforeAsyncStopAndJoinsAdmittedReload$' -count=1`: passed.
- `rtk go test -race ./internal/cli -count=1`: passed, 727 tests.
- `rtk go vet ./internal/cli`: passed.
- `rtk make lint`: passed, 0 issues.
- `rtk git diff --check`: passed.

This report records the scoped package/test checks above; it does not claim `make verify` or an end-to-end CLI journey.
