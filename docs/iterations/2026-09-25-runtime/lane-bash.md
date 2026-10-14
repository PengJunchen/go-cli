# Streaming Bash lane evidence

Branch: `codex/runtime-r0-bash`
Worktree: `/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/bash`
Base: `0014900`

`StreamingBashToolImpl` now gives stdout and stderr one shared `MaxOutput`
budget, matching the public option and the regular Bash tool's shared writer.
It keeps only complete lines within that payload budget, forwards those lines,
and adds one fixed `[output truncated]` marker to the result and stream sink when
later output is discarded. The marker is fixed-size overhead outside the
retained payload cap. `ToolResult.Metadata["truncated"]` makes that state
machine-readable.

Both pipe readers continue scanning after the shared cap. If Scanner stops on a
token or read error, the reader marks truncation, drains the remaining pipe,
records `read_error`, and returns the wrapped error after `cmd.Wait`. If that
drain fails, it cancels the command context. Readers are joined after their
pipes reach EOF; this is not a guarantee that `ExecuteStreaming` returns by its
context deadline. `exec.CommandContext` kills the direct Bash process, and
`wg.Wait` precedes `cmd.Wait`; a descendant that inherited stdout/stderr can
keep the pipe readers blocked after Bash is killed. The finite-child repro below
measured a 711 ms return for a 100 ms deadline. This process-tree ownership
limitation is pre-existing in base `0014900` and remains for R1.

The cancellation path currently returns a wrapped `context.Canceled`, while
its log event is `streaming_bash.timeout` and metadata says `error=timed out`.
A line longer than Scanner's 1 MiB token limit is not partially retained: its
token bytes buffered by Scanner are discarded, the remaining pipe is drained,
the result is marked truncated, and the scanner error is returned to the caller
and recorded in `read_error`.

R1 acceptance: define platform-appropriate ownership for descendants created
by shell commands; cancellation/deadline must stop or contain descendants that
hold inherited pipes, join readers, and return within a documented cleanup
bound. Verify with finite and timeout-resistant child processes, inherited
stdout/stderr, cancellation and deadline cases, and process-leak checks. R0
tests continue to use builtins only and do not claim process-tree cancellation.

The red log records the previous timeout behavior for finite 200 KB stdout,
simultaneous stdout/stderr output, and a 2 MB single-line token. The green log
records the exact passing StreamingBash test list and the full `internal/tools`
race run:

- [bash-red.log](bash-red.log)
- [bash-green.log](bash-green.log)
- [bash-descendant-repro.log](bash-descendant-repro.log)

The long-line case now drains and returns a clear scanner error with bounded
output and `truncated=true`; it does not silently claim complete capture.
