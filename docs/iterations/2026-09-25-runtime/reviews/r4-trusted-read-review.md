# R4 trusted-read source review

Reviewed baseline: `5fee63bc46f00f2d6833306abf39eaeb4158fd52` (`codex/runtime-stabilization`). This records source findings and an effect-classification boundary for the R4 implementation; it does not approve a product change.

## Findings

- **Grep has a real option/config execution path.** [`grep.go`](../../../../internal/tools/grep.go#L202) appends the compiled pattern as a bare argument before the resolved path, then invokes `rg` with inherited environment. A pattern beginning with `--pre=sh` is valid as a Go regexp but can be parsed by ripgrep as an option that starts an external preprocessor; `RIPGREP_CONFIG_PATH` can also inject ripgrep options. Keep `rg` available only with `--no-config`, remove `RIPGREP_CONFIG_PATH`, pass the pattern through `-e`, and terminate options with `--` before the path. These are argv/config hardening, not a general OS sandbox.
- **Find has an fd execution path.** [`find.go`](../../../../internal/tools/find.go#L169) places the caller's pattern in option position before the search path. `fd` accepts `--exec`/`--exec-batch`; a dash-leading pattern can therefore change the operation into command execution. Add `--` before the pattern. The current `find` fallback passes the resolved root first and the pattern as the value of `-name` ([`find.go`](../../../../internal/tools/find.go#L203)), so that particular expression does not expose the same option path.
- **Read and ls are implemented with Go filesystem calls.** [`read.go`](../../../../internal/tools/read.go#L104) uses `Lstat`/`Stat`/`Open`/`ReadDir`/`ReadAll`; [`ls.go`](../../../../internal/tools/ls.go#L114) uses `Stat`/`WalkDir`. I found no subprocess or repository-config execution path in these operations. However, default `RegisterDefaults` creates `NewReadTool()` without a workdir or path-whitelist option, and `ReadTool.resolvePath` reaches [`resolveWithinWorkdir`](../../../../internal/tools/write.go#L66), which accepts absolute paths as-is. Thus `read` can read outside the workspace even when `RegisterDefaults` receives a path whitelist; [`builtins.go`](../../../../internal/tools/builtins.go#L125) wires that optional whitelist to grep/find/ls, but not read. Grep/find/ls also accept absolute paths when no whitelist is configured; when one is configured, their `resolveAndValidatePath` path enforces it. This is not execution/write, but it prevents a workspace-bounded `read` claim without a separate path-scope fix.
- **Git status, diff, and blame cannot be certified as read effects from this implementation.** The Go wrapper uses `exec.CommandContext("git", ...)` and inherits the caller's environment ([`git.go`](../../../../internal/tools/git.go#L675)). `Status` runs fixed `status --porcelain` ([`git.go`](../../../../internal/tools/git.go#L213)); Git normally refreshes and writes cached stat data to `.git/index`, while `--no-optional-locks` suppresses that optional write. The status content comparison can still reach `index_fd` from `ce_compare_data` in racy-stat cases; that conversion uses the path's clean/process filter. `Diff` runs `diff` ([`git.go`](../../../../internal/tools/git.go#L190)) and converts worktree content; `Blame` passes the path directly to `git blame` ([`git.go`](../../../../internal/tools/git.go#L375)), whose worktree-to-index conversion can run the same filter. `.gitattributes` selects a filter while repository config supplies its `clean` or `process` executable. The process filter takes precedence when configured. Therefore `status`, `diff`, and `blame` must remain `unknown` or outside the trusted `workspace_read` allowlist in this iteration. `--no-optional-locks`, `-c core.fsmonitor=false`, `--no-ext-diff`, and `--no-textconv` do not disable clean/process filters.
- **Other fixed Git reads still need their helper-specific controls.** If `log` is retained, force `-c log.showSignature=false`: Git documents that `log.showSignature=true` assumes `--show-signature`, which invokes the configured GPG program. Put `--` before `blame` paths to prevent path-as-option interpretation. These controls close those known paths but do not make status/diff/blame safe.
- **LSP is a trusted configured process, but its query is not workspace-confined by the tool.** `lsp_query` validates a fixed operation set ([`lsp_tool.go`](../../../../internal/tools/lsp_tool.go#L88)); `rename` returns a `WorkspaceEdit` and does not apply it ([`lsp.go`](../../../../internal/tools/lsp.go#L385)). The server is launched from configured `ServerCommand` ([`lsp.go`](../../../../internal/tools/lsp.go#L127)); the CLI loads project configuration through `LoadTrusted` ([`main.go`](../../../../cmd/cli/main.go#L23), [`config.go`](../../../../internal/config/config.go#L124)). The `uri` argument is not checked against the workspace root. Keep the effect `unknown` by default unless the caller's trusted server and URI scope are established; there is no tool-argument `workspace/executeCommand` or edit-apply operation here.

## Git filter reproduction

Observed with `git version 2.54.0` in a disposable temporary repository. A `filter.spy.clean` helper appended one line to `hits` and copied stdin to stdout. `.gitattributes` contained `*.txt filter=spy`, and `.git/config` contained `filter.spy.clean=<temporary filter.sh>`.

This minimal setup clears the marker after the initial add/commit, changes the tracked file to different content of the same length, and aligns its mtime with the index to exercise the racy-stat content check:

```sh
d=$(mktemp -d)
trap 'rm -rf "$d"' EXIT
git -C "$d" init -q
git -C "$d" config user.name Audit
git -C "$d" config user.email audit@example.invalid
git -C "$d" config core.trustctime false
cat >"$d/filter.sh" <<EOF
#!/bin/sh
echo clean >>"$d/hits"
cat
EOF
chmod +x "$d/filter.sh"
git -C "$d" config filter.spy.clean "$d/filter.sh"
printf '*.txt filter=spy\n' >"$d/.gitattributes"
printf 'before!!\n' >"$d/a.txt"
git -C "$d" add .gitattributes a.txt
git -C "$d" commit -qm init
: >"$d/hits"
printf 'after!!!\n' >"$d/a.txt"
touch -r "$d/.git/index" "$d/a.txt"
git -C "$d" --no-optional-locks status --porcelain >/dev/null
wc -l <"$d/hits"  # 3 helper invocations
: >"$d/hits"
git -C "$d" --no-pager diff --no-ext-diff --no-textconv >/dev/null
wc -l <"$d/hits"  # 3 helper invocations
```

The test exercises a configured `clean` helper; Git's filter documentation states that a configured `process` filter takes precedence for the same clean conversion. Thus the observed counts are evidence for the clean case, while the process case follows the same documented conversion selection and remains within the unknown boundary.

Primary references: [ripgrep guide](https://github.com/BurntSushi/ripgrep/blob/master/GUIDE.md), [fd guide](https://github.com/sharkdp/fd), Git v2.54.0 [`read-cache.c`](https://github.com/git/git/blob/v2.54.0/read-cache.c), [`object-file.c`](https://github.com/git/git/blob/v2.54.0/object-file.c), [`diff.c`](https://github.com/git/git/blob/v2.54.0/diff.c), [`blame.c`](https://github.com/git/git/blob/v2.54.0/blame.c), [Git attributes: filter and long-running process](https://git-scm.com/docs/gitattributes), [Git status background refresh](https://git-scm.com/docs/git-status#_background_refresh), and [Git config `log.showSignature`](https://git-scm.com/docs/git-config#Documentation/git-config.txt-logshowSignature).

## Implementation review

Reviewed `28fa725472cc683f25a4cdf8fa1f7345cef6cd80` and follow-up `9c48e50740db1665d192b5045160933338a03ef3` on `codex/runtime-read-tool-safety`, based on `f00aa63057e64faea014e2af756a730462be5b07`. The scoped diff is accepted for the argv/helper hardening slice. It does not grant Git, LSP, or unscoped Read a trusted `workspace_read` effect.

The rg call now uses `--no-config`, `-e <pattern>`, and `-- <path>`, supplies glob as one `--glob=<value>` argument, and removes `RIPGREP_CONFIG_PATH`. The fd call places `--` before the pattern and always supplies the pattern slot, including an explicit empty pattern. Git read calls disable the specific fsmonitor, index-maintenance, external-diff, textconv, and signature helpers covered by the change; they strip inline config-override variables while preserving `GIT_DIR`, `GIT_WORK_TREE`, and `GIT_INDEX_FILE`. Status/diff/blame remain `unknown` because clean/process filters can still execute. The implementation comment states this limitation and the lane adds no trusted-read allowlist entries.

The first snapshot's `runReadOnly` error path discarded `exec` errors. Follow-up `9c48e50` wraps the original error, retains Git's diagnostic text, and joins `ctx.Err()` when cancellation kills the child. Its tests assert `errors.As` for a missing executable and `errors.Is(context.Canceled)` for a canceled invocation, plus diagnostic retention. This closes the review blocker.

I independently ran all ten lane tests with `-race` on the final `9c48e50` snapshot; they passed (`internal/tools`, 1.287s). The builder's full `internal/tools` race run passed on `28fa725` (38.599s); I did not repeat the full package run after the error-wrapping-only follow-up.

I also exercised real helper behavior in isolated temporary directories. System ripgrep 15.2.0, given a hostile `RIPGREP_CONFIG_PATH` containing `--pre=<marker helper>`, returned the expected match under the new `--no-config -e ... -- path` invocation and did not create the marker. The host had no fd binary, so I installed fd 10.5.0 under `/tmp` only. With a temporary `touch` helper on `PATH`, the old vulnerable argv (`fd --type f --max-depth 3 --exec=touch .`) created the marker; the hardened argv (`fd --type f --max-depth 3 -- --exec=touch .`) did not. These checks validate the actual helper boundary alongside the fake-executable argv assertions.

Remaining effect boundary: the Git runner suppresses known helpers, but repository `filter.<name>.clean` / `filter.<name>.process` remains executable during status/diff/blame, so those operations must stay `unknown`; the same applies to `lsp_query` absent trusted server and URI scope. Read remains filesystem-only Go code but is not workspace-bound by the default constructor. The mandatory Read path-scope/constructor-whitelist work is a separate R4 slice and is not approved by this review.
