# Grep source review: `25a68d3`

## Verdict

**Accept the bounded Grep capture slice with one P2 test-coverage follow-up.** I found no P1 source defect. The JSON capture, managed-helper lifecycle, fallback gate, and one-entry DFS behavior match the reviewed design at the pinned source snapshot. The follow-up below is limited to directly exercising a root-directory open error through the existing injected opener; the implementation already wraps that cause.

## Snapshot and scope

- Target: `25a68d3ebcadcf9d38ccb50529765ca158189553`.
- Exact parent/base: `5db266977b260ba0f1707e766f327bcba92671dd`.
- Worktree: `/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/repair-20260929-grep-review-25a6`, detached at the target SHA and clean.
- Reviewed against `docs/iterations/2026-09-29-runtime/grep-capture-design.md` and `docs/iterations/2026-09-29-runtime/reports/grep-builder.md` at the target.
- The target diff is limited to Grep implementation/tests and its builder report. `git diff --check` across base to target passes. No source changes or probes were made for this review.

## Finding

### [P2] Add a deterministic requested-root directory-open failure test

`grepPureGoWithOpen` distinguishes requested-root open errors from descendant errors and wraps the root cause with `%w` (`internal/tools/grep_capture.go:339-353`). The fixture suite tests a missing root (`grep_capture_test.go:175-187`) and injected root read/close failures (`:251-271`), but does not make the injected `openDir` fail for the requested root. The existing opener seam makes this deterministic without filesystem permission assumptions. Add a small assertion that the returned error preserves a sentinel with `errors.Is`; no source change is indicated by the current code.

This is a test-completeness item for the explicit requested-root open/enumeration/close contract, not evidence of a current behavior bug.

## Verified behavior

- `GrepTool.Execute` resolves and validates the root before calling `search` or selecting the helper (`internal/tools/grep.go:153-165`). The JSON invocation uses `--no-config --json -n`, places pattern and root safely, strips only `RIPGREP_CONFIG_PATH`, and inherits other environment entries (`grep_capture.go:95-127`).
- Capture budgets retain complete serialized records, clamp negative limits, and mark truncation only when a matching record is omitted (`grep_capture.go:39-91`). The writer caps one JSON frame at 8 MiB, clears retained frame data on typed failure or saturation, returns the full write count so the owner continues draining, and only finishes after the managed owner returns (`:149-208`). Parsing preserves decoded path bytes, accepts text/base64 forms, requires positive line numbers, and normalizes LF/CR like `ScanLines` (`:211-289`).
- Typed protocol and frame errors cannot fall back to Go; ordinary fallback remains gated by the shared lifecycle policy (`grep.go:200-225`). Lifecycle completion still comes from the existing process owner, and the escaped-pipe tests assert cancellation cause, no fallback, and withheld completion proof (`search_helpers_lifecycle_unix_test.go:40-103`). The Grep change does not edit the owner or proof implementation.
- The Go walker uses `ReadDir(1)` with immediate DFS and keeps directory handles on the recursion stack (`grep_capture.go:309-458`). It returns requested-root read/close failures with causes, treats descendant access errors as incomplete results, checks cancellation around reads and after EOF, and closes handles while unwinding. Scanner failures return errors instead of partial output (`:473-517`). Tests cover observed DFS prefix/width and handle bounds, missing root, descendant failure, root read/close causes, and EOF cancellation (`grep_capture_test.go:130-271`); scanner-error/no-partial behavior is covered in `grep_safety_test.go:86-99`.
- Real-rg tests cover a single-file root, colon and LF paths, directory-root rendering for the LF path, and CRLF/final-CR normalization (`grep_test.go:196-208`, `grep_capture_test.go:273-303`). Unit and shim tests cover split/final frames, base64 bytes, malformed fields, invalid base64, exact/overflow capture limits, drain markers after saturation/oversized frames, argv/environment, and no typed-error fallback (`grep_capture_test.go:24-128, 305-395`).
- The builder report records the compile-successful single-file RED and passing focused Grep tests, full `internal/tools` race suite, approval/core dependent race tests, CLI selection, `go vet`, scoped lint, and `git diff --check`. I reviewed that report but did not rerun those suites. No real production validation is claimed. The documented non-parity limits for hidden/ignored/binary files, Rust-vs-Go regex semantics, and traversal-time/total-file-bytes bounds are accurate for this change.

## Validation record

Reviewer-run: exact target/parent verification, source and fixture review, and `git diff --check 5db266977b260ba0f1707e766f327bcba92671dd 25a68d3ebcadcf9d38ccb50529765ca158189553` (pass). Test and lint results above are builder-reported evidence, not reviewer reruns.

## Integrated requested-root opener follow-up

The original P2 was test coverage only: the reviewed implementation already wrapped the requested-root open cause; the fixture did not exercise that injected `openDir` error path. An independent diff review checked the supplemental test patch `927850f`; its integrated equivalent is `8ccd6c2` (the test-file diff between those snapshots is identical). The current integrated source contains `TestGrepPureGoRootOpenErrorPreservesCauseAndFailsClosed` in `internal/tools/grep_capture_test.go`.

Source/fixture review confirms the test uses an existing requested-root directory and a sentinel opener error, requires `errors.Is(err, cause)`, checks the root-open context, and asserts the injected opener is called only for that root. It verifies no matches escape, both `capture.Truncated()` and the returned `truncated` are false, and the failure is not converted into descendant best-effort truncation. Its `GrepTool.Execute` case also checks a forced-Go missing root preserves `os.ErrNotExist`, returns empty output, and leaves metadata nil. This closes the P2 coverage follow-up; it does not report a product-code defect or close R4.

Root-run integrated validation on source `8ccd6c2`: `rtk proxy env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb GOFLAGS='-p=2 -trimpath' go test -race -count=1 ./internal/tools` — PASS, 45.447s (recorded execution session 86238, exit 0). Later commits `960f1ee` and `182e1bf` changed documentation only; the tested product source remained unchanged. This is a root-run package race result, distinct from the original reviewer-run tests above; no additional test run was performed for this addendum.
