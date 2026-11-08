package tools

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	defaultProcessSessionOutputBytes     = 1 << 20
	defaultProcessSessionReadBytes       = 16 << 10
	maxProcessSessionReadBytes           = 64 << 10
	defaultProcessSessionCount           = 16
	processSessionHistoryCount           = 16
	processSessionClosureObserverTimeout = 20 * time.Second
)

var (
	// ErrProcessSessionManagerClosed indicates that the owner no longer accepts
	// process-session operations.
	ErrProcessSessionManagerClosed = errors.New("tools: process-session manager is closed")
	// ErrProcessSessionLimit indicates that the manager already owns its maximum
	// number of process sessions.
	ErrProcessSessionLimit = errors.New("tools: process-session limit reached")
	// ErrProcessSessionSandboxRequired indicates that no Bash sandbox was
	// configured for the process-session start operation.
	ErrProcessSessionSandboxRequired = errors.New("tools: process-session Bash sandbox is required")
	// ErrProcessSessionScopeDenied indicates that a call does not belong to this
	// manager's opaque owner and workspace scope.
	ErrProcessSessionScopeDenied = errors.New("tools: process-session scope denied")
	// ErrProcessSessionCompletionUnknown indicates that the owned process group
	// or its output readers could not be positively joined.
	ErrProcessSessionCompletionUnknown = errors.New("tools: process-session completion is unknown")
	// ErrProcessSessionsUnsupported indicates that the current platform has no
	// supported local process-group owner.
	ErrProcessSessionsUnsupported = errors.New("tools: process sessions are unsupported on this platform")
)

// ProcessSessionStatus describes the public state of one owned process.
type ProcessSessionStatus string

const (
	ProcessSessionStarting ProcessSessionStatus = "starting"
	ProcessSessionRunning  ProcessSessionStatus = "running"
	ProcessSessionStopping ProcessSessionStatus = "stopping"
	ProcessSessionExited   ProcessSessionStatus = "exited"
	ProcessSessionStopped  ProcessSessionStatus = "stopped"
)

// ProcessSessionSnapshot contains safe, bounded state for one process session.
// It does not disclose a process ID, command, or workspace path.
type ProcessSessionSnapshot struct {
	SessionID         string
	Status            ProcessSessionStatus
	ExitCode          *int
	OutputStartOffset int64
	TotalBytes        int64
	Truncated         bool
}

// ProcessSessionOutput contains one bounded output slice and its cursor state.
type ProcessSessionOutput struct {
	SessionID         string
	Status            ProcessSessionStatus
	Output            string
	Offset            int64
	NextOffset        int64
	OutputStartOffset int64
	TotalBytes        int64
	Truncated         bool
}

// ProcessSessionScope identifies the host and workspace allowed to access a
// session. RunID is captured from the current agent run for provenance.
type ProcessSessionScope struct {
	OwnerID     string
	RunID       string
	WorkspaceID string
}

// ProcessSessionScopeResolver resolves the trusted owner, current agent run,
// and workspace for one tool call.
type ProcessSessionScopeResolver func(context.Context) (ProcessSessionScope, error)

// ProcessSessionManagerConfig configures one host-owned process-session
// manager.
type ProcessSessionManagerConfig struct {
	OwnerID                     string
	WorkspaceID                 string
	Workdir                     string
	Sandbox                     BashSandbox
	ResourceLimits              ResourceLimits
	MaxSessions                 int
	MaxOutput                   int
	RequireDeliveryConfirmation bool
	ClosureObserver             ProcessSessionLeaseObserver
	ScopeResolver               ProcessSessionScopeResolver
}

// ProcessSessionHandoff is the typed, additive CLI record emitted only after
// the invocation host accepts an active process-session manager.
type ProcessSessionHandoff struct {
	Kind        string   `json:"kind"`
	Version     int      `json:"version"`
	Status      string   `json:"status"`
	OwnerID     string   `json:"owner_id"`
	RunID       string   `json:"run_id"`
	WorkspaceID string   `json:"workspace_id"`
	SessionIDs  []string `json:"session_ids"`
}

// LocalProcessSessionManager owns local background commands and their output
// readers for one invocation/workspace scope.
type LocalProcessSessionManager struct {
	mu        sync.Mutex
	ctx       context.Context
	cancel    context.CancelFunc
	config    ProcessSessionManagerConfig
	sessions  map[string]*managedProcessSession
	delivered map[string]ProcessSessionDeliveryBinding
	closed    bool
	wg        sync.WaitGroup
	closeOnce sync.Once
	closeDone chan struct{}
	closeErr  error
}

type managedProcessSession struct {
	mu                        sync.Mutex
	leasePublicationMu        sync.Mutex
	manager                   *LocalProcessSessionManager
	id                        string
	originRunID               string
	action                    ProcessSessionActionRef
	ctx                       context.Context
	cancel                    context.CancelFunc
	done                      chan struct{}
	output                    *processSessionBuffer
	status                    ProcessSessionStatus
	stopRequested             bool
	exitCode                  *int
	cleanupErr                error
	startedAt                 time.Time
	processGroupID            int
	identity                  *ProcessExecutionIdentity
	ownerGeneration           string
	leaseObserver             ProcessSessionLeaseObserver
	leaseObserverErr          error
	startActionPublished      bool
	groupAbsent               bool
	leaderWaited              bool
	readersJoined             bool
	leaseClosed               chan struct{}
	leaseClosedOnce           sync.Once
	leaseClosureAttempted     chan struct{}
	leaseClosureAttemptedOnce sync.Once
}

// NewLocalProcessSessionManager creates a manager whose child processes live
// until explicitly stopped or the supplied host context is canceled.
func NewLocalProcessSessionManager(ownerCtx context.Context, config ProcessSessionManagerConfig) (*LocalProcessSessionManager, error) {
	if ownerCtx == nil {
		return nil, errors.New("tools: process-session owner context is required")
	}
	if config.OwnerID == "" || config.WorkspaceID == "" {
		return nil, errors.New("tools: process-session owner and workspace IDs are required")
	}
	if config.ScopeResolver == nil {
		return nil, errors.New("tools: process-session scope resolver is required")
	}
	if config.Workdir == "" {
		config.Workdir = defaultBashWorkdir
	}
	if config.MaxSessions == 0 {
		config.MaxSessions = defaultProcessSessionCount
	}
	if config.MaxSessions < 1 {
		return nil, errors.New("tools: process-session maximum must be positive")
	}
	if config.MaxOutput == 0 {
		config.MaxOutput = defaultProcessSessionOutputBytes
	}
	if config.MaxOutput < 1 {
		return nil, errors.New("tools: process-session output limit must be positive")
	}
	ctx, cancel := context.WithCancel(ownerCtx)
	return &LocalProcessSessionManager{
		ctx:       ctx,
		cancel:    cancel,
		config:    config,
		sessions:  make(map[string]*managedProcessSession),
		delivered: make(map[string]ProcessSessionDeliveryBinding),
		closeDone: make(chan struct{}),
	}, nil
}

// Start validates and starts one command in a dedicated owned process group.
func (m *LocalProcessSessionManager) Start(ctx context.Context, command string) (ProcessSessionSnapshot, error) {
	if m == nil {
		return ProcessSessionSnapshot{}, errors.New("tools: process-session manager is required")
	}
	if ctx == nil {
		return ProcessSessionSnapshot{}, errors.New("tools: process-session call context is required")
	}
	if !ownedCommandPlatformSupported() {
		return ProcessSessionSnapshot{}, ErrProcessSessionsUnsupported
	}
	if strings.TrimSpace(command) == "" {
		return ProcessSessionSnapshot{}, errors.New("tools: process-session command is empty")
	}
	scope, err := m.resolveScope(ctx)
	if err != nil {
		return ProcessSessionSnapshot{}, err
	}
	if m.config.Sandbox == nil {
		return ProcessSessionSnapshot{}, ErrProcessSessionSandboxRequired
	}
	workdir := m.config.Workdir
	if absolute, absErr := filepath.Abs(workdir); absErr == nil {
		workdir = absolute
	}
	if err := m.config.Sandbox.Validate(ctx, command, workdir); err != nil {
		return ProcessSessionSnapshot{}, err
	}
	resourceSettings, err := effectiveBashResourceSettings(m.config.ResourceLimits, m.config.Sandbox)
	if err != nil {
		return ProcessSessionSnapshot{}, fmt.Errorf("tools: prepare process-session resource limits: %w", err)
	}
	action, hasAction := processSessionActionFromContext(ctx)
	if hasAction && (action.SessionID == "" || action.WorkspaceID != scope.WorkspaceID || action.RunID != scope.RunID || action.ActionID == "" || action.OwnerGeneration == "") {
		return ProcessSessionSnapshot{}, ErrProcessSessionScopeDenied
	}
	if err := ctx.Err(); err != nil {
		return ProcessSessionSnapshot{}, err
	}
	if err := m.ctx.Err(); err != nil {
		return ProcessSessionSnapshot{}, errors.Join(ErrProcessSessionManagerClosed, err)
	}

	id, err := opaqueProcessSessionID()
	if err != nil {
		return ProcessSessionSnapshot{}, fmt.Errorf("tools: create process-session ID: %w", err)
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ProcessSessionSnapshot{}, ErrProcessSessionManagerClosed
	}
	m.trimCompletedLocked()
	if m.activeSessionCountLocked() >= m.config.MaxSessions {
		m.mu.Unlock()
		return ProcessSessionSnapshot{}, ErrProcessSessionLimit
	}
	sessionCtx, sessionCancel := context.WithCancel(m.ctx)
	ownerGeneration := ""
	if hasAction {
		ownerGeneration = action.OwnerGeneration
	}
	session := &managedProcessSession{
		manager:               m,
		id:                    id,
		originRunID:           scope.RunID,
		action:                action,
		ownerGeneration:       ownerGeneration,
		ctx:                   sessionCtx,
		cancel:                sessionCancel,
		done:                  make(chan struct{}),
		leaseObserver:         processSessionLeaseObserverFromContext(ctx),
		leaseClosed:           make(chan struct{}),
		leaseClosureAttempted: make(chan struct{}),
		output:                newProcessSessionBuffer(m.config.MaxOutput),
		status:                ProcessSessionStarting,
		startedAt:             time.Now(),
	}
	m.sessions[id] = session
	m.wg.Add(1)
	m.mu.Unlock()

	cmd := exec.Command("bash", "-c", command)
	cmd.Dir = m.config.Workdir
	cmd.Env = filteredEnv(nil)
	startCtx, cancelStart := context.WithCancel(ctx)
	stopOwnerCancel := context.AfterFunc(m.ctx, cancelStart)
	startCtx = WithProcessExecutionIdentityObserver(startCtx, func(_ context.Context, identity ProcessExecutionIdentity) error {
		if identity.OwnerPID != os.Getpid() || identity.LeaderPID <= 0 || identity.ProcessGroupID != identity.LeaderPID ||
			identity.HostGeneration == "" || identity.ProcessStartToken == "" {
			return errors.New("process-session start returned an incomplete owned process identity")
		}
		session.mu.Lock()
		copy := identity
		session.identity = &copy
		session.processGroupID = identity.ProcessGroupID
		session.mu.Unlock()
		if observer := processSessionLeaseObserverFromContext(startCtx); observer != nil {
			if err := observer(startCtx, session.leaseSnapshot(ProcessSessionLeaseGated)); err != nil {
				return fmt.Errorf("persist gated process-session lease: %w", err)
			}
		}
		return nil
	})
	managed, err := startManagedLocalBashCommand(startCtx, sessionCancel, cmd, resourceSettings)
	stopOwnerCancel()
	cancelStart()
	if err != nil {
		sessionCancel()
		m.mu.Lock()
		delete(m.sessions, id)
		m.mu.Unlock()
		m.wg.Done()
		return ProcessSessionSnapshot{}, fmt.Errorf("tools: start process session: %w", err)
	}
	session.mu.Lock()
	if session.status == ProcessSessionStarting {
		session.status = ProcessSessionRunning
	}
	session.mu.Unlock()
	collectorReady := make(chan struct{})
	go m.collectSession(session, managed, cmd, collectorReady)
	if observer := processSessionLeaseObserverFromContext(ctx); observer != nil {
		if err := observer(ctx, session.leaseSnapshot(ProcessSessionLeaseActive)); err != nil {
			session.requestStop()
			close(collectorReady)
			<-session.done
			return ProcessSessionSnapshot{}, errors.Join(fmt.Errorf("persist active process-session lease: %w", err), session.completionErr())
		}
	}
	close(collectorReady)
	if err := ctx.Err(); err != nil {
		session.requestStop()
		<-session.done
		if cleanupErr := session.completionErr(); cleanupErr != nil {
			return ProcessSessionSnapshot{}, errors.Join(err, ErrProcessSessionCompletionUnknown, cleanupErr)
		}
		return ProcessSessionSnapshot{}, err
	}
	return session.snapshot(), nil
}

// Read returns at most maxBytes of retained combined output beginning at
// offset. Bytes before the retained window are reported as truncated.
func (m *LocalProcessSessionManager) Read(ctx context.Context, id string, offset int64, maxBytes int) (ProcessSessionOutput, error) {
	session, err := m.lookup(ctx, id)
	if err != nil {
		return ProcessSessionOutput{}, err
	}
	if offset < 0 {
		return ProcessSessionOutput{}, errors.New("tools: process-session output offset must be non-negative")
	}
	if maxBytes < 1 || maxBytes > maxProcessSessionReadBytes {
		return ProcessSessionOutput{}, fmt.Errorf("tools: process-session read limit must be between 1 and %d bytes", maxProcessSessionReadBytes)
	}
	output, start, total, wasTruncated, next, err := session.output.read(offset, maxBytes)
	if err != nil {
		return ProcessSessionOutput{}, err
	}
	snapshot := session.snapshot()
	return ProcessSessionOutput{
		SessionID:         session.id,
		Status:            snapshot.Status,
		Output:            output,
		Offset:            maxInt64(offset, start),
		NextOffset:        next,
		OutputStartOffset: start,
		TotalBytes:        total,
		Truncated:         wasTruncated || snapshot.Truncated,
	}, nil
}

// Status returns the current public state of one owned session.
func (m *LocalProcessSessionManager) Status(ctx context.Context, id string) (ProcessSessionSnapshot, error) {
	session, err := m.lookup(ctx, id)
	if err != nil {
		return ProcessSessionSnapshot{}, err
	}
	return session.snapshot(), nil
}

// Stop cancels one session and waits for its process group and readers to join.
func (m *LocalProcessSessionManager) Stop(ctx context.Context, id string) (ProcessSessionSnapshot, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	session, err := m.lookup(ctx, id)
	if err != nil {
		return ProcessSessionSnapshot{}, err
	}
	session.requestStop()
	select {
	case <-session.done:
		if session.completionErr() != nil {
			return session.snapshot(), errors.Join(ErrProcessSessionCompletionUnknown, session.completionErr())
		}
		return session.snapshot(), nil
	case <-ctx.Done():
		return session.snapshot(), ctx.Err()
	}
}

// OwnerID returns the opaque invocation owner ID bound to this manager.
func (m *LocalProcessSessionManager) OwnerID() string { return m.config.OwnerID }

// WorkspaceID returns the opaque workspace ID bound to this manager.
func (m *LocalProcessSessionManager) WorkspaceID() string { return m.config.WorkspaceID }

// AdmissionCapability returns an unforgeable runtime capability for ordinary
// work from the same original run and owner generation while this session is
// live. No persisted snapshot can recreate this manager pointer.
func (m *LocalProcessSessionManager) AdmissionCapability(sessionID, runID, ownerGeneration string) (ProcessSessionAdmissionCapability, bool) {
	if m == nil || sessionID == "" || runID == "" || ownerGeneration == "" {
		return ProcessSessionAdmissionCapability{}, false
	}
	m.mu.Lock()
	session := m.sessions[sessionID]
	closed := m.closed || m.ctx.Err() != nil
	m.mu.Unlock()
	if closed || session == nil {
		return ProcessSessionAdmissionCapability{}, false
	}
	session.mu.Lock()
	valid := session.originRunID == runID && session.ownerGeneration == ownerGeneration
	session.mu.Unlock()
	if !valid {
		return ProcessSessionAdmissionCapability{}, false
	}
	active, err := session.active()
	if err != nil || !active {
		return ProcessSessionAdmissionCapability{}, false
	}
	return ProcessSessionAdmissionCapability{
		manager: m, sessionID: sessionID, runID: runID,
		workspaceID: m.config.WorkspaceID, ownerGeneration: ownerGeneration,
	}, true
}

func (m *LocalProcessSessionManager) validAdmissionCapability(capability ProcessSessionAdmissionCapability) bool {
	if capability.manager != m {
		return false
	}
	if capability.sessionID == "" {
		m.mu.Lock()
		if m.closed || m.ctx.Err() != nil {
			m.mu.Unlock()
			return false
		}
		activeSessions := make([]*managedProcessSession, 0, len(m.sessions))
		for _, session := range m.sessions {
			activeSessions = append(activeSessions, session)
		}
		m.mu.Unlock()
		for _, session := range activeSessions {
			session.mu.Lock()
			matches := session.originRunID == capability.runID && session.ownerGeneration == capability.ownerGeneration &&
				session.cleanupErr == nil && session.leaseObserverErr == nil
			session.mu.Unlock()
			if matches && !session.leaseIsClosed() {
				return true
			}
		}
		return false
	}
	m.mu.Lock()
	if m.closed || m.ctx.Err() != nil {
		m.mu.Unlock()
		return false
	}
	session := m.sessions[capability.sessionID]
	m.mu.Unlock()
	if session == nil {
		return false
	}
	session.mu.Lock()
	matches := session.originRunID == capability.runID && session.ownerGeneration == capability.ownerGeneration &&
		session.cleanupErr == nil && session.leaseObserverErr == nil
	session.mu.Unlock()
	return matches && !session.leaseIsClosed()
}

// ownsAdmissionLease rejects durable lease facts that do not describe the
// exact process session still held by this manager. The persisted record is
// useful for locating a lease, but never substitutes for matching the live
// session's immutable action and captured process identity.
func (m *LocalProcessSessionManager) ownsAdmissionLease(capability ProcessSessionAdmissionCapability, lease ProcessSessionLeaseSnapshot) bool {
	if m == nil || capability.manager != m || lease.OwnerID != m.config.OwnerID ||
		lease.WorkspaceID != m.config.WorkspaceID || lease.RunID != capability.runID ||
		lease.OwnerGeneration != capability.ownerGeneration || lease.Identity == nil {
		return false
	}
	m.mu.Lock()
	if m.closed || m.ctx.Err() != nil {
		m.mu.Unlock()
		return false
	}
	session := m.sessions[lease.SessionID]
	m.mu.Unlock()
	if session == nil {
		return false
	}
	session.mu.Lock()
	matches := session.id == lease.SessionID && session.originRunID == lease.RunID &&
		session.ownerGeneration == lease.OwnerGeneration && session.action == lease.Action &&
		session.status == ProcessSessionRunning && lease.Status == ProcessSessionRunning &&
		session.identity != nil && *session.identity == *lease.Identity &&
		session.processGroupID == lease.Identity.ProcessGroupID
	session.mu.Unlock()
	if !matches {
		return false
	}
	active, err := session.active()
	return err == nil && active
}

func (m *LocalProcessSessionManager) ownsAdmissionLeaseForAdmission(capability ProcessSessionAdmissionCapability, lease ProcessSessionLeaseSnapshot) bool {
	if m == nil || capability.manager != m || lease.OwnerID != m.config.OwnerID ||
		lease.WorkspaceID != m.config.WorkspaceID || lease.RunID != capability.runID ||
		lease.OwnerGeneration != capability.ownerGeneration || lease.Identity == nil {
		return false
	}
	m.mu.Lock()
	if m.closed || m.ctx.Err() != nil {
		m.mu.Unlock()
		return false
	}
	session := m.sessions[lease.SessionID]
	m.mu.Unlock()
	if session == nil {
		return false
	}
	session.mu.Lock()
	matches := session.id == lease.SessionID && session.originRunID == lease.RunID &&
		session.ownerGeneration == lease.OwnerGeneration && session.action == lease.Action &&
		session.leaseObserverErr == nil && session.cleanupErr == nil &&
		session.identity != nil && *session.identity == *lease.Identity &&
		session.processGroupID == lease.Identity.ProcessGroupID
	joined := session.groupAbsent && session.leaderWaited && session.readersJoined
	status := session.status
	session.mu.Unlock()
	if !matches || session.leaseIsClosed() {
		return false
	}
	if status == ProcessSessionExited || status == ProcessSessionStopped {
		return joined
	}
	return status == ProcessSessionRunning || status == ProcessSessionStopping
}

func (m *LocalProcessSessionManager) leaseLifecycle(lease ProcessSessionLeaseSnapshot) (bool, bool, error) {
	if m == nil || lease.SessionID == "" || lease.Identity == nil || lease.OwnerID != m.config.OwnerID ||
		lease.WorkspaceID != m.config.WorkspaceID || lease.RunID == "" || lease.OwnerGeneration == "" {
		return false, false, ErrProcessSessionCompletionUnknown
	}
	m.mu.Lock()
	closed := m.closed || m.ctx.Err() != nil
	session := m.sessions[lease.SessionID]
	m.mu.Unlock()
	if closed || session == nil {
		return false, false, ErrProcessSessionCompletionUnknown
	}
	session.mu.Lock()
	matches := session.originRunID == lease.RunID && session.ownerGeneration == lease.OwnerGeneration &&
		session.action == lease.Action && session.identity != nil && *session.identity == *lease.Identity &&
		session.processGroupID == lease.Identity.ProcessGroupID && session.cleanupErr == nil && session.leaseObserverErr == nil
	status := session.status
	joined := session.groupAbsent && session.leaderWaited && session.readersJoined
	session.mu.Unlock()
	if !matches {
		return false, false, ErrProcessSessionCompletionUnknown
	}
	if session.leaseIsClosed() {
		return false, true, nil
	}
	if status == ProcessSessionExited || status == ProcessSessionStopped {
		if !joined {
			return false, false, ErrProcessSessionCompletionUnknown
		}
		return false, true, nil
	}
	if status == ProcessSessionStopping {
		return false, true, nil
	}
	if status == ProcessSessionRunning {
		active, err := session.active()
		if err != nil {
			return false, false, err
		}
		if active {
			return true, false, nil
		}
		return false, true, nil
	}
	return false, false, ErrProcessSessionCompletionUnknown
}

func (m *LocalProcessSessionManager) withStartLeasePublication(sessionID string, callback func(ProcessSessionLeaseSnapshot) error) error {
	if m == nil || sessionID == "" || callback == nil {
		return ErrProcessSessionNotFound
	}
	m.mu.Lock()
	closed := m.closed || m.ctx.Err() != nil
	session := m.sessions[sessionID]
	m.mu.Unlock()
	if closed || session == nil {
		return ErrProcessSessionManagerClosed
	}
	session.leasePublicationMu.Lock()
	defer session.leasePublicationMu.Unlock()
	snapshot, err := session.currentLeaseSnapshot()
	if err != nil {
		return err
	}
	if err := callback(snapshot); err != nil {
		return err
	}
	session.mu.Lock()
	session.startActionPublished = true
	session.mu.Unlock()
	return nil
}

func (m *LocalProcessSessionManager) startActionPublished(sessionID string) bool {
	if m == nil || sessionID == "" {
		return false
	}
	m.mu.Lock()
	session := m.sessions[sessionID]
	m.mu.Unlock()
	if session == nil {
		return false
	}
	session.mu.Lock()
	published := session.startActionPublished
	session.mu.Unlock()
	return published
}

// ManagerAdmissionCapability admits actions from this same live manager/run/
// owner generation, covering every session currently owned by that manager.
func (m *LocalProcessSessionManager) ManagerAdmissionCapability(runID, ownerGeneration string) (ProcessSessionAdmissionCapability, bool) {
	if m == nil || runID == "" || ownerGeneration == "" {
		return ProcessSessionAdmissionCapability{}, false
	}
	capability := ProcessSessionAdmissionCapability{
		manager: m, runID: runID, workspaceID: m.config.WorkspaceID, ownerGeneration: ownerGeneration,
	}
	if !capability.Allows(runID, m.config.WorkspaceID, ownerGeneration) {
		return ProcessSessionAdmissionCapability{}, false
	}
	return capability, true
}

// RequireDeliveryConfirmation reports whether a durable terminal commit must
// precede invocation-host handoff.
func (m *LocalProcessSessionManager) RequireDeliveryConfirmation() bool {
	return m != nil && m.config.RequireDeliveryConfirmation
}

// ConfirmDelivery binds a committed terminal to the current exact live lease
// set and returns a nonserializable manager capability for the host.
func (m *LocalProcessSessionManager) ConfirmDelivery(binding ProcessSessionDeliveryBinding) (ProcessSessionDelivery, error) {
	if m == nil || binding.OwnerID != m.config.OwnerID || binding.WorkspaceID != m.config.WorkspaceID || binding.RunID == "" || len(binding.SessionIDs) == 0 {
		return ProcessSessionDelivery{}, errors.New("tools: process-session delivery binding is invalid")
	}
	current, active, err := m.Handoff(binding.RunID)
	if err != nil {
		return ProcessSessionDelivery{}, err
	}
	if !active || !equalProcessSessionIDs(binding.SessionIDs, current.SessionIDs) {
		return ProcessSessionDelivery{}, errors.New("tools: process-session delivery does not match the live manager sessions")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.ctx.Err() != nil {
		return ProcessSessionDelivery{}, ErrProcessSessionManagerClosed
	}
	m.delivered[binding.RunID] = ProcessSessionDeliveryBinding{
		OwnerID: binding.OwnerID, RunID: binding.RunID, WorkspaceID: binding.WorkspaceID,
		SessionIDs: append([]string(nil), binding.SessionIDs...),
	}
	return ProcessSessionDelivery{Binding: cloneProcessSessionDeliveryBinding(binding), manager: m}, nil
}

// ValidateDelivery checks the private manager capability and its current
// active session set before stdout handoff.
func (m *LocalProcessSessionManager) ValidateDelivery(delivery ProcessSessionDelivery, runID string) error {
	if m == nil || delivery.manager != m || delivery.Binding.RunID != runID {
		return errors.New("tools: process-session delivery capability does not belong to this manager")
	}
	m.mu.Lock()
	stored, ok := m.delivered[runID]
	m.mu.Unlock()
	if !ok || !equalProcessSessionDeliveryBinding(stored, delivery.Binding) {
		return errors.New("tools: process-session delivery has not been confirmed")
	}
	current, active, err := m.Handoff(runID)
	if err != nil {
		return err
	}
	if !active || !equalProcessSessionIDs(current.SessionIDs, delivery.Binding.SessionIDs) {
		return errors.New("tools: process-session delivery no longer matches live sessions")
	}
	return nil
}

func cloneProcessSessionDeliveryBinding(binding ProcessSessionDeliveryBinding) ProcessSessionDeliveryBinding {
	binding.SessionIDs = append([]string(nil), binding.SessionIDs...)
	return binding
}

func equalProcessSessionDeliveryBinding(left, right ProcessSessionDeliveryBinding) bool {
	return left.OwnerID == right.OwnerID && left.RunID == right.RunID && left.WorkspaceID == right.WorkspaceID && equalProcessSessionIDs(left.SessionIDs, right.SessionIDs)
}

func (m *LocalProcessSessionManager) leaseSnapshot(sessionID string, phase ProcessSessionLeasePhase) ProcessSessionLeaseSnapshot {
	if m == nil {
		return ProcessSessionLeaseSnapshot{}
	}
	m.mu.Lock()
	session := m.sessions[sessionID]
	m.mu.Unlock()
	if session == nil {
		return ProcessSessionLeaseSnapshot{}
	}
	return session.leaseSnapshot(phase)
}

func (s *managedProcessSession) currentLeaseSnapshot() (ProcessSessionLeaseSnapshot, error) {
	if s == nil || s.manager == nil {
		return ProcessSessionLeaseSnapshot{}, ErrProcessSessionNotFound
	}
	s.mu.Lock()
	phase := ProcessSessionLeaseUnknown
	switch s.status {
	case ProcessSessionRunning:
		phase = ProcessSessionLeaseActive
	case ProcessSessionExited, ProcessSessionStopped:
		if s.cleanupErr == nil && s.groupAbsent && s.leaderWaited && s.readersJoined {
			phase = ProcessSessionLeaseStopped
		}
	}
	if s.cleanupErr != nil || phase == ProcessSessionLeaseUnknown {
		s.mu.Unlock()
		return ProcessSessionLeaseSnapshot{}, ErrProcessSessionCompletionUnknown
	}
	snapshot := s.leaseSnapshotLocked(phase)
	s.mu.Unlock()
	_, totalBytes, truncated := s.output.snapshot()
	snapshot.OutputBytes = totalBytes
	snapshot.OutputTruncated = truncated
	return snapshot, nil
}

func (s *managedProcessSession) leaseSnapshot(phase ProcessSessionLeasePhase) ProcessSessionLeaseSnapshot {
	if s == nil || s.manager == nil {
		return ProcessSessionLeaseSnapshot{}
	}
	s.mu.Lock()
	snapshot := s.leaseSnapshotLocked(phase)
	s.mu.Unlock()
	_, totalBytes, truncated := s.output.snapshot()
	snapshot.OutputBytes = totalBytes
	snapshot.OutputTruncated = truncated
	return snapshot
}

func (s *managedProcessSession) leaseSnapshotLocked(phase ProcessSessionLeasePhase) ProcessSessionLeaseSnapshot {
	identity := s.identity
	if identity != nil {
		copy := *identity
		identity = &copy
	}
	snapshot := ProcessSessionLeaseSnapshot{
		SessionID:       s.id,
		OwnerID:         s.manager.config.OwnerID,
		WorkspaceID:     s.manager.config.WorkspaceID,
		RunID:           s.originRunID,
		OwnerGeneration: s.ownerGeneration,
		Action:          s.action,
		Phase:           phase,
		Identity:        identity,
		Status:          s.status,
		GroupAbsent:     s.groupAbsent,
		LeaderWaited:    s.leaderWaited,
		ReadersJoined:   s.readersJoined,
	}
	return snapshot
}

func (s *managedProcessSession) leaseIsClosed() bool {
	if s == nil {
		return false
	}
	select {
	case <-s.leaseClosed:
		return true
	default:
		return false
	}
}

// SessionClosed returns a channel that closes only after physical cleanup and
// any configured durable lease-closure observer have completed.
func (m *LocalProcessSessionManager) SessionClosed(sessionID string) <-chan struct{} {
	if m == nil || sessionID == "" {
		return nil
	}
	m.mu.Lock()
	session := m.sessions[sessionID]
	m.mu.Unlock()
	if session == nil {
		return nil
	}
	return session.leaseClosed
}

// SessionClosureAttempted returns a channel closed after the manager's
// physical cleanup observer returns, whether it succeeds or fails.
func (m *LocalProcessSessionManager) SessionClosureAttempted(sessionID string) <-chan struct{} {
	if m == nil || sessionID == "" {
		return nil
	}
	m.mu.Lock()
	session := m.sessions[sessionID]
	m.mu.Unlock()
	if session == nil {
		return nil
	}
	return session.leaseClosureAttempted
}

// OwnerClosed returns the manager owner's completion channel. It closes only
// after Close has joined every session and attempted durable closure callbacks.
func (m *LocalProcessSessionManager) OwnerClosed() <-chan struct{} {
	if m == nil {
		return nil
	}
	return m.closeDone
}

// MarkLeaseClosed records that a caller durably resolved this exact lease.
func (m *LocalProcessSessionManager) MarkLeaseClosed(sessionID string) bool {
	if m == nil || sessionID == "" {
		return false
	}
	m.mu.Lock()
	session := m.sessions[sessionID]
	m.mu.Unlock()
	if session == nil {
		return false
	}
	session.mu.Lock()
	closed := session.groupAbsent && session.leaderWaited && session.readersJoined
	session.mu.Unlock()
	if !closed {
		return false
	}
	session.leaseClosedOnce.Do(func() { close(session.leaseClosed) })
	return true
}

// Handoff describes active sessions owned by this manager. A separate host
// must accept this value before it is delivered to the caller.
func (m *LocalProcessSessionManager) Handoff(runID string) (ProcessSessionHandoff, bool, error) {
	if m == nil {
		return ProcessSessionHandoff{}, false, errors.New("tools: process-session manager is required")
	}
	if runID == "" {
		return ProcessSessionHandoff{}, false, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.ctx.Err() != nil {
		return ProcessSessionHandoff{}, false, ErrProcessSessionManagerClosed
	}
	ids := make([]string, 0, len(m.sessions))
	for id, session := range m.sessions {
		active, err := session.active()
		if err != nil {
			return ProcessSessionHandoff{}, false, err
		}
		if session.originRunID == runID && active {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return ProcessSessionHandoff{}, false, nil
	}
	sort.Strings(ids)
	return ProcessSessionHandoff{
		Kind:        "process_session_handoff",
		Version:     1,
		Status:      "delivered-active",
		OwnerID:     m.config.OwnerID,
		RunID:       runID,
		WorkspaceID: m.config.WorkspaceID,
		SessionIDs:  ids,
	}, true, nil
}

// ValidateHandoff confirms that a proposed handoff still describes the exact
// active sessions owned by this manager.
func (m *LocalProcessSessionManager) ValidateHandoff(handoff ProcessSessionHandoff) error {
	current, active, err := m.Handoff(handoff.RunID)
	if err != nil {
		return err
	}
	if !active || handoff.Kind != current.Kind || handoff.Version != current.Version ||
		handoff.Status != current.Status || handoff.OwnerID != current.OwnerID ||
		handoff.RunID != current.RunID || handoff.WorkspaceID != current.WorkspaceID ||
		!equalProcessSessionIDs(handoff.SessionIDs, current.SessionIDs) {
		return errors.New("tools: process-session handoff does not match the active host-owned sessions")
	}
	return nil
}

// Close cancels every session and joins every leader, process group, and local
// output reader. A timed-out caller does not detach the manager's join owner.
func (m *LocalProcessSessionManager) Close(ctx context.Context) error {
	if m == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.closeOnce.Do(func() {
		m.mu.Lock()
		m.closed = true
		m.mu.Unlock()
		m.cancel()
		go func() {
			m.wg.Wait()
			m.mu.Lock()
			sessions := make([]*managedProcessSession, 0, len(m.sessions))
			for _, session := range m.sessions {
				sessions = append(sessions, session)
			}
			m.mu.Unlock()
			for _, session := range sessions {
				session.leasePublicationMu.Lock()
				if session.completionErr() != nil {
					m.mu.Lock()
					m.closeErr = errors.Join(m.closeErr, ErrProcessSessionCompletionUnknown)
					m.mu.Unlock()
					session.leasePublicationMu.Unlock()
					continue
				}
				select {
				case <-session.leaseClosed:
					session.leasePublicationMu.Unlock()
					continue
				default:
				}
				phase := ProcessSessionLeaseUnknown
				session.mu.Lock()
				if session.groupAbsent && session.leaderWaited && session.readersJoined {
					phase = ProcessSessionLeaseStopped
				}
				session.mu.Unlock()
				if observer := m.config.ClosureObserver; observer != nil {
					cleanupCtx, cancel := m.closureObserverContext()
					observerErr := observer(cleanupCtx, session.leaseSnapshot(phase))
					cancel()
					if observerErr != nil {
						session.mu.Lock()
						session.leaseObserverErr = observerErr
						session.mu.Unlock()
						m.mu.Lock()
						m.closeErr = errors.Join(m.closeErr, fmt.Errorf("persist process-session host closure: %w", observerErr))
						m.mu.Unlock()
						session.leasePublicationMu.Unlock()
						continue
					}
				}
				session.mu.Lock()
				session.leaseObserverErr = nil
				session.mu.Unlock()
				m.MarkLeaseClosed(session.id)
				session.leasePublicationMu.Unlock()
			}
			close(m.closeDone)
		}()
	})
	select {
	case <-m.closeDone:
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *LocalProcessSessionManager) closureObserverContext() (context.Context, context.CancelFunc) {
	ownerCtx := context.Background()
	if m != nil && m.ctx != nil {
		ownerCtx = context.WithoutCancel(m.ctx)
	}
	return context.WithTimeout(ownerCtx, processSessionClosureObserverTimeout)
}

func (m *LocalProcessSessionManager) lookup(ctx context.Context, id string) (*managedProcessSession, error) {
	if m == nil {
		return nil, errors.New("tools: process-session manager is required")
	}
	if ctx == nil {
		return nil, errors.New("tools: process-session call context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	scope, err := m.resolveScope(ctx)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	session, ok := m.sessions[id]
	if !ok {
		return nil, ErrProcessSessionNotFound
	}
	session.mu.Lock()
	sameRun := session.originRunID == scope.RunID
	session.mu.Unlock()
	if !sameRun {
		return nil, ErrProcessSessionScopeDenied
	}
	return session, nil
}

func (m *LocalProcessSessionManager) resolveScope(ctx context.Context) (ProcessSessionScope, error) {
	if m.config.ScopeResolver == nil {
		return ProcessSessionScope{}, errors.New("tools: process-session scope resolver is required")
	}
	scope, err := m.config.ScopeResolver(ctx)
	if err != nil {
		return ProcessSessionScope{}, fmt.Errorf("tools: resolve process-session scope: %w", err)
	}
	if scope.OwnerID != m.config.OwnerID || scope.WorkspaceID != m.config.WorkspaceID || scope.RunID == "" {
		return ProcessSessionScope{}, ErrProcessSessionScopeDenied
	}
	return scope, nil
}

func (m *LocalProcessSessionManager) collectSession(session *managedProcessSession, managed *managedLocalCommand, cmd *exec.Cmd, ready <-chan struct{}) {
	defer m.wg.Done()
	defer session.cancel()
	<-ready
	lifecycle := managed.collect(session.ctx, func(reader io.Reader, stream string) (bool, error) {
		_, err := io.Copy(session.output.writer(), reader)
		return err == nil, err
	})
	var exitCode *int
	if cmd.ProcessState != nil {
		code := cmd.ProcessState.ExitCode()
		exitCode = &code
	}
	session.finish(lifecycle, exitCode)
	session.leasePublicationMu.Lock()
	phase := ProcessSessionLeaseUnknown
	session.mu.Lock()
	if session.groupAbsent && session.leaderWaited && session.readersJoined {
		phase = ProcessSessionLeaseStopped
	}
	observer := session.leaseObserver
	startActionPublished := session.startActionPublished
	session.mu.Unlock()
	if observer == nil {
		m.MarkLeaseClosed(session.id)
	} else {
		cleanupCtx, cancel := m.closureObserverContext()
		err := observer(cleanupCtx, session.leaseSnapshot(phase))
		cancel()
		if err != nil {
			session.mu.Lock()
			session.leaseObserverErr = err
			session.mu.Unlock()
		} else if phase == ProcessSessionLeaseStopped && startActionPublished {
			m.MarkLeaseClosed(session.id)
		}
	}
	session.leaseClosureAttemptedOnce.Do(func() { close(session.leaseClosureAttempted) })
	session.leasePublicationMu.Unlock()
	m.mu.Lock()
	m.trimCompletedLocked()
	m.mu.Unlock()
}

func (m *LocalProcessSessionManager) activeSessionCountLocked() int {
	active := 0
	for _, session := range m.sessions {
		session.mu.Lock()
		if session.status == ProcessSessionStarting || session.status == ProcessSessionRunning || session.status == ProcessSessionStopping {
			active++
		}
		session.mu.Unlock()
	}
	return active
}

// trimCompletedLocked retains a small recent history while keeping completed
// sessions from consuming active capacity or growing without bound.
func (m *LocalProcessSessionManager) trimCompletedLocked() {
	completed := make([]*managedProcessSession, 0, len(m.sessions))
	for _, session := range m.sessions {
		session.mu.Lock()
		done := session.status == ProcessSessionExited || session.status == ProcessSessionStopped
		closed := false
		select {
		case <-session.leaseClosed:
			closed = true
		default:
		}
		session.mu.Unlock()
		if done && closed {
			completed = append(completed, session)
		}
	}
	if len(completed) <= processSessionHistoryCount {
		return
	}
	sort.Slice(completed, func(left, right int) bool {
		return completed[left].startedAt.Before(completed[right].startedAt)
	})
	for _, session := range completed[:len(completed)-processSessionHistoryCount] {
		delete(m.sessions, session.id)
	}
}

func (s *managedProcessSession) requestStop() {
	s.mu.Lock()
	if s.status == ProcessSessionRunning {
		s.status = ProcessSessionStopping
		s.stopRequested = true
	}
	s.mu.Unlock()
	s.cancel()
}

func (s *managedProcessSession) finish(lifecycle managedLocalResult, exitCode *int) {
	s.mu.Lock()
	s.exitCode = exitCode
	s.cleanupErr = errors.Join(lifecycle.cleanupErr, lifecycle.readerErr)
	if !lifecycle.containmentProven || lifecycle.cleanupErr != nil || lifecycle.readerErr != nil || lifecycle.forcedPipeClose {
		s.cleanupErr = errors.Join(s.cleanupErr, errProcessContainmentUnconfirmed)
	}
	s.groupAbsent = lifecycle.containmentProven
	s.leaderWaited = lifecycle.containmentProven
	s.readersJoined = lifecycle.readersJoined
	if s.stopRequested || lifecycle.interrupted {
		s.status = ProcessSessionStopped
	} else {
		s.status = ProcessSessionExited
	}
	s.mu.Unlock()
	close(s.done)
}

func (s *managedProcessSession) active() (bool, error) {
	s.mu.Lock()
	status := s.status
	pgid := s.processGroupID
	select {
	case <-s.done:
		s.mu.Unlock()
		return false, nil
	default:
	}
	s.mu.Unlock()
	if status != ProcessSessionRunning || pgid <= 0 {
		return false, nil
	}
	alive, err := commandProcessGroupAlive(pgid)
	if err != nil {
		return false, fmt.Errorf("tools: inspect process-session group: %w", err)
	}
	return alive, nil
}

func (s *managedProcessSession) snapshot() ProcessSessionSnapshot {
	s.mu.Lock()
	status := s.status
	exitCode := s.exitCode
	s.mu.Unlock()
	outputStart, total, truncated := s.output.snapshot()
	var code *int
	if exitCode != nil {
		copy := *exitCode
		code = &copy
	}
	return ProcessSessionSnapshot{
		SessionID:         s.id,
		Status:            status,
		ExitCode:          code,
		OutputStartOffset: outputStart,
		TotalBytes:        total,
		Truncated:         truncated,
	}
}

func (s *managedProcessSession) completionErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cleanupErr
}

func opaqueProcessSessionID() (string, error) {
	var data [24]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data[:]), nil
}

func equalProcessSessionIDs(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}
