# Cache key privacy builder report

## Scope

Branch `codex/repair-20260929-cache`, based on `17ab7b76ef0b7636abff1e5a055de7acf6f6bb31`. This slice changes only the `newProductionToolWrapper` / `toolCacheKey` region of `internal/cli/assemble.go`, adds `internal/cli/assemble_cache_privacy_test.go`, and records this report. It does not change cache constructors, durable execution, or the production FIFO cache implementation.

## Change

Cache keys are now fixed-size `tool-cache:v1:<sha256-hex>` values. The digest input is domain-separated with `go-cli/tool-cache/v1`, then includes the tool name and canonical JSON arguments separated by NUL. Raw JSON is no longer retained in FIFO keys or exposed through its existing `cache_key` slog/span fields.

If `json.Marshal` fails, the wrapper skips both cache lookup and insertion and runs the tool normally on every call. It does not fall back to a tool-name-only key and does not surface the marshal error or its contents. A nil cache remains disabled, and mutation tools still bypass cache lookup/insertion.

## Test evidence

The compile-successful RED run failed on the prior behavior. It demonstrated raw arguments in spy cache keys and real FIFO log/span fields, a repeated unsupported argument being cached under the tool name, and the old key failing the fixed digest format/domain expectation. Mutation and nil-cache bypass behavior already passed.

After implementation, the focused suite passed:

```text
rtk proxy env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb go test -count=1 -run '^TestToolCache' -v ./internal/cli
```

It covers stable keys across map insertion order, different keys for tool-name/argument changes, the exact domain-separated digest, absence of sentinel/raw JSON from real FIFO logs and exported spans, marshal-failure bypass on repeated execution, and mutation/nil-cache bypass.

## Validation

The targeted race suite and full CLI race suite passed:

```text
rtk proxy env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb go test -race -p 2 -count=1 -run '^TestToolCache' ./internal/cli
rtk proxy env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb go test -race -p 2 -count=1 ./internal/cli
```

Static and formatting checks passed:

```text
rtk proxy env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb go vet ./internal/cli
rtk golangci-lint run ./internal/cli
rtk git diff --cached --check
```

This is local unit/race/static evidence only; it makes no live or production validation claim.
