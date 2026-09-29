# R7 remaining design final review

**Reviewed design:** [`r7-remaining-design.md`](../r7-remaining-design.md), SHA-256 `685b4eef2a30ebe1ac2e7a6579e39b393f2938ef5e824206bd970f6b5855a702`.
**Prior reviews:** [`r7-remaining-design-review.md`](r7-remaining-design-review.md) and [`r7-remaining-design-r2-review.md`](r7-remaining-design-r2-review.md).
**Source reviewed:** current checkout `068cd32`; relevant executable-source findings below were checked against this checkout. No product source was changed and no tests were run.

## Verdict

**ACCEPT for builder handoff. No remaining P0, P1, or P2 design findings.** The design closes the P0 capability-forgery concern and the P1 gaps from both prior reviews. It gives the builder an explicit R6 trust prerequisite, followed by bounded R7 skill and hook work, and keeps tracing/audit work open under its existing ledger status.

## Findings

None.

## Evidence

- **Manager-minted authority is non-forgeable at the public API boundary.** `VerifiedProjectSources` has approval-package-private state and constructor; only a successful `CheckProjectSourceSnapshot` store comparison may mint it. Raw snapshots and `ProjectSourceTrustStatus` are explicitly non-authoritative, and `Config` keeps the capability in a private, non-serialized field. The design also requires a forged-config assembly test and prevents a grant from authorizing the same invocation ([design lines 33–43, 57–59, 63–64, 105](../r7-remaining-design.md#L33)). This corrects the prior P0. The current path-only `TrustManager` and MCP-only fingerprint remain accurately identified as insufficient ([`trust.go`](../../../../internal/approval/trust.go#L22), [`trust_store.go`](../../../../internal/approval/trust_store.go#L14)).

- **The full six-candidate config identity is explicit.** All present candidates, including shadowed lower-priority candidates, are read once and included with exact bytes; absent candidates are recorded; only the winning candidate is parsed from captured bytes. Acceptance covers presence transitions, edits to shadowed candidates, priority changes, and no fallback ([design lines 51, 105](../r7-remaining-design.md#L51)). This closes the prior P1 against the R6 manifest contract ([R6 lines 40–42](../../2026-09-25-runtime/r6-entrypoint-design.md#L40)).

- **Project executable references outside the canonical root fail closed.** The rule covers project config references to hooks, MCP, skills, commands, extensions, and other executable sources; the narrowly allowed user-skill root is available only to user/global config and never inherits project trust. Resolution and containment use canonical paths and `filepath.Rel` ([design lines 52, 73](../r7-remaining-design.md#L52)).

- **The early-help and MCP provenance seams are sufficiently named.** The builder must reuse one existing `CommandRegistry`, its built-in registration seam, and the same global flag parser before config loading; only parser-recognized global help/version dispatches early, while other commands retain full config validation ([design lines 59, 97, 105](../r7-remaining-design.md#L59)). This addresses the present `Run`/`RunWithRegistry` split and config-first `main` path ([`cli.go`](../../../../internal/cli/cli.go#L77), [`main.go`](../../../../cmd/cli/main.go#L89)). The design separately requires provenance-marked user/global MCP servers and verified project-only overlay data, rejects ambiguous merged servers, and names the merged-nonempty shortcut as forbidden ([design lines 23, 63–65, 97, 105](../r7-remaining-design.md#L63)); that matches the current source gap ([`assemble.go`](../../../../internal/cli/assemble.go#L2365)).

- **Skill and hook handoffs preserve the bounded and isolated contracts.** Skills consume only the verified snapshot, retain bytes through parse and deferred registration, and use explicit DFS, entry/depth/candidate/file/total limits, chunk cancellation checks, symlink rejection, deterministic ordering, and all-or-nothing capture ([design lines 69–75](../r7-remaining-design.md#L69)). Hooks use typed extension-owned DTOs, deep JSON-safe copies, stable error classes, owner identity preserved through staging, and Pass/Block-only behavior; no raw errors or arbitrary block reasons cross the bridge ([design lines 87–93](../r7-remaining-design.md#L87)). These directly cover the current path-based skill loader and raw `core` payload bridge ([`skill_loader.go`](../../../../internal/skill/skill_loader.go#L138), [`extension_bridge.go`](../../../../internal/cli/extension_bridge.go#L39)).

- **Trace, audit, and cost work is carried forward without an accidental closure claim.** The design preserves one root tracer/exporter, assembly ownership and shutdown order, privacy/status rules, bounded delivery accounting with pending/quarantine semantics, durable audit-chain behavior, and unknown/unavailable cost/stat handling. It explicitly leaves `TRACE-01..06`, `APR-007`, and `RT-09` open, and its `R7-DES-01` / `R7-BLD-02` status matches the referenced execution ledger ([design lines 5–12](../r7-remaining-design.md#L5), [ledger lines 1543–1591 and 1634–1673](../../2026-09-25-runtime/execution-tasks.json#L1543)). This is consistent with the R7 lifecycle contract ([R7 lines 9–17, 129–159, 221–228](../../2026-09-25-runtime/r7-lifecycle-design.md#L9)).

## Builder handoff

Implement and review `R6-TRUST-SNAPSHOT + ROOT-EARLY-DISPATCH` first, including persistent manifest checking, same-byte config parsing, one-manager wiring, forged-config denial, early root help/version, and explicit MCP provenance. Only after that prerequisite is integrated should builders implement the bounded skill loader/CLI consumer and typed hook bridge. Keep MCP generation dependent on the verified provenance seam. Treat every listed behavior case as pending until it is exercised on the named real config, assembly, or CLI path. The trace/audit items remain separate open work and must not be marked complete by this handoff.
