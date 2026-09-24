# R0 privacy code review

Review date: 2026-09-25  
Review target: integrated commit `2d3c333` (lane source `ac24ed2`)  
Review type: independent source review and focused race tests; no product files changed.

## Current decision

**Not accepted yet: two P1 redaction gaps remain.** The default prompt, tool-argument, plan-reason, cache-key, and nested slog attribute cases are covered, and the real CLI tracer wiring is present. However, raw error text can still reach both slog attributes and trace status messages. The privacy slice needs a source canary through the provider-error path before it is closed.

## What passed review

`cmd/cli.run` wraps the active default slog handler before configuration or command execution and restores it on return. The root `newTracing` path wraps the async exporter, including the stdout fallback when trace-file creation fails. `internal/cli.assembleTracing` independently wraps the exporter used by the production runtime. Thus the default one-shot and interactive CLI paths cover both actual tracers and the log file.

The exporter copies span/event attribute slices before masking. The slog wrapper recursively handles keyed and grouped values, carries sensitivity through `WithGroup`, copies group slices before recursion, and preserves the original record. The focused race selection passed:

```text
GOMAXPROCS=2 go test -race -p 2 ./internal/tracing ./cmd/cli \
  -run '^(TestRedactingSlogHandlerMasksPromptAndToolArgs|TestRedactingSlogHandlerMasksSensitiveGroups|TestRedactingSlogHandlerDoesNotMutateOriginalRecord|TestRedactingSlogHandlerConcurrentGroupDoesNotMutateRecord|TestDefaultTraceContainsNoPrompt|TestDefaultInteractiveTraceContainsNoPrompt|TestNewTracingFileFailureDoesNotEmitRawPrompt)$' \
  -count=1 -timeout 90s

ok   github.com/pengjunchen/go-cli/internal/tracing
ok   github.com/pengjunchen/go-cli/cmd/cli
```

The real CLI canaries cover prompt arguments, interactive input, a tool argument, `enter_plan_mode(reason)`, and the trace-file failure fallback. Existing tests also cover nested groups, scoped groups, source-record immutability, and concurrent handling.

## Required fixes

1. **[P1] Redact error-valued slog attributes.** `isSensitiveAttributeKey` does not classify `err`, `error`, `cause`, or `original_error` as sensitive. Current production code writes raw error strings under these keys, including `DefaultFailureTurnSynthesizer.Synthesize` / `FailureSynthesisMiddleware.Run` (`error` and `original_error`) and `NormalizeError` (`cause`). Provider error chains can contain a remote HTTP response body; truncating that body to 512 bytes does not make it safe to log. Extend the default redaction policy to these free-form error fields and add a provider non-2xx canary that echoes a prompt/tool-input marker.

2. **[P1] Redact span status messages.** `RedactingExporter.ExportSpan` copies and redacts span attributes and event attributes but forwards `SpanData.StatusMessage` untouched. Production error paths such as `LoopAgent` set the span error description from `err.Error()`. The same provider response body can therefore leak into JSONL/OTLP trace output even after the slog fields are fixed. Redact non-empty status messages under the default `redact` policy (preserving explicit `full` behavior) and make the real CLI provider-error canary inspect every trace and log output for the marker.

The privacy slice should remain pending until both paths are covered by the real CLI canary and the affected race tests pass. These findings do not invalidate the existing successful-path canaries; they identify error-path gaps those tests do not exercise.

## Scope notes

The explicit `RedactionLevelFull` configuration remains an intentional opt-out, and directly constructed exporters or custom log handlers outside the default CLI wrappers remain outside this R0 slice as recorded in `lane-privacy.md`. The current slog operation messages in reviewed production paths are fixed identifiers; the wrapper intentionally preserves them.
