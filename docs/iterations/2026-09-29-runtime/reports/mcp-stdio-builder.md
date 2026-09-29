# MCP stdio lifecycle and response-bound Builder report

## Baseline and scope

- Base: `1144e1ebc9f339a04d502f0bdca04069c9d30ff5`.
- Worktree/branch: `/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/repair-20260929-mcp-stdio` / `codex/repair-20260929-mcp-stdio`.
- Owner lifecycle source: `4c73ad49f070327a48d46619a3efacf2c85dd691`.
- Bounded stdio framing source: `81a3eed1848249b14ad8a3d2457b2551ccd3ca04`.
- Scope: private ownership and completion for stdio children and their active transport I/O, plus a bounded stdio response reader. No HTTP adapter, registry, CLI, core, or reload code changed.

## Change

The stdio adapter now retains one private owner from successful child start until the exact `exec.Cmd` has been waited and active transport I/O has returned. Failed and canceled initialize handshakes close only the owned pipes and kill through the retained `Process` handle. Cleanup joins before returning when it completes within the caller's context; if the caller's context ends first, the operation returns a typed pending error carrying the exact completion channel while retaining the owner. `Disconnect` fences an in-flight connect and shares that single cleanup operation. A request cancellation callback starts the same cleanup, interrupts owned pipe I/O, and is joined before the request returns. It does not create a detached reader goroutine. Caller-supplied transports remain caller-owned; their owners must release blocked reads.

Stdio reads now use bounded `ReadSlice` fragments and a 10 MiB byte budget per serialized request. The budget includes notifications, malformed frames, and responses with unrelated IDs. Fragment-size checks happen before append, and buffer capacity cannot grow past the cap. A complete newline-terminated response exactly at the cap is accepted; an oversized or unterminated frame returns a typed stdio protocol error. Owned stdio protocol failures trigger the same child cleanup and expose its exact `ExecutionDone` receipt. A dispatched `tools/call` remains outcome-unknown and is not replayed. The HTTP adapter and its behavior are unchanged.

## TDD and verification

- Owner lifecycle RED against the base exposed failed initialization dropping the child handle before a positive `Wait`, and a canceled call remaining blocked on a silent peer without a joined execution receipt.
- Bounded-read RED compiled on the owner-lifecycle source. Oversized newline, unterminated, and aggregate ignored-frame cases returned only a context-deadline error instead of a typed protocol failure. The exact-size boundary case passed before the bound was added.
- `rtk env GOMAXPROCS=2 go test -race ./internal/mcp -run '^TestOwnedStdio' -count=10 -timeout=120s` — passed on owner-lifecycle source.
- `rtk env GOMAXPROCS=2 go test -race ./internal/mcp -run '^TestOwnedStdioResponseBudget' -count=3 -timeout=60s` — passed on the combined source.
- `rtk env GOMAXPROCS=2 go test -race ./internal/mcp -count=1` — passed.
- `rtk env GOMAXPROCS=2 go test -race ./internal/cli -run '^(TestInteractiveCmd_RegisterMCPTools_(NilConfig|EmptyServers|StdioServer|StreamableHTTPServer)|TestNewMCPClientForDoctorSelectsStreamableHTTPAdapter|TestToMCPConfigSelectsStreamableHTTPForURL)$' -count=1` — passed.
- `rtk go vet ./internal/mcp ./internal/cli` — passed.
- `rtk golangci-lint run --allow-serial-runners ./internal/mcp` — passed with no issues.
- `rtk git diff --check` — passed.

The helper process exercises actual stdio MCP framing and child cleanup, but it is a Go test helper rather than the production CLI process tree. These checks do not establish production CLI orphan behavior or a full CLI end-to-end journey. Full `make verify` was not run.
