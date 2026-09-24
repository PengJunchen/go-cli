# R0-BLD02 TRACE01 lane: trace and log privacy

Status: implementation and lane-level checks complete, including the provider-error P1; awaiting unified audit.

## Scope and behavior

The default CLI tracer now redacts span attributes, event attributes, and non-empty status messages before data reaches the async queue and its JSONL or stdout exporter. The CLI also wraps its configured `slog` handler, so prompt, tool content, and `err`/`error`/`cause`/`original_error` fields emitted by the root CLI, Assembly, and REPL logging paths are masked before they reach the log file. The assembly tracer already had an exporter wrapper; this change closes the separate default `cmd/cli` tracer path.

Default redaction covers explicit sensitive attributes and known content keys, including prompt/message/args/cache-key fields, credentials, plan-mode `reason` and `summary`, hook/tool `output` and `command`, PR `title`, and error fields. Masked strings retain their byte length. Provider error summaries no longer include free-form response bodies; their typed category and HTTP status remain on error spans, while the span's `ERROR` status remains visible with a length-only status message. Span names, tool names, trace and span identifiers, timing, and numeric count metadata remain observable. The CLI invocation command label is now `command_name` so that command identity stays visible while free-form command text is redacted.

The slog handler redacts a sensitive parent group as a whole, carries sensitivity through `WithGroup("args")`, and copies group slices before recursion. Regression tests cover nested groups, scoped groups, original-record immutability, and concurrent handling of one record.

## Evidence

- [`evidence/privacy-red.log`](evidence/privacy-red.log) runs the three real CLI canaries against baseline `0014900`; all fail on captured raw prompt or tool-argument content. The one-shot canary sends the prompt through `enter_plan_mode(reason)` and also exercises a tool argument and idempotency cache key. The interactive canary captures the default REPL trace and log output, and the fallback canary covers trace-file creation failure.
- The same red log also records the new expected-red provider-error canaries: StatusMessage leakage, missing slog error-field masking, and a production CLI provider non-2xx body echoed into trace/log capture.
- [`evidence/privacy-green.log`](evidence/privacy-green.log) records the exact canary and redaction test lists, their passing runs, and the affected-package race run.

The P1 lane race check was `GOMAXPROCS=2 go test -race -p 2 ./internal/tracing ./internal/llm ./internal/core ./cmd/cli -count=1`. The captured canaries run through the production CLI and default `newTracing` plus Assembly; output is checked across JSONL trace files, the configured slog log file, stdout, and stderr.

## Limits

This is key-based default redaction, not a guarantee that every arbitrary or future attribute name is safe. `ProviderError.Message` remains available in memory for provider error classification; `Error()` omits that body, and default trace/log paths redact the named error fields. Callers that directly serialize that struct field or add custom logging still need their own boundary. Directly constructed exporters, custom log handlers outside the CLI wrapper, alternate telemetry destinations, and broader OTLP/Kafka identity or queue policy remain outside TRACE01 and need separate R7 review. An explicit trace redaction level of `full` still opts out of exporter masking; the CLI slog handler continues to mask known sensitive fields.
