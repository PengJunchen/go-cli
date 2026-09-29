# Grep bounded-capture builder report

## Scope and result

Branch: `codex/repair-20260929-grep`, based on integrated HEAD `5db266977b260ba0f1707e766f327bcba92671dd`.

This change is limited to Grep implementation, Grep-specific tests, the Grep argv/lifecycle fixtures, and this report. It replaces the plain-text rg parser and unbounded Go walk with bounded capture. It does not edit the accepted directory capture implementation, the managed process owner, Find, approvals, CI, rules, or shared ledgers.

Ripgrep now runs with `--no-config --json -n`, one line-number flag, and incrementally parses newline-framed events through the existing managed stdout writer. It decodes path and line data from either JSON `text` or base64 `bytes`, keeps raw path bytes in `match.File`, normalizes line content like `bufio.ScanLines`, and quotes only displayed paths that contain control bytes or invalid UTF-8. Frames are capped at 8 MiB. Protocol and frame errors are typed, clear retained frame storage, and are terminal. The writer returns the full byte count so the existing owner keeps draining stdout and joins both readers before Grep returns.

Both backends use one whole-record capture that clamps negative budgets to zero and counts displayed path bytes, separators, decimal line number, content, and newline. The truncation marker remains outside the output budget. Exact count/byte limits do not report truncation until a matching record is actually omitted. After Go capture saturates, scanning continues so scanner errors and cancellation remain visible.

The Go fallback now uses depth-first `os.File.ReadDir(1)`. Enumeration memory is independent of sibling-directory width; active directory handles are proportional to DFS depth. Requested-root open/read/close failures preserve their wrapped causes, descendant access failures set `truncated`, scanner errors return no partial result, and cancellation after empty/EOF is an error. Glob checks apply to files only and never prune directories.

This deliberately changes Go fallback order from `filepath.WalkDir` lexical order to the filesystem order observed through `ReadDir(1)`. Ripgrep keeps its JSON event order. Count/output caps retain each backend's complete prefix; the public docs and prior Grep tests did not promise lexical order or a cross-backend capped prefix. No top-K sorting or cross-backend prefix equivalence is claimed.

## RED and behavior evidence

The compile-successful primary RED used the installed ripgrep 15.2.0 against a temporary single-file fixture. Before implementation, `TestGrepRipgrepSingleFileRetainsPathLineAndContent` failed: the expected path/line/content match was nil because the old plain `-n` parser discarded rg's single-file `line:content` row. The same test passes through the managed JSON route now.

New and updated tests cover:

- Exact real-rg path, line, and content for a file root; colon-bearing and LF-bearing names; one-record LF path display and metadata.
- CRLF and terminal-CR line normalization against forced Go for simple visible literal fixtures only.
- Negative, zero, exact, and exceeded count/output budgets, whole-record omission, and quoted path byte accounting.
- Split JSON writes, `text` and base64 `bytes`, a final frame without LF, malformed JSON/data/base64, and an event one byte beyond 8 MiB.
- Filesystem-observed `ReadDir(1)` DFS prefix order, nested-file glob behavior, file roots, root causes, descendant incompleteness, scanner errors, and cancellation after empty EOF.
- PATH-local rg shims for exact argv, stripped `RIPGREP_CONFIG_PATH`, inherited unrelated environment, saturation followed by malformed ignored bytes, completion marker visibility, malformed protocol fail-closed behavior, and oversized-frame drain/no fallback.
- Existing escaped-writer cancellation/unknown-outcome tests, updated so the fake rg emits a valid JSON match before the escaped writer.

The memory proof is structural: the DFS test records every `ReadDir` request and maximum returned entries, tracks concurrent/closed directory handles, and asserts retained match count and serialized bytes. A six-level empty DFS cancels at EOF, checks every opened handle closed, and compares Darwin `/dev/fd` names before/after. Five normal writer-backed shims assert `readersJoined` and `containmentProven` and check descriptor count returns within one of baseline. An oversized helper writes its completion marker after the 8 MiB overflow; Grep observes the marker before returning the typed error.

The source reuses `runManagedSearchCommandWithStdoutWriter`, `managedSearchLifecycleError`, and `shouldFallbackAfterManagedHelper`; it reuses the existing `matchGlob` predicate. It keeps Grep's own `ReadDir(1)` DFS loop because descent must happen immediately while only one entry is held. The shared directory capture and managed-owner files remain unchanged.

## Validation

Environment: Go 1.26.4, Darwin/arm64; real ripgrep 15.2.0 available.

```text
rtk proxy env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb go test -count=1 -run '^(TestGrep|TestParseRipgrepJSONFrame)$' ./internal/tools
PASS

rtk proxy env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb go test -race -p 2 -count=1 ./internal/tools
ok github.com/pengjunchen/go-cli/internal/tools 43.351s

rtk proxy env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb go test -race -p 2 -count=1 -run '^TestGrep' ./internal/tools
PASS after the final Grep reader-interface factoring.

rtk proxy env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb go test -race -p 2 -count=1 ./internal/approval ./internal/core
PASS: internal/approval and internal/core

rtk proxy env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb go test -race -p 2 -count=1 ./internal/cli -run '^TestAssembleAgentConfiguredPathsConstrainDefaultFileTools$'
PASS

rtk proxy env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb go vet ./internal/tools
PASS

rtk golangci-lint run --allow-serial-runners ./internal/tools/...
No issues found. The runner help was checked and confirms --allow-serial-runners serializes concurrent invocations around its lock.

rtk git diff --check
PASS
```

`go test -list '^Test(Grep|ParseRipgrepJSON|Find|Read|Ls)'` also completed and showed the Grep-focused cases plus existing R4 selections.

## Backend limits

The real-rg CRLF/final-CR fixture verifies line normalization only. Ripgrep still applies its default hidden/ignore/binary selection while the Go fallback scans regular files without those filters. Go regexp and Rust regex also differ for some Unicode character classes and boundaries, including `\w` and `\b`. This change does not claim general pattern/backend parity, a production/live validation, or a traversal-time/total-file-bytes bound.
