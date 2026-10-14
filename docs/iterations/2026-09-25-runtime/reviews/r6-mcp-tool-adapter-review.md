# R6 MCP Tool Adapter Review

Reviewed fixed commit `4e9d96566d4285c14353b6cbdab77ff694e0a7f1` against base `009f31c` in `/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/r6-mcp-tool-adapter`. The product diff is limited to `internal/mcp/tool_adapter.go` and `internal/mcp/tool_adapter_test.go`.

## Findings

No blocker found in this slice. It meets the approved R6 adapter handoff requirements:

- `Execute` creates `tool.call`, passes the returned `spanCtx` into `MCPClient.CallTool`, and defers `span.End()` before every success, error, nil-result, or panic return. The trace test verifies the adapter span is exported and that the client-created child span has the adapter span as parent with the same trace ID.
- A nil result with nil client error returns `ErrInvalidToolResult` through `%w`, marks the span unsuccessful, and returns without dereferencing the result.
- A client error remains the returned error and is marked unsuccessful. MCP `IsError` remains a protocol-level tool result: the adapter returns the content with `tools.ToolResult.IsError=true`, and marks the span unsuccessful rather than dropping the flag.
- `SpanFromContext` derives the client context from the caller context, preserving its cancellation/deadline. With tracing inactive it returns the caller context unchanged. This slice does not add retries or alter HTTP/registry behavior.

## Validation

I enumerated the adapter tests with `rtk env GOMAXPROCS=2 go test -list 'TestMCPIsErrorAndNilPropagate|TestMCPToolAdapterPassesSpanContext|TestMCPToolAdapterExecutePropagatesError|TestMCPToolAdapter' ./internal/mcp`. The list included existing normalization, end-to-end, error propagation, registry, and prompt-injection cases, plus the new nil/`IsError` and span-propagation cases.

The focused boundary tests passed independently under race detection:

```text
rtk env GOMAXPROCS=2 go test -p 2 -race -count=1 -run '^(TestMCPToolAdapterExecutePropagatesError|TestMCPIsErrorAndNilPropagate|TestMCPToolAdapterPassesSpanContext)$' ./internal/mcp
ok github.com/pengjunchen/go-cli/internal/mcp 1.015s
```

The full MCP package race and lint results were supplied by the builder and were not repeated. **Verdict: accept `4e9d96566d4285c14353b6cbdab77ff694e0a7f1` for the MCP tool adapter slice only.** This does not approve MCP Streamable HTTP, reload, registry-generation, or run-owner lifecycle gates.
