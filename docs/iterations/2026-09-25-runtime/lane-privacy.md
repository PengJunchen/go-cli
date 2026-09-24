# R0-BLD02 TRACE01 lane: trace and log privacy

Status: implementation and lane-level checks complete; awaiting unified audit.

## Scope and behavior

The default CLI tracer now redacts span attributes and event attributes before data reaches the async queue and its JSONL or stdout exporter. The CLI also wraps its configured `slog` handler, so prompt and tool content emitted by the root CLI, Assembly, and REPL logging paths is masked before it reaches the log file. The assembly tracer already had an exporter wrapper; this change closes the separate default `cmd/cli` tracer path.

Default redaction covers explicit sensitive attributes and known content keys, including prompt/message/args/cache-key fields, credentials, plan-mode `reason` and `summary`, hook/tool `output` and `command`, and PR `title`. Masked strings retain their byte length. Span names, tool names, trace and span identifiers, status, timing, and numeric count metadata remain observable. The CLI invocation command label is now `command_name` so that command identity stays visible while free-form command text is redacted.

The slog handler redacts a sensitive parent group as a whole, carries sensitivity through `WithGroup("args")`, and copies group slices before recursion. Regression tests cover nested groups, scoped groups, original-record immutability, and concurrent handling of one record.

## Evidence

- [`evidence/privacy-red.log`](evidence/privacy-red.log) runs the three real CLI canaries against baseline `0014900`; all fail on captured raw prompt or tool-argument content. The one-shot canary sends the prompt through `enter_plan_mode(reason)` and also exercises a tool argument and idempotency cache key. The interactive canary captures the default REPL trace and log output, and the fallback canary covers trace-file creation failure.
- [`evidence/privacy-green.log`](evidence/privacy-green.log) records the exact canary and redaction test lists, their passing runs, and the tracing package race run.

The lane race check was `GOMAXPROCS=2 go test -race -p 2 ./internal/tracing -count=1`. The captured canaries run through the production CLI and default `newTracing` plus Assembly; output is checked across JSONL trace files and the configured slog log file.

## Limits

This is key-based default redaction, not a guarantee that every arbitrary or future attribute name is safe. Directly constructed exporters, custom log handlers outside the CLI wrapper, alternate telemetry destinations, and broader OTLP/Kafka identity or queue policy remain outside TRACE01 and need separate R7 review. An explicit trace redaction level of `full` still opts out of exporter masking; the CLI slog handler continues to mask known sensitive fields.
