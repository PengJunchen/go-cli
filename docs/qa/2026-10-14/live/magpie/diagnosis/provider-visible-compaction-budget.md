# Provider-visible compaction budget evidence

The compaction estimator now counts the message terms that are present on the
OpenAI-compatible request: message role/name/tool-call ID, visible text, and
JSON-encoded tool arguments. Non-nil content blocks shadow `Content`, matching
the request-envelope estimator. Durable routing recalculates these estimates on
its defensive copy instead of trusting a stale positive `EstimatedTokens` hint.
An argument graph that cannot be JSON encoded receives the `MaxInt` estimate;
summary cut accumulation saturates rather than wrapping it into a small number.

The fixture budget updates below preserve each test's original boundary while
accounting for the added provider-visible terms.

| Test intent | Previous budget | Updated budget | Budget basis |
|---|---:|---:|---|
| Factory summary output fits | 30 | 34 | System message 9 plus projected summary user message 25 |
| Micro normalization preserves legacy summary and tool group | 180 | 223 | Exact post-micro provider-visible estimate |
| Truncating drops the complete two-call group | 2 | 26 | Current user 8 plus one receipt 18; a partial receipt would violate the group assertion |
| Truncating retains the complete tool group | 3 | 81 | Full provider-visible item estimate: calls, JSON `null` args, both receipts, and current user |
| Unified micro-first and quality evaluator micro strategy | 300 | 523 | Exact post-micro provider-visible estimate |
| Non-durable preflight drops old user context and retains tool group/current input | 11 | 56 | Retained suffix: system 7 + assistant call 21 + tool receipt 21 + current user 7; old user adds 7 |
| Reused-loop fresh estimate accepts then rejects oversized input | 10 | 13 | First request: system 7 + user 6; second request remains over budget |
| Durable current-input projection is rejected after summary succeeds | 100 | 104 | System message 7 + one-token summary projection 79; preserving the 1,018-token current message as well cannot fit |
| Durable compaction sync-failure path | 3 | 7 | Bounded summary projection costs 3; retained system and current user cost 2 each |
| Durable projection/reopen path | 3 | 7 | Same 3-token bounded summary projection + 2-token retained system + 2-token current user |

The oversized-current direct preflight case uses 451 copies of the 14-character
`current-input-` string. That is 6,314 body characters; with the user role it
fits alone in the 6,400 budget, while the 91-token provider-visible summary
placeholder plus the current message totals 6,409. The source-safe cut therefore
has to cover the current input, and the durable guard rejects it without
committing a projection.

The real run `184011-290bc55c` exposed a separate durable-summary boundary. Its
summary model request succeeded with 6,651 input and 379 output tokens, but the
trace recorded `current_tokens=108075`, `max_tokens=102400`, and a post-summary
estimate of 102,760. The summary cut had reserved the 13-character placeholder,
then substituted the larger real summary, so the still-correct final budget
check rejected it and no typed durable projection was published.

The durable path now reserves the configured bounded summary body together
with the projected user role and untrusted-summary prefix before selecting a
cut. Legacy compaction keeps its existing placeholder reservation. The body is
still clamped by the configured estimator, and the final estimate of the actual
provider-visible result remains authoritative; if estimator behavior across
prefix/body boundaries makes that result exceed budget, compaction still fails
closed. A 112-token rune-count regression proves the exact boundary: summary
role/prefix/body is 98 tokens and the untouched current input is 14, so the cut
must cover all three complete prior source entries. The durable current-input
negative uses a one-token cap and verifies that a summary plus its framing may
fit by itself while the core guard still rejects consuming the current source.

The durable stale-cache regression places a 2,000-character JSON write argument
behind `EstimatedTokens: 1`, includes its matching receipt and a distinct
current transcript input, then verifies routing goes directly to a typed
summary, keeps the current source at the suffix, and leaves the caller's cache
hint unchanged. The cyclic-argument regression verifies JSON serialization
failure remains the `MaxInt` sentinel and cannot wrap into an under-budget cut.

Validation for the summary-headroom patch was run on the worktree based on
`1897678c09eb243a7154e321eca9bb85ae030894`. The focused regressions and complete
normal and race package runs passed for `internal/compaction` and
`internal/core`; `rtk proxy go vet ./internal/compaction ./internal/core`
passed; `rtk proxy /Users/yinyuepeng/go/bin/golangci-lint run
./internal/compaction ./internal/core` reported 0 issues; and `gofmt` plus
`git diff --check` passed.

## Legacy E2E fixture boundaries

The final review also updated `tests/e2e_20260802/04_compaction_e2e_test.go` to
assert provider-visible budgets. Its three `system` messages cost exactly nine
tokens under the heuristic estimator: two for each role and one for each
four-character `sysN` body. Truncation preserves all three at budget 9; budget 8
returns `ErrMandatorySystemPromptOverBudget` and the original items.

The three complete assistant-call/tool-receipt groups in the micro-compaction
fixture cost exactly 63 tokens after result placeholders are inserted, including
roles, call IDs/names, JSON arguments, and receipts. They fit at budget 63 and
retain all three linked call groups and placeholder assertions. The E2E file's
`tokenSum` helper delegates to public `EstimateTokens`; its full-conversation
example now expects 27 provider-visible tokens instead of counting only text.

On reviewed commit `49dbf9755d860b877edd45ed9a73161ab5ebbb8f`, the complete tagged
package passes normally and under race:
`rtk proxy go test -tags=e2e ./tests/e2e_20260802 -count=1` and
`rtk proxy go test -race -tags=e2e ./tests/e2e_20260802 -count=1`. The absolute
lint command `rtk proxy /Users/yinyuepeng/go/bin/golangci-lint run
./tests/e2e_20260802` reports 0 issues.
