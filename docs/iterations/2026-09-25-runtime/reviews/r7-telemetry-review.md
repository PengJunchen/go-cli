# R7 telemetry partial review

## Decision

**Accepted for the telemetry slice only.** Reviewed fixed commit `78b694ca0103f008ef350a6a082b360bcc347347` (parent `94dece02924847b4b46d08e021121ccb1b95cc4b`) in a clean detached worktree. No blocking finding. This does not close the remaining R7 lifecycle work or its end-to-end gates.

The implementation matches the telemetry requirements in `r7-lifecycle-design.md:145-151`:

- `metricKey` emits a versioned tuple frame containing byte-length-prefixed name, label count, and sorted key/value fields. Parsing boundaries are independent of delimiter contents; Go `len(string)` counts UTF-8 bytes, and `sort.Strings` gives deterministic byte ordering. Existing separator-collision tests pass. A review-only Unicode probe also confirmed the expected byte lengths for multibyte name/key/value fields and distinct keys; that temporary test was removed and is not part of the commit.
- The regular-series existence check, cap check, insertion/aggregation, and dropped-counter increment share `t.mu`. Existing series still aggregate at capacity; new series are rejected once `len(values) == 1024`. `Snapshot` takes the matching read lock and copies regular series plus both reserved counters as one consistent view. The concurrent cap test asserts exactly 1024 regular series and the expected rejected count.
- Reserved counter names are rejected before storage. The counters live in dedicated fields and are copied into snapshots directly; no recursive `Record` path exists. Attempts with and without labels are tested.
- Built-in metric families enforce fixed label keys. Unknown keys and missing required dimensions return classified errors; out-of-domain values are folded per label dimension to `other` and counted. Token families reject all labels. Other metric names retain caller labels but use the same global series cap.
- `recordToolTelemetry` maps known tool names to bounded classes and every unknown/custom name to `other`; it sends only `tool_class` and the boolean `cache_hit` label. The classification is confined to telemetry instrumentation and does not participate in tool routing, authorization, or approval.

## Validation

Independent targeted checks on the fixed snapshot:

- `rtk go test -race ./internal/production -run 'Test(Telemetry|MetricKey|AuditTelemetryEndToEnd|TelemetryAndIdempotentCacheIntegration)' -count=1` — passed (19 tests).
- `rtk go test -race ./internal/cli -run '^TestRecordToolTelemetryUsesBoundedToolClasses$' -count=1` — passed.
- Review-only Unicode byte-framing probe — passed; temporary test file removed.

The builder-reported full production race and CLI target race were not repeated.

## Non-blocking scope note

`Record` does not reject NaN/Inf or prevent arithmetic overflow of a regular metric sum. The current CLI producers supply durations and token/count values, and numeric-domain validation is outside this cardinality slice; I do not consider it a blocker here. If arbitrary metric values later become untrusted or exported snapshots require finite numbers, specify and test that separately.
