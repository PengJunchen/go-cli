package session

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/tools"
)

var (
	// ErrWorkspaceQuarantined means an unresolved marker blocks this action.
	ErrWorkspaceQuarantined = errors.New("session: workspace is quarantined by unresolved actions")
	// ErrWorkspaceIndexUnavailable means the global authority cannot be read or written safely.
	ErrWorkspaceIndexUnavailable = errors.New("session: workspace index is unavailable")
	// ErrWorkspaceIndexCorrupt means the global authority contains invalid durable data.
	ErrWorkspaceIndexCorrupt = errors.New("session: workspace index is corrupt")
)

const maxWorkspaceIndexActions = 256

type workspaceIndexRecordKind string

const (
	workspaceIndexInitialized   workspaceIndexRecordKind = "workspace_initialized"
	workspaceIndexDispatchReady workspaceIndexRecordKind = "dispatch_ready"
	workspaceIndexResolution    workspaceIndexRecordKind = "execution_resolution"
)

// WorkspaceDispatchAction persists action identity and effect facts only. It
// deliberately has no field for tool arguments or credentials.
type WorkspaceDispatchAction struct {
	Action            PendingActionRef                   `json:"action"`
	ToolIdentity      *tools.TrustedToolIdentitySnapshot `json:"tool_identity,omitempty"`
	EffectSet         []tools.TrustedToolEffect          `json:"effect_set"`
	ExecutionIdentity *ExecutionIdentity                 `json:"execution_identity,omitempty"`
}

// WorkspaceDispatchBatch is the complete session reservation batch whose
// global marker must be synced before any contained executor starts.
type WorkspaceDispatchBatch struct {
	WorkspaceID   string                    `json:"workspace_id"`
	SessionID     string                    `json:"session_id"`
	RunID         string                    `json:"run_id"`
	ActionBatchID string                    `json:"action_batch_id"`
	Actions       []WorkspaceDispatchAction `json:"actions"`
}

type WorkspaceDispatchResult uint8

const (
	// WorkspaceDispatchNotCommitted indicates this call did not grant a dispatch permit.
	WorkspaceDispatchNotCommitted WorkspaceDispatchResult = iota
	// WorkspaceDispatchCommittedNow is returned only after this call appended
	// and synced a new marker while holding the workspace lock.
	WorkspaceDispatchCommittedNow
	// WorkspaceDispatchAlreadyExists reports an idempotent marker observation;
	// it never grants permission to replay the action.
	WorkspaceDispatchAlreadyExists
)

type workspaceIndexIO struct {
	write func(*os.File, []byte) (int, error)
	sync  func(*os.File) error
}

// WorkspaceQuarantineIndex stores cross-session dispatch markers and recovery
// resolutions under the user's standard config directory. Its advisory lock
// coordinates cooperating processes for that same config root; it is not a
// distributed lease or OS sandbox. Call EnsureWorkspace before any session may
// persist an intent that could be dispatched.
type WorkspaceQuarantineIndex struct {
	root string
	io   workspaceIndexIO
}

type workspaceIndexRecord struct {
	Version       uint16                    `json:"version"`
	Seq           uint64                    `json:"seq"`
	RecordID      string                    `json:"record_id"`
	Kind          workspaceIndexRecordKind  `json:"kind"`
	WorkspaceID   string                    `json:"workspace_id"`
	SessionID     string                    `json:"session_id,omitempty"`
	RunID         string                    `json:"run_id,omitempty"`
	ActionBatchID string                    `json:"action_batch_id,omitempty"`
	Actions       []WorkspaceDispatchAction `json:"actions,omitempty"`
	Action        *PendingActionRef         `json:"action,omitempty"`
	CheckpointID  string                    `json:"checkpoint_entry_id,omitempty"`
	CheckpointSeq uint64                    `json:"checkpoint_seq,omitempty"`
	Resolution    *core.ExecutionResolution `json:"resolution,omitempty"`
}

type workspaceBatchKey struct {
	sessionID string
	runID     string
	batchID   string
}

type workspaceOccurrenceKey struct {
	sessionID string
	workspace string
	runID     string
	ordinal   uint64
	toolCall  string
}

type workspaceActionKey struct {
	workspaceOccurrenceKey
	batchID  string
	actionID string
}

type workspaceIndexedAction struct {
	ref        PendingActionRef
	action     WorkspaceDispatchAction
	marker     *workspaceIndexRecord
	resolution *workspaceIndexRecord
}

type workspaceIndexState struct {
	records       []workspaceIndexRecord
	batches       map[workspaceBatchKey]*workspaceIndexRecord
	actionsByKey  map[workspaceActionKey]*workspaceIndexedAction
	actionsByCall map[workspaceOccurrenceKey]*workspaceIndexedAction
	actionsByID   map[string]*workspaceIndexedAction
}

type workspaceIndexHandle struct {
	index *WorkspaceQuarantineIndex
	file  *os.File
	state *workspaceIndexState
}

// NewWorkspaceQuarantineIndex fixes the authority root to os.UserConfigDir;
// callers cannot relocate it per session.
func NewWorkspaceQuarantineIndex() (*WorkspaceQuarantineIndex, error) {
	root, err := os.UserConfigDir()
	if err != nil {
		return nil, fmt.Errorf("session: resolve user config directory: %w", err)
	}
	if root == "" {
		return nil, errors.New("session: user config directory is empty")
	}
	return &WorkspaceQuarantineIndex{root: root}, nil
}

func newWorkspaceQuarantineIndex(configDir string, ioOps workspaceIndexIO) *WorkspaceQuarantineIndex {
	return &WorkspaceQuarantineIndex{root: configDir, io: ioOps}
}

// EnsureWorkspace explicitly initializes a fresh index under its sibling lock.
// If a lock exists without its index, the missing authority is treated as
// possible interrupted initialization and fails closed rather than proving
// absence. Existing indexes are only validated; they are never reset here.
func (i *WorkspaceQuarantineIndex) EnsureWorkspace(ctx context.Context, workspaceID string) (retErr error) {
	if err := contextErr(ctx); err != nil {
		return err
	}
	directory, err := i.workspaceDirectory()
	if err != nil {
		return err
	}
	if !validWorkspaceID(workspaceID) {
		return fmt.Errorf("session: invalid workspace ID %q", workspaceID)
	}
	if err := ensureWorkspaceIndexDirectory(i.root); err != nil {
		return fmt.Errorf("session: prepare workspace index directory: %w", err)
	}
	lockExists, err := regularFileExists(i.lockPath(workspaceID))
	if err != nil {
		return workspaceIndexUnavailable("inspect workspace lock", err)
	}
	if !lockExists {
		indexExists, err := regularFileExists(i.indexPath(workspaceID))
		if err != nil {
			return workspaceIndexUnavailable("inspect orphaned workspace index", err)
		}
		if indexExists {
			return workspaceIndexUnavailable("index exists without its prior lock witness", errors.New("orphaned workspace index"))
		}
	}

	lockFile, createdLock, err := i.openWorkspaceLock(ctx, directory, workspaceID, true)
	if err != nil {
		return workspaceIndexUnavailable("open workspace lock", err)
	}
	defer func() { retErr = errors.Join(retErr, closeWorkspaceLock(lockFile)) }()
	if err := contextErr(ctx); err != nil {
		return err
	}

	indexPath := i.indexPath(workspaceID)
	indexExists, err := regularFileExists(indexPath)
	if err != nil {
		return workspaceIndexUnavailable("inspect workspace index", err)
	}
	if !indexExists {
		if !createdLock {
			return workspaceIndexUnavailable("lock exists but authoritative index is missing", os.ErrNotExist)
		}
		if err := i.createInitialWorkspaceIndex(directory, workspaceID); err != nil {
			return workspaceIndexUnavailable("initialize workspace index", err)
		}
	} else if createdLock {
		return workspaceIndexUnavailable("index exists without its prior lock witness", errors.New("orphaned workspace index"))
	}

	file, _, err := i.readLockedIndex(directory, workspaceID)
	if err != nil {
		return err
	}
	return errors.Join(contextErr(ctx), file.Close())
}

// BeginDispatchBatch appends one complete dispatch-ready marker under the
// workspace lock. The caller must already hold the corresponding session lock
// and must start no executor unless the result is CommittedNow. CommittedNow
// is only the durable-index leg: the caller must still pass the prepared
// invocation's single-use final dispatch fence. An identical marker returns
// AlreadyExists, never a replay permit, including after its resolution is
// durable. Any error returns NotCommitted: even if a failed write or Sync may
// have left a visible marker, only a later locked retry can observe it, and
// that retry returns AlreadyExists without authorizing execution.
func (i *WorkspaceQuarantineIndex) BeginDispatchBatch(ctx context.Context, batch WorkspaceDispatchBatch) (WorkspaceDispatchResult, error) {
	if err := contextErr(ctx); err != nil {
		return WorkspaceDispatchNotCommitted, err
	}
	normalized, err := normalizeWorkspaceDispatchBatch(batch)
	if err != nil {
		return WorkspaceDispatchNotCommitted, err
	}
	var result WorkspaceDispatchResult
	err = i.withLockedIndex(ctx, normalized.WorkspaceID, func(handle *workspaceIndexHandle) error {
		// A prior append may have become visible before Sync reported an error.
		// Confirm the current bytes before using any resolution row to release
		// quarantine or using this operation as an admission check.
		if err := handle.index.syncFile(handle.file); err != nil {
			return fmt.Errorf("session: confirm workspace index durability before admission: %w", err)
		}
		if err := contextErr(ctx); err != nil {
			return err
		}
		key := workspaceBatchKey{sessionID: normalized.SessionID, runID: normalized.RunID, batchID: normalized.ActionBatchID}
		if existing := handle.state.batches[key]; existing != nil {
			if existing.WorkspaceID != normalized.WorkspaceID || !reflect.DeepEqual(existing.Actions, normalized.Actions) {
				return errors.New("session: workspace dispatch batch ID was reused with different action references")
			}
			result = WorkspaceDispatchAlreadyExists
			return nil
		}
		for _, action := range normalized.Actions {
			occurrence := workspaceOccurrenceFor(action.Action)
			if existing := handle.state.actionsByCall[occurrence]; existing != nil {
				return errors.New("session: workspace action occurrence already has a different dispatch marker")
			}
			if existing := handle.state.actionsByID[action.Action.ActionID]; existing != nil {
				return errors.New("session: workspace action ID is already assigned to another occurrence")
			}
		}
		if handle.state.hasUnresolvedMarker() {
			// Safe reads need both a trusted effect and an enforced path-scope
			// permit from R4. This storage-only adapter has no such permit, so it
			// conservatively blocks every new action while anything is unresolved.
			return ErrWorkspaceQuarantined
		}
		record := workspaceIndexRecord{
			Version: 1, RecordID: workspaceDispatchRecordID(normalized),
			Kind: workspaceIndexDispatchReady, WorkspaceID: normalized.WorkspaceID,
			SessionID: normalized.SessionID, RunID: normalized.RunID,
			ActionBatchID: normalized.ActionBatchID, Actions: normalized.Actions,
		}
		if err := contextErr(ctx); err != nil {
			return err
		}
		return handle.appendRecords(record)
	})
	if err != nil {
		return WorkspaceDispatchNotCommitted, err
	}
	if result == WorkspaceDispatchAlreadyExists {
		return result, nil
	}
	return WorkspaceDispatchCommittedNow, nil
}

// ApplyDurableResolutions mirrors only checkpoints already validated by the
// session loader. It never opens or calls back into a session file. Replaying
// an identical checkpoint re-syncs the index before acknowledging it.
func (i *WorkspaceQuarantineIndex) ApplyDurableResolutions(ctx context.Context, resolutions []ValidatedExecutionResolution) error {
	if err := contextErr(ctx); err != nil {
		return err
	}
	if len(resolutions) == 0 {
		return nil
	}
	groups, err := groupValidatedResolutions(resolutions)
	if err != nil {
		return err
	}
	for _, workspaceID := range sortedWorkspaceIDs(groups) {
		group := groups[workspaceID]
		if err := i.withLockedIndex(ctx, workspaceID, func(handle *workspaceIndexHandle) error {
			if err := contextErr(ctx); err != nil {
				return err
			}
			return handle.applyResolutions(group)
		}); err != nil {
			return err
		}
	}
	return nil
}

// InspectDispatchMarkers returns one decision per input reference in order.
// A missing, malformed, or unavailable index is an error, never proof of an
// absent marker.
func (i *WorkspaceQuarantineIndex) InspectDispatchMarkers(ctx context.Context, refs []PendingActionRef) ([]PendingActionDecision, error) {
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	decisions := make([]PendingActionDecision, len(refs))
	if len(refs) == 0 {
		return decisions, nil
	}
	groups := make(map[string][]indexedPendingAction)
	seen := make(map[workspaceOccurrenceKey]struct{}, len(refs))
	for position, ref := range refs {
		if err := ref.Validate(); err != nil {
			return nil, fmt.Errorf("session: invalid recovery action at index %d: %w", position, err)
		}
		if !validWorkspaceID(ref.WorkspaceID) {
			return nil, fmt.Errorf("session: invalid workspace ID in recovery action at index %d", position)
		}
		key := workspaceOccurrenceFor(ref)
		if _, exists := seen[key]; exists {
			return nil, fmt.Errorf("session: duplicate recovery action occurrence at index %d", position)
		}
		seen[key] = struct{}{}
		groups[ref.WorkspaceID] = append(groups[ref.WorkspaceID], indexedPendingAction{position: position, ref: ref})
	}
	for _, workspaceID := range sortedPendingWorkspaceIDs(groups) {
		group := groups[workspaceID]
		if err := i.withLockedIndex(ctx, workspaceID, func(handle *workspaceIndexHandle) error {
			// A resolution row whose earlier Sync failed is not accepted as
			// complete until this locked read can sync the index successfully.
			if err := handle.index.syncFile(handle.file); err != nil {
				return fmt.Errorf("session: confirm workspace index durability before inspection: %w", err)
			}
			if err := contextErr(ctx); err != nil {
				return err
			}
			for _, pending := range group {
				decisions[pending.position] = PendingActionDecision{
					Action:      pending.ref,
					MarkerState: handle.state.markerState(pending.ref),
				}
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}
	return decisions, nil
}

type indexedPendingAction struct {
	position int
	ref      PendingActionRef
}

func (i *WorkspaceQuarantineIndex) workspaceDirectory() (string, error) {
	if i == nil || i.root == "" {
		return "", workspaceIndexUnavailable("empty workspace index root", errors.New("missing root"))
	}
	return filepath.Join(i.root, "go-cli", "workspace-quarantine"), nil
}

func (i *WorkspaceQuarantineIndex) indexPath(workspaceID string) string {
	directory, _ := i.workspaceDirectory()
	return filepath.Join(directory, workspaceID+".jsonl")
}

func (i *WorkspaceQuarantineIndex) lockPath(workspaceID string) string {
	directory, _ := i.workspaceDirectory()
	return filepath.Join(directory, workspaceID+".lock")
}

func (i *WorkspaceQuarantineIndex) openWorkspaceLock(ctx context.Context, directory, workspaceID string, create bool) (*os.File, bool, error) {
	name := workspaceID + ".lock"
	var file *os.File
	created := false
	if create {
		opened, err := openSessionFileNoFollow(directory, name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			file = opened
			created = true
			if err := i.syncFile(file); err != nil {
				_ = file.Close()
				return nil, false, err
			}
			if err := syncDirectory(directory); err != nil {
				_ = file.Close()
				return nil, false, err
			}
		} else if !errors.Is(err, os.ErrExist) {
			return nil, false, err
		}
	}
	if file == nil {
		opened, err := openSessionFileNoFollow(directory, name, os.O_RDWR, 0)
		if err != nil {
			return nil, false, err
		}
		file = opened
	}
	if err := validateWorkspaceFileMode(file, 0o600, name); err != nil {
		_ = file.Close()
		return nil, false, err
	}
	if err := flockExclusiveContext(ctx, file); err != nil {
		_ = file.Close()
		return nil, false, err
	}
	return file, created, nil
}

func (i *WorkspaceQuarantineIndex) withLockedIndex(ctx context.Context, workspaceID string, fn func(*workspaceIndexHandle) error) (retErr error) {
	if err := contextErr(ctx); err != nil {
		return err
	}
	if !validWorkspaceID(workspaceID) {
		return fmt.Errorf("session: invalid workspace ID %q", workspaceID)
	}
	directory, err := i.workspaceDirectory()
	if err != nil {
		return err
	}
	if err := validateWorkspaceIndexDirectory(directory); err != nil {
		return workspaceIndexUnavailable("workspace directory is unavailable", err)
	}
	if err := validatePrivateDirectory(filepath.Dir(directory)); err != nil {
		return workspaceIndexUnavailable("workspace index parent is unavailable", err)
	}
	lock, _, err := i.openWorkspaceLock(ctx, directory, workspaceID, false)
	if err != nil {
		return workspaceIndexUnavailable("open workspace lock", err)
	}
	defer func() { retErr = errors.Join(retErr, closeWorkspaceLock(lock)) }()
	if err := contextErr(ctx); err != nil {
		return err
	}

	file, state, err := i.readLockedIndex(directory, workspaceID)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, file.Close()) }()
	if err := contextErr(ctx); err != nil {
		return err
	}
	if _, err := file.Seek(0, io.SeekEnd); err != nil {
		return workspaceIndexUnavailable("seek workspace index", err)
	}
	return fn(&workspaceIndexHandle{index: i, file: file, state: state})
}

func (i *WorkspaceQuarantineIndex) readLockedIndex(directory, workspaceID string) (*os.File, *workspaceIndexState, error) {
	name := workspaceID + ".jsonl"
	file, err := openSessionFileNoFollow(directory, name, os.O_RDWR, 0)
	if err != nil {
		return nil, nil, workspaceIndexUnavailable("open workspace index", err)
	}
	if err := validateWorkspaceFileMode(file, 0o600, name); err != nil {
		_ = file.Close()
		return nil, nil, workspaceIndexUnavailable("validate workspace index permissions", err)
	}
	records, err := readWorkspaceIndex(file, directory, workspaceID)
	if err != nil {
		_ = file.Close()
		return nil, nil, fmt.Errorf("%w: %v", ErrWorkspaceIndexCorrupt, err)
	}
	state, err := validateWorkspaceIndexRecords(records, workspaceID)
	if err != nil {
		_ = file.Close()
		return nil, nil, fmt.Errorf("%w: %v", ErrWorkspaceIndexCorrupt, err)
	}
	return file, state, nil
}

func (i *WorkspaceQuarantineIndex) createInitialWorkspaceIndex(directory, workspaceID string) error {
	indexPath := filepath.Join(directory, workspaceID+".jsonl")
	if exists, err := regularFileExists(indexPath); err != nil {
		return err
	} else if exists {
		return errors.New("workspace index unexpectedly exists during bootstrap")
	}
	genesis := workspaceIndexRecord{
		Version: 1, Seq: 1, RecordID: workspaceGenesisRecordID(workspaceID),
		Kind: workspaceIndexInitialized, WorkspaceID: workspaceID,
	}
	line, err := marshalWorkspaceIndexRecord(genesis)
	if err != nil {
		return err
	}
	for attempt := 0; attempt < 8; attempt++ {
		var random [12]byte
		if _, err := rand.Read(random[:]); err != nil {
			return fmt.Errorf("generate workspace index temp name: %w", err)
		}
		tempName := workspaceID + ".jsonl.init-" + hex.EncodeToString(random[:])
		temp, err := openSessionFileNoFollow(directory, tempName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return err
		}
		writeErr := i.writeFile(temp, line)
		if writeErr == nil {
			writeErr = i.syncFile(temp)
		}
		closeErr := temp.Close()
		if writeErr != nil || closeErr != nil {
			removeErr := removeWorkspaceTemp(directory, tempName)
			return errors.Join(writeErr, closeErr, removeErr)
		}
		if err := os.Rename(filepath.Join(directory, tempName), indexPath); err != nil {
			return errors.Join(err, removeWorkspaceTemp(directory, tempName))
		}
		return syncDirectory(directory)
	}
	return errors.New("unable to allocate workspace index temp file")
}

func removeWorkspaceTemp(directory, filename string) error {
	removeErr := os.Remove(filepath.Join(directory, filename))
	if errors.Is(removeErr, os.ErrNotExist) {
		removeErr = nil
	}
	return errors.Join(removeErr, syncDirectory(directory))
}

func (h *workspaceIndexHandle) appendRecords(records ...workspaceIndexRecord) error {
	if len(records) == 0 {
		return nil
	}
	if uint64(len(h.state.records)) > ^uint64(0)-uint64(len(records)) {
		return errors.New("session: workspace index sequence exhausted")
	}
	candidate := append([]workspaceIndexRecord(nil), h.state.records...)
	lines := make([][]byte, len(records))
	for n := range records {
		record := records[n]
		record.Version = 1
		record.Seq = uint64(len(candidate)) + 1
		if record.RecordID == "" {
			return errors.New("session: workspace index record ID is required")
		}
		line, err := marshalWorkspaceIndexRecord(record)
		if err != nil {
			return err
		}
		lines[n] = line
		candidate = append(candidate, record)
	}
	if _, err := validateWorkspaceIndexRecords(candidate, h.state.records[0].WorkspaceID); err != nil {
		return fmt.Errorf("session: reject invalid workspace index append: %w", err)
	}
	var batch bytes.Buffer
	for _, line := range lines {
		batch.Write(line)
	}
	if err := h.index.writeFile(h.file, batch.Bytes()); err != nil {
		return fmt.Errorf("session: append workspace index: %w", err)
	}
	if err := h.index.syncFile(h.file); err != nil {
		return fmt.Errorf("session: sync workspace index: %w", err)
	}
	return nil
}

func (h *workspaceIndexHandle) applyResolutions(resolutions []ValidatedExecutionResolution) error {
	newRecords := make([]workspaceIndexRecord, 0, len(resolutions))
	needsSync := false
	for _, resolution := range resolutions {
		key := workspaceActionFor(resolution.Action)
		indexed := h.state.actionsByKey[key]
		if indexed == nil {
			if h.state.hasConflictingMarkerReference(resolution.Action) {
				return errors.New("session: execution resolution is covered by a mismatched global marker")
			}
			if resolution.Resolution.Evidence != core.ResolutionNeverDispatched {
				return errors.New("session: execution resolution has no matching global dispatch marker")
			}
			// BLD-01 validated EvidenceRef against the exact durable
			// not_dispatched receipt and final dispatch fence before calling us.
			continue
		}
		if indexed.ref != resolution.Action {
			return errors.New("session: execution resolution action reference differs from global marker")
		}
		candidate := workspaceIndexRecord{
			Version: 1, RecordID: workspaceResolutionRecordID(resolution.Action),
			Kind: workspaceIndexResolution, WorkspaceID: resolution.Action.WorkspaceID,
			Action: refPointer(resolution.Action), CheckpointID: resolution.CheckpointEntryID,
			CheckpointSeq: resolution.CheckpointSeq, Resolution: resolutionPointer(resolution.Resolution),
		}
		if existing := indexed.resolution; existing != nil {
			if !sameWorkspaceResolution(*existing, candidate) {
				return errors.New("session: conflicting global execution resolution for action")
			}
			needsSync = true
			continue
		}
		newRecords = append(newRecords, candidate)
	}
	if len(newRecords) > 0 {
		return h.appendRecords(newRecords...)
	}
	if needsSync {
		if err := h.index.syncFile(h.file); err != nil {
			return fmt.Errorf("session: confirm existing workspace resolution sync: %w", err)
		}
	}
	return nil
}

func (s *workspaceIndexState) markerState(ref PendingActionRef) DispatchMarkerState {
	indexed := s.actionsByCall[workspaceOccurrenceFor(ref)]
	if indexed == nil {
		if s.hasConflictingMarkerReference(ref) {
			return DispatchMarkerUnverifiable
		}
		return DispatchMarkerAbsent
	}
	if indexed.ref != ref {
		return DispatchMarkerUnverifiable
	}
	if indexed.resolution != nil {
		return DispatchMarkerResolved
	}
	return DispatchMarkerPresent
}

func (s *workspaceIndexState) hasConflictingMarkerReference(ref PendingActionRef) bool {
	if indexed := s.actionsByCall[workspaceOccurrenceFor(ref)]; indexed != nil {
		return indexed.ref != ref
	}
	if ref.ActionID != "" && s.actionsByID[ref.ActionID] != nil {
		return true
	}
	if ref.ActionBatchID != "" {
		key := workspaceBatchKey{sessionID: ref.SessionID, runID: ref.RunID, batchID: ref.ActionBatchID}
		if s.batches[key] != nil {
			return true
		}
	}
	return false
}

func (s *workspaceIndexState) hasUnresolvedMarker() bool {
	for _, action := range s.actionsByKey {
		if action.resolution == nil {
			return true
		}
	}
	return false
}

func (i *WorkspaceQuarantineIndex) writeFile(file *os.File, data []byte) error {
	for len(data) > 0 {
		var written int
		var err error
		if i.io.write != nil {
			written, err = i.io.write(file, data)
		} else {
			written, err = file.Write(data)
		}
		if written < 0 || written > len(data) {
			return errors.New("session: workspace index writer returned invalid byte count")
		}
		data = data[written:]
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func (i *WorkspaceQuarantineIndex) syncFile(file *os.File) error {
	if i.io.sync != nil {
		return i.io.sync(file)
	}
	return file.Sync()
}

func readWorkspaceIndexFile(path, sidecarDir, workspaceID string) ([]workspaceIndexRecord, error) {
	directory := filepath.Dir(path)
	name := filepath.Base(path)
	file, err := openSessionFileNoFollow(directory, name, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return readWorkspaceIndex(file, sidecarDir, workspaceID)
}

func readWorkspaceIndex(file *os.File, sidecarDir, workspaceID string) ([]workspaceIndexRecord, error) {
	if file == nil {
		return nil, errors.New("session: cannot read nil workspace index")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seek workspace index: %w", err)
	}
	reader := bufio.NewReaderSize(file, 64*1024)
	records := make([]workspaceIndexRecord, 0)
	var row []byte
	var offset int64
	var completeOffset int64
	for {
		fragment, readErr := reader.ReadSlice('\n')
		if len(row)+len(fragment) > maxDurableJSONLineBytes+1 {
			return nil, fmt.Errorf("workspace JSONL row exceeds %d bytes", maxDurableJSONLineBytes)
		}
		row = append(row, fragment...)
		offset += int64(len(fragment))
		switch readErr {
		case nil:
			line := row[:len(row)-1]
			if len(line) > maxDurableJSONLineBytes {
				return nil, fmt.Errorf("workspace JSONL row exceeds %d bytes", maxDurableJSONLineBytes)
			}
			record, err := decodeWorkspaceIndexRecord(line)
			if err != nil {
				return nil, fmt.Errorf("decode workspace JSONL row ending at byte %d: %w", offset, err)
			}
			records = append(records, record)
			completeOffset = offset
			row = row[:0]
		case bufio.ErrBufferFull:
			if len(row) > maxDurableJSONLineBytes {
				return nil, fmt.Errorf("workspace JSONL row exceeds %d bytes", maxDurableJSONLineBytes)
			}
		case io.EOF:
			if len(row) == 0 {
				return records, nil
			}
			if len(row) > maxDurableJSONLineBytes {
				return nil, fmt.Errorf("workspace JSONL row exceeds %d bytes", maxDurableJSONLineBytes)
			}
			record, err := decodeWorkspaceIndexRecord(row)
			if err == nil {
				if _, seekErr := file.Seek(0, io.SeekEnd); seekErr != nil {
					return nil, fmt.Errorf("seek unterminated workspace row: %w", seekErr)
				}
				if err := writeFileBytes(file, []byte{'\n'}); err != nil {
					return nil, fmt.Errorf("delimit valid unterminated workspace row: %w", err)
				}
				if err := file.Sync(); err != nil {
					return nil, fmt.Errorf("sync workspace row delimiter: %w", err)
				}
				records = append(records, record)
				return records, nil
			}
			if !isTruncatedJSON(err) {
				return nil, fmt.Errorf("malformed final workspace JSONL row at byte %d: %w", offset, err)
			}
			if sidecarDir == "" {
				sidecarDir = filepath.Dir(file.Name())
			}
			if err := quarantineTornSuffix(file.Name(), sidecarDir, row); err != nil {
				return nil, fmt.Errorf("quarantine truncated workspace index suffix: %w", err)
			}
			if err := file.Truncate(completeOffset); err != nil {
				return nil, fmt.Errorf("truncate workspace index suffix: %w", err)
			}
			if err := file.Sync(); err != nil {
				return nil, fmt.Errorf("sync repaired workspace index: %w", err)
			}
			return records, nil
		default:
			return nil, fmt.Errorf("read workspace JSONL: %w", readErr)
		}
	}
}

func decodeWorkspaceIndexRecord(line []byte) (workspaceIndexRecord, error) {
	var record workspaceIndexRecord
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return record, errors.New("workspace JSONL row must be an object")
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return record, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return record, errors.New("workspace JSONL row contains multiple JSON values")
		}
		return record, err
	}
	return record, nil
}

func validateWorkspaceIndexRecords(records []workspaceIndexRecord, workspaceID string) (*workspaceIndexState, error) {
	if len(records) == 0 {
		return nil, errors.New("workspace index has no initialization witness")
	}
	state := &workspaceIndexState{
		records:       append([]workspaceIndexRecord(nil), records...),
		batches:       make(map[workspaceBatchKey]*workspaceIndexRecord),
		actionsByKey:  make(map[workspaceActionKey]*workspaceIndexedAction),
		actionsByCall: make(map[workspaceOccurrenceKey]*workspaceIndexedAction),
		actionsByID:   make(map[string]*workspaceIndexedAction),
	}
	seenRecordIDs := make(map[string]struct{}, len(records))
	for position := range state.records {
		record := &state.records[position]
		if record.Version != 1 || record.WorkspaceID != workspaceID || record.Seq != uint64(position+1) || record.RecordID == "" {
			return nil, fmt.Errorf("workspace row %d has invalid version, workspace, sequence, or ID", position)
		}
		if _, exists := seenRecordIDs[record.RecordID]; exists {
			return nil, fmt.Errorf("workspace index repeats record ID %q", record.RecordID)
		}
		seenRecordIDs[record.RecordID] = struct{}{}
		if position == 0 {
			if record.Kind != workspaceIndexInitialized || record.RecordID != workspaceGenesisRecordID(workspaceID) || record.Seq != 1 || record.SessionID != "" || record.RunID != "" || record.ActionBatchID != "" || len(record.Actions) != 0 || record.Action != nil || record.Resolution != nil || record.CheckpointID != "" || record.CheckpointSeq != 0 {
				return nil, errors.New("workspace index has an invalid initialization witness")
			}
			continue
		}
		if record.Kind == workspaceIndexInitialized {
			return nil, errors.New("workspace index contains a later initialization witness")
		}
		switch record.Kind {
		case workspaceIndexDispatchReady:
			if err := state.addDispatchRecord(record); err != nil {
				return nil, fmt.Errorf("workspace dispatch record %q: %w", record.RecordID, err)
			}
		case workspaceIndexResolution:
			if err := state.addResolutionRecord(record); err != nil {
				return nil, fmt.Errorf("workspace resolution record %q: %w", record.RecordID, err)
			}
		default:
			return nil, fmt.Errorf("workspace index has unsupported record kind %q", record.Kind)
		}
	}
	return state, nil
}

func (s *workspaceIndexState) addDispatchRecord(record *workspaceIndexRecord) error {
	if record.SessionID == "" || record.RunID == "" || record.ActionBatchID == "" || record.RecordID != workspaceDispatchRecordID(WorkspaceDispatchBatch{WorkspaceID: record.WorkspaceID, SessionID: record.SessionID, RunID: record.RunID, ActionBatchID: record.ActionBatchID}) || len(record.Actions) == 0 || len(record.Actions) > maxWorkspaceIndexActions || record.Action != nil || record.Resolution != nil || record.CheckpointID != "" || record.CheckpointSeq != 0 {
		return errors.New("incomplete or invalid dispatch-ready batch envelope")
	}
	key := workspaceBatchKey{sessionID: record.SessionID, runID: record.RunID, batchID: record.ActionBatchID}
	if s.batches[key] != nil {
		return errors.New("duplicate dispatch-ready batch")
	}
	for _, action := range record.Actions {
		if err := validateWorkspaceDispatchAction(action, record.WorkspaceID, record.SessionID, record.RunID, record.ActionBatchID); err != nil {
			return err
		}
		canonicalEffects, err := normalizeEffectFacts(action.EffectSet)
		if err != nil || !reflect.DeepEqual(canonicalEffects, action.EffectSet) {
			return errors.New("dispatch action effect facts are empty or non-canonical")
		}
		ref := action.Action
		occurrence := workspaceOccurrenceFor(ref)
		actionKey := workspaceActionFor(ref)
		if s.actionsByCall[occurrence] != nil || s.actionsByKey[actionKey] != nil || s.actionsByID[ref.ActionID] != nil {
			return errors.New("duplicate workspace action occurrence or ID")
		}
		indexed := &workspaceIndexedAction{ref: ref, action: cloneWorkspaceDispatchAction(action), marker: record}
		s.actionsByCall[occurrence] = indexed
		s.actionsByKey[actionKey] = indexed
		s.actionsByID[ref.ActionID] = indexed
	}
	s.batches[key] = record
	return nil
}

func (s *workspaceIndexState) addResolutionRecord(record *workspaceIndexRecord) error {
	if record.SessionID != "" || record.RunID != "" || record.ActionBatchID != "" || len(record.Actions) != 0 || record.Action == nil || record.Resolution == nil || record.CheckpointID == "" || record.CheckpointSeq == 0 || record.RecordID != workspaceResolutionRecordID(*record.Action) {
		return errors.New("incomplete or invalid execution-resolution envelope")
	}
	if err := validateWorkspaceResolution(*record.Action, record.CheckpointID, record.CheckpointSeq, *record.Resolution); err != nil {
		return err
	}
	key := workspaceActionFor(*record.Action)
	indexed := s.actionsByKey[key]
	if indexed == nil || indexed.ref != *record.Action {
		return errors.New("execution resolution has no exact prior dispatch-ready action")
	}
	if indexed.resolution != nil {
		return errors.New("duplicate execution resolution for action")
	}
	indexed.resolution = record
	return nil
}

func normalizeWorkspaceDispatchBatch(batch WorkspaceDispatchBatch) (WorkspaceDispatchBatch, error) {
	if !validWorkspaceID(batch.WorkspaceID) || batch.SessionID == "" || batch.RunID == "" || batch.ActionBatchID == "" {
		return WorkspaceDispatchBatch{}, errors.New("session: workspace dispatch batch has incomplete identity")
	}
	if len(batch.Actions) == 0 || len(batch.Actions) > maxWorkspaceIndexActions {
		return WorkspaceDispatchBatch{}, fmt.Errorf("session: workspace dispatch batch has %d actions, expected 1..%d", len(batch.Actions), maxWorkspaceIndexActions)
	}
	normalized := WorkspaceDispatchBatch{
		WorkspaceID: batch.WorkspaceID, SessionID: batch.SessionID, RunID: batch.RunID,
		ActionBatchID: batch.ActionBatchID, Actions: make([]WorkspaceDispatchAction, len(batch.Actions)),
	}
	seenCalls := make(map[string]struct{}, len(batch.Actions))
	seenActionIDs := make(map[string]struct{}, len(batch.Actions))
	for n, action := range batch.Actions {
		if err := validateWorkspaceDispatchAction(action, batch.WorkspaceID, batch.SessionID, batch.RunID, batch.ActionBatchID); err != nil {
			return WorkspaceDispatchBatch{}, fmt.Errorf("session: invalid action %d in dispatch batch: %w", n, err)
		}
		if _, exists := seenCalls[action.Action.ToolCallID]; exists {
			return WorkspaceDispatchBatch{}, fmt.Errorf("session: dispatch batch repeats tool-call ID %q", action.Action.ToolCallID)
		}
		if _, exists := seenActionIDs[action.Action.ActionID]; exists {
			return WorkspaceDispatchBatch{}, fmt.Errorf("session: dispatch batch repeats action ID %q", action.Action.ActionID)
		}
		seenCalls[action.Action.ToolCallID] = struct{}{}
		seenActionIDs[action.Action.ActionID] = struct{}{}
		normalized.Actions[n] = cloneWorkspaceDispatchAction(action)
		effects, err := normalizeEffectFacts(action.EffectSet)
		if err != nil {
			return WorkspaceDispatchBatch{}, fmt.Errorf("session: invalid trusted effect facts for action %q: %w", action.Action.ActionID, err)
		}
		normalized.Actions[n].EffectSet = effects
	}
	return normalized, nil
}

func validateWorkspaceDispatchAction(action WorkspaceDispatchAction, workspaceID, sessionID, runID, batchID string) error {
	ref := action.Action
	if err := ref.Validate(); err != nil {
		return err
	}
	if ref.WorkspaceID != workspaceID || ref.SessionID != sessionID || ref.RunID != runID || ref.ActionBatchID != batchID || ref.ActionID == "" {
		return errors.New("action reference does not match its dispatch batch")
	}
	if action.ToolIdentity != nil {
		identity := action.ToolIdentity
		if identity.RegistrationSource == "" || identity.ToolName == "" || identity.RegistrationGeneration == 0 || !isHexDigest(identity.ExecutorFingerprint) || !isHexDigest(identity.ScopeFingerprint) {
			return errors.New("action has invalid trusted tool identity facts")
		}
	}
	if action.ExecutionIdentity != nil {
		if err := action.ExecutionIdentity.Validate(); err != nil {
			return fmt.Errorf("action has invalid execution identity: %w", err)
		}
	}
	return nil
}

func normalizeEffectFacts(effects []tools.TrustedToolEffect) ([]tools.TrustedToolEffect, error) {
	if len(effects) == 0 {
		return []tools.TrustedToolEffect{tools.EffectUnknown}, nil
	}
	seen := make(map[tools.TrustedToolEffect]struct{}, len(effects))
	values := make([]tools.TrustedToolEffect, 0, len(effects))
	for _, effect := range effects {
		switch effect {
		case tools.EffectWorkspaceRead, tools.EffectWorkspaceWrite, tools.EffectGitMutation, tools.EffectNetworkRead, tools.EffectNetworkWrite, tools.EffectExternal, tools.EffectControl, tools.EffectUnknown:
		default:
			return nil, fmt.Errorf("unsupported effect %q", effect)
		}
		if _, exists := seen[effect]; exists {
			continue
		}
		seen[effect] = struct{}{}
		values = append(values, effect)
	}
	sort.Slice(values, func(a, b int) bool { return values[a] < values[b] })
	return values, nil
}

func validateWorkspaceResolution(ref PendingActionRef, checkpointID string, checkpointSeq uint64, resolution core.ExecutionResolution) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	if ref.ActionID == "" || checkpointID == "" || checkpointSeq == 0 || resolution.RunID != ref.RunID || resolution.ActionID != ref.ActionID || resolution.EvidenceRef == "" {
		return errors.New("execution resolution has incomplete or mismatched identity")
	}
	switch resolution.Evidence {
	case core.ResolutionNeverDispatched, core.ResolutionLocalResourcesStopped, core.ResolutionRemoteTerminal, core.ResolutionOperatorReconciled:
		return nil
	default:
		return fmt.Errorf("unsupported execution resolution evidence %q", resolution.Evidence)
	}
}

func groupValidatedResolutions(resolutions []ValidatedExecutionResolution) (map[string][]ValidatedExecutionResolution, error) {
	groups := make(map[string][]ValidatedExecutionResolution)
	seen := make(map[workspaceActionKey]struct{}, len(resolutions))
	for n, resolution := range resolutions {
		if err := validateWorkspaceResolution(resolution.Action, resolution.CheckpointEntryID, resolution.CheckpointSeq, resolution.Resolution); err != nil {
			return nil, fmt.Errorf("session: invalid validated execution resolution %d: %w", n, err)
		}
		key := workspaceActionFor(resolution.Action)
		if _, exists := seen[key]; exists {
			return nil, fmt.Errorf("session: duplicate execution resolution action at index %d", n)
		}
		seen[key] = struct{}{}
		groups[resolution.Action.WorkspaceID] = append(groups[resolution.Action.WorkspaceID], resolution)
	}
	return groups, nil
}

func sortedWorkspaceIDs(groups map[string][]ValidatedExecutionResolution) []string {
	ids := make([]string, 0, len(groups))
	for id := range groups {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func sortedPendingWorkspaceIDs(groups map[string][]indexedPendingAction) []string {
	ids := make([]string, 0, len(groups))
	for id := range groups {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func validateWorkspaceIndexDirectory(directory string) error {
	info, err := os.Lstat(directory)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return fmt.Errorf("workspace quarantine directory must be a non-symlink 0700 directory")
	}
	return nil
}

func ensureWorkspaceIndexDirectory(configRoot string) error {
	if configRoot == "" {
		return errors.New("user config root is empty")
	}
	appDir := filepath.Join(configRoot, "go-cli")
	indexDir := filepath.Join(appDir, "workspace-quarantine")
	if err := mkdirDurably(appDir, 0o700); err != nil {
		return err
	}
	if err := validatePrivateDirectory(appDir); err != nil {
		return err
	}
	if err := mkdirDurably(indexDir, 0o700); err != nil {
		return err
	}
	return validateWorkspaceIndexDirectory(indexDir)
}

func validatePrivateDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return fmt.Errorf("workspace index parent %q must be a non-symlink 0700 directory", path)
	}
	return nil
}

func validateWorkspaceFileMode(file *os.File, expected os.FileMode, name string) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != expected {
		return fmt.Errorf("workspace file %q must be regular with mode %04o", name, expected)
	}
	return nil
}

func regularFileExists(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false, fmt.Errorf("workspace index path %q is not a regular file", path)
	}
	return true, nil
}

func validWorkspaceID(workspaceID string) bool {
	if workspaceID == "" || len(workspaceID) > 128 {
		return false
	}
	for n, character := range workspaceID {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || (n > 0 && (character == '_' || character == '-')) {
			continue
		}
		return false
	}
	return true
}

func workspaceIndexUnavailable(operation string, err error) error {
	if err == nil {
		return ErrWorkspaceIndexUnavailable
	}
	return fmt.Errorf("%w: %s: %w", ErrWorkspaceIndexUnavailable, operation, err)
}

func closeWorkspaceLock(file *os.File) error {
	if file == nil {
		return nil
	}
	return errors.Join(flockUnlock(file), file.Close())
}

func marshalWorkspaceIndexRecord(record workspaceIndexRecord) ([]byte, error) {
	line, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("session: encode workspace index record: %w", err)
	}
	if len(line) > maxDurableJSONLineBytes {
		return nil, fmt.Errorf("session: workspace index row exceeds %d bytes", maxDurableJSONLineBytes)
	}
	return append(line, '\n'), nil
}

func workspaceGenesisRecordID(workspaceID string) string {
	return stableWorkspaceRecordID("genesis", workspaceID)
}

func workspaceDispatchRecordID(batch WorkspaceDispatchBatch) string {
	return stableWorkspaceRecordID("dispatch", batch.WorkspaceID, batch.SessionID, batch.RunID, batch.ActionBatchID)
}

func workspaceResolutionRecordID(ref PendingActionRef) string {
	return stableWorkspaceRecordID("resolution", ref.WorkspaceID, ref.SessionID, ref.RunID, ref.ActionBatchID, ref.ActionID)
}

func stableWorkspaceRecordID(kind string, parts ...string) string {
	var data strings.Builder
	data.WriteString(kind)
	for _, part := range parts {
		data.WriteByte(0)
		data.WriteString(part)
	}
	digest := sha256.Sum256([]byte(data.String()))
	return kind + ":" + hex.EncodeToString(digest[:])
}

func sameWorkspaceResolution(existing, candidate workspaceIndexRecord) bool {
	return existing.Kind == candidate.Kind && existing.WorkspaceID == candidate.WorkspaceID && existing.RecordID == candidate.RecordID && existing.Action != nil && candidate.Action != nil && *existing.Action == *candidate.Action && existing.CheckpointID == candidate.CheckpointID && existing.CheckpointSeq == candidate.CheckpointSeq && existing.Resolution != nil && candidate.Resolution != nil && *existing.Resolution == *candidate.Resolution
}

func refPointer(ref PendingActionRef) *PendingActionRef {
	copy := ref
	return &copy
}

func resolutionPointer(resolution core.ExecutionResolution) *core.ExecutionResolution {
	copy := resolution
	return &copy
}

func cloneWorkspaceDispatchAction(action WorkspaceDispatchAction) WorkspaceDispatchAction {
	if action.ToolIdentity != nil {
		copy := *action.ToolIdentity
		action.ToolIdentity = &copy
	}
	if action.EffectSet != nil {
		action.EffectSet = append([]tools.TrustedToolEffect(nil), action.EffectSet...)
	}
	if action.ExecutionIdentity != nil {
		copy := *action.ExecutionIdentity
		action.ExecutionIdentity = &copy
	}
	return action
}

func workspaceOccurrenceFor(ref PendingActionRef) workspaceOccurrenceKey {
	return workspaceOccurrenceKey{sessionID: ref.SessionID, workspace: ref.WorkspaceID, runID: ref.RunID, ordinal: ref.AssistantTranscriptOrdinal, toolCall: ref.ToolCallID}
}

func workspaceActionFor(ref PendingActionRef) workspaceActionKey {
	return workspaceActionKey{workspaceOccurrenceKey: workspaceOccurrenceFor(ref), batchID: ref.ActionBatchID, actionID: ref.ActionID}
}

var _ RecoveryAuthority = (*WorkspaceQuarantineIndex)(nil)
