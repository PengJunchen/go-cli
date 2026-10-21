package capability

import (
	"context"
	"errors"
	"reflect"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

func testBuilder(t *testing.T, name string, closed *atomic.Int32) func(context.Context, *Scope) (Snapshot, string, error) {
	t.Helper()
	return func(_ context.Context, scope *Scope) (Snapshot, string, error) {
		if closed != nil {
			if err := scope.Defer(name, func(context.Context) error { closed.Add(1); return nil }); err != nil {
				return Snapshot{}, "", err
			}
		}
		snapshot, err := NewSnapshot(Input{Prompt: name, Nodes: []Node{{ID: name}}})
		return snapshot, name, err
	}
}

func TestGenerationLeaseFailureAndInvocationDrain(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	manager := NewManager[string]()
	var closed atomic.Int32
	if err := manager.Reload(ctx, testBuilder(t, "old", &closed)); err != nil {
		t.Fatal(err)
	}
	lease, err := manager.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	call, err := lease.BeginCall(ctx, "old")
	if err != nil {
		t.Fatal(err)
	}
	if err = manager.Reload(ctx, func(_ context.Context, s *Scope) (Snapshot, string, error) {
		_ = s.Defer("failed", func(context.Context) error { return nil })
		return Snapshot{}, "", errors.New("setup failed")
	}); err == nil {
		t.Fatal("candidate failure published")
	}
	if err = manager.Reload(ctx, testBuilder(t, "new", nil)); err != nil {
		t.Fatal(err)
	}
	lease.Release()
	if closed.Load() != 0 {
		t.Fatal("parent release closed active invocation")
	}
	if _, err = lease.BeginCall(ctx, "again"); !errors.Is(err, ErrLeaseReleased) {
		t.Fatalf("released parent: %v", err)
	}
	if err = call.Complete(); err != nil {
		t.Fatal(err)
	}
	call.Release()
	if err = manager.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if closed.Load() != 1 {
		t.Fatalf("close count %d", closed.Load())
	}
}

func TestGenerationPublishRejectsStaleRevision(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	manager := NewManager[string]()
	if err := manager.Reload(ctx, testBuilder(t, "initial", nil)); err != nil {
		t.Fatal(err)
	}
	entered, proceed := make(chan struct{}), make(chan struct{})
	oldDone := make(chan error, 1)
	var closed atomic.Int32
	var staleCommits atomic.Int32
	go func() {
		oldDone <- manager.ReloadWithCommit(ctx, func(ctx context.Context, s *Scope) (Snapshot, string, error) {
			close(entered)
			<-proceed
			return testBuilder(t, "stale", &closed)(ctx, s)
		}, func() error { staleCommits.Add(1); return nil })
	}()
	<-entered
	newDone := make(chan error, 1)
	go func() { newDone <- manager.Reload(ctx, testBuilder(t, "latest", nil)) }()
	for manager.Diagnostics().DesiredRevision < 3 {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		default:
			runtime.Gosched()
		}
	}
	close(proceed)
	if err := <-oldDone; !errors.Is(err, ErrStale) {
		t.Fatalf("stale result %v", err)
	}
	if err := <-newDone; err != nil {
		t.Fatal(err)
	}
	lease, err := manager.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Value() != "latest" {
		t.Fatal("older candidate won")
	}
	lease.Release()
	if staleCommits.Load() != 0 {
		t.Fatal("stale candidate executed host commit")
	}
	if closed.Load() != 1 {
		t.Fatal("failed candidate owner leaked")
	}
	if err := manager.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestGenerationCoalescesUntilRetiringDrains(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	m := NewManager[string]()
	if err := m.Reload(ctx, testBuilder(t, "a", nil)); err != nil {
		t.Fatal(err)
	}
	lease, _ := m.Acquire(ctx)
	if err := m.Reload(ctx, testBuilder(t, "b", nil)); err != nil {
		t.Fatal(err)
	}
	blocked, cancelBlocked := context.WithCancel(ctx)
	cancelBlocked()
	called := false
	err := m.Reload(blocked, func(context.Context, *Scope) (Snapshot, string, error) { called = true; return Snapshot{}, "", nil })
	if !errors.Is(err, context.Canceled) || called {
		t.Fatalf("third generation built: %v/%v", called, err)
	}
	lease.Release()
	if err = m.Reload(ctx, testBuilder(t, "c", nil)); err != nil {
		t.Fatal(err)
	}
	if err = m.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestRevokeFencesNonCooperativeCompletion(t *testing.T) {
	ctx := context.Background()
	m := NewManager[string]()
	if err := m.Reload(ctx, testBuilder(t, "a", nil)); err != nil {
		t.Fatal(err)
	}
	lease, _ := m.Acquire(ctx)
	call, _ := lease.BeginCall(ctx, "external")
	m.Revoke()
	if !errors.Is(call.Complete(), ErrRevoked) {
		t.Fatal("late result accepted")
	}
	if call.Context().Err() == nil {
		t.Fatal("active callback not canceled")
	}
	newLease, _ := m.Acquire(ctx)
	if _, err := newLease.BeginCall(ctx, "external"); !errors.Is(err, ErrRevoked) {
		t.Fatal("reacquiring old generation restored authorization")
	}
	newLease.Release()
	call.Release()
	lease.Release()
	if err := m.Reload(ctx, testBuilder(t, "fresh", nil)); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestScopePanicReentryAndCloseWaiting(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	scope := NewScope()
	order := []int{}
	_ = scope.Defer("first", func(context.Context) error { order = append(order, 1); return nil })
	_ = scope.Defer("panic", func(context.Context) error { order = append(order, 2); panic("fixture") })
	_ = scope.Defer("reentrant", func(context.Context) error {
		if !errors.Is(scope.Close(ctx), ErrCloseInProgress) {
			return errors.New("reentrant close was not rejected")
		}
		order = append(order, 3)
		return nil
	})
	if err := scope.Close(ctx); err == nil {
		t.Fatal("panic was not reported")
	}
	if !reflect.DeepEqual(order, []int{3, 2, 1}) {
		t.Fatalf("cleanup order %v", order)
	}
	if err := scope.WaitClosed(ctx); err == nil {
		t.Fatal("cleanup error lost")
	}
}

func TestManagerCloseWaitsForCandidateBuilder(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	m := NewManager[string]()
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- m.Reload(ctx, func(ctx context.Context, s *Scope) (Snapshot, string, error) {
			close(entered)
			<-release
			return testBuilder(t, "candidate", nil)(ctx, s)
		})
	}()
	<-entered
	stop, cancelStop := context.WithCancel(ctx)
	cancelStop()
	if err := m.Close(stop); !errors.Is(err, context.Canceled) {
		t.Fatalf("stuck builder reported closed: %v", err)
	}
	close(release)
	if err := <-done; err == nil {
		t.Fatal("candidate published after close")
	}
	if err := m.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotDetachesAndRejectsInvalidGraphs(t *testing.T) {
	input := Input{Prompt: "actual prompt", Sources: []Source{{ID: "file", Content: []byte("aa")}}, Nodes: []Node{{ID: "consumer", Requires: []string{"store@1"}}, {ID: "provider", Provides: []string{"store@1"}}}}
	snapshot, err := NewSnapshot(input)
	if err != nil {
		t.Fatal(err)
	}
	nodes := snapshot.Nodes()
	nodes[0].Provides[0] = "changed"
	if snapshot.Nodes()[0].Provides[0] != "store@1" {
		t.Fatal("mutable snapshot")
	}
	input.Sources[0].Content = []byte("bb")
	changed, err := NewSnapshot(input)
	if err != nil {
		t.Fatal(err)
	}
	if changed.ID() == snapshot.ID() {
		t.Fatal("same length content not hashed")
	}
	input.Nodes = []Node{{ID: "a", Requires: []string{"b"}, Provides: []string{"a"}}, {ID: "b", Requires: []string{"a"}, Provides: []string{"b"}}}
	if _, err = NewSnapshot(input); err == nil {
		t.Fatal("cycle accepted")
	}
	input.Nodes = []Node{{ID: "a", Requires: []string{"missing"}}}
	if _, err = NewSnapshot(input); err == nil {
		t.Fatal("missing provider accepted")
	}
}

func BenchmarkGenerationAcquire(b *testing.B) {
	m := NewManager[string]()
	snapshot, _ := NewSnapshot(Input{Prompt: "benchmark"})
	_ = m.Reload(context.Background(), func(context.Context, *Scope) (Snapshot, string, error) { return snapshot, "payload", nil })
	defer func() {
		if err := m.Close(context.Background()); err != nil {
			b.Fatal(err)
		}
	}()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		lease, _ := m.Acquire(context.Background())
		lease.Release()
	}
}

func TestGenerationHostCommitRejectsFailureAndCanceledBuilder(t *testing.T) {
	ctx := context.Background()
	m := NewManager[string]()
	requireCurrent := func(want string) {
		t.Helper()
		lease, err := m.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer lease.Release()
		if lease.Value() != want {
			t.Fatalf("current=%q want=%q", lease.Value(), want)
		}
	}
	if err := m.Reload(ctx, testBuilder(t, "old", nil)); err != nil {
		t.Fatal(err)
	}
	commits := 0
	rejected := errors.New("host publication rejected")
	err := m.ReloadWithCommit(ctx, testBuilder(t, "failed", nil), func() error { commits++; return rejected })
	if !errors.Is(err, rejected) || commits != 1 {
		t.Fatalf("failed commit %v/%d", err, commits)
	}
	requireCurrent("old")
	canceled, cancel := context.WithCancel(ctx)
	err = m.ReloadWithCommit(canceled, func(ctx context.Context, s *Scope) (Snapshot, string, error) {
		snapshot, value, err := testBuilder(t, "canceled", nil)(ctx, s)
		cancel()
		return snapshot, value, err
	}, func() error { commits++; return nil })
	if !errors.Is(err, context.Canceled) || commits != 1 {
		t.Fatalf("canceled candidate committed %v/%d", err, commits)
	}
	requireCurrent("old")
	if err = m.ReloadWithCommit(ctx, testBuilder(t, "new", nil), func() error { commits++; return nil }); err != nil {
		t.Fatal(err)
	}
	requireCurrent("new")
	if commits != 2 {
		t.Fatal(commits)
	}
	if err = m.Close(ctx); err != nil {
		t.Fatal(err)
	}
}
