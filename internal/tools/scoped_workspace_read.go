package tools

import (
	"path/filepath"
	"strings"
	"sync/atomic"
)

// ScopedWorkspaceReadProof is an opaque, single-use admission capability for
// one prepared built-in workspace reader invocation.
type ScopedWorkspaceReadProof struct {
	identity               TrustedToolIdentity
	effects                TrustedEffectSet
	schemaDigest           [32]byte
	argumentDigest         [32]byte
	callID                 string
	generations            DispatchFenceSnapshot
	canonicalWorkspaceRoot string
	whitelistRoots         []string
	scopeFingerprint       [32]byte
	fence                  *DispatchFence
	state                  *scopedWorkspaceReadProofState
}

type scopedWorkspaceReadProofState struct {
	claimed atomic.Bool
}

// ScopedWorkspaceReadProofFor issues a proof only for an actual private
// prepared invocation created from an attested built-in ReadTool or LSTool.
func ScopedWorkspaceReadProofFor(invocation PreparedInvocation, canonicalRoot string) (*ScopedWorkspaceReadProof, bool) {
	prepared, ok := invocation.(*preparedInvocation)
	if !ok || prepared == nil || !prepared.scopedReadProofEligible || prepared.fence == nil || prepared.schedule != nil || prepared.execute == nil {
		return nil, false
	}
	if canonicalRoot == "" || canonicalRoot != prepared.canonicalWorkspaceRoot || !isCanonicalWorkspaceRootPath(canonicalRoot) {
		return nil, false
	}
	if prepared.completionProof != CompletionInlinePipelineJoined || prepared.identity.RegistrationSource() != RegistrationSourceBuiltin {
		return nil, false
	}
	if prepared.identity.ToolName() != "read" && prepared.identity.ToolName() != "ls" {
		return nil, false
	}
	effects := prepared.effects.Values()
	if len(effects) != 1 || effects[0] != EffectWorkspaceRead {
		return nil, false
	}
	if len(prepared.workspaceReadWhitelist) == 0 || !canonicalWorkspaceReadRoots(canonicalRoot, prepared.workspaceReadWhitelist) {
		return nil, false
	}
	scopeFingerprint := trustedWorkspaceReadScope(prepared.workspaceReadWhitelist).Fingerprint()
	if scopeFingerprint == ([32]byte{}) || scopeFingerprint != prepared.identity.Scope().Fingerprint() {
		return nil, false
	}
	callID := prepared.call.ToolCall().ID
	if callID == "" || prepared.fence.Snapshot() != prepared.generations {
		return nil, false
	}
	if !prepared.scopedReadProofIssued.CompareAndSwap(false, true) {
		return nil, false
	}

	return &ScopedWorkspaceReadProof{
		identity:               prepared.identity,
		effects:                newTrustedEffectSet(effects...),
		schemaDigest:           prepared.schema.Digest(),
		argumentDigest:         prepared.call.Digest(),
		callID:                 callID,
		generations:            prepared.generations,
		canonicalWorkspaceRoot: canonicalRoot,
		whitelistRoots:         append([]string(nil), prepared.workspaceReadWhitelist...),
		scopeFingerprint:       scopeFingerprint,
		fence:                  prepared.fence,
		state:                  &scopedWorkspaceReadProofState{},
	}, true
}

// MatchesAction reports whether this proof still matches the exact captured
// runtime identity, effect set, digests, call ID, workspace root, and fence.
func (proof *ScopedWorkspaceReadProof) MatchesAction(
	identity TrustedToolIdentity,
	effects TrustedEffectSet,
	schemaDigest [32]byte,
	argumentDigest [32]byte,
	callID string,
	canonicalRoot string,
) bool {
	if proof == nil || proof.state == nil || proof.state.claimed.Load() {
		return false
	}
	return proof.matchesCapturedFacts(identity, effects.Values(), schemaDigest, argumentDigest, callID, canonicalRoot)
}

// MatchesWorkspaceAction rechecks the identity, effect facts, call binding,
// root, and current fence at the quarantine index boundary. The session sink
// first matches the exact schema and argument digests from its prepared intent.
func (proof *ScopedWorkspaceReadProof) MatchesWorkspaceAction(
	identity TrustedToolIdentitySnapshot,
	effects []TrustedToolEffect,
	callID string,
	canonicalRoot string,
) bool {
	if proof == nil || proof.state == nil || proof.state.claimed.Load() {
		return false
	}
	if identity != proof.identity.Snapshot() || !equalWorkspaceReadEffects(effects, proof.effects.Values()) {
		return false
	}
	return proof.matchesCapturedFacts(proof.identity, proof.effects.Values(), proof.schemaDigest, proof.argumentDigest, callID, canonicalRoot)
}

// ClaimWorkspaceAdmission atomically consumes a proof after the quarantine
// index has confirmed the matching action facts and workspace root.
func (proof *ScopedWorkspaceReadProof) ClaimWorkspaceAdmission(
	identity TrustedToolIdentitySnapshot,
	effects []TrustedToolEffect,
	callID string,
	canonicalRoot string,
) bool {
	if !proof.MatchesWorkspaceAction(identity, effects, callID, canonicalRoot) {
		return false
	}
	return proof.state.claimed.CompareAndSwap(false, true)
}

func (proof *ScopedWorkspaceReadProof) matchesCapturedFacts(
	identity TrustedToolIdentity,
	effects []TrustedToolEffect,
	schemaDigest [32]byte,
	argumentDigest [32]byte,
	callID string,
	canonicalRoot string,
) bool {
	if proof.identity != identity || !equalWorkspaceReadEffects(effects, proof.effects.Values()) ||
		proof.schemaDigest != schemaDigest || proof.argumentDigest != argumentDigest ||
		proof.callID != callID || proof.canonicalWorkspaceRoot != canonicalRoot ||
		canonicalRoot == "" || proof.scopeFingerprint != proof.identity.Scope().Fingerprint() ||
		!isCanonicalWorkspaceRootPath(canonicalRoot) ||
		!canonicalWorkspaceReadRoots(canonicalRoot, proof.whitelistRoots) {
		return false
	}
	return proof.fence != nil && proof.fence.Snapshot() == proof.generations
}

func equalWorkspaceReadEffects(left, right []TrustedToolEffect) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return len(left) == 1 && left[0] == EffectWorkspaceRead
}

func isCanonicalWorkspaceRootPath(root string) bool {
	return root != "" && filepath.IsAbs(root) && filepath.Clean(root) == root && resolveSymlinks(root) == root
}

func canonicalWorkspaceReadRoots(workspaceRoot string, roots []string) bool {
	if !isCanonicalWorkspaceRootPath(workspaceRoot) || len(roots) == 0 {
		return false
	}
	for _, root := range roots {
		if !isCanonicalWorkspaceRootPath(root) || !pathIsEqualOrNested(workspaceRoot, root) {
			return false
		}
	}
	return true
}

func pathIsEqualOrNested(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}
