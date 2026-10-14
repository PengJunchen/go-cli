# MCP config generation reload design

**Status:** revised implementation handoff; source unchanged. **Base:** `fb4d0cc216b3e121aa5afdec8597c7bd94e51a23`. This design completes the config and publication gap only. It reuses the existing `DefaultToolRegistry`, `DeferredToolRegistryAdapter`, `DispatchFence`, prepared invocation, `HotReloadLifecycle`, and Assembly MCP resource owner. It adds no registry or event store and preserves the accepted synchronous MCP reload-admission fence.

## Contract and error classes

An MCP config reload builds one fresh candidate generation. Read the currently selected config source, parse its complete server set, create fresh clients, connect and list every declared server, then validate the complete normalized-name and input-schema set before publication. No candidate tool is visible before one source-owned registry/DTR update advances the existing Registry fence once.

Failures have two different outcomes:

| Condition | Required state transition |
|---|---|
| Ordinary candidate failure while its config source remains trusted: malformed bytes, read error other than selected-file absence, endpoint/connect/list failure, invalid name, absent/malformed/unsupported schema | Retain the complete active generation and its clients. Do not publish any candidate subset. Return and expose the error; later edits/manual reload may retry. |
| Trust re-check says the active project source is revoked | Fail closed: atomically remove/disable that source's deferred names and materialized definitions through the Registry fence, staling old prepared calls; retire the old generation after already committed calls join. Return the trust error. |
| Checked registry publication rejects the candidate, e.g. a name is owned by a foreign registration | Apply the R6 fail-closed rule: atomically disable/remove the old source-owned generation and advance the Registry fence, then retire its clients after already committed calls join. Return the collision error. Never leave a stale executable generation because replacement failed. |
| A valid empty declaration or deletion of the already selected source file | Publish an empty generation and remove that source's tools. Do not fall through to another config file. |

The disable operation removes only names/definitions/reservations owned by the selected MCP source. It cannot remove a colliding foreign tool. The empty-source removal path does not run candidate collision validation, so a candidate collision does not prevent fail-closed removal. Every checked candidate-publication failure takes this disable path, not just name collisions. If the existing registry fence itself is unavailable and disable cannot be committed, report a hard source-disable failure, retain all old clients/resources, and require the Assembly owner to stop using/close this runtime; do not claim successful revocation or cleanup. This is an assembly-fatal invariant failure, not an ordinary candidate rejection.

After publication, an older prepared invocation fails its existing full `DispatchFenceSnapshot` comparison before its executor runs. A call that wins the final commit first holds a lease on its exact MCP generation through actual executor completion; retirement waits for that lease and then disconnects/joins the old client. This protects local ownership only and does not establish a remote side-effect outcome.

## Current source and proposed API

`internal/cli/assemble.go` currently discovers a path, connects clients in `registerMCPToolsOwned`, and calls `registerMCPToolBatch`, which registers each tool separately with `RegisterDeferred`. `createAndStartHotReloader` captures one old client and a callback with no error result. Replace that CLI path with one complete candidate builder and one config-level reloader per selected file. Keep the old `HotReloadLifecycle` contract and `Done`/`Wait` join behavior.

Use a narrow lock-owning DTR transaction instead of a callback under `DispatchFence`:

```go
type MCPConfigReloadFunc func(context.Context, string) error

func NewConfigHotReloader(reload MCPConfigReloadFunc, opts ...HotReloaderOption) HotReloader

type MCPToolCandidate struct {
	Definition ToolDefinition
	Lease      PreparedDispatchLease
}

type MCPGenerationUpdate struct { /* private fields; owns the DTR transaction lock */ }

func (a *DeferredToolRegistryAdapter) BeginMCPGenerationUpdate(
	ctx context.Context,
	sourceID string,
	generation uint64,
	tools []MCPToolCandidate,
) (*MCPGenerationUpdate, error)

func (u *MCPGenerationUpdate) Publish() error
func (u *MCPGenerationUpdate) Disable() error
func (u *MCPGenerationUpdate) Abort()

type PreparedDispatchLease interface {
	Acquire() (release func(), err error)
}
```

`BeginMCPGenerationUpdate` freezes/prevalidates candidate metadata, then takes and retains `DeferredToolRegistryAdapter.mu`; it runs no network or client work. The CLI next takes `assemblyMCPResources.mu`, checks `closing`, and calls `Publish` while holding both locks. `Publish` contains no external callback: it takes the shared fence and backing registry locks, checks reservations/ownership, updates registry plus DTR-owned maps in one short publication, and increments Registry generation once. Successful `Publish` consumes the transaction and releases DTR. A checked publication error changes no state and leaves the transaction lock held, so the caller can run `Disable` for the fail-closed R6 transition or `Abort` after handling an invariant failure. Successful `Disable` consumes/releases the transaction. The CLI updates current/retiring owner pointers before releasing the owner mutex. If Close set `closing` while the builder waited for DTR, the CLI aborts the transaction and cleans the staged candidate without publishing.

`Disable` is the same held transaction's fail-closed empty update. It removes that source's owned definitions, DTR loaders/cache, and reservations, and advances the Registry fence once without validating or removing foreign names. On any checked `Publish` failure, call `Disable` before releasing the transaction lock, then retire the old generation. Trust revocation uses `Disable` directly. `Abort` only releases the DTR lock and is safe after an unpublished candidate failure. None of these operations invokes owner code inside the fence callback.

The `sourceID` is an assembly-local opaque owner key, not a file path, endpoint, credential, or durable authority. `generation` increases monotonically for that owner. Add owner/generation fields to DTR entries and `toolRegistrationMetadata`; use a distinct MCP registration-source label and `unknownTrustedEffects()`. Do not infer network-read trust from server declarations, schema, project trust, or successful connection.

The DTR update stores the complete source-owned loader/name/schema set as one generation; MCP tools stay lazy. Each candidate loader is an immutable closure over its copied definition and client-generation lease, and its frozen schema metadata is prepared before publication. On `Load`, register a source-owned definition only if its owner/generation token is still current. The existing DTR write lock serializes loader execution and registration against replacement: if `Load` wins, replacement waits and removes its result; if replacement wins, it removes the old loader/cache before releasing the lock. Keep the generation-token recheck as defense against a future loader implementation moving work outside that lock.

`DeferredToolRegistryAdapter.List` must acquire the DTR read lock **before** taking the concrete backing `DefaultToolRegistry.List` snapshot, and retain it while merging that snapshot with deferred placeholders. The current backing-first implementation can capture old materialized definitions, wait through a generation publication, and then merge new placeholders; fixing only the write transaction cannot prevent this mixed-generation list. The read path follows DTR → registry order and invokes no owner or fence callback. A barrier regression must force publication between the old backing snapshot and the deferred merge on the baseline, then prove the corrected path returns one complete old or new generation, never a mixture. Single-key Get/materialization and prepared registration snapshots keep their existing generation checks; they do not replace the list consistency test.

Track active MCP name reservations in both the DTR and `DefaultToolRegistry`, including names not yet materialized. Ordinary `DeferredToolRegistryAdapter.Register`/`RegisterDeferred` and direct `DefaultToolRegistry.Register` must reject attempts to shadow a reserved MCP name; only the matching generation transaction/materializer may update it. Candidate collisions with foreign eager tools, deferred names, or reservations fail before mutation. Materialization rechecks reservation owner and generation under the registry lock so it cannot overwrite a late foreign owner.

Do not cache a source-owned definition in `loaded` until the underlying owner-aware registry publication succeeds. If that publication fails, leave the source loader and reservation installed, clear any attempted loaded entry, and return a retryable error. A later load may retry. Failed materialization must not bump the Registry fence; each successful materialization may use the existing single-tool registry publication. The config-generation batch itself still has exactly one fence increment, independent of tool count.

## Prepared dispatch lease boundary

Carry the optional `PreparedDispatchLease` from source-owned registration metadata through `registeredToolSnapshot` into `preparedInvocationConfig`. **Do not call `Acquire` inside the global fence.** In the final dispatch method, first acquire a speculative generation reference outside the fence. Then run the existing full-snapshot and context check inside `DispatchFence.commit`; on success mark `DispatchCommitted`, otherwise release the speculative reference immediately and do not execute. If replacement wins after a speculative old lease was acquired, the old snapshot check fails and release occurs before return; if final commit wins first, the pre-acquired lease keeps that generation alive.

Store the successful release in the one-use prepared dispatch path and defer it across every post-commit exit: cancellation checked after commit, ordinary executor return/error, panic unwind, queued worker execution, and streaming execution through the synchronous stream join. Release exactly once. `Acquire` is outside the fence and may wait without blocking registry/approval/plan publication; the lease implementation itself must not acquire DTR, owner, or fence locks. The final fence callback stays limited to checking `ctx.Err()` and setting the commit marker.

## Config source selection and candidate building

Resolve the selected file independently of parsed declarations:

1. A non-empty `rc.MCP.Servers` remains static in-memory configuration with no watched file.
2. Otherwise, select the trusted project `.go-cli/mcp.json` when that file is present, even if its valid declaration set is empty or its contents are malformed. If no trusted/present project file is selected, select the global file when present and accepted by the existing global-file validation. Source precedence is decided by presence/trust, not successful parse or non-empty server count.
3. Freeze that selected path for the Assembly lifetime. Parse only it. A malformed trusted higher-priority project file is an initial source error and does not fall through to global. Keep a watcher for the selected file so a later valid edit can recover. A valid empty project file present at startup is selected and watched so adding a server is supported. If no file is selected at startup, creating one later is outside this slice. Deleting the selected file means an empty generation, not fallback.

For each callback, check trust before treating a missing file as an empty declaration. If the active project source is no longer trusted, use the fail-closed disable transition above. Preserve current `servers` array and `mcpServers` map formats. Parse/validation errors retain the old generation when trust remains. Reject duplicate server names, empty server/tool names, invalid transport declarations, and duplicate normalized names; sort map-form keys before staging for deterministic validation.

Create fresh clients for every current declaration, and have the owner retain each staged client before `Connect`. Connect and list every declared server before publication; any ordinary failure rejects the whole candidate and disconnects/joins every staged client without changing the active generation. Do not reconnect/reuse active clients. Copy each returned MCP tool into candidate-owned immutable data before capturing it in a deferred loader.

MCP wire decoding must preserve schema presence separately from the map value. Add an `InputSchemaPresent` bit (or equivalent `RawMessage` presence capture) to `MCPTool`; `MCPToolAdapter.Parameters` must distinguish absent from present `{}`. Accept an explicitly present empty JSON object as a zero-argument schema by normalizing it to the prepared validator's frozen object form with empty `properties` and `additionalProperties:false`. Reject a genuinely absent schema, explicit `null`, non-object schema, malformed JSON value, or unsupported prepared-schema keyword before publication. Do not silently turn absence into `{}` or `{}` into an unconstrained argument bag.

Initial MCP registration uses the same generation-wide all-or-nothing candidate path. This is an **intentional behavior change**: current assembly skips a failed server and registers other successful servers, but the accepted generation contract does not expose a partial source generation. On initial candidate failure, the existing outer Assembly behavior may warn and continue without tools from that source; it must join every staged client and emit no partial registration success log. A malformed selected file likewise selects that source, produces no MCP tools, does not fall through, and remains watched for repair.

## Client ownership prerequisites

The current `MCPClient` interface exposes `Connect`, `Disconnect`, `ListTools`, and `CallTool`, but no exact stdio request/process completion owner. `HotReloadLifecycle.Done/Wait` joins the reload callback, not a child process or blocked request. HTTP connect/close pending errors expose an exact operation `Done`; this does not close the stdio gaps.

The separate stdio prerequisite remains pending assignment/acceptance before this builder may claim generation-safe reload for stdio sources: (1) a failed initialize must retain the child through kill and `cmd.Wait`, with a positive completion result; (2) cancellation of an in-flight request to a silent child must interrupt its blocked read/owned process and join that exact request before its generation lease is released. Current `Disconnect()==nil` after failed handshake is not proof of child reap, and checking `ctx.Done()` before blocking `ReadString` is not proof of request cancellation. Do not design a second generic owner here. Root must assign and accept the exact stdio adapter lifecycle seam first. Until then, gate candidate publication for stdio-backed sources and report that transport as unsupported for this reload slice; HTTP-only local peers do not clear this prerequisite.

For supported clients, `assemblyMCPResources` retains candidate/current/retired generation records with client set, lease count/drained signal, retiring state, and exact disconnect completion. Candidate failures disconnect/join unpublished clients. After publication, retired generations stay owned until leases drain and every client disconnect has a positive terminal signal. A pending disconnect waits on its exact `Done()` and re-enters the existing idempotent disconnect path. Do not infer quiescence from a nil result when the client has no accepted completion proof.

## Locks and close ordering

The order is deliberately `DTR -> assemblyMCPResources owner -> DispatchFence -> DefaultToolRegistry` for generation publication. Deferred `Load` takes DTR, then fence, then backing registry. Final prepared dispatch acquires its speculative generation lease before the fence; lease acquire/release never obtains DTR, owner, or fence. No owner/DTR locks are acquired from a fence callback.

The candidate builder performs read/parse/connect/list/schema work without any DTR, owner, fence, or registry lock. `BeginMCPGenerationUpdate` may wait for DTR while holding no owner lock. This is critical: `assemblyCloseOwner.Close` must be able to take the owner's short mutex and synchronously publish `closing=true` even while a loader holds DTR. After the builder obtains the DTR transaction, it takes owner; if Close won, it observes `closing`, aborts, unlocks DTR, and joins candidate cleanup. If publication wins, it holds owner through the brief fence transaction and updates the current pointer before releasing owner; Close then observes that generation.

Preserve the existing accepted order after synchronous admission fencing: Close requests reload stop, waits the RunSlot idle signal, joins reloader `Done/Wait`, then joins client/generation cleanup. A generation retirement callback waits for already committed calls before disconnect. This order is viable for supported HTTP clients; stdio remains gated on its separate read/process join prerequisite above. Do not hold DTR/owner/fence while waiting for calls or disconnecting.

## Reload errors and RED acceptance

The config reload callback returns `error`. Candidate build errors are observable failed attempts, not watcher-terminal failures: record one in-memory `LastAttempt` value or equivalent status and emit one structured failure log; keep the watcher alive for a later edit/manual trigger. Do not log a successful generation publication after partial work. On a trust or collision failure, report the candidate error only after the fail-closed disable transaction and old-generation retirement have been initiated/joined as required. A retirement failure after a successful publication is reported separately; it cannot roll back the linearized new generation. If disable publication itself fails, return the hard source-disable error and keep resources owned.

Before implementation, add deterministic RED barriers around the public Assembly/DTR/prepared paths. These tests must distinguish ordinary availability failures from authority revocation and disable failures:

| Test | RED assertion |
|---|---|
| `TestMCPConfigSourceSelectionUsesPresencePrecedence` | A trusted present empty project file is selected and watched; malformed higher-priority content does not fall through to valid global config; a selected file deletion clears its generation. |
| `TestMCPConfigReloadPublishesCompleteGeneration` | Endpoint/tool-set replacement exposes the new complete set, removes old names, and no observer sees a mixed generation. |
| `TestMCPConfigReloadFailureRetainsTrustedGeneration` | Malformed bytes, connect/list failure, or unsupported schema while trust remains valid keeps all old tools/clients and exposes an attempt error; a later valid edit succeeds. |
| `TestMCPTrustRevocationDisablesAndStalesGeneration` | Revoke project trust; atomically remove the source, bump the Registry fence, reject old prepared invocation before execution, and join old committed leases before disconnect. |
| `TestMCPRegistryCollisionDisablesOldSource` | Candidate name collides with a foreign owner; no candidate prefix appears, the old source is disabled by the source-only removal path, foreign tool remains, and old prepared calls stale. |
| `TestMCPDTRWaitDoesNotBlockSynchronousCloseFence` | Hold DTR in a blocked deferred load; let a publisher wait for DTR; Close still sets `closing` synchronously; after load release the publisher aborts without fence publication and joins staged clients. |
| `TestMCPPreparedLeaseAcquireOutsideFence` | Block speculative `Acquire`; unrelated fence publication proceeds. Then test both race orders: stale snapshot releases exactly once without execution; winning dispatch holds lease through ordinary/queued/streaming executor return, cancellation, and panic unwind. |
| `TestMCPDeferredLoadCannotRepublishRetiredGeneration` | Block an old source-owned Load; replacement serializes behind it then removes its result; replacement-first clears old loader/cache; subsequent Load cannot resurrect the old executor. |
| `TestMCPDeferredLateCollisionAndRetryableLoad` | Late eager/deferred registration cannot shadow a reserved name. Forced backing-registry publication failure does not cache a source definition; the loader remains retryable, and only a successful publication bumps the fence. |
| `TestMCPInputSchemaPresenceAndEmptyObject` | Explicit `inputSchema:{}` is preserved as present and accepted as zero-argument; absent, null, and non-object schemas fail distinctly before publication. |
| `TestMCPInitialCandidateIsAllOrNothing` | With two declared servers and one failure, outer Assembly continues without tools from that source, no partial MCP tool appears, and every staged client is joined. This is an intentional change from current per-server partial registration. |

Stdio process/request join probes belong to the separate adapter prerequisite, not this generation builder test claim. Use local in-process MCP HTTP peers and controlled non-stdio `MCPClient` doubles here; they establish this repository's path only, not official SDK interoperability or remote effect outcomes. Run focused `internal/tools`, `internal/mcp`, and `internal/cli` race suites, `go vet`, and lint after implementation; this design revision ran no tests.

## Implementation ownership

| File | Scoped responsibility |
|---|---|
| `internal/cli/assemble.go` | Presence/trust-based source selection; complete candidate staging; transaction order DTR then owner; one config reloader; initial all-or-nothing behavior. |
| `internal/cli/assembly_lifecycle.go` | Candidate/current/retired ownership; quick synchronous `closing` fence; lease/client completion join. |
| `internal/mcp/hot_reload.go` | Error-returning config cycle, attempt status, continued watch after ordinary candidate failure, existing `Done`/`Wait` lifecycle. |
| `internal/mcp/mcp.go`, `internal/mcp/adapter.go`, `internal/mcp/tool_adapter.go` | Preserve schema presence; stdio lifecycle remains a separately assigned prerequisite before stdio reload can be enabled. |
| `internal/tools/deferred.go` | Lock-owning source generation transaction, generation-tagged loaders, name reservations, serialized Load/replace, cache only after successful publication. |
| `internal/tools/registry.go`, `internal/tools/trusted_registry.go`, `internal/tools/prepared.go`, `internal/tools/prepared_registry.go` | Checked owner-scoped batch/remove fence update; unknown MCP effects; pre-acquired lease carried through final fence check and joined execution. |
| Focused tests in those packages | RED barriers above, plus existing close/reloader/prepared-registry lifecycle tests. |

This is design, not implementation or release evidence. Config-driven MCP reload stays gated until the HTTP-supported generation slice is implemented and reviewed; stdio remains unsupported until its distinct process/request completion prerequisite is accepted.
