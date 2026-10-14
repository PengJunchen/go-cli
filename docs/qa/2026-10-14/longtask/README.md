# Long-task binary and PTY regression gate

Baseline: `develop` at `e77e8058c51e5370a2ecc3eacb5fce0419829bcf`.

The gate in `tests/e2e_binary_longtask_test.go` builds the real `./cmd/cli`
binary with Go's race detector and talks only to an isolated local
OpenAI-compatible HTTP server. It covers three distinct behaviors:

1. A non-TTY `interactive` process consumes 120 real user submissions. The
   mock returns OpenAI SSE chunks; the test checks all 120 HTTP requests have
   `stream: true`, each request contains the corresponding current input, the
   last request preserves turn 120 and drops turn 1, and the production
   truncating compactor emitted its runtime log.
2. A PTY-backed interactive process receives a real Bash tool call, approves it
   through the TUI, enters a follow-up with `f`, and verifies both the user
   follow-up and the Bash tool result in the next HTTP request. A file written
   by Bash independently proves the tool side effect. The shell remains held
   behind a release file until the production follow-up queue-acceptance log
   appears, so visible editor text alone cannot release the command.
3. A second PTY process approves a Bash command that waits before writing a
   file, presses standalone Escape during execution, and verifies the CLI shows
   `[interrupted]`, returns before the delayed write, sends no continuation
   request, and leaves the file absent after the command's full delay.

The two PTY helper files allocate a native PTY on Darwin and Linux. Linux does
not skip based on the `CI` environment variable: the test attempts to allocate
and configure `/dev/ptmx`, and skips with the specific OS error only when that
host cannot provide a PTY. Linux CI hosts with PTY support run both scenarios.

Run from the repository root:

```sh
go test -race -tags=e2e ./tests -run '^TestE2EBinaryLongTask$' -count=1 -v
```

The child binary is separately built with `go build -race`; `go test -race`
alone would instrument only the test process. Each child receives a fresh
temporary HOME, XDG paths, TMPDIR, working directory, local mock endpoint, and
dummy API key. External API key variables and proxy variables are cleared.

The two PTY children run with `--no-sandbox`. These checks exercise the
approval flow, real Bash side effects, Escape cancellation, and actual local
HTTP request contents; they do not verify OS sandbox isolation or enforcement.

`results.json` records the actual run and evidence category. Raw test event logs
are under `rawlogs/`. This is a mocked-provider CLI/PTY regression check; it
does not claim validation against a real model provider.
