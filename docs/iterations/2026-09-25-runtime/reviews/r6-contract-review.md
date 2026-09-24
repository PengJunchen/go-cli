# R6 entrypoint design review

Reviewed authority: `r6-entrypoint-design.md` at `dfa419d0f0631b8092e2d7d943329f797f3f8521`. The prior review was against `4413962b36c3fe8b2800db228ab7810d5175f2eb`. The plural `r6-entrypoints-design.md` is not used. Source review used the current CLI/config/ACP/MCP sources, R2/R4 contract snapshot `4f8d1a0`, and the R3 credential resolver proposal referenced by R6.

Decision: **approved for design only**. The four findings below are closed by `dfa419d`. This does not approve implementation or claim that R6 execution gates have run; builders remain subject to the documented R1/R2/R3/R4 dependencies and R6 real-entrypoint verification.

## Closure of original findings

1. **P1 — Project config candidates and snapshots: closed.** R6 now lists all six paths in actual loader precedence, binds each path's present/absent state and present content, selects the first present candidate, forbids same-load fallback after source deletion/change, and requires fingerprinting, parsing, and registration to consume the same frozen bytes. The acceptance cases cover omitted higher-priority candidates appearing and selected-source deletion/fallback.

2. **P1 — MCP explicit cancel vs. transport disconnect: closed.** The revision distinguishes explicit local/Run-owner cancellation from unexpected HTTP/SSE EOF/reset. Explicit cancel stops local work and best-effort sends `notifications/cancelled` for the exact in-flight request ID; remote outcome remains unknown absent trusted terminal/quiescence evidence, with no replay. Unexpected disconnect only detaches transport and does not notify cancellation or retry the call. Session DELETE is explicitly teardown, not proof of remote stop. See the versioned [Transports](https://modelcontextprotocol.io/specification/2025-06-18/basic/transports), [Lifecycle](https://modelcontextprotocol.io/specification/2025-06-18/basic/lifecycle), and [Cancellation](https://modelcontextprotocol.io/specification/2025-06-18/basic/utilities/cancellation) requirements.

3. **P1 — Private ACP approval wire shape and exact-call binding: closed.** R6 now specifies an authenticated task/session-scoped `approval.request` event on `/events` and `POST /approval/respond`, including version, correlation, one-use ID, decision enum, response statuses, and rejection behavior. Server-side approval binding names the unique `(RunID, assistant ordinal, ToolCallID)` and immutable R4 `PreparedInvocation` identity, registration generation, effects, schema snapshot, and normalized-argument digest; the wire shape excludes raw arguments, credentials, and schema/digest. The acceptance list covers stale, replayed, expired, cross-subject/session/task/run/call and `always_allow` cases. The profile remains explicitly private; no official ACP interoperability is implied.

4. **P2 — MCP GET negotiation: closed.** The revision requires GET `Accept: text/event-stream`, includes GET Accept in fixture assertions, and specifies SSE or `405 Method Not Allowed` when the endpoint has no SSE stream. See the versioned [Transports](https://modelcontextprotocol.io/specification/2025-06-18/basic/transports).

## Boundaries accepted

- R6 correctly keeps `go-cli-acp-json/1` explicitly private and does not call the existing JSON routes gRPC or official ACP. The revision above only freezes this private profile's approval path.
- Fixing MCP at `2025-06-18`, rejecting unsupported negotiated versions, and keeping legacy HTTP+SSE as a separate opt-in profile matches the requested scope. The document does not chase the newest protocol or claim unimplemented interoperability.
- The MCP OAuth boundary is appropriately narrow: authorization is optional in MCP; an unsupported OAuth challenge fails closed, and R6 does not need to build a generic OAuth stack. Static bearer origin confinement remains distinct from OAuth and is correctly specified.
- R6 treats R2 durable sessions and R4 trusted effects/dispatch as dependencies, not as already implemented. It withholds persistence claims without R2 durable acknowledgment and blocks untrusted/unknown tools without R4 proof. Those source/implementation dependencies must remain gates for the relevant builders, but are not additional defects in this design.
- Prompt/help ordering, `AssembleAgent` reuse, Web unavailable disclosure, per-session/task event isolation, one-use approval, and run-level cancellation/status reporting are source-grounded and can be implemented without adding a generic Runtime framework.
