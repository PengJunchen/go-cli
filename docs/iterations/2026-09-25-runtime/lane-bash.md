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
drain fails, it cancels the command context so the tool does not wait forever
on a producer whose pipe cannot be consumed. Reader goroutines are joined
before `ExecuteStreaming` returns. Tests use Bash builtins only and start no
child processes; process-tree ownership remains outside this lane.

The red log records the previous timeout behavior for finite 200 KB stdout,
simultaneous stdout/stderr output, and a 2 MB single-line token. The green log
records the exact passing StreamingBash test list and the full `internal/tools`
race run:

- [bash-red.log](bash-red.log)
- [bash-green.log](bash-green.log)

The long-line case now drains and returns a clear scanner error with bounded
output and `truncated=true`; it does not silently claim complete capture.
