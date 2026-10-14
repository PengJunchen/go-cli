# Read-only review: durable summary headroom

## Verdict

**Source review: PASS, no blocking finding.** This is a code review result, not live-run acceptance and not a claim that the compaction scenario has passed against Magpie. The exact 6.1 live run `20261014-190417-930a46d9` is being assessed separately; only its own frozen report and durable evidence can establish live acceptance.

## Reviewed change

- Base revision: `1897678c09eb243a7154e321eca9bb85ae030894`.
- Production change: `internal/compaction/summary.go` only.
- Production diff SHA-256: `d1af5e76c45f261ddfa76d36aa6ee032cb94fc60f028e1fb17adc99fd0d89723`.
- Test changes: `internal/compaction/summary_test.go`, `internal/compaction/unified_test.go`, and `internal/core/durable_compaction_session_test.go`.

The durable summary cut search now reserves the summary role, the untrusted-summary framing, and the configured maximum summary-body estimate before selecting a whole-group prefix. The estimator-based summary body cap remains `WithMaxSummaryTokens`'s contract; the added role and framing are reserved separately. The returned typed projection keeps the raw summary body, while the provider projection adds the untrusted wrapper once.

## Safety checks reviewed

- Durable eligibility validation remains before any summary call; unknown tool receipts still make durable history ineligible for compaction. No eligibility or receipt-proof guard was changed.
- Unified compaction still sends over-budget durable history only through a typed summary projection. Missing/failed summary output does not fall through to lossy truncation.
- The selected cut remains at a complete item-group boundary. The new rune-count boundary fixture covers the prior prefix fully while preserving the exact current source-backed input; it checks that caller-owned input is unchanged.
- The final assembled result is still re-estimated and rejected with `ErrRequiresTruncating` when it exceeds the real budget. Core durable preflight tests still reject any summary that consumes the current input and block provider dispatch/commit.
- Existing core tests cover durable sync before provider dispatch, a single untrusted wrapper in the provider-visible history, exact source boundary, and equivalence after JSONL reopen.
- Arithmetic for the new reservation uses the existing saturating token-add helper. The returned summary body is capped using the configured estimator; a non-additive estimator can cause conservative cut selection or a final fail-closed budget error, but does not bypass the exact-result check.

## Boundary fixtures and test provenance

The new compaction fixture uses summary cap `20` and a `112`-token rune-count budget: the maximum body, user role, and untrusted prefix plus the unchanged current input fit exactly after the complete prior source prefix is summarized. The durable core fixtures use a one-token summary cap and a seven-token boundary where the projected summary and retained system/current messages fit exactly under their deterministic test estimator. A separate oversized-current-input fixture continues to reject before provider dispatch.

The production and test diffs were independently reviewed read-only. **The package normal/race tests, vet, lint, and diff checks are owner-reported results** from `magpie_install`; this review did not rerun those gates. Their results do not substitute for the separate real-provider compaction, persisted-summary, and post-compaction recall evidence in run `190417`.
