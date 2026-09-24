# R4 trusted-read argument and helper hardening

Branch: `codex/runtime-read-tool-safety`  
Worktree: `/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/read-tool-safety`  
Base: `f00aa63057e64faea014e2af756a730462be5b07`  
Status: code and tests frozen at `28fa725`; targeted and full `internal/tools` race suites passed; awaiting independent diff review.

This slice hardens concrete invocation boundaries for Grep, Find, and Git. Ripgrep now receives the pattern through `-e`, the path after `--`, `--no-config`, and an environment without `RIPGREP_CONFIG_PATH`; glob values use one `--glob=<value>` argument. `fd` receives `--` before the model-supplied pattern. Tests use fake executables to inspect the actual argv and environment.

Git's fixed read invocations use a narrow runner that disables configured fsmonitor, external diff, signature verification, untracked-cache maintenance, and optional index writes. Diff explicitly disables external diff and textconv; Blame places `--` before the file path. Config-override environment variables are removed while host-selected `GIT_DIR`, `GIT_WORK_TREE`, and `GIT_INDEX_FILE` are preserved. Mutating Git commands continue to use the existing `run` method.

This does not establish Git operations as pure reads. Repository `.gitattributes` and `.git/config` can still cause `status`, `diff`, or `blame` to execute clean/filter-process helpers while converting worktree data. Suppressing those helpers would change repository semantics, so these Git tools remain `unknown` for effect classification. The scoped changes do not add or expand a trusted-read allowlist.

Exact RED and GREEN commands, test names, exit codes, and outcomes are recorded in [read-tool-safety-red.log](evidence/read-tool-safety-red.log) and [read-tool-safety-green.log](evidence/read-tool-safety-green.log). The explicit empty-pattern assertion was added after RED and passed in the targeted and full GREEN runs.
