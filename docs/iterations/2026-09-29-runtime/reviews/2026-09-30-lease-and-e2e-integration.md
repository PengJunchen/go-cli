# MCP lease and tagged E2E integration review

Reviewed source commits: `76bdf397` (integrated as `94f5e8b`), `01126cee`
(`d53d4a5`), `7c1f27dc` (`a354dc2`), and `b00ff799` (`2fc5e79`).

The MCP lease is captured with the immutable tool registration and acquired
before the final dispatch-fence commit, outside the fence lock. Direct,
streaming, and queued-worker paths release it after their synchronous
executor returns, including stale-fence, cancellation, error, and panic
paths. The generation serializes retirement and lease admission. Root review
accepted this narrow slice; the CLI generation reloader and source retirement
remain separate work. A process-group completion may outlive the executor
return, so that proof path must retain the lease until its joined completion
signal closes before the source retirement contract can be called complete.

The E2E changes repair fixtures against existing product contracts. Phase19
tests now use prepared approval stages and assert legacy opaque wrappers fail
closed. Phase23 sends a follow-up only while its sub-agent is running. Phase22
tracing now runs the real CLI and reads its flushed JSONL spans. Phase46 keeps
the caller-owned pipe blocked, asserts a truthful pending cleanup result,
then closes the peer and joins it. No production ACP or tracing behavior was
relaxed by these test changes.

Integrated verification: `go test -race -count=1 ./internal/tools
./internal/mcp ./internal/cli` passed 2,262 tests; the focused tagged tracing
selector passed 2 tests; Phase19/23 selector passed 5 tests; Phase46 stdio
selector passed 6 tests across three repetitions. The full tagged E2E gate
at `a354dc2` still failed exactly one test:
`TestET_Phase22_AllCapabilities_AC1_ToolRegistryCompleteness`, whose fixture
expects removed model-facing Plan Mode tools. Thirteen tests were skipped.
These are not release-gate completion evidence; the complete fixed-SHA
`make verify`, tagged gate, Dockerfile build, UI journey, and final review
remain pending.
