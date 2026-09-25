# R3 configured model catalog handoff

Status: approved bounded implementation slice. This handoff records the catalog and selector contract only; it does not close R3 or authorize native protocol support.

## Membership and identity

The shared resolver snapshot is the only model-list authority. Catalog membership is the union of static `Models()` entries from registered, enabled OpenAI-compatible providers and the final effective primary and configured small-model requests. Primary model selection keeps the existing precedence (`Provider.Model`, then `Model.Name`, then the effective CLI/default model). A small model without a provider uses the primary provider identity. `models.dev` may enrich metadata for an already-listed `(ProviderID, ModelID)` only; it cannot add entries or enable a provider, and listing must not fetch models.

Use the existing `ModelSpec{ProviderID, ModelID, Info, Known}` as the typed catalog and switch identity. Deduplicate by canonical provider ID plus exact model ID, preserve equal model IDs from different providers, and sort by provider ID then model ID. The current marker also compares the full pair.

## Selector and request behavior

Add scoped selector list/build/switch methods while keeping existing model-name methods compatible. A scoped switch constructs a new request for the requested provider/model through the assembly-owned resolver; it never carries a current credential reference or secret to another identity. Bare `/model <id>` remains scoped to the current provider. Provider changes use an explicit `--provider <id> -- <model-id>` form, so model IDs containing `/` stay intact and the parser never guesses by splitting a model name.

Interactive switches within the configured primary provider use primary model/provider sampling. A catalog entry also configured as the small model does not acquire small-model sampling merely because it is in the catalog. A switch to another provider uses only sampling explicitly associated with that target identity; absent target-specific settings, it uses the generic default. Credential resolution remains bound to canonical provider, protocol, and normalized origin.

## Verification

Add catalog tests for precedence, primary/small membership, static/configured union, same-provider deduplication, cross-provider duplicate preservation, deterministic sorting, and exact current selection. Add selector tests for legacy bare switching, scoped switching, and failed-switch state retention. Add local `httptest` assembly coverage with two providers using the same model ID and distinct canary credentials/origins; verify endpoint, model, sampling, and absence of cross-scope credential leakage. Assert model listing performs no network discovery, disabled native identities remain excluded, and neither list output nor errors contain credential values. Do not use real providers or keys.
