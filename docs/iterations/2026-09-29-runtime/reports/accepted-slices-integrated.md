# Accepted slice integration checks

Product source through `584d57b` on `codex/runtime-repair-20260929` includes
the independently accepted stream breaker, MCP stdio lifecycle/frame bounds,
and scoped quarantine read. Subsequent commits up to this report change only
documentation.

On Darwin arm64, each command below exited 0 after its corresponding source
was cherry-picked:

```sh
rtk env GOMAXPROCS=2 GOFLAGS='-p=2 -trimpath' go test -race -count=1 ./internal/production ./internal/llm ./internal/core ./internal/cli
rtk env GOMAXPROCS=2 GOFLAGS='-p=2 -trimpath' go test -race -count=1 ./internal/mcp ./internal/cli -run '^(TestOwnedStdio|TestAdapterDisconnectPreservesExternalConnection|TestInteractiveCmd_RegisterMCPTools_|TestNewMCPClientForDoctorSelectsStreamableHTTPAdapter|TestToMCPConfigSelectsStreamableHTTPForURL)'
rtk env GOMAXPROCS=2 GOFLAGS='-p=2 -trimpath' go test -race -count=1 ./internal/tools ./internal/core ./internal/session ./internal/cli
```

All selected packages reported `ok`. The first command predates the later MCP
and scoped-read source; the third runs on the combined product source. These
checks do not replace final fixed-SHA `make verify`, actual CLI crash/UI
matrices, or independent review of later source changes.
