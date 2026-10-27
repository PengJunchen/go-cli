package tools

import "sync/atomic"

// ownedSubagentTool is created only through the host lifecycle constructor.
// Merely implementing an interface or using its tool name grants no proof.
type ownedSubagentTool struct{ *SubagentTool }

// NewOwnedSubagentTool declares the host's reviewed sub-agent lifecycle: each
// child owns an independent durable session and all of its actual effects, and
// Dispatch joins those resources. The CLI calls this only after validating its
// private managed factory. Ordinary SubagentTool registrations have no such
// contract. Registration must still explicitly attest joined completion.
func NewOwnedSubagentTool(dispatcher SubagentDispatcher) ToolDefinition {
	return &ownedSubagentTool{NewSubagentTool(dispatcher)}
}

// ManagedDelegationProof binds one pure orchestration callback to its exact
// sealed invocation. Its children carry their own workspace dispatch markers;
// this proof never exempts a child effect or grants permission to execute it.
type ManagedDelegationProof struct {
	identity    TrustedToolIdentity
	schema      [32]byte
	arguments   [32]byte
	callID      string
	root        string
	fence       *DispatchFence
	generations DispatchFenceSnapshot
	claimed     atomic.Bool
}

// ManagedDelegationProofFor issues a single-use capability only for the private
// owned-subagent definition captured by the sealed prepared registry.
func ManagedDelegationProofFor(invocation PreparedInvocation, canonicalRoot string) (*ManagedDelegationProof, bool) {
	prepared, ok := invocation.(*preparedInvocation)
	if !ok || prepared == nil || !prepared.managedDelegation || prepared.consumed.Load() || prepared.fence == nil || prepared.execute == nil || prepared.schedule != nil || prepared.completionProof != CompletionInlinePipelineJoined || !isCanonicalWorkspaceRootPath(canonicalRoot) || prepared.fence.Snapshot() != prepared.generations || !prepared.delegationProofIssued.CompareAndSwap(false, true) {
		return nil, false
	}
	return &ManagedDelegationProof{identity: prepared.identity, schema: prepared.schema.Digest(), arguments: prepared.call.Digest(), callID: prepared.call.ToolCall().ID, root: canonicalRoot, fence: prepared.fence, generations: prepared.generations}, true
}

// MatchesAction validates exact identity, schema, arguments, call and workspace.
func (p *ManagedDelegationProof) MatchesAction(identity TrustedToolIdentity, effects TrustedEffectSet, schema, arguments [32]byte, callID, root string) bool {
	return p != nil && !p.claimed.Load() && p.identity == identity && p.schema == schema && p.arguments == arguments && p.callID == callID && p.root == root && isCanonicalWorkspaceRootPath(root) && onlyControlEffect(effects.Values()) && p.fence != nil && p.fence.Snapshot() == p.generations
}

// MatchesWorkspaceAction rechecks the immutable facts after the session sink
// matched schema and argument digests against the same invocation.
func (p *ManagedDelegationProof) MatchesWorkspaceAction(identity TrustedToolIdentitySnapshot, effects []TrustedToolEffect, callID, root string) bool {
	return p != nil && p.identity.Snapshot() == identity && onlyControlEffect(effects) && p.MatchesAction(p.identity, newTrustedEffectSet(effects...), p.schema, p.arguments, callID, root)
}

// ClaimWorkspaceAdmission consumes the exact proof under the workspace lock.
func (p *ManagedDelegationProof) ClaimWorkspaceAdmission(identity TrustedToolIdentitySnapshot, effects []TrustedToolEffect, callID, root string) bool {
	return p.MatchesWorkspaceAction(identity, effects, callID, root) && p.claimed.CompareAndSwap(false, true)
}

func onlyControlEffect(effects []TrustedToolEffect) bool {
	return len(effects) == 1 && effects[0] == EffectControl
}
