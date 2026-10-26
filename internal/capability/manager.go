package capability

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
)

var (
	// ErrUnavailable means no complete generation can be acquired.
	ErrUnavailable = errors.New("capability: generation unavailable")
	// ErrStale means a more recent reload or revocation superseded a candidate.
	ErrStale = errors.New("capability: candidate superseded")
	// ErrRevoked rejects invocations and late results after revocation.
	ErrRevoked = errors.New("capability: invocation revoked; external outcome may be unknown")
	// ErrLeaseReleased rejects calls through a released parent lease.
	ErrLeaseReleased = errors.New("capability: run lease released")
	// ErrCleanupFailed prevents repeated generations accumulating failed owners.
	ErrCleanupFailed = errors.New("capability: retiring owner cleanup failed")
	// ErrRestartRequired identifies process-wide exclusive resources that cannot be double-opened.
	ErrRestartRequired = errors.New("capability: resource update requires restart")
)

type generation[T any] struct {
	snapshot                Snapshot
	value                   T
	scope                   *Scope
	refs                    int
	epoch                   uint64
	retired, cleanupStarted bool
	done                    chan struct{}
	err                     error
}

// Diagnostics reports bounded control-plane state without exposing payloads or secrets.
type Diagnostics struct {
	Current, Retiring      string
	Candidate              bool
	DesiredRevision, Epoch uint64
	Closed                 bool
	CleanupError           string
}

// Manager owns one current and at most one candidate or retiring generation.
// T is a composition-root-private, immutable payload, never an external mutable DTO.
type Manager[T any] struct {
	mu                sync.Mutex
	gate              chan struct{}
	current, retiring *generation[T]
	closed            bool
	revision, epoch   uint64
	buildDone         chan struct{}
	buildCancel       context.CancelFunc
	calls             map[*Invocation[T]]struct{}
	cleanupErr        error
	closing           []*generation[T]
}

// NewManager creates an empty manager without starting a background worker.
func NewManager[T any]() *Manager[T] {
	m := &Manager[T]{gate: make(chan struct{}, 1), calls: make(map[*Invocation[T]]struct{})}
	m.gate <- struct{}{}
	return m
}

// ReloadWithCommit adds a host-only atomic publication step after final
// cancellation/revision checks and immediately before switching current.
// commit must perform only bounded in-memory owner/registry publication: no
// third-party callbacks, I/O, waits, Acquire/manager reentry, or partial mutation
// when returning error. The caller must keep its registry/owner lock order fixed.
func (m *Manager[T]) ReloadWithCommit(ctx context.Context, build func(context.Context, *Scope) (Snapshot, T, error), commit func() error) error {
	return m.reload(ctx, build, commit)
}

// Reload prepares a complete generation outside the lock; newer intents supersede older builders.
func (m *Manager[T]) Reload(ctx context.Context, build func(context.Context, *Scope) (Snapshot, T, error)) error {
	return m.reload(ctx, build, nil)
}

func (m *Manager[T]) reload(ctx context.Context, build func(context.Context, *Scope) (Snapshot, T, error), commit func() error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if build == nil {
		return errors.New("capability: nil candidate builder")
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrUnavailable
	}
	m.revision++
	revision, epoch := m.revision, m.epoch
	m.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-m.gate:
	}
	defer func() { m.gate <- struct{}{} }()
	for {
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			return ErrUnavailable
		}
		if revision != m.revision || epoch != m.epoch {
			m.mu.Unlock()
			return ErrStale
		}
		if m.cleanupErr != nil {
			err := m.cleanupErr
			m.mu.Unlock()
			return errors.Join(ErrCleanupFailed, err)
		}
		old := m.retiring
		if old == nil {
			m.mu.Unlock()
			break
		}
		done := old.done
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-done:
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	scope := NewScope()
	buildCtx, cancel := context.WithCancel(ctx)
	buildDone := make(chan struct{})
	m.mu.Lock()
	if m.closed || revision != m.revision || epoch != m.epoch {
		m.mu.Unlock()
		cancel()
		return ErrStale
	}
	m.buildDone, m.buildCancel = buildDone, cancel
	m.mu.Unlock()
	defer func() {
		cancel()
		m.mu.Lock()
		m.buildCancel = nil
		m.buildDone = nil
		close(buildDone)
		m.mu.Unlock()
	}()
	snapshot, value, err := invokeBuilder(buildCtx, scope, build)
	if err == nil {
		err = buildCtx.Err()
	}
	m.mu.Lock()
	if err == nil {
		err = buildCtx.Err()
	}
	if err == nil && (m.closed || revision != m.revision || epoch != m.epoch) {
		err = ErrStale
	}
	if err == nil && snapshot.ID() == "" {
		err = errors.New("capability: candidate has no snapshot")
	}
	if err == nil && commit != nil {
		err = commit()
	}
	if err != nil {
		m.mu.Unlock()
		cleanupErr := scope.Close(context.Background())
		if cleanupErr != nil {
			m.mu.Lock()
			m.cleanupErr = errors.Join(m.cleanupErr, cleanupErr)
			m.mu.Unlock()
		}
		return errors.Join(err, cleanupErr)
	}
	old := m.current
	m.current = &generation[T]{snapshot: snapshot, value: value, scope: scope, epoch: epoch, done: make(chan struct{})}
	if old != nil {
		old.retired = true
		m.retiring = old
	}
	start := old != nil && old.refs == 0
	if start {
		old.cleanupStarted = true
	}
	m.mu.Unlock()
	slog.Debug("capability.generation.published", "generation", snapshot.ID(), "revision", revision)
	if start {
		m.cleanup(old)
	}
	return nil
}

func invokeBuilder[T any](ctx context.Context, scope *Scope, build func(context.Context, *Scope) (Snapshot, T, error)) (snapshot Snapshot, value T, err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("capability: candidate builder panicked")
		}
	}()
	return build(ctx, scope)
}

// Acquire pins the complete current generation until Release and all child calls finish.
func (m *Manager[T]) Acquire(ctx context.Context) (*Lease[T], error) {
	if ctx != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.current == nil {
		return nil, ErrUnavailable
	}
	m.current.refs++
	return &Lease[T]{manager: m, generation: m.current, epoch: m.current.epoch}, nil
}

// Diagnostics returns a detached view of the manager's resource state.
func (m *Manager[T]) Diagnostics() Diagnostics {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := Diagnostics{Candidate: m.buildDone != nil, DesiredRevision: m.revision, Epoch: m.epoch, Closed: m.closed}
	if m.current != nil {
		d.Current = m.current.snapshot.ID()
	}
	if m.retiring != nil {
		d.Retiring = m.retiring.snapshot.ID()
	}
	if m.cleanupErr != nil {
		d.CleanupError = m.cleanupErr.Error()
	}
	return d
}

// Revoke linearizes revocation, denies new calls from old leases, and cancels active calls.
func (m *Manager[T]) Revoke() uint64 {
	m.mu.Lock()
	m.epoch++
	epoch := m.epoch
	cancels := make([]context.CancelFunc, 0, len(m.calls))
	for call := range m.calls {
		cancels = append(cancels, call.cancel)
	}
	buildCancel := m.buildCancel
	m.mu.Unlock()
	if buildCancel != nil {
		buildCancel()
	}
	for _, cancel := range cancels {
		cancel()
	}
	return epoch
}

// Close stops admission, cancels candidate/calls, and waits for actual cleanup.
// A deadline bounds waiting; owned work remains tracked until it really finishes.
func (m *Manager[T]) Close(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.Lock()
	m.closed = true
	m.epoch++
	if m.buildCancel != nil {
		m.buildCancel()
	}
	buildDone := m.buildDone
	owners := append([]*generation[T](nil), m.closing...)
	if m.retiring != nil {
		exists := false
		for _, owner := range owners {
			if owner == m.retiring {
				exists = true
				break
			}
		}
		if !exists {
			owners = append(owners, m.retiring)
		}
	}
	if m.current != nil {
		owners = append(owners, m.current)
		m.current.retired = true
		m.current = nil
	}
	m.closing = owners
	starts := []*generation[T]{}
	for _, g := range owners {
		if g.refs == 0 && !g.cleanupStarted {
			g.cleanupStarted = true
			starts = append(starts, g)
		}
	}
	cancels := make([]context.CancelFunc, 0, len(m.calls))
	for call := range m.calls {
		cancels = append(cancels, call.cancel)
	}
	m.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	for _, g := range starts {
		m.cleanup(g)
	}
	if buildDone != nil {
		select {
		case <-buildDone:
		case <-ctx.Done():
			return fmt.Errorf("capability: candidate cleanup pending: %w", ctx.Err())
		}
	}
	var errs []error
	for _, g := range owners {
		select {
		case <-g.done:
			if g.err != nil {
				errs = append(errs, g.err)
			}
		case <-ctx.Done():
			return errors.Join(errors.Join(errs...), fmt.Errorf("capability: generation %s still draining: %w", g.snapshot.ID(), ctx.Err()))
		}
	}
	m.mu.Lock()
	cleanupErr := m.cleanupErr
	m.mu.Unlock()
	return errors.Join(errors.Join(errs...), cleanupErr)
}

func (m *Manager[T]) cleanup(g *generation[T]) {
	go func() {
		err := g.scope.Close(context.Background())
		if errors.Is(err, ErrCloseInProgress) {
			err = g.scope.WaitClosed(context.Background())
		}
		m.mu.Lock()
		g.err = err
		if err != nil {
			m.cleanupErr = errors.Join(m.cleanupErr, err)
		}
		if m.retiring == g {
			m.retiring = nil
		}
		close(g.done)
		m.mu.Unlock()
	}()
}

func (m *Manager[T]) releaseLocked(g *generation[T]) bool {
	g.refs--
	if g.retired && g.refs == 0 && !g.cleanupStarted {
		g.cleanupStarted = true
		return true
	}
	return false
}

// Lease pins a private runtime payload; its Value must never be mutated after publication.
type Lease[T any] struct {
	manager    *Manager[T]
	generation *generation[T]
	epoch      uint64
	released   bool
}

// Snapshot returns immutable metadata.
func (l *Lease[T]) Snapshot() Snapshot { return l.generation.snapshot }

// Value returns the composition root's frozen payload, not an external SDK value.
func (l *Lease[T]) Value() T { return l.generation.value }

// Release drops the parent reference once; child invocations retain their own references.
func (l *Lease[T]) Release() {
	m := l.manager
	m.mu.Lock()
	if l.released {
		m.mu.Unlock()
		return
	}
	l.released = true
	start := m.releaseLocked(l.generation)
	m.mu.Unlock()
	if start {
		m.cleanup(l.generation)
	}
}

// BeginCall authorizes an independent invocation before entering a callback.
func (l *Lease[T]) BeginCall(ctx context.Context, capabilityID string) (*Invocation[T], error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m := l.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	if l.released {
		return nil, ErrLeaseReleased
	}
	if m.closed || l.epoch != m.epoch {
		return nil, ErrRevoked
	}
	callCtx, cancel := invocationLifetime(ctx)
	call := &Invocation[T]{lease: l, ctx: callCtx, cancel: cancel, capability: capabilityID}
	l.generation.refs++
	m.calls[call] = struct{}{}
	return call, nil
}

// Invocation retains resources through callback completion and checks revocation again.
type Invocation[T any] struct {
	lease      *Lease[T]
	ctx        context.Context
	cancel     context.CancelFunc
	capability string
	released   bool
}

// Context returns the cancellation context supplied to the callback.
func (c *Invocation[T]) Context() context.Context { return c.ctx }

// Complete checks the current epoch even if the callback ignored cancellation.
func (c *Invocation[T]) Complete() error {
	m := c.lease.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || c.lease.epoch != m.epoch {
		return ErrRevoked
	}
	return c.ctx.Err()
}

// Release drops the independent invocation reference exactly once.
func (c *Invocation[T]) Release() {
	m := c.lease.manager
	m.mu.Lock()
	if c.released {
		m.mu.Unlock()
		return
	}
	c.released = true
	delete(m.calls, c)
	start := m.releaseLocked(c.lease.generation)
	m.mu.Unlock()
	c.cancel()
	if start {
		m.cleanup(c.lease.generation)
	}
}

// invocationLifetime transfers cancellation ownership to Invocation.Release;
// Revoke and Close also cancel every currently registered invocation.
func invocationLifetime(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithCancel(ctx)
}
