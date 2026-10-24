# Long-task binary and PTY regression gate

> Retention note (2026-10-24): raw event JSONL and cross-compile output files were removed after their conclusions were preserved in `evidence.md` and `results.json`. References below are historical provenance, not available raw files.


Starting baseline: `develop` at `e77e8058c51e5370a2ecc3eacb5fce0419829bcf`.
The integrated verification and tagged E2E gates passed on
`6aa6f9318f3d3c40822c368e489c9284cd932072`; see `results.json` and
[`evidence.md`](evidence.md) for the run details.

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
   `[interrupted]`, returns before the delayed write, sends no automatic
   continuation request, and leaves the file absent after the command's full
   delay. A separate explicit user prompt then completes as a fresh turn.

The PTY cases wait for shell-written PID and start files to prove Bash began;
text in the approval view is only a command preview. Follow-up also waits for
the production queue-acceptance log before releasing Bash, then checks the
actual next HTTP request and side-effect file. Failure cleanup signals a Bash
process group only after matching the fixture PID, process-group ID, and unique
command token twice. Cleanup does not signal an unverified process group after
the Bash leader exits. The cancellation fixture's foreground sleep can then
remain for at most five seconds; it cannot write the side-effect file without
its parent, and the test waits through the full delay before checking it.

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
