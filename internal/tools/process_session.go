package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const (
	// ProcessSessionToolName is the builtin tool's stable wire name.
	ProcessSessionToolName = "process_session"
	// ProcessSessionOperationStart starts a host-owned process.
	ProcessSessionOperationStart = "start"
	// ProcessSessionOperationRead reads bounded process output.
	ProcessSessionOperationRead = "read"
	// ProcessSessionOperationStatus reads process status.
	ProcessSessionOperationStatus = "status"
	// ProcessSessionOperationStop stops a process.
	ProcessSessionOperationStop = "stop"
)

// ProcessSessionLeasePhase is the persisted lifecycle of a host-owned process.
type ProcessSessionLeasePhase string

const (
	ProcessSessionLeaseGated     ProcessSessionLeasePhase = "gated"
	ProcessSessionLeaseActive    ProcessSessionLeasePhase = "active"
	ProcessSessionLeaseDelivered ProcessSessionLeasePhase = "delivered_live"
	ProcessSessionLeaseStopping  ProcessSessionLeasePhase = "stopping"
	ProcessSessionLeaseStopped   ProcessSessionLeasePhase = "stopped"
	ProcessSessionLeaseUnknown   ProcessSessionLeasePhase = "unknown"
)

// ProcessSessionActionRef binds a process lease to one exact prepared action.
type ProcessSessionActionRef struct {
	SessionID                  string
	WorkspaceID                string
	RunID                      string
	ActionBatchID              string
	ActionID                   string
	OwnerGeneration            string
	AssistantTranscriptOrdinal uint64
	ToolCallID                 string
	SchemaDigest               [32]byte
	ArgumentDigest             [32]byte
}

// ProcessSessionLeaseSnapshot records process ownership facts, never runtime authority.
type ProcessSessionLeaseSnapshot struct {
	SessionID       string
	OwnerID         string
	WorkspaceID     string
	RunID           string
	OwnerGeneration string
	Action          ProcessSessionActionRef
	Phase           ProcessSessionLeasePhase
	Identity        *ProcessExecutionIdentity
	Status          ProcessSessionStatus
	OutputBytes     int64
	OutputTruncated bool
	GroupAbsent     bool
	LeaderWaited    bool
	ReadersJoined   bool
}

// ProcessSessionActionRecord binds one ordinary process_session API call to
// the lease it read or controlled. It is a durable fact, not authority.
type ProcessSessionActionRecord struct {
	LeaseSessionID string                  `json:"lease_session_id"`
	LeaseAction    ProcessSessionActionRef `json:"lease_action"`
	Action         ProcessSessionActionRef `json:"action"`
	Operation      string                  `json:"operation"`
}

// CloneProcessSessionLeaseSnapshot copies persisted fact fields while keeping
// the nested process identity immutable to callers.
func CloneProcessSessionLeaseSnapshot(snapshot ProcessSessionLeaseSnapshot) ProcessSessionLeaseSnapshot {
	if snapshot.Identity != nil {
		identity := *snapshot.Identity
		snapshot.Identity = &identity
	}
	return snapshot
}

// ProcessSessionDeliveryBinding binds one run terminal to the complete set of
// live process sessions accepted by the same invocation host.
type ProcessSessionDeliveryBinding struct {
	OwnerID     string   `json:"owner_id"`
	RunID       string   `json:"run_id"`
	WorkspaceID string   `json:"workspace_id"`
	SessionIDs  []string `json:"session_ids"`
}

// ProcessSessionDelivery carries the host-local live authority produced only
// after the durable terminal binding is committed. Its manager pointer is not
// serialized and cannot be restored from a session snapshot.
type ProcessSessionDelivery struct {
	Binding ProcessSessionDeliveryBinding `json:"binding"`
	manager *LocalProcessSessionManager
}

// CloneProcessSessionDelivery returns an isolated view while preserving its
// unexported, same-manager runtime authority.
func CloneProcessSessionDelivery(delivery *ProcessSessionDelivery) *ProcessSessionDelivery {
	if delivery == nil {
		return nil
	}
	return &ProcessSessionDelivery{Binding: cloneProcessSessionDeliveryBinding(delivery.Binding), manager: delivery.manager}
}

// ProcessSessionLeaseObserver synchronously records lease transitions before
// a command is released or a host handoff is reported.
type ProcessSessionLeaseObserver func(context.Context, ProcessSessionLeaseSnapshot) error

type processSessionActionContextKey struct{}
type processSessionLeaseObserverContextKey struct{}

// WithProcessSessionAction binds a prepared invocation to its exact durable action.
func WithProcessSessionAction(ctx context.Context, action ProcessSessionActionRef, observer ProcessSessionLeaseObserver) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = context.WithValue(ctx, processSessionActionContextKey{}, action)
	if observer != nil {
		ctx = context.WithValue(ctx, processSessionLeaseObserverContextKey{}, observer)
	}
	return ctx
}

func processSessionActionFromContext(ctx context.Context) (ProcessSessionActionRef, bool) {
	if ctx == nil {
		return ProcessSessionActionRef{}, false
	}
	ref, ok := ctx.Value(processSessionActionContextKey{}).(ProcessSessionActionRef)
	return ref, ok
}

func processSessionLeaseObserverFromContext(ctx context.Context) ProcessSessionLeaseObserver {
	if ctx == nil {
		return nil
	}
	observer, _ := ctx.Value(processSessionLeaseObserverContextKey{}).(ProcessSessionLeaseObserver)
	return observer
}

// ProcessSessionAdmissionCapability is an unforgeable same-manager capability
// used only to admit actions from the run that owns an active process session.
type ProcessSessionAdmissionCapability struct {
	manager         *LocalProcessSessionManager
	sessionID       string
	runID           string
	workspaceID     string
	ownerGeneration string
}

// ProcessSessionAdmissionBinding is an opaque snapshot of a manager-issued
// admission capability's immutable payload. It is useful only for exact
// identity checks and cannot mint or restore runtime authority.
type ProcessSessionAdmissionBinding struct {
	manager         *LocalProcessSessionManager
	sessionID       string
	runID           string
	workspaceID     string
	ownerGeneration string
}

// CaptureBinding returns the immutable payload bound to this exact capability.
func (capability *ProcessSessionAdmissionCapability) CaptureBinding() ProcessSessionAdmissionBinding {
	if capability == nil {
		return ProcessSessionAdmissionBinding{}
	}
	return ProcessSessionAdmissionBinding{
		manager: capability.manager, sessionID: capability.sessionID, runID: capability.runID,
		workspaceID: capability.workspaceID, ownerGeneration: capability.ownerGeneration,
	}
}

// MatchesBinding reports whether this capability still carries the exact
// payload captured earlier. It does not check liveness or grant admission.
func (capability *ProcessSessionAdmissionCapability) MatchesBinding(binding ProcessSessionAdmissionBinding) bool {
	return capability != nil && capability.manager == binding.manager && capability.sessionID == binding.sessionID &&
		capability.runID == binding.runID && capability.workspaceID == binding.workspaceID &&
		capability.ownerGeneration == binding.ownerGeneration
}

// Allows reports whether this exact live manager lease admits the supplied action scope.
func (capability ProcessSessionAdmissionCapability) Allows(runID, workspaceID, ownerGeneration string) bool {
	if capability.manager == nil || runID == "" || workspaceID == "" || ownerGeneration == "" ||
		runID != capability.runID || workspaceID != capability.workspaceID || ownerGeneration != capability.ownerGeneration {
		return false
	}
	return capability.manager.validAdmissionCapability(capability)
}

// AllowsLease matches this capability to a persisted lease fact while
// requiring its original manager to still own a live session.
func (capability ProcessSessionAdmissionCapability) AllowsLease(lease ProcessSessionLeaseSnapshot) bool {
	if capability.manager == nil || lease.SessionID == "" ||
		(capability.sessionID != "" && lease.SessionID != capability.sessionID) ||
		lease.RunID != capability.runID || lease.WorkspaceID != capability.workspaceID ||
		lease.OwnerGeneration != capability.ownerGeneration ||
		(lease.Phase != ProcessSessionLeaseActive && lease.Phase != ProcessSessionLeaseDelivered) ||
		lease.Identity == nil {
		return false
	}
	return capability.manager.ownsAdmissionLease(capability, lease)
}

// AllowsLeaseForAdmission checks exact manager ownership while a healthy
// process is being joined and its durable stopped receipt is being synced.
// Physical-liveness consumers must continue to use AllowsLease.
func (capability ProcessSessionAdmissionCapability) AllowsLeaseForAdmission(lease ProcessSessionLeaseSnapshot) bool {
	if capability.manager == nil || lease.SessionID == "" ||
		(capability.sessionID != "" && lease.SessionID != capability.sessionID) ||
		lease.RunID != capability.runID || lease.WorkspaceID != capability.workspaceID ||
		lease.OwnerGeneration != capability.ownerGeneration ||
		(lease.Phase != ProcessSessionLeaseActive && lease.Phase != ProcessSessionLeaseDelivered) ||
		lease.Identity == nil || !capability.Allows(lease.RunID, lease.WorkspaceID, lease.OwnerGeneration) {
		return false
	}
	return capability.manager.ownsAdmissionLeaseForAdmission(capability, lease)
}

// ProcessSessionInvocation is available only for a captured first-party
// ProcessSessionTool prepared invocation.
type ProcessSessionInvocation struct {
	manager   *LocalProcessSessionManager
	operation string
	call      ToolCall
}

// Operation returns the frozen process-session operation.
func (invocation ProcessSessionInvocation) Operation() string { return invocation.operation }

// Call returns a defensive copy of the frozen process-session tool call.
func (invocation ProcessSessionInvocation) Call() ToolCall {
	return ToolCall{ID: invocation.call.ID, Name: invocation.call.Name, Args: cloneFrozenMap(invocation.call.Args)}
}

// AdmissionCapability returns a live capability for a session started by this
// exact manager and original run.
func (invocation ProcessSessionInvocation) AdmissionCapability(sessionID, runID, ownerGeneration string) (ProcessSessionAdmissionCapability, bool) {
	if invocation.manager == nil {
		return ProcessSessionAdmissionCapability{}, false
	}
	return invocation.manager.AdmissionCapability(sessionID, runID, ownerGeneration)
}

// AllowsLease validates a durable lease snapshot against this exact manager's
// live session, including its immutable action and captured process identity.
func (invocation ProcessSessionInvocation) AllowsLease(lease ProcessSessionLeaseSnapshot) bool {
	if invocation.manager == nil {
		return false
	}
	capability := ProcessSessionAdmissionCapability{
		manager: invocation.manager, sessionID: lease.SessionID, runID: lease.RunID,
		workspaceID: lease.WorkspaceID, ownerGeneration: lease.OwnerGeneration,
	}
	return capability.AllowsLease(lease)
}

// WithStartLeasePublication serializes the start receipt's durable publication
// with the manager's physical-closure observer and supplies the latest joined
// or live lease snapshot to the callback.
func (invocation ProcessSessionInvocation) WithStartLeasePublication(
	sessionID string,
	callback func(ProcessSessionLeaseSnapshot) error,
) error {
	if invocation.manager == nil || invocation.operation != ProcessSessionOperationStart || callback == nil {
		return ErrProcessSessionOperation
	}
	return invocation.manager.withStartLeasePublication(sessionID, callback)
}

// StartActionPublished reports whether this manager has synced the exact
// successful start receipt and its correlated process-session action row.
func (invocation ProcessSessionInvocation) StartActionPublished(sessionID string) bool {
	return invocation.manager != nil && invocation.manager.startActionPublished(sessionID)
}

// LeaseLifecycle reports whether the exact owned lease is physically live or
// is in healthy manager-owned cleanup. It does not authorize mutations.
func (invocation ProcessSessionInvocation) LeaseLifecycle(lease ProcessSessionLeaseSnapshot) (live, closing bool, err error) {
	if invocation.manager == nil {
		return false, false, ErrProcessSessionManagerClosed
	}
	return invocation.manager.leaseLifecycle(lease)
}

// SessionClosed reports only durable lease closure, after the owner has
// stopped and joined the process group/readers and persisted that closure.
func (invocation ProcessSessionInvocation) SessionClosed(sessionID string) <-chan struct{} {
	if invocation.manager == nil || sessionID == "" {
		return nil
	}
	return invocation.manager.SessionClosed(sessionID)
}

// SessionClosureAttempted closes after the physical cleanup observer has
// persisted closure or reported that persistence failed. It is never itself
// evidence that the durable lease was resolved.
func (invocation ProcessSessionInvocation) SessionClosureAttempted(sessionID string) <-chan struct{} {
	if invocation.manager == nil || sessionID == "" {
		return nil
	}
	return invocation.manager.SessionClosureAttempted(sessionID)
}

// OwnerClosed reports that the same process-session manager finished its
// shutdown attempt. A closed owner channel without SessionClosed is a failed
// durable closure observation, not a quiescence proof.
func (invocation ProcessSessionInvocation) OwnerClosed() <-chan struct{} {
	if invocation.manager == nil {
		return nil
	}
	return invocation.manager.OwnerClosed()
}

// MarkLeaseClosed closes the manager-local owner channel after a caller has
// durably persisted the positive stopped-group evidence.
func (invocation ProcessSessionInvocation) MarkLeaseClosed(sessionID string) bool {
	return invocation.manager != nil && invocation.manager.MarkLeaseClosed(sessionID)
}

// DeliveryBinding returns the exact active session set for the original run.
func (invocation ProcessSessionInvocation) DeliveryBinding(runID string) (ProcessSessionDeliveryBinding, bool, error) {
	if invocation.manager == nil {
		return ProcessSessionDeliveryBinding{}, false, nil
	}
	handoff, active, err := invocation.manager.Handoff(runID)
	if err != nil || !active {
		return ProcessSessionDeliveryBinding{}, active, err
	}
	return ProcessSessionDeliveryBinding{
		OwnerID: handoff.OwnerID, RunID: handoff.RunID, WorkspaceID: handoff.WorkspaceID,
		SessionIDs: append([]string(nil), handoff.SessionIDs...),
	}, true, nil
}

// ConfirmTerminalDelivery returns a host-local capability after the durable
// terminal checkpoint has accepted this exact live session set.
func (invocation ProcessSessionInvocation) ConfirmTerminalDelivery(binding ProcessSessionDeliveryBinding) (ProcessSessionDelivery, error) {
	if invocation.manager == nil {
		return ProcessSessionDelivery{}, errors.New("tools: process-session invocation has no local manager")
	}
	return invocation.manager.ConfirmDelivery(binding)
}

// ProcessSessionInvocationFor recognizes only the exact sealed builtin tool.
func ProcessSessionInvocationFor(invocation PreparedInvocation) (ProcessSessionInvocation, bool) {
	prepared, ok := invocation.(*preparedInvocation)
	if !ok || prepared == nil {
		return ProcessSessionInvocation{}, false
	}
	tool, ok := prepared.definition.(*ProcessSessionTool)
	if !ok || tool == nil || prepared.identity.RegistrationSource() != RegistrationSourceBuiltin {
		return ProcessSessionInvocation{}, false
	}
	manager, ok := tool.manager.(*LocalProcessSessionManager)
	if !ok || manager == nil {
		return ProcessSessionInvocation{}, false
	}
	call := prepared.call.ToolCall()
	operation, _ := call.Args["operation"].(string)
	return ProcessSessionInvocation{manager: manager, operation: operation, call: call}, true
}

var (
	// ErrProcessSessionNotFound indicates that the requested session is not
	// owned by this process-session manager.
	ErrProcessSessionNotFound = errors.New("tools: process session not found")
	// ErrProcessSessionOperation indicates that a process-session call is invalid.
	ErrProcessSessionOperation = errors.New("tools: invalid process-session operation")
)

// ProcessSessionOperations defines the operations exposed by the
// process_session tool.
type ProcessSessionOperations interface {
	Start(context.Context, string) (ProcessSessionSnapshot, error)
	Read(context.Context, string, int64, int) (ProcessSessionOutput, error)
	Status(context.Context, string) (ProcessSessionSnapshot, error)
	Stop(context.Context, string) (ProcessSessionSnapshot, error)
}

// ProcessSessionTool starts, reads, checks, and stops host-owned background
// process sessions.
type ProcessSessionTool struct {
	manager ProcessSessionOperations
}

var (
	_ ToolDefinition   = (*ProcessSessionTool)(nil)
	_ Parameterized    = (*ProcessSessionTool)(nil)
	_ PromptGuideliner = (*ProcessSessionTool)(nil)
)

// NewProcessSessionTool returns a tool backed by the supplied process-session
// manager.
func NewProcessSessionTool(manager ProcessSessionOperations) *ProcessSessionTool {
	return &ProcessSessionTool{manager: manager}
}

// Name returns the tool name.
func (*ProcessSessionTool) Name() string { return ProcessSessionToolName }

// Description returns a brief description of the tool.
func (*ProcessSessionTool) Description() string {
	return "process_session: manage one host-owned background process. Use operation start with command, then read/status/stop with the returned session_id. Output reads are bounded and use byte offsets. Sessions end when stopped, naturally completed, or their host closes."
}

// PromptGuidelines gives the model concise process-session guidance.
func (*ProcessSessionTool) PromptGuidelines() []string {
	return []string{
		"Use process_session start for a server or worker that must remain available after its tool call returns.",
		"Save the returned session_id, then use read or status to inspect it and stop when finished.",
		"Ordinary bash remains synchronous; do not leave background jobs running from bash.",
	}
}

// Parameters returns the JSON Schema for process-session operations.
func (*ProcessSessionTool) Parameters() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"operation": map[string]any{
				"type": "string",
				"enum": []any{ProcessSessionOperationStart, ProcessSessionOperationRead, ProcessSessionOperationStatus, ProcessSessionOperationStop},
			},
			"command":    map[string]any{"type": "string", "description": "Command to start in the configured workspace."},
			"session_id": map[string]any{"type": "string", "description": "Opaque session ID returned by start."},
			"offset":     map[string]any{"type": "integer", "minimum": 0, "description": "Byte offset for read; defaults to the oldest retained output."},
			"max_bytes":  map[string]any{"type": "integer", "minimum": 1, "maximum": maxProcessSessionReadBytes, "description": "Maximum output bytes returned by read."},
		},
		"required":             []any{"operation"},
		"additionalProperties": false,
	}
}

// Execute runs one process-session operation.
func (t *ProcessSessionTool) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	if t == nil || t.manager == nil {
		return nil, errors.New("tools: process-session manager is required")
	}
	operation, ok := call.Args["operation"].(string)
	if !ok {
		return nil, fmt.Errorf("%w: operation must be a string", ErrProcessSessionOperation)
	}
	if err := validateProcessSessionArguments(call.Args, operation); err != nil {
		return nil, err
	}
	switch operation {
	case ProcessSessionOperationStart:
		command, ok := call.Args["command"].(string)
		if !ok || strings.TrimSpace(command) == "" {
			return nil, fmt.Errorf("%w: start requires a non-empty command", ErrProcessSessionOperation)
		}
		snapshot, err := t.manager.Start(ctx, command)
		if err != nil {
			return nil, fmt.Errorf("process_session: start: %w", err)
		}
		result, resultErr := processSessionSnapshotResult(call.ID, snapshot, "Process session started.")
		if resultErr == nil {
			if manager, ok := t.manager.(*LocalProcessSessionManager); ok {
				phase := ProcessSessionLeaseActive
				if snapshot.Status == ProcessSessionExited || snapshot.Status == ProcessSessionStopped {
					phase = ProcessSessionLeaseStopped
				}
				lease := manager.leaseSnapshot(snapshot.SessionID, phase)
				result.ProcessSessionLease = &lease
				if capability, allowed := manager.ManagerAdmissionCapability(lease.RunID, lease.OwnerGeneration); allowed {
					result.ProcessSessionAdmission = &capability
				}
			}
		}
		return result, resultErr
	case ProcessSessionOperationRead:
		sessionID, err := processSessionID(call.Args)
		if err != nil {
			return nil, err
		}
		offset, err := optionalNonNegativeInteger(call.Args, "offset", 0)
		if err != nil {
			return nil, err
		}
		maxBytes, err := optionalPositiveInteger(call.Args, "max_bytes", defaultProcessSessionReadBytes, maxProcessSessionReadBytes)
		if err != nil {
			return nil, err
		}
		output, err := t.manager.Read(ctx, sessionID, offset, maxBytes)
		if err != nil {
			return nil, fmt.Errorf("process_session: read: %w", err)
		}
		content, encodeErr := json.Marshal(map[string]any{
			"session_id":          output.SessionID,
			"status":              output.Status,
			"output":              output.Output,
			"offset":              output.Offset,
			"next_offset":         output.NextOffset,
			"output_start_offset": output.OutputStartOffset,
			"total_bytes":         output.TotalBytes,
			"truncated":           output.Truncated,
		})
		if encodeErr != nil {
			return nil, fmt.Errorf("process_session: encode read result: %w", encodeErr)
		}
		return &ToolResult{
			ToolCallID: call.ID,
			Output:     string(content),
			ProcessSessionLease: func() *ProcessSessionLeaseSnapshot {
				if manager, ok := t.manager.(*LocalProcessSessionManager); ok {
					lease := manager.leaseSnapshot(output.SessionID, ProcessSessionLeaseActive)
					return &lease
				}
				return nil
			}(),
			Metadata: map[string]any{
				"session_id":          output.SessionID,
				"status":              output.Status,
				"offset":              output.Offset,
				"next_offset":         output.NextOffset,
				"output_start_offset": output.OutputStartOffset,
				"total_bytes":         output.TotalBytes,
				"truncated":           output.Truncated,
			},
		}, nil
	case ProcessSessionOperationStatus, ProcessSessionOperationStop:
		sessionID, err := processSessionID(call.Args)
		if err != nil {
			return nil, err
		}
		var snapshot ProcessSessionSnapshot
		if operation == ProcessSessionOperationStatus {
			snapshot, err = t.manager.Status(ctx, sessionID)
		} else {
			snapshot, err = t.manager.Stop(ctx, sessionID)
		}
		if err != nil {
			return nil, fmt.Errorf("process_session: %s: %w", operation, err)
		}
		message := "Process session status retrieved."
		if operation == ProcessSessionOperationStop {
			message = "Process session stopped."
		}
		result, resultErr := processSessionSnapshotResult(call.ID, snapshot, message)
		if resultErr == nil {
			if manager, ok := t.manager.(*LocalProcessSessionManager); ok {
				phase := ProcessSessionLeaseActive
				if snapshot.Status == ProcessSessionExited || snapshot.Status == ProcessSessionStopped {
					phase = ProcessSessionLeaseStopped
				}
				lease := manager.leaseSnapshot(snapshot.SessionID, phase)
				result.ProcessSessionLease = &lease
			}
		}
		return result, resultErr
	default:
		return nil, fmt.Errorf("%w: %q must be start, read, status, or stop", ErrProcessSessionOperation, operation)
	}
}

func validateProcessSessionArguments(args map[string]any, operation string) error {
	allowed := map[string]struct{}{"operation": {}}
	switch operation {
	case ProcessSessionOperationStart:
		allowed["command"] = struct{}{}
	case ProcessSessionOperationRead:
		allowed["session_id"] = struct{}{}
		allowed["offset"] = struct{}{}
		allowed["max_bytes"] = struct{}{}
	case ProcessSessionOperationStatus, ProcessSessionOperationStop:
		allowed["session_id"] = struct{}{}
	}
	for key := range args {
		if _, ok := allowed[key]; !ok {
			return fmt.Errorf("%w: unexpected argument %q", ErrProcessSessionOperation, key)
		}
	}
	return nil
}

func processSessionID(args map[string]any) (string, error) {
	value, ok := args["session_id"].(string)
	if !ok || strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%w: session_id must be a non-empty string", ErrProcessSessionOperation)
	}
	return value, nil
}

func processSessionSnapshotResult(callID string, snapshot ProcessSessionSnapshot, message string) (*ToolResult, error) {
	metadata := map[string]any{
		"session_id":          snapshot.SessionID,
		"status":              snapshot.Status,
		"output_start_offset": snapshot.OutputStartOffset,
		"total_bytes":         snapshot.TotalBytes,
		"truncated":           snapshot.Truncated,
	}
	if snapshot.ExitCode != nil {
		metadata["exit_code"] = *snapshot.ExitCode
	}
	content, err := json.Marshal(metadata)
	if err != nil {
		return nil, fmt.Errorf("process_session: encode status result: %w", err)
	}
	return &ToolResult{ToolCallID: callID, Output: string(content), Metadata: metadata}, nil
}

func optionalNonNegativeInteger(args map[string]any, name string, fallback int64) (int64, error) {
	value, exists := args[name]
	if !exists {
		return fallback, nil
	}
	parsed, ok := integerArgument(value)
	if !ok || parsed < 0 {
		return 0, fmt.Errorf("%w: %s must be a non-negative integer", ErrProcessSessionOperation, name)
	}
	return parsed, nil
}

func optionalPositiveInteger(args map[string]any, name string, fallback, maximum int) (int, error) {
	value, exists := args[name]
	if !exists {
		return fallback, nil
	}
	parsed, ok := integerArgument(value)
	if !ok || parsed <= 0 || parsed > int64(maximum) {
		return 0, fmt.Errorf("%w: %s must be between 1 and %d", ErrProcessSessionOperation, name, maximum)
	}
	return int(parsed), nil
}

func integerArgument(value any) (int64, bool) {
	switch number := value.(type) {
	case int:
		return int64(number), true
	case int32:
		return int64(number), true
	case int64:
		return number, true
	case uint:
		if uint64(number) > uint64(^uint64(0)>>1) {
			return 0, false
		}
		return int64(number), true
	case uint64:
		if number > uint64(^uint64(0)>>1) {
			return 0, false
		}
		return int64(number), true
	case float64:
		if number < 0 || number != float64(int64(number)) {
			return 0, false
		}
		return int64(number), true
	default:
		return 0, false
	}
}
