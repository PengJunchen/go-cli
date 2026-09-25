# R7 Tracing Discovery Code Review

Review date: 2026-09-25  
Review type: independent fixed-commit implementation review

## Reviewed snapshot

| Item | Snapshot |
|---|---|
| Base commit | `659605c55a856344fe3f7fbb91c1de768eabe4e3` (`fix(tracing): unify CLI exporter ownership`) |
| Final reviewed commit | `5fdc12e47aa752fc478bddb7b7a4c7cae2b7315f` (`fix(tracing): make default trace files invocation-unique`) |
| Worktree | `/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/r7-tracing-discovery` |
| Worktree state | clean at final review time |

## Decision

**Accepted for this R7 tracing discovery slice.** The initial finding about second-resolution default filenames was closed by `5fdc12e`. This is not approval of the broader R7 lifecycle work or its gates.

### Closure of the default JSONL file identity finding

The default now uses the existing 128-bit `generateID()` (`internal/tracing/runtime.go:74-79`; implementation at `internal/tracing/span.go:193-197`). The project supports Go 1.24+, where `crypto/rand.Read` fills the buffer or panics on a random-source failure; the ignored return therefore does not silently turn a failed read into a zero/empty ID. The resulting 32-character hex ID has negligible collision probability. Explicit `SessionID` remains unchanged and retains append behavior.

`TestRunConsecutiveInvocationsUseDistinctTraceFiles` performs two real `run("version")` calls back-to-back against the same directory, requires two files and disjoint trace identities. `TestBuildTracingRuntimeExplicitSessionIDAppends` verifies explicit IDs still use one append file with both trace IDs.

## Accepted checks on this snapshot

- `BuildTracingRuntime` returns before constructing a sink or async worker when tracing is disabled or `none` is selected. Only empty/default, `jsonl`, `stdout`, and `none` are supported; unsupported values such as `otlp` and `kafka` return `UnsupportedExporterError` before trace-file creation or stdout fallback (`internal/tracing/runtime.go:50-94`). The supported-exporter predicate is shared with config validation. `go list` confirmed `internal/config` may import `internal/tracing` without a cycle because `internal/tracing` does not import `internal/config`.
- A JSONL creation failure is returned to the CLI and does not fall back to stdout. The CLI reports runtime creation errors before command dispatch (`cmd/cli/main.go:72-75`).
- `run` owns the root invocation span and exporter: it ends the root before flushing and shutting down the exporter, reports shutdown errors to stderr, and changes a successful exit to failure when shutdown fails (`cmd/cli/main.go:92-114`). Command failure still follows the same root-end and shutdown path.
- Assembly extracts the tracer from the invocation context and does not create or shut down another exporter (`internal/cli/assemble.go:766-769`). Tests cover both the absent-owner no-op path and reuse of the root tracer with its parent span identity.
- The real prompt test uses an HTTP provider and verifies one trace ID across CLI/assembly spans, one JSONL file for that run, and absence of prompt/tool-argument canaries from default trace and log output. The provider-error path also exercises cleanup after a failed HTTP request.

The original `659605c` implementation review enumerated and ran the following focused tests; all four package runs passed under the race detector:

```text
rtk env GOMAXPROCS=2 go test -race -p 2 -count=1 -run '^(TestSupportedExporterNames|TestBuildTracingRuntimeDisabledAndNoneCreateNoArtifacts|TestBuildTracingRuntimeJSONLUsesSingleTraceIdentity|TestBuildTracingRuntimeStdoutUsesConfiguredWriter|TestBuildTracingRuntimeRejectsUnsupportedExportersBeforeSideEffects|TestBuildTracingRuntimeJSONLFailureDoesNotFallbackToStdout|TestAssembleAgentWithoutInvocationTracerIsNoop|TestAssembleAgentReusesInvocationTracerAndDoesNotShutdownIt|TestDefaultValidator_InvalidTracing|TestDefaultValidatorTracingExporterMatchesRuntimeSupport|TestNewTracingDisabledReturnsNil|TestNewTracingNoneReturnsNil|TestNewTracingUnsupportedExporterReturnsTypedError|TestRunDisabledAndNoneTracingCreateNoTraceArtifacts|TestRunStdoutTracingUsesCLIStdoutWriter|TestRunUnsupportedTracingExporterFailsBeforeCommand|TestRunEmitsRealSpans|TestRunEmitsErrorSpanStatus|TestNewTracingSpanNonNoop|TestNewTracingFileFailureDoesNotFallbackToStdout|TestDefaultTraceContainsNoPrompt|TestDefaultTraceAndLogsRedactProviderErrorBody|TestDefaultInteractiveTraceContainsNoPrompt)$' ./internal/tracing ./internal/cli ./internal/config ./cmd/cli
```

After `5fdc12e`, the delta tests and adjacent JSONL identity test passed under the race detector, and `go vet` passed for both affected packages:

```text
rtk env GOMAXPROCS=2 go test -race -p 2 -count=1 -run '^(TestRunConsecutiveInvocationsUseDistinctTraceFiles|TestRunEmitsRealSpans|TestBuildTracingRuntimeExplicitSessionIDAppends|TestBuildTracingRuntimeJSONLUsesSingleTraceIdentity)$' ./cmd/cli ./internal/tracing
rtk env GOMAXPROCS=2 go vet ./cmd/cli ./internal/tracing
```

The reviewed scope intentionally does not claim the full R7 gate. In particular, stdout tracing mixed with prompt JSON or interactive streaming is an acknowledged follow-up preflight/rejection case, not a result of this review.
