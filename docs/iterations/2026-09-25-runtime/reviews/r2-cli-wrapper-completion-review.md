# R2 CLI wrapper completion attestation review

Reviewed fixed source commit `29765ab24707837abf82bc19863630b398247d83` (parent `b7f01bcc9ffa2d9dcd62ebf5dd80b462ddbe3a20`) in detached worktree `/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/r2-cli-sync-wrappers-review-29765ab`. The source worktree remained clean. This follow-up closes the default CLI Read/Ls wrapper-chain gap identified in `r2-read-ls-completion-review.md`; it does not assess the `runOwner` consumer or close broader R2/R4.

## Finding

**The bounded CLI attestation is accepted, subject to a documentation-only clarification below.** The assembly constructs the fixed chain `PromptInjectionToolWrapper -> newProductionToolWrapper(nil, s.auditLog, s.telemetry, ...) -> ResultMaskingWrapper` and installs that exact local slice in `PreparedToolStages` (`internal/cli/assemble.go:1225-1248`). The helper grants the synchronous attestation only when telemetry is a non-nil `*production.DefaultTelemetry` and audit is nil or a non-nil `*production.DefaultAuditLog` (`:1255-1268`). Assembly creates these components itself; audit is created from the configured path through `NewDefaultAuditLog`. There is no CLI option that injects a custom audit or telemetry sidecar. A replacement sidecar fails the type check and suppresses the proof.

The production wrapper receives a nil result cache, invokes `next`, then calls audit and telemetry before returning (`internal/cli/assemble.go:2624-2667`). `DefaultAuditLog.Log` holds its mutex while building the hash and appending the JSONL line; `appendLine` closes the file before returning (`internal/production/audit.go:118-200`). `DefaultTelemetry.Record` updates its in-memory maps under its mutex before returning (`internal/production/telemetry.go:91-138`). These implementations do not detach tool execution work. The wrapper ignores audit/telemetry errors, so the completion proof means that the configured append/record attempt returned; it does not prove the audit record was successfully written or made crash-durable with `fsync`.

`Dispatch` validates context before entering execution; after the inline signal begins, the deferred completion is finalized only after the captured executor and its wrappers return (`internal/tools/prepared.go:688-748`). A pre-cancelled dispatch therefore does not get a completion signal. If cancellation occurs while Read/Ls is running, any error return still passes synchronously through the wrapper chain before `Done` closes. The new assembly test exercises Read/Ls success and error paths, asserts `EffectUnknown` for each, and confirms four audit lines after all calls (`internal/cli/assemble_prepared_completion_test.go:17-78`). It does not deterministically cancel an assembled Read/Ls mid-call; source review supports this cancellation behavior, while `TestPreparedCompletionCanceledQueueWaitsForExactAcceptedJob` covers the separate queued-mutation rule that cancellation is not job completion.

`SynchronousExecutionWrappers` is a public, explicit trusted-host attestation accepted by `SetPreparedToolStages`; the flag is not cryptographically bound to wrapper identities. Under the approved trust boundary, this is deliberate: the actual CLI path fixes the concrete wrapper slice and exposes no custom sidecar injection, while a host that deliberately replaces stages must explicitly re-attest its own chain. That does not grant effects: the assembled Read/Ls regression continues to assert `EffectUnknown`. Do not describe arbitrary custom wrappers as automatically trusted or claim sandbox, diff-generator, or workspace-read authorization from this completion proof.

## Documentation follow-up

The fixed CLI trace runtime wraps enabled JSONL/stdout exporters in an invocation-owned `AsyncExporter` (`internal/tracing/runtime.go:49-94`). `DefaultAuditLog.Log` and `DefaultTelemetry.Record` create spans from the tool context, and `localSpan.End` calls the exporter (`internal/production/audit.go:118-166`, `internal/production/telemetry.go:91-138`, `internal/tracing/span.go:265-281`). With the CLI runtime, that call enqueues the diagnostic span and returns before the exporter worker flushes it. This is a separately owned R7 diagnostics pipeline, not a tool mutation continuing after executor return. The `PreparedToolStages` contract should say explicitly that completion proves the tool pipeline and synchronous configured audit append/telemetry update have returned; it does not promise audit `fsync` or tracing-exporter flush. The CLI tracing owner drains its exporter at invocation shutdown, and pending diagnostic export must not be reported as an unknown tool execution effect. This is a documentation clarification, not an implementation blocker.

## Validation

Test discovery was listed before running the target race:

- `rtk proxy go test -list 'TestPreparedCompletion' ./internal/tools` listed 18 completion tests.
- `rtk proxy go test -list 'TestAssembledReadAndLSCompletionCoversDefaultWrappers' ./internal/cli` listed the assembly regression.
- `rtk proxy go test -list 'TestAuditLogConcurrentWrites|TestAuditLogLogAndQueryRoundTrip' ./internal/production` listed the two direct audit tests.

Reviewer-run target race passed:

```text
rtk env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb go test -race -p 2 -count=1 -run '^(TestPreparedCompletion.*|TestAssembledReadAndLSCompletionCoversDefaultWrappers|TestAuditLogConcurrentWrites|TestAuditLogLogAndQueryRoundTrip)$' ./internal/tools ./internal/cli ./internal/production
ok  github.com/pengjunchen/go-cli/internal/tools       1.224s
ok  github.com/pengjunchen/go-cli/internal/cli         1.086s
ok  github.com/pengjunchen/go-cli/internal/production  1.039s
```

No behavioral RED was run for this review. No product source was modified. The report is intentionally unstaged in the shared main checkout.

## Scope limits

This review covers the default CLI wrapper attestation and its local completion signal only. It does not validate session persistence, the durable run-owner consumer, remote or Bash completion, or full R2/R4/R5/R8 closure. Completion here is not authorization and does not change `EffectUnknown`.
