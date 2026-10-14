# R7 Lifecycle Contract Review

Review date: 2026-09-25  
Review type: independent design review; no product changes or tests run.

## Reviewed snapshots

| Artifact | Snapshot |
|---|---|
| `r7-lifecycle-design.md` | SHA256 `bb7362a94d0a5688907e631cf264b4e20cc21b906294d3ea7cbf6c58bf8975ea` |
| `execution-tasks.json` | SHA256 `77ca27a64445369b6dfed2f10aa732b19052aaa8928ed7c70432da470bb58af7` |
| Source tree | `32db593f9e50b768c4fb83c4cba93ced2fcff092` |
| Approved R4 seam | `4f8d1a0d72eebc25fcf06c1fcb897d64bf73d41c`; reviewed R4 document SHA256 `3dff4e89b91bc0aa170638020d2d16cde0f9c5899208d97c3a94cbe07b7a6724` |

## Initial decision (before amendment)

**Return for four narrow contract clarifications before builder handoff.** The capability matrix and principal decisions are grounded in the current default path: one tracer owner, no false OTLP/Kafka support, bounded best-effort forwarding, and load-all skill tools rather than pretending `Match` is wired. The R7 proposal is consistent with R1's run-owner boundary and R4's prepared-dispatch/effect boundary at a high level. Pending R2/R4/R6 implementation is not a reason to reject the design; the handoffs below need to name those contracts explicitly.

The plan is not over-frameworked: it reuses the current assembly, extension manager, skill adapter, and exporter path. Keep it that way; no general plugin/runtime/job framework is needed.

## Findings

### P1 — Make close timeout truthful and wire every default close caller

`r7-lifecycle-design.md:47-52` requires `Close(ctx) error`, says the caller's wait is bounded, and also says the background close worker must not leak. That cannot be guaranteed for arbitrary trusted in-process `.so` code: `Extension.Shutdown(ctx)` and hook callbacks can ignore cancellation, and Go cannot forcibly stop them. The same limitation applies to an exporter whose `ExportSpan` blocks while ignoring its context. R1 already defines the safe vocabulary for this case: caller timeout is `cleanup_pending`; an unproved stop remains owned/quarantined and cannot release resources or be reported quiescent (`r1-contract.md`, required runtime semantics). The R7 design should explicitly reuse that model for extension callbacks and exporter delivery: contexts are cooperative, the owner remains tracked after a bounded caller return, and dependent sinks stay open until all producers are proven stopped. Do not promise forced cancellation or silently abandon an untracked goroutine.

The admission side is also underspecified. `Close` says it stops new work, but R1's `RunSlotGuard` only serializes runs; it has no closed/admission state. R7 must identify one assembly-close fence shared by `Harness.Submit` and `TurnRunner.RunTurn`, then wait on the existing R1 run owner rather than introducing a second run lifecycle.

The default call sites still use fire-and-forget cleanup: `prompt.go:140`, `repl_session.go:135`, and `serve_cmd.go:185,262-264` retain `Cleanup func()` or discard its result. The serve path also returns success after cleanup errors (`serve_cmd.go:259-266`). `cmd/cli/main.go:99-111` prints exporter shutdown errors but leaves the exit code unchanged. Add these production callers to R7-BLD-01/02 and specify how a close/shutdown error affects the command result. Assembly setup-error cleanup at `assemble.go:324-343` must likewise join cleanup errors with the build error as the design says.

Required acceptance: close rejects a concurrent/new submission; a blocked extension callback or sink outlives the caller deadline without being reported closed/drained; after release, the same owner finishes cleanup exactly once; prompt, interactive, serve, and root-exporter close failures reach the command's returned status. Keep R1's run state as the authority for active runs.

### P1 — Bind bounded skill loading to trusted immutable source bytes and R4 preparation

The per-file/root/count limits and symlink checks at `r7-lifecycle-design.md:88-102` are useful resource bounds, but they do not establish which skill bytes are trusted or eliminate a check/read race. The current default path checks trust only for auto-discovered relative project skills; an explicit `rc.Skill.Dir` bypasses that check (`internal/cli/assemble.go:1942-1967`). The R6 source contract requires each project skill source to be canonicalized and snapshotted once, with the same immutable bytes used for fingerprinting, parsing, and registration; a path may not be hashed and then reopened (`r6-entrypoint-design.md:40-42`). R7 should state that its loader consumes those frozen source snapshots and their directory membership, not just a path. A configured project root or a source outside the trusted root must not inherit trust from the project directory; until the R6 snapshot/trust handoff exists, that source should remain unavailable/fail closed. This is an explicit dependency, not a demand that R6 implementation already be complete.

Align skill parameter validation with R4's prepare boundary. R4 requires the normalized call and schema to be frozen together, with schema validation before approval (`r4-prepared-dispatch-design.md:39-48,70-78`). R7 currently says `Parameterized` plus validation “before Execute”; make explicit that the preparer snapshots and validates the same schema before approval/dispatch. `SkillDefinition.Tools()` remains descriptive only and cannot mint effects or satisfy R4 authorization; dynamically loaded skills keep the effect classification R4 assigns to their trusted registration (unknown unless separately proved).

Required acceptance: change a skill file between source capture and loader use and prove the parser/registered prompt still uses only the captured bytes; reject changed membership, outside-root sources, and symlink escapes; prove malformed skill args are rejected before the approval callback and no dispatch occurs. Retain the proposed 1 MiB/file, 128 candidates, 16 MiB/root, and cancellation limits as defaults unless a reviewed measurement justifies a change.

### P2 — Make telemetry-label acceptance injective and bound label domains

`R7-BLD-02` asks for `TestTelemetryLabelsAreInjective` and the design says metrics must avoid raw/high-cardinality labels, but it does not define the encoding or allowed label domains. The current `metricKey` concatenates raw `key=value` pairs (`internal/production/telemetry.go:109-133`), so distinct inputs collide: `{"a":"x,b=y"}` and `{"a":"x","b":"y"}` both become `metric{a=x,b=y}` and their values merge. Specify an injective canonical encoding (for example a length-prefixed or canonical structured tuple) and a finite key/value policy; arbitrary skill, extension, or tool names must not create unbounded label cardinality. The named regression should assert the colliding inputs remain distinct through `DefaultTelemetry.Snapshot`, not only that label ordering is deterministic.

For async tracing, `TraceSpan.End()` has no return value and logs an `ExportSpan` error (`internal/tracing/span.go:253-271`); a queue-full or sink failure therefore cannot be observed through the span caller. The design already proposes counters or a shutdown report (`r7-lifecycle-design.md:129-137`), but leaves them as alternatives. Choose the single owner-visible observation path used by the default CLI and the tests, and assert enqueue/drop, sink failure, close rejection, and shutdown abandonment there. This keeps “best effort” truthful without adding a second delivery framework.

### P2 — Export the promised `block` hook action

The support decision promises `pass` and `block` (`r7-lifecycle-design.md:12,74,149`), but `internal/extension/types.go` currently exports only `HookActionPass`; the block constant is package-private (`hookActionBlock`). Extension authors cannot name the supported action through the public API. Export `HookActionBlock` with its before-run/after-run semantics, or downgrade the advertised support to pass-only. Add a tiny external-package extension fixture that compiles and returns the public block action; retain explicit failure for `terminate`, `replace`, and unknown values.

## Scope and evidence limits

The existing R7 rows cover the right production seams, but add the close callers identified above to the owned file/test boundary and make the new acceptance assertions part of R7-BLD-01/02. Keep the R0 final-byte sentinel check through the real CLI, tool, extension hook, JSONL and slog path (`r7-lifecycle-design.md:139,183`); checking in-memory span attributes is insufficient. Keep OTLP/Kafka unavailable until the stated external conformance checks pass. No full or targeted tests were run because this assignment is design review only.

## Follow-up review at `1367b7e56ef5619ce45ce259f9469dade6bbeab2`

Reviewed the R7 design change at the fixed commit above; the design file SHA256 at that commit is `ec9e16816bce1b910852b61c2579b1cd4c3e9eb467cecf701bd36ec989434e6d`. `git diff --check` passed for the commit. The follow-up is limited to closure of the four findings above; no tests were run.

| Finding | Status | Evidence in the amended design |
|---|---|---|
| Close ownership, R1 run reuse, deadline state, and default caller propagation | **Closed** | The design adds only `RunAdmissionCloser.CloseAdmission()` on the existing shared guard, says `ClaimRun` and close admission linearize together, retains active R1 claims/owners/sinks after timeout, and carries cleanup errors through prompt, REPL, serve, setup failure, and root CLI exit handling. It explicitly treats `.so`/exporter cancellation as cooperative and requires retained ownership until positive completion. The new acceptance cases exercise admission races, uncooperative callbacks/sinks, and every default caller. |
| Skill frozen-source and R4 preparation boundary | **Closed** | Project skills must use R6's frozen bytes and directory membership for digest, parse, registration, and prompt generation; configured/out-of-root sources cannot inherit trust, and no fallback reopens by path. Skill schemas are frozen and validated by `PrepareToolCall` before approval; dynamic skill effects remain R4-minted/unknown, and `tools` stays descriptive. New tests cover changed bytes/membership and pre-approval rejection. |
| Truthful telemetry encoding, label cardinality, and exporter reporting | **Closed by root ownership decision** | The design now defines length-prefixed canonical tuple keys, finite producer domains, unknown-value folding, a 1,024-series cap, reserved drop counters, and one CLI `ShutdownReport` for exporter state. Root has assigned `internal/production/telemetry.go` and `internal/production/telemetry_test.go` to R7-BLD-02. The `internal/cli/assemble.go` producer-label and trace-owner hunks are serialized under root approval and coordinated with R2/R4/R6 ownership; builders must not overlap edits to the same functions. Designer will synchronize this ownership into the design file in a small follow-up commit. |
| Public `HookActionBlock` | **Closed** | The decision and bridge contract now explicitly export `HookActionBlock`; an external-package compile/behavior fixture is required, while `terminate`, `replace`, and unknown actions remain unsupported. |

## Updated decision

The four findings are closed, and the design avoids a second Run owner or a generic runtime framework. **Approved for design-only builder handoff.** Root's ownership decision assigns `internal/production/telemetry.go` and `internal/production/telemetry_test.go` to R7-BLD-02; the designer will mirror that assignment in the design file in a small follow-up commit. The `assemble.go` producer-label/trace-owner hunks remain subject to root's serial ownership approval and R2/R4/R6 coordination. This approval is not implementation acceptance or phase-gate completion; the R1/R4/R6 seams remain governed by their separate implementation gates. No product tests were run.
