package tools

import (
	"context"
	"crypto/sha256"
	"errors"
	"path/filepath"
	"strings"
)

type preparedReadInvocationKey struct{}
type preparedReadExecutingKey struct{}

// PreparedReadEvidence is an opaque proof that the captured stock Read tool
// returned the exact source bytes for one prepared invocation.
type PreparedReadEvidence struct {
	invocation     *preparedInvocation
	callID         string
	argumentDigest [32]byte
	canonicalRoot  string
	canonicalPath  string
	sourceDigest   [32]byte
}

// CanonicalPath returns the canonical workspace file path covered by the
// successful read.
func (e *PreparedReadEvidence) CanonicalPath() string {
	if e == nil {
		return ""
	}
	return e.canonicalPath
}

// SourceDigest returns the digest of the complete source bytes read before
// any display window or output projection was applied.
func (e *PreparedReadEvidence) SourceDigest() [32]byte {
	if e == nil {
		return [32]byte{}
	}
	return e.sourceDigest
}

// PreparedReadEvidenceFor returns evidence only when result came from the
// exact successful, completed stock Read invocation.
func PreparedReadEvidenceFor(invocation PreparedInvocation, result *ToolResult) (*PreparedReadEvidence, bool) {
	p := stockPreparedRead(invocation)
	if p == nil || result == nil || result.IsError || !p.DispatchCommitted() ||
		p.CompletionSignal().Proof() != CompletionInlinePipelineJoined {
		return nil, false
	}
	evidence := result.preparedReadEvidence
	if evidence == nil || evidence.invocation != p || evidence.callID != p.call.id ||
		evidence.argumentDigest != p.call.Digest() || evidence.canonicalRoot != p.canonicalWorkspaceRoot ||
		evidence.canonicalPath == "" || evidence.sourceDigest == ([32]byte{}) {
		return nil, false
	}
	return evidence, true
}

func stockPreparedRead(invocation PreparedInvocation) *preparedInvocation {
	p, ok := invocation.(*preparedInvocation)
	if !ok || p == nil || p.identity.RegistrationSource() != RegistrationSourceBuiltin ||
		p.identity.ToolName() != "read" || p.completionProof != CompletionInlinePipelineJoined ||
		p.canonicalWorkspaceRoot == "" {
		return nil
	}
	if _, ok := p.definition.(*ReadTool); !ok {
		return nil
	}
	effects := p.effects.Values()
	if len(effects) != 1 || effects[0] != EffectWorkspaceRead {
		return nil
	}
	return p
}

func withPreparedReadInvocation(ctx context.Context, invocation PreparedInvocation) context.Context {
	if p := stockPreparedRead(invocation); p != nil && p.DispatchCommitted() {
		return context.WithValue(ctx, preparedReadInvocationKey{}, p)
	}
	return ctx
}

func withPreparedReadExecution(ctx context.Context, definition ToolDefinition) context.Context {
	if _, ok := definition.(*ReadTool); !ok {
		return ctx
	}
	if p, ok := ctx.Value(preparedReadInvocationKey{}).(*preparedInvocation); ok &&
		stockPreparedRead(p) == p && p.DispatchCommitted() {
		return context.WithValue(ctx, preparedReadExecutingKey{}, p)
	}
	return ctx
}

func bindPreparedReadEvidence(ctx context.Context, path string, source []byte) *PreparedReadEvidence {
	if ctx == nil {
		return nil
	}
	p, ok := ctx.Value(preparedReadExecutingKey{}).(*preparedInvocation)
	if !ok || stockPreparedRead(p) != p || !p.DispatchCommitted() {
		return nil
	}
	tool, ok := p.definition.(*ReadTool)
	if !ok {
		return nil
	}
	rawPath, ok := p.call.args["path"].(string)
	if !ok {
		return nil
	}
	expectedPath, err := tool.resolvePath(rawPath)
	if err != nil {
		return nil
	}
	root := p.canonicalWorkspaceRoot
	canonicalPath, ok := canonicalPreparedWorkspacePath(root, path)
	if !ok {
		return nil
	}
	expectedCanonicalPath, ok := canonicalPreparedWorkspacePath(root, expectedPath)
	if !ok || canonicalPath != expectedCanonicalPath {
		return nil
	}
	return &PreparedReadEvidence{
		invocation: p, callID: p.call.id, argumentDigest: p.call.Digest(),
		canonicalRoot: root, canonicalPath: canonicalPath, sourceDigest: sha256.Sum256(source),
	}
}

// PreparedEditMismatchEvidence is an opaque proof that one exact prepared
// stock Edit invocation failed to match its captured source snapshot.
type PreparedEditMismatchEvidence struct {
	invocation      *preparedInvocation
	callID          string
	argumentDigest  [32]byte
	canonicalRoot   string
	canonicalPath   string
	sourceDigest    [32]byte
	oldStringDigest [32]byte
}

// CanonicalPath returns the canonical workspace path whose edit match failed.
func (e *PreparedEditMismatchEvidence) CanonicalPath() string {
	if e == nil {
		return ""
	}
	return e.canonicalPath
}

// SourceDigest returns the digest of the source snapshot used by the failed
// edit match.
func (e *PreparedEditMismatchEvidence) SourceDigest() [32]byte {
	if e == nil {
		return [32]byte{}
	}
	return e.sourceDigest
}

// PreparedEditMismatchEvidenceFor returns evidence only for the exact
// completed stock Edit invocation that produced the typed match failure.
func PreparedEditMismatchEvidenceFor(invocation PreparedInvocation, err error) (*PreparedEditMismatchEvidence, bool) {
	p := stockPreparedEdit(invocation)
	if p == nil || !PreparedEditMatchKnown(invocation, err) {
		return nil, false
	}
	var failure *preparedEditMatchError
	for current := err; current != nil; current = errors.Unwrap(current) {
		if _, joined := current.(interface{ Unwrap() []error }); joined {
			return nil, false
		}
		if unknown, ok := current.(interface{ OutcomeUnknown() bool }); ok && unknown.OutcomeUnknown() {
			return nil, false
		}
		if candidate, ok := current.(*preparedEditMatchError); ok {
			failure = candidate
			break
		}
	}
	if failure == nil || failure.invocation != p || failure.cause == nil || failure.cause.sourceDigest == ([32]byte{}) {
		return nil, false
	}
	edit, ok := p.definition.(*EditFileTool)
	if !ok {
		return nil, false
	}
	root, ok := canonicalEditWorkspaceRoot(edit, failure.cause.path)
	if !ok {
		return nil, false
	}
	canonicalPath, ok := canonicalPreparedWorkspacePath(root, failure.cause.path)
	if !ok {
		return nil, false
	}
	call := p.call.ToolCall()
	oldString, ok := call.Args["old_string"].(string)
	if !ok {
		return nil, false
	}
	return &PreparedEditMismatchEvidence{
		invocation: p, callID: p.call.id, argumentDigest: p.call.Digest(),
		canonicalRoot: root, canonicalPath: canonicalPath, sourceDigest: failure.cause.sourceDigest,
		oldStringDigest: sha256.Sum256([]byte(oldString)),
	}, true
}

// BlocksRetryFor reports whether a retry is the same path and old_string as
// the failed edit and a successful same-path read still observes unchanged
// source bytes. The replacement text and model-generated call ID do not count
// as progress.
func (e *PreparedEditMismatchEvidence) BlocksRetryFor(read *PreparedReadEvidence, invocation PreparedInvocation) bool {
	p := stockPreparedEdit(invocation)
	if e == nil || p == nil || !validPreparedReadEvidence(read) ||
		read.canonicalRoot != e.canonicalRoot ||
		read.canonicalPath != e.canonicalPath || read.sourceDigest != e.sourceDigest {
		return false
	}
	edit, ok := p.definition.(*EditFileTool)
	if !ok {
		return false
	}
	call := p.call.ToolCall()
	path, ok := call.Args["file_path"].(string)
	if !ok {
		return false
	}
	abspath, err := resolveAndValidatePath("edit", edit.Workdir, path, edit.whitelist)
	if err != nil {
		return false
	}
	root, ok := canonicalEditWorkspaceRoot(edit, abspath)
	if !ok || root != e.canonicalRoot {
		return false
	}
	canonicalPath, ok := canonicalPreparedWorkspacePath(root, abspath)
	if !ok || canonicalPath != e.canonicalPath {
		return false
	}
	oldString, ok := call.Args["old_string"].(string)
	return ok && sha256.Sum256([]byte(oldString)) == e.oldStringDigest
}

func canonicalEditWorkspaceRoot(edit *EditFileTool, path string) (string, bool) {
	if edit == nil || path == "" || edit.whitelist.invalid || edit.whitelist.unrestricted() {
		return "", false
	}
	canonicalPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", false
	}
	var best string
	for _, candidate := range edit.whitelist.paths {
		root, err := filepath.EvalSymlinks(candidate)
		if err != nil || filepath.Clean(root) != candidate {
			continue
		}
		rel, err := filepath.Rel(root, canonicalPath)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		if len(root) > len(best) {
			best = root
		}
	}
	return best, best != ""
}

func validPreparedReadEvidence(e *PreparedReadEvidence) bool {
	if e == nil || e.invocation == nil || e.callID == "" || e.argumentDigest == ([32]byte{}) ||
		e.canonicalRoot == "" || e.canonicalPath == "" || e.sourceDigest == ([32]byte{}) {
		return false
	}
	p := stockPreparedRead(e.invocation)
	return p == e.invocation && p.DispatchCommitted() && p.call.id == e.callID &&
		p.call.Digest() == e.argumentDigest && p.canonicalWorkspaceRoot == e.canonicalRoot &&
		p.CompletionSignal().Proof() == CompletionInlinePipelineJoined
}

func canonicalPreparedWorkspacePath(root, path string) (string, bool) {
	if root == "" || path == "" {
		return "", false
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil || filepath.Clean(canonicalRoot) != root {
		return "", false
	}
	canonicalPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", false
	}
	canonicalPath = filepath.Clean(canonicalPath)
	rel, err := filepath.Rel(canonicalRoot, canonicalPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return canonicalPath, true
}

var ErrPreparedEditRetryBlocked = errors.New("tools: edit retry blocked until the observed source changes")

type preparedEditRetryBlockedError struct{ invocation *preparedInvocation }

func (e *preparedEditRetryBlockedError) Error() string { return ErrPreparedEditRetryBlocked.Error() }
func (e *preparedEditRetryBlockedError) Unwrap() error { return ErrPreparedEditRetryBlocked }

type preparedEditRetryGate struct {
	mismatch *PreparedEditMismatchEvidence
	read     *PreparedReadEvidence
}

type preparedEditRetryGateKey struct{}
type preparedEditRetryExecutingKey struct{}

// WithPreparedEditRecoveryGate installs a run-local stale-match gate. Its
// only effect is to reject an identical path/old_string retry while the
// successful read still observes the original mismatch snapshot.
func WithPreparedEditRecoveryGate(ctx context.Context, mismatch *PreparedEditMismatchEvidence, read *PreparedReadEvidence) context.Context {
	if ctx == nil || mismatch == nil || !validPreparedReadEvidence(read) ||
		read.canonicalRoot != mismatch.canonicalRoot || read.canonicalPath != mismatch.canonicalPath {
		return ctx
	}
	return context.WithValue(ctx, preparedEditRetryGateKey{}, preparedEditRetryGate{mismatch: mismatch, read: read})
}

// PreparedEditRetryBlockedBeforeDispatch reports whether a captured stock edit
// matches the unchanged-source recovery gate before its durable action is
// reserved or its executor is entered.
func PreparedEditRetryBlockedBeforeDispatch(ctx context.Context, invocation PreparedInvocation) bool {
	if ctx == nil {
		return false
	}
	p := stockPreparedEdit(invocation)
	if p == nil {
		return false
	}
	gate, ok := ctx.Value(preparedEditRetryGateKey{}).(preparedEditRetryGate)
	return ok && gate.mismatch != nil && gate.mismatch.BlocksRetryFor(gate.read, p)
}

func withPreparedEditRetryExecution(ctx context.Context, definition ToolDefinition) context.Context {
	if _, ok := definition.(*EditFileTool); !ok {
		return ctx
	}
	if p, ok := ctx.Value(preparedEditExecutingKey{}).(*preparedInvocation); ok && stockPreparedEdit(p) == p && p.DispatchCommitted() {
		return context.WithValue(ctx, preparedEditRetryExecutingKey{}, p)
	}
	return ctx
}

func rejectUnchangedPreparedEditRetry(ctx context.Context, path, oldString string) error {
	if ctx == nil {
		return nil
	}
	p, ok := ctx.Value(preparedEditRetryExecutingKey{}).(*preparedInvocation)
	if !ok || stockPreparedEdit(p) != p || !p.DispatchCommitted() {
		return nil
	}
	gate, ok := ctx.Value(preparedEditRetryGateKey{}).(preparedEditRetryGate)
	if !ok || gate.mismatch == nil || !validPreparedReadEvidence(gate.read) {
		return nil
	}
	call := p.call.ToolCall()
	callOldString, ok := call.Args["old_string"].(string)
	if !ok || callOldString != oldString {
		return nil
	}
	edit, ok := p.definition.(*EditFileTool)
	if !ok {
		return nil
	}
	abspath, err := resolveAndValidatePath("edit", edit.Workdir, path, edit.whitelist)
	if err != nil {
		return nil
	}
	canonicalPath, ok := canonicalPreparedWorkspacePath(gate.mismatch.canonicalRoot, abspath)
	if !ok || canonicalPath != gate.mismatch.canonicalPath || gate.read.canonicalPath != canonicalPath ||
		gate.read.sourceDigest != gate.mismatch.sourceDigest ||
		sha256.Sum256([]byte(oldString)) != gate.mismatch.oldStringDigest {
		return nil
	}
	return &preparedEditRetryBlockedError{invocation: p}
}

// PreparedEditRetryBlockedKnown reports whether err came from the stale-edit
// guard for this exact completed stock Edit invocation.
func PreparedEditRetryBlockedKnown(invocation PreparedInvocation, err error) bool {
	p := stockPreparedEdit(invocation)
	if p == nil || !p.DispatchCommitted() || p.CompletionSignal().Proof() != CompletionQueuedJobJoined &&
		p.CompletionSignal().Proof() != CompletionInlinePipelineJoined {
		return false
	}
	for current := err; current != nil; current = errors.Unwrap(current) {
		if _, joined := current.(interface{ Unwrap() []error }); joined {
			return false
		}
		if blocked, ok := current.(*preparedEditRetryBlockedError); ok {
			return blocked != nil && blocked.invocation == p
		}
	}
	return false
}
