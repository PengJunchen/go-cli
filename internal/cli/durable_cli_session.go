package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/session"
)

var (
	errDurableSessionDirectoryRequired        = errors.New("cli: durable session directory is required")
	errDurableSessionPathNotDirectory         = errors.New("cli: durable session path must be a directory")
	errDurableSessionBranchRequiresPhysicalID = errors.New("cli: a durable branch requires an exact physical session ID")
)

// ErrDurableCLIUnsupported identifies a durable CLI operation outside the
// supported command boundary.
var ErrDurableCLIUnsupported = errors.New("cli: durable CLI feature is unsupported")

// UnsupportedDurableCLIError identifies a durable CLI operation that is
// intentionally outside the currently supported command boundary.
type UnsupportedDurableCLIError struct {
	Feature string
}

func durableHistoryMutationUnsupported(command string) error {
	switch command {
	case "clear", "compact", "retry":
		return &UnsupportedDurableCLIError{Feature: "durable /" + command}
	default:
		return nil
	}
}

func (err *UnsupportedDurableCLIError) Error() string {
	if err == nil || err.Feature == "" {
		return ErrDurableCLIUnsupported.Error()
	}
	verb := "is"
	if strings.HasSuffix(strings.ToLower(err.Feature), "sessions") {
		verb = "are"
	}
	return fmt.Sprintf("cli: %s %s unsupported", err.Feature, verb)
}

func (err *UnsupportedDurableCLIError) Unwrap() error {
	return ErrDurableCLIUnsupported
}

// durableCLISession owns the selected physical session for one prompt or REPL
// invocation.
type durableCLISession struct {
	manager        *durableSessionManager
	index          *session.WorkspaceQuarantineIndex
	workspaceID    string
	storeDirectory string
}

// newDurableCLISession resolves one explicit invocation root, initializes its
// workspace authority, and prepares either a new or exact physical session.
func newDurableCLISession(
	ctx context.Context,
	storeDirectory string,
	displayLabel string,
	resumePhysicalID string,
	resumeBranchID string,
) (*durableCLISession, error) {
	if storeDirectory == "" {
		return nil, errDurableSessionDirectoryRequired
	}
	if err := validateDurableSessionStorePath(storeDirectory); err != nil {
		return nil, err
	}
	if resumePhysicalID == "" && resumeBranchID != "" {
		return nil, errDurableSessionBranchRequiresPhysicalID
	}
	ctx = normalizeSessionContext(ctx)

	canonicalRoot, workspaceID, err := resolveWorkspaceIdentity(os.Getwd)
	if err != nil {
		return nil, fmt.Errorf("cli: resolve durable session workspace: %w", err)
	}
	index, err := session.NewWorkspaceQuarantineIndex()
	if err != nil {
		return nil, fmt.Errorf("cli: open durable session workspace authority: %w", err)
	}
	if err := index.EnsureWorkspace(ctx, workspaceID); err != nil {
		return nil, fmt.Errorf("cli: initialize durable session workspace authority: %w", err)
	}
	preparer := session.NewDurableJSONLSessionPreparer(storeDirectory, index)
	manager, err := newDurableSessionManagerForWorkspace(preparer, canonicalRoot, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("cli: create durable session manager: %w", err)
	}

	if resumePhysicalID == "" {
		err = manager.prepareNew(ctx, displayLabel)
	} else {
		err = manager.selectResume(ctx, resumePhysicalID, resumeBranchID)
	}
	if err != nil {
		closeErr := manager.requestClose(context.WithoutCancel(ctx))
		if closeErr != nil {
			err = errors.Join(err, fmt.Errorf("cli: close durable session after selection failure: %w", closeErr))
		}
		return nil, err
	}

	return &durableCLISession{
		manager: manager, index: index, workspaceID: workspaceID,
		storeDirectory: storeDirectory,
	}, nil
}

func validateDurableSessionStorePath(storeDirectory string) error {
	if strings.TrimSpace(storeDirectory) == "" {
		return errDurableSessionDirectoryRequired
	}
	if strings.EqualFold(filepath.Ext(filepath.Clean(storeDirectory)), ".jsonl") {
		return fmt.Errorf("%w: %q is a legacy JSONL file", errDurableSessionPathNotDirectory, storeDirectory)
	}
	info, err := os.Stat(storeDirectory)
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("%w: %q is a file", errDurableSessionPathNotDirectory, storeDirectory)
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return fmt.Errorf("cli: inspect durable session store path %q: %w", storeDirectory, err)
	}
	return nil
}

// snapshotSelected returns one defensive snapshot while holding a temporary
// run lease so a concurrent close request cannot close its prepared handle.
func (cliSession *durableCLISession) snapshotSelected() (session.PreparedSessionSnapshot, error) {
	if cliSession == nil || cliSession.manager == nil {
		return session.PreparedSessionSnapshot{}, ErrNoDurableSessionSelected
	}
	lease, err := cliSession.manager.acquireRunLease()
	if err != nil {
		return session.PreparedSessionSnapshot{}, fmt.Errorf("cli: borrow selected durable session for snapshot: %w", err)
	}
	defer lease.CloseRunCommit()

	snapshot, err := lease.PreparedSession().Snapshot()
	if err != nil {
		return session.PreparedSessionSnapshot{}, fmt.Errorf("cli: snapshot selected durable session: %w", err)
	}
	if snapshot.SessionID != lease.SessionID() || snapshot.ActiveBranchID != lease.BranchID() {
		return session.PreparedSessionSnapshot{}, fmt.Errorf("%w: snapshot identity or active branch changed", ErrDurableSessionUnavailable)
	}
	return snapshot.Clone(), nil
}

// bindRun leases the selected session and attaches its actual run-commit sink
// and physical identity to ctx. On success, the core run owner returns the
// lease through the sink's CloseRunCommit capability.
func (cliSession *durableCLISession) bindRun(ctx context.Context) (context.Context, *session.SessionRunCommitSink, error) {
	if cliSession == nil || cliSession.manager == nil {
		return nil, nil, ErrNoDurableSessionSelected
	}
	lease, err := cliSession.manager.acquireRunLease()
	if errors.Is(err, ErrDurableSessionUnavailable) {
		if reopenErr := cliSession.manager.reopenPoisoned(ctx); reopenErr != nil {
			return nil, nil, errors.Join(fmt.Errorf("cli: recover exact durable session before run: %w", reopenErr), err)
		}
		lease, err = cliSession.manager.acquireRunLease()
	}
	if err != nil {
		return nil, nil, fmt.Errorf("cli: acquire durable session run lease: %w", err)
	}
	return bindDurableSessionRun(ctx, lease, cliSession.index)
}

// bindDurableSessionRun gives the actual session adapter ownership of a
// successful lease. A failed adapter construction returns the borrow directly
// so the manager can close or replace its base handle.
func bindDurableSessionRun(
	ctx context.Context,
	lease *durableSessionRunLease,
	index *session.WorkspaceQuarantineIndex,
) (context.Context, *session.SessionRunCommitSink, error) {
	if lease == nil {
		return nil, nil, ErrNoDurableSessionSelected
	}
	sink, err := session.NewSessionRunCommitSink(
		lease.PreparedSession(), index, lease.WorkspaceID(), lease.CloseRunCommit,
	)
	if err != nil {
		lease.CloseRunCommit()
		return nil, nil, fmt.Errorf("cli: construct durable run-commit sink: %w", err)
	}
	binding := core.RunCommitBinding{
		SessionID: lease.SessionID(), WorkspaceID: lease.WorkspaceID(),
		CanonicalWorkspaceRoot: lease.CanonicalWorkspaceRoot(),
	}
	return core.WithRunCommit(ctx, sink, binding), sink, nil
}

// requestClose prevents new leases and closes the selected prepared handle
// only after every run owner has returned its lease. A pending result retains
// the manager's reference so the caller can retry the close request.
func (cliSession *durableCLISession) requestClose(ctx context.Context) error {
	if cliSession == nil || cliSession.manager == nil {
		return nil
	}
	return cliSession.manager.requestClose(ctx)
}
