# MCP generation reload design review

**Reviewed design:** [`mcp-generation-design.md`](../mcp-generation-design.md), based on `fb4d0cc216b3e121aa5afdec8597c7bd94e51a23`.
**Source checkout:** `1144e1ebc9f339a04d502f0bdca04069c9d30ff5`.
**Review type:** source-grounded design review only. No product files or tests were changed or run.

## Verdict

**Reject frozen implementation handoff pending the findings below.** The design identifies the right production seam: one fresh config candidate, one source-owned batch publication on the existing registry fence, prepared-call invalidation, and client retirement after active calls join. It must fix the close-lock order, make the lease acquisition safe for the fence, close two MCP stdio ownership gaps, and reconcile failed-publication behavior with the R6 fail-closed contract before a builder treats this as frozen.

## Findings

### P1 — The publisher can hold Close's synchronous admission fence while waiting for a deferred load

The proposed acquisition order is `assemblyMCPResources.mu -> DeferredToolRegistryAdapter.mu -> DispatchFence` ([design lines 65–72](../mcp-generation-design.md#L65)). `DefaultDeferredToolRegistry.Load` holds its write lock while invoking the loader and while registering the result in the backing registry ([`deferred.go:104-145`](../../../../internal/tools/deferred.go#L104)). A source-owned load can therefore keep the DTR lock while executing loader/registration work. If publication takes the owner mutex first and then waits for DTR, `assemblyCloseOwner.Close` cannot synchronously set `closing`: its first MCP action takes that same owner mutex ([`assembly_lifecycle.go:132-147`](../../../../internal/cli/assembly_lifecycle.go#L132)). A blocked load can thus block Close's promised admission fence.

Acquire the DTR transaction lock before the owner mutex, then check `closing`, publish under the fence, and update the current-generation pointer while holding the owner mutex. Close takes only the owner mutex, so it can still win during a slow DTR-lock wait; if it wins, the waiting publisher observes `closing` and cleans its staged candidate. Keep the full update path `DTR -> owner -> fence -> registry`, with no DTR/owner lock acquisition from the fence callback. Add a barrier test where a deferred load holds DTR, a publisher waits, and Close still synchronously rejects new reload admission before that load is released.

### P1 — `PreparedDispatchLease.Acquire` is an arbitrary method call inside the global fence

The API declares an interface method, then calls it under `DispatchFence.commit` before setting `DispatchCommitted` ([design lines 32–34, 45](../mcp-generation-design.md#L32)). The fence's existing contract says its commit marker must not block, enqueue, perform I/O, or invoke tools ([`prepared.go:179-197`](../../../../internal/tools/prepared.go#L179)). A comment that each implementation will be short does not constrain an interface call; a future lease implementation can block, acquire locks in a reverse order, or re-enter the fence while every registry/approval/plan commit is stopped.

Do not invoke the lease interface under the fence. Acquire the ref before entering `fence.commit`, then perform the existing full-snapshot/context check and set the commit marker under the fence; if that check loses, release immediately and do not execute. A speculative ref acquired just before a winning publication is safe: the old snapshot then fails the fence comparison and releases without dispatch. Alternatively use a concrete tools-owned refcount handle with an intrinsically local, non-callback acquire operation. Specify where the release is retained and deferred so all post-commit exits, including queued execution, cancellation after commit, panic, and streaming dispatch, release exactly once.

### P1 — Source removal on trust/publication failure conflicts with the R6 fail-closed rule

The design says every prepublication trust or collision error retains the full old generation ([design lines 7, 52–53](../mcp-generation-design.md#L7)). Its test contract also requires old definitions and clients to remain active after a failed candidate ([line 88](../mcp-generation-design.md#L88)). This includes a revoked project-trust gate and a checked registry collision. The original R6 contract says a registry-update failure disables that server's tools rather than retaining a stale executable definition ([`r6-entrypoint-design.md:85`](../../../../docs/iterations/2026-09-25-runtime/r6-entrypoint-design.md#L85)); its trust matrix also requires revoked project MCP configuration not to remain registered ([line 110](../../../../docs/iterations/2026-09-25-runtime/r6-entrypoint-design.md#L110)). With no fence publication on failure, old prepared calls also retain their prior snapshot and can still dispatch to the old client.

Separate ordinary candidate-build failures from authority withdrawal and registry publication failure. At minimum, a failed trust re-check must atomically disable/remove that source and stale prepared calls, then retire the old generation after its already-committed calls join. A checked registry-publication collision must follow the R6 disable rule as well; define a fail-closed removal path and its error handling. If retaining the old generation for any other candidate failure is intended, state that narrow availability policy explicitly and add tests showing it does not apply to trust revocation or failed registry publication.

### P1 — A failed stdio handshake can lose the child before the candidate owner can join it

The candidate contract requires ownership before `Connect` and a positive join for every failed candidate client ([design lines 53, 63–64](../mcp-generation-design.md#L53)). In the current stdio adapter, an initialize failure kills the child, clears `c.proc`, and returns without `Wait` ([`adapter.go:251-262`](../../../../internal/mcp/adapter.go#L251)). `Disconnect` then returns immediately because `connected` is false ([`adapter.go:269-276`](../../../../internal/mcp/adapter.go#L269)); the Assembly's default-client branch treats nil from that call as complete ([`assembly_lifecycle.go:561-569`](../../../../internal/cli/assembly_lifecycle.go#L561)). The proposed candidate cleanup therefore cannot prove that this child exited and was reaped.

Include the stdio adapter's failed-connect cleanup in this handoff: keep the child owned until kill/Wait completion, and do not let `Disconnect()==nil` serve as a false terminal signal after a failed handshake. Add a subprocess test with a child that starts, fails or stalls initialization, and verifies candidate rejection does not lose the child or report cleanup complete early.

### P1 — A live stdio call can prevent the disconnect that would unblock it

`JSONRPCLineTransport.Request` checks `ctx.Done()` before each read, but then performs a blocking `ReadString` that does not observe cancellation ([`adapter.go:88-97`](../../../../internal/mcp/adapter.go#L88)). For a silent stdio server, the prepared call cannot return and release its generation lease. The proposed retirement waits for leases before disconnecting ([design lines 9, 63](../mcp-generation-design.md#L9)), while Assembly close waits for the RunSlot to become idle before it joins reloaders and reaches `closeClients` ([`assembly_lifecycle.go:582-593,617-646`](../../../../internal/cli/assembly_lifecycle.go#L582)). The close path therefore cannot reach the disconnect that kills the child and breaks the blocked read; both generation retirement and Assembly close can remain pending indefinitely.

Define and implement the local cancellation/join path for in-flight stdio requests before claiming the old-client join contract. A cancellation must interrupt the read (or close/kill an owned resource through an exact request owner), and the request/child must be positively joined before its lease is released and the old generation is disconnected. If a client implementation cannot be interrupted, preserve it as pending/quarantined and expose that exact completion; do not silently treat a lease wait or `Disconnect` return as a completed call. Add a silent-child barrier test covering both reload retirement and Assembly close.

### P2 — Empty input schemas are collapsed into “missing” before validation

The design rejects a missing schema but accepts a valid JSON-object schema after freezing and prepared-subset validation ([design line 54](../mcp-generation-design.md#L54)). The existing MCP adapter returns `nil` from `Parameters()` whenever `ArgsSchema` has length zero ([`tool_adapter.go:121-128`](../../../../internal/mcp/tool_adapter.go#L121)). Thus a valid `inputSchema: {}` for a zero-argument tool is indistinguishable from a missing schema at the metadata seam and will reject the whole config generation. The current wire structs also do not retain an explicit presence bit for `inputSchema` ([`mcp.go:53-62`](../../../../internal/mcp/mcp.go#L53)).

Carry schema presence separately from the map value through MCP list decoding and the candidate metadata path. Accept and freeze `{}` as a present empty object, while continuing to reject a genuinely absent or non-object schema. Add a zero-argument MCP tool fixture with `inputSchema: {}` and a distinct missing-schema rejection case.

### P2 — The selected-file resolver still has an undefined empty/invalid-file policy

The current selector returns a config path only when the parsed declaration set is nonempty ([`assemble.go:2336-2395`](../../../../internal/cli/assemble.go#L2336)). `registerMCPToolsOwned` then exits before installing a reloader when `servers` is empty ([`assemble.go:2203-2207`](../../../../internal/cli/assemble.go#L2203)). Consequently, a trusted, valid empty project file present at Assembly startup is not selected or watched, so editing that existing file to add a server is outside the stated “selected source can add servers” behavior. The selector also continues to the global path after read/parse failures ([`assemble.go:2364-2376`](../../../../internal/cli/assemble.go#L2364)), while the design says a failed candidate must not fall through from project to global ([design line 52](../mcp-generation-design.md#L52)).

Resolve source precedence from the trusted/present source independently of its declarations, freeze that selected path at startup, and parse only that path for the generation. Define whether an empty trusted file is watched (the stated add-server behavior implies yes) and whether a malformed higher-priority file is a startup error; do not let the parse result silently select a lower-priority file. Add startup-empty and higher-priority-malformed cases to the source-selection tests.

### P2 — Deferred-name collision and load-cache rollback are not closed across lazy materialization

The batch collision check is described at publication time, but the names remain deferred. Later, ordinary `RegisterDeferred` replaces an existing loader by name without checking ownership ([`deferred.go:66-81`](../../../../internal/tools/deferred.go#L66)); ordinary eager `Register` is last-registration-wins ([`registry.go:20-22,53-56`](../../../../internal/tools/registry.go#L20)). A registration arriving after the batch can therefore overwrite a source-owned deferred entry or be overwritten when that entry is eventually materialized. Also, `Load` stores the definition in `loaded` before backing-registry registration, and leaves it there when registration fails ([`deferred.go:141-145`](../../../../internal/tools/deferred.go#L141)). Subsequent loads take the cached fast path and cannot retry the failed registration.

Define how active generation names are reserved against later eager/deferred registrations, or make source-owned materialization reject a foreign current owner without overwriting it. Do not cache a source-owned definition as loaded until its metadata/definition publication succeeds; on failure restore the unloaded, retryable state and retain the loader. Add tests for a late name collision and a forced registry-registration failure followed by a successful retry, asserting no foreign executor is replaced and the Registry fence advances only for successful publication/materialization.

### P2 — Describe startup all-or-nothing as an intentional behavior change

The design calls the no-MCP-tools-on-startup-error behavior the “existing outer Assembly policy” ([design line 59](../mcp-generation-design.md#L59)). The outer layer does warn and continue ([`assemble.go:976-979`](../../../../internal/cli/assemble.go#L976)), but the current registration loop logs a connect/list failure and continues registering other successful servers ([`assemble.go:2247-2259`](../../../../internal/cli/assemble.go#L2247)). The generation-wide all-or-nothing startup policy is accepted for this contract; the text should identify it as an intentional behavior change rather than existing behavior.

Keep an initial-assembly test: with two declared servers and one failure, Assembly continues per its outer policy, exposes no MCP tool from the incomplete source, and joins every staged client. This makes the accepted atomic-generation baseline explicit and guards candidate cleanup.

## Review boundary

No source or product tests were run. This review does not accept implementation, local MCP peer coverage, or full R6 completion. The existing one-owner close fence and `HotReloadLifecycle` completion signal remain useful seams; the required fixes should preserve them rather than add a second registry or cleanup owner.
