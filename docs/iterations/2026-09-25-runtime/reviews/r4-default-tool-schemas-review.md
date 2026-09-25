# R4 default tool parameter schemas review

**Target:** `a60e2386c95e56e2d7046f840e1f26b9aaa1c7dd`  
**Worktree:** `/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/r4-default-tool-schemas`  
**Verdict:** **Accepted.** I found no schema or prepared-dispatch blocker in this change.

## Contract review

The five new default schemas match the intended parameter boundary:

| Tool | Required fields | Optional fields and constraints | Extra properties |
| --- | --- | --- | --- |
| `read` | `path: string` | — | rejected |
| `bash` | `command: string` | — | rejected |
| `grep` | `pattern: string` | `glob`, `path`: strings | rejected |
| `find` | — | `path`, `pattern`: strings; `type`: `""`, `f`, or `d`; `max_depth`: integer, no minimum | rejected |
| `ls` | — | `path`: string; `all`, `long`: booleans; `sort`: `""`, `name`, `time`, or `size` | rejected |

The registered definitions are observed through the prepared proxy, so the schema test verifies the public `Parameterized` capability and the captured schema. Mutating a returned schema does not change a subsequent `Parameters()` result. Existing `write` and `edit` schemas remain unchanged.

Prepared-call tests verify that missing required fields, wrong types, fractional `max_depth`, invalid enum values, and unknown keys are rejected before the policy callback or execution wrapper. Valid optional defaults execute as expected: Find without filters searches the full tree; empty Find filters are accepted; `ls` defaults to name sorting and hides dotfiles; explicit empty sort also resolves to name.

The schemas do not impose `minLength` on required strings. A temporary prepared-path probe confirmed that blank `read.path`, `bash.command`, and `grep.pattern` pass schema validation and reach the approved execution wrapper, after which the concrete executor returns its existing missing-value error. This matches the intended structural schema contract; it does not imply the executor accepts an empty operation.

The Find enum and Ls sort enum are exact lowercase protocol values. Although their executors normalize casing, values outside the schema enum are intentionally rejected before policy/dispatch, tightening the accepted model protocol.

## Numeric argument compatibility

OpenAI's streaming argument accumulator and the Claude native adapter decode tool JSON with `json.Unmarshal` into `map[string]any` (`internal/llm/stream_accumulator.go:204-216`, `internal/llm/native.go:810-825`). Without `UseNumber`, provider-decoded numeric values are `float64`. The prepared JSON clone preserves scalar Go types (`internal/tools/prepared.go:534-538`), and the Find executor accepts `float64` for `max_depth` (`internal/tools/find.go:129-134`).

A temporary end-to-end probe decoded a real JSON argument object using the same `encoding/json` path, verified the policy snapshot still contained `float64(1)`, and dispatched Find through the protected registry. The result included a root-level file and excluded its nested child, showing the decoded depth value survived preparation and affected executor behavior. No regression to `json.Number` was observed.

## Validation

- `rtk proxy go test -list '^Test(RegisteredDefaultToolsExposeAccurateParameterSchemas|DefaultToolPreparedProxyValidCallsAndOptionalDefaults|DefaultToolPreparedProxyRejectsInvalidArgumentsBeforePolicyOrDispatch)$' ./internal/tools`: exit 0; all three targets listed.
- Those three tests under `rtk env GOMAXPROCS=2 rtk proxy go test -race -p 2 ./internal/tools -run '^Test(RegisteredDefaultToolsExposeAccurateParameterSchemas|DefaultToolPreparedProxyValidCallsAndOptionalDefaults|DefaultToolPreparedProxyRejectsInvalidArgumentsBeforePolicyOrDispatch)$' -count=10`: exit 0.
- Temporary provider-decoded `float64` Find probe: listed before execution; targeted `-race -count=3` passed.
- Temporary blank-string schema/executor probe: listed before execution; passed and observed existing concrete executor errors.
- `rtk env GOMAXPROCS=2 rtk proxy go test -race -p 2 -count=1 ./internal/tools`: passed, 42.250s.
- `rtk env GOMAXPROCS=2 rtk proxy golangci-lint run ./internal/tools`: passed, 0 issues.
- `rtk git diff --check a60e2386^ a60e2386`: passed.
- Temporary probes were removed; the reviewed worktree is clean at the target hash.
