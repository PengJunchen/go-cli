# MCP generation reload design rereview

**Reviewed design:** [`mcp-generation-design.md`](../mcp-generation-design.md), SHA-256 `1123b3285e9fa9f5df4eb083875a676e651f7e0b1fbe708178bdd804ebd4c1fc`.
**Product source:** `25577ec5b3876439216ae2423014d8719efed1db`; reviewed at local HEAD `090303945508076bd2f4193d01305135e2e4dd6a`, a documentation-only descendant with no `internal/` diff.
**Review type:** static design/source rereview. No product files changed and no tests run.

## Verdict

**Reject frozen handoff pending P1-1.** The revision resolves the prior lock-order, fence-callback lease, fail-closed trust/publication, stdio ownership, schema-presence, selector, reservation/cache, and startup all-or-nothing findings. One read-side seam still prevents the stated complete-generation visibility guarantee.

## Blocking finding

### P1 — `DeferredToolRegistryAdapter.List` can combine an old registry snapshot with new DTR state

The acceptance contract says replacement exposes a complete generation and “no observer sees a mixed generation” ([design line 115](../mcp-generation-design.md#L115)). The proposed writer serializes under DTR then owner then DispatchFence then `DefaultToolRegistry` ([design lines 56, 100–102](../mcp-generation-design.md#L56)). The existing composed reader takes those state locks in a different order: [`DeferredToolRegistryAdapter.List`](../../../../internal/tools/deferred.go#L349) calls the backing registry's `List` first, then acquires `DTR.mu.RLock` at line 355. The backing [`DefaultToolRegistry.List`](../../../../internal/tools/registry.go#L84) copies its eager definitions under its own registry read lock.

A concrete interleaving is: an old MCP tool has already materialized; `List` copies that old eager definition and releases the registry lock; the publisher acquires DTR, publishes the replacement, clears the old loaded entry, installs the new loader set, and releases DTR; then `List` acquires DTR and merges the new loader set into its old eager copy. Removed old definitions remain in the result, while new names can appear as stubs. For a same-name replacement, the old eager definition masks the new stub. This violates the complete-generation contract even though the write transaction itself updates both stores under one fence.

The minimal compatible read-side rule is to acquire `DTR.mu.RLock` **before** taking the backing `DefaultToolRegistry` snapshot, hold it through merging the deferred names, and release it afterward. That serializes the overlay with the existing `DTR -> owner -> DispatchFence -> DefaultToolRegistry` writer order and does not add work to a fence callback. Assembly currently constructs the adapter over `tools.NewDefaultToolRegistry()` ([`assemble.go:890–892`](../../../../internal/cli/assemble.go#L890)); the generation path should use that concrete, local snapshot operation rather than call an arbitrary registry callback while holding shared locks. If generic backing registries must remain supported, use a bounded generation-validated retry that releases locks before calling their `List` method.

Add a deterministic barrier to `TestMCPConfigReloadPublishesCompleteGeneration`: pause a reader after it has copied the old backing list, publish a different generation, then resume it. The result must be a complete old snapshot or a complete new snapshot, never the combination above.

## Related seam checks

The remaining read and write paths are compatible with the revised transaction once the list overlay is fixed:

- `DefaultToolRegistry.Get` reads one definition under `r.mu.RLock` ([`registry.go:71–80`](../../../../internal/tools/registry.go#L71)). `DeferredToolRegistryAdapter.Get` either returns that single-key snapshot or enters `Load` on a miss ([`deferred.go:325–343`](../../../../internal/tools/deferred.go#L325)). A source-owned `Load` holds DTR's write lock across loader execution and backing registration ([`deferred.go:90–151`](../../../../internal/tools/deferred.go#L90)); replacement uses that same lock, so it cannot be followed by an old loader registration. The prepared metadata lookup also takes the shared fence before the registry read ([`trusted_registry.go:495–529`](../../../../internal/tools/trusted_registry.go#L495)), and final dispatch compares the full captured snapshot.
- The existing ordinary deferred registration replaces a loader under DTR's write lock ([`deferred.go:69–81`](../../../../internal/tools/deferred.go#L69)); ordinary eager registration is last-registration-wins ([`registry.go:53–57`](../../../../internal/tools/registry.go#L53)). The revision correctly requires reservations in both stores and checks on adapter eager/deferred registration and direct backing-registry eager registration. The reservation guard must live on the actual promoted `DefaultDeferredToolRegistry.RegisterDeferred` path as well, so the adapter's current embedded method cannot bypass it.
- The revision correctly moves `loaded` publication until after owner-aware backing registration succeeds. Today `Load` writes `loaded[name]` before calling `underlying.Register`, leaving the failed definition cached ([`deferred.go:141–146`](../../../../internal/tools/deferred.go#L141)); the specified retryable rollback closes that gap.

The source-owned candidate and retirement contract remains design only. Stdio must stay gated until its separate child-process and silent-request join prerequisite is accepted; this rereview does not accept stdio support or full R6 completion.

## Final rereview addendum

**Rereviewed design SHA-256:** `37fa91cb2f71b0e0280039d765e3e14080f5655decaab0cbd7daddf850da5015`.
**Reviewed checkout:** `25b88ba890f59be488e2ea5defbf09186baf873d`; the MCP, DTR, backing registry, and prepared-dispatch source seams reviewed above are unchanged from the earlier source snapshot.

The amended design closes P1-1 for the Assembly generation path. It now requires `DeferredToolRegistryAdapter.List` to acquire and hold the DTR read lock before the concrete backing `DefaultToolRegistry.List` snapshot and through the deferred-placeholder merge. This matches the generation writer's DTR-before-registry order, serializes the composite reader with publication, and keeps owner/fence callbacks out of the read path. Assembly uses the concrete `DefaultToolRegistry` backing, whose `List` takes only its local registry read lock and copies the registered definitions. If a future caller exposes source-generation updates over a generic backing registry, it must retain the bounded generation-validated retry noted above. The added barrier contract reproduces the old-snapshot/new-DTR interleaving on the baseline and requires a complete old or new result after the fix.

**Final design verdict for this SHA: Frozen ACCEPT.** The initial rejection above applies to SHA `1123b3285e9fa9f5df4eb083875a676e651f7e0b1fbe708178bdd804ebd4c1fc` and is superseded by this addendum only for the amended design hash. This accepts the HTTP-supported design handoff, not implementation, tests, release, stdio support, or full R6 completion; the design continues to gate stdio until its separate process/request join prerequisite is accepted.
