package extension

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type r7LifecycleExt struct {
	name      string
	init      func(context.Context, ExtensionRegistry) error
	shutdown  func(context.Context) error
	initCalls atomic.Int32
	stopCalls atomic.Int32
}

var _ Extension = (*r7LifecycleExt)(nil)

func (e *r7LifecycleExt) Name() string { return e.name }
func (e *r7LifecycleExt) Init(ctx context.Context, registry ExtensionRegistry) error {
	e.initCalls.Add(1)
	if e.init != nil {
		return e.init(ctx, registry)
	}
	return nil
}
func (e *r7LifecycleExt) Shutdown(ctx context.Context) error {
	e.stopCalls.Add(1)
	if e.shutdown != nil {
		return e.shutdown(ctx)
	}
	return nil
}

func r7ReleaseOnce(channel chan struct{}) func() {
	var once sync.Once
	return func() { once.Do(func() { close(channel) }) }
}

func TestExtensionOrderStable(t *testing.T) {
	ctx := context.Background()
	registry := newConcreteRegistry()
	const count = 16
	expected := make([]string, count)
	commandOrder := make([]string, 0, count)
	for i := 0; i < count; i++ {
		name := fmt.Sprintf("item-%02d", i)
		expected[i] = name
		require.NoError(t, registry.RegisterTool(ctx, testTool{name: name}))
		require.NoError(t, registry.RegisterHook(ctx, newRegTestHook(name)))
		require.NoError(t, registry.RegisterMiddleware(ctx, newRegTestMiddleware(name)))
		require.NoError(t, registry.RegisterProvider(testProvider{name: name}))
		commandOrder = append(commandOrder, "")
		index := i
		require.NoError(t, registry.RegisterCommand(name, func([]string) error {
			commandOrder[index] = name
			return nil
		}))
	}

	toolNames := make([]string, 0, count)
	for _, tool := range registry.AllTools() {
		toolNames = append(toolNames, tool.Name())
	}
	hookNames := make([]string, 0, count)
	for _, hook := range registry.AllHooks() {
		hookNames = append(hookNames, hook.Name())
	}
	middlewareNames := make([]string, 0, count)
	for _, middleware := range registry.AllMiddleware() {
		middlewareNames = append(middlewareNames, middleware.Name())
	}
	assert.Equal(t, expected, toolNames)
	assert.Equal(t, expected, hookNames)
	assert.Equal(t, expected, middlewareNames)
	providerNames := make([]string, 0, count)
	for _, provider := range registry.AllProviders() {
		providerNames = append(providerNames, provider.Name())
	}
	assert.Equal(t, expected, providerNames)
	for _, command := range registry.AllCommands() {
		require.NoError(t, command(nil))
	}
	assert.Equal(t, expected, commandOrder)

	loader := newMgrTestPluginLoader("test")
	first := &r7LifecycleExt{name: "first", init: func(_ context.Context, staging ExtensionRegistry) error {
		for _, name := range []string{"first-a", "first-b"} {
			if err := staging.RegisterTool(context.Background(), testTool{name: name}); err != nil {
				return err
			}
		}
		return nil
	}}
	second := &r7LifecycleExt{name: "second", init: func(_ context.Context, staging ExtensionRegistry) error {
		return staging.RegisterTool(context.Background(), testTool{name: "second-a"})
	}}
	loader.SetResult("path-a", []Extension{first})
	loader.SetResult("path-b", []Extension{second})
	manager := NewPluginManager(loader)
	require.NoError(t, manager.Load(ctx, []string{"path-a", "path-b"}))
	require.NoError(t, manager.Init(ctx))
	committedNames := make([]string, 0, 3)
	for _, tool := range manager.Tools() {
		committedNames = append(committedNames, tool.Name())
	}
	assert.Equal(t, []string{"first-a", "first-b", "second-a"}, committedNames)
	require.NoError(t, manager.Shutdown(ctx))
}

func TestExtensionInitFailurePropagates(t *testing.T) {
	initErr := errors.New("extension init failed")
	secondInitErr := errors.New("second extension init failed")
	loader := newMgrTestPluginLoader("test")
	failed := &r7LifecycleExt{name: "failed", init: func(_ context.Context, registry ExtensionRegistry) error {
		if err := registry.RegisterTool(context.Background(), testTool{name: "must-not-commit"}); err != nil {
			return err
		}
		return initErr
	}}
	failedSecond := &r7LifecycleExt{name: "failed-second", init: func(context.Context, ExtensionRegistry) error {
		return secondInitErr
	}}
	ready := &r7LifecycleExt{name: "ready"}
	loader.SetResult("plugins", []Extension{failed, failedSecond, ready})
	manager := NewPluginManager(loader)
	require.NoError(t, manager.Load(context.Background(), []string{"plugins"}))

	err := manager.Init(context.Background())
	require.ErrorIs(t, err, initErr)
	require.ErrorIs(t, err, secondInitErr)
	assert.Empty(t, manager.Tools(), "failed Init staging entries must not be visible")
	assert.Equal(t, int32(1), failed.initCalls.Load())
	assert.Equal(t, int32(0), failed.stopCalls.Load(), "Init failure uses the extension's own rollback")
	assert.Equal(t, int32(1), failedSecond.initCalls.Load())
	assert.Equal(t, int32(0), failedSecond.stopCalls.Load())
	assert.Equal(t, int32(1), ready.initCalls.Load(), "init errors must not prevent later extensions")
	assert.Equal(t, extensionStateFailed, manager.coordinator.state("failed"))
	require.NoError(t, manager.Shutdown(context.Background()))
	assert.Equal(t, int32(1), ready.stopCalls.Load())
	assert.Equal(t, int32(0), failed.stopCalls.Load())
}

func TestPluginManagerLoadRejectsDuplicateExtensionNamesAtomically(t *testing.T) {
	loader := newMgrTestPluginLoader("test")
	loader.SetResult("duplicate-path", []Extension{
		&r7LifecycleExt{name: "duplicate-a"},
		&r7LifecycleExt{name: "duplicate-a"},
	})
	good := &r7LifecycleExt{name: "good"}
	loader.SetResult("good-path", []Extension{good})
	manager := NewPluginManager(loader)

	err := manager.Load(context.Background(), []string{"duplicate-path", "good-path"})
	require.ErrorIs(t, err, ErrExtensionNameConflict)
	extensions := manager.Extensions()
	require.Len(t, extensions, 1, "one duplicate path must not partially append its returned slice")
	assert.Same(t, good, extensions[0])
}

func TestExtensionConcurrentInitAndShutdownSafe(t *testing.T) {
	coordinator := newExtensionCoordinator(nil)
	initStarted := make(chan struct{})
	releaseInit := make(chan struct{})
	var started sync.Once
	ext := &r7LifecycleExt{name: "once", init: func(context.Context, ExtensionRegistry) error {
		started.Do(func() { close(initStarted) })
		<-releaseInit
		return nil
	}}

	const callers = 12
	initErrs := make(chan error, callers)
	var initWG sync.WaitGroup
	for i := 0; i < callers; i++ {
		initWG.Add(1)
		go func() {
			defer initWG.Done()
			initErrs <- coordinator.initExtension(context.Background(), ext)
		}()
	}
	<-initStarted
	close(releaseInit)
	initWG.Wait()
	close(initErrs)
	for err := range initErrs {
		assert.NoError(t, err)
	}
	assert.Equal(t, int32(1), ext.initCalls.Load(), "same extension instance initializes once")

	shutdownErrs := make(chan error, callers)
	var shutdownWG sync.WaitGroup
	for i := 0; i < callers; i++ {
		shutdownWG.Add(1)
		go func() {
			defer shutdownWG.Done()
			shutdownErrs <- coordinator.shutdownExtension(context.Background(), ext)
		}()
	}
	shutdownWG.Wait()
	close(shutdownErrs)
	for err := range shutdownErrs {
		assert.NoError(t, err)
	}
	assert.Equal(t, int32(1), ext.stopCalls.Load(), "same extension instance shuts down once")
	assert.Equal(t, extensionState("stopped"), coordinator.state(ext.name))
}

func TestExtensionStagingCommitConflictShutsDownAndJoinsErrors(t *testing.T) {
	registry := newConcreteRegistry()
	original := newRegTestHook("collision")
	require.NoError(t, registry.RegisterHook(context.Background(), original))
	cleanupErr := errors.New("shutdown after conflict failed")
	ext := &r7LifecycleExt{name: "conflict", init: func(_ context.Context, staging ExtensionRegistry) error {
		if err := staging.RegisterTool(context.Background(), testTool{name: "must-not-leak"}); err != nil {
			return err
		}
		return staging.RegisterHook(context.Background(), newRegTestHook("collision"))
	}, shutdown: func(context.Context) error { return cleanupErr }}
	coordinator := newExtensionCoordinator(registry)

	err := coordinator.initExtension(context.Background(), ext)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "collision")
	assert.ErrorIs(t, err, cleanupErr, "post-Init commit conflicts must retain cleanup errors")
	assert.Nil(t, registry.tool("must-not-leak"), "commit conflicts must not partially publish staging entries")
	assert.Same(t, original, registry.Hook("collision"))
	assert.Equal(t, int32(1), ext.stopCalls.Load(), "successful Init followed by failed commit gets one cleanup Shutdown")
	assert.Equal(t, extensionState("stopped_with_error"), coordinator.state(ext.name))
}

func TestExtensionWaiterCancellationDoesNotCancelOwner(t *testing.T) {
	coordinator := newExtensionCoordinator(nil)
	started := make(chan struct{})
	release := make(chan struct{})
	releaseCallback := r7ReleaseOnce(release)
	defer releaseCallback()
	var startOnce sync.Once
	ext := &r7LifecycleExt{name: "waiter", init: func(context.Context, ExtensionRegistry) error {
		startOnce.Do(func() { close(started) })
		<-release
		return nil
	}}
	ownerResult := make(chan error, 1)
	go func() { ownerResult <- coordinator.initExtension(context.Background(), ext) }()
	<-started

	waiterCtx, cancelWaiter := context.WithCancel(context.Background())
	waiterResult := make(chan error, 1)
	go func() { waiterResult <- coordinator.initExtension(waiterCtx, ext) }()
	cancelWaiter()
	select {
	case err := <-waiterResult:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("canceled waiter did not stop waiting")
	}
	assert.Equal(t, int32(1), ext.initCalls.Load(), "waiter must not start another Init callback")
	releaseCallback()
	require.NoError(t, <-ownerResult)
	assert.Equal(t, extensionStateRunning, coordinator.state(ext.name))
	assert.Equal(t, int32(0), ext.stopCalls.Load(), "waiter cancellation must not cancel the owner")
	require.NoError(t, coordinator.shutdownExtension(context.Background(), ext))
}

func TestUncooperativeExtensionInitReturnsCleanupPending(t *testing.T) {
	registry := newConcreteRegistry()
	started := make(chan struct{})
	release := make(chan struct{})
	releaseCallback := r7ReleaseOnce(release)
	defer releaseCallback()
	ext := &r7LifecycleExt{name: "slow-init", init: func(_ context.Context, staging ExtensionRegistry) error {
		if err := staging.RegisterTool(context.Background(), testTool{name: "late-tool"}); err != nil {
			return err
		}
		close(started)
		<-release
		return nil
	}}
	coordinator := newExtensionCoordinator(registry)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- coordinator.initExtension(ctx, ext) }()
	<-started
	cancel()

	var initErr error
	select {
	case initErr = <-result:
	case <-time.After(time.Second):
		releaseCallback()
		t.Fatal("canceled Init owner did not return while callback remained blocked")
	}
	assert.Equal(t, extensionStateCleanupPending, coordinator.state(ext.name))
	// Release the uncooperative callback only after confirming bounded caller return.
	releaseCallback()
	require.ErrorIs(t, initErr, ErrCleanupPending)
	require.Eventually(t, func() bool { return coordinator.state(ext.name) == extensionStateStopped }, time.Second, time.Millisecond)
	assert.Nil(t, registry.tool("late-tool"), "a canceled Init must discard its staging registry")
	assert.Equal(t, int32(1), ext.stopCalls.Load(), "late successful Init must be cleaned up exactly once")
}

func TestUncooperativeExtensionShutdownReturnsCleanupPending(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	releaseCallback := r7ReleaseOnce(release)
	defer releaseCallback()
	ext := &r7LifecycleExt{name: "slow-shutdown", shutdown: func(context.Context) error {
		close(started)
		<-release
		return nil
	}}
	coordinator := newExtensionCoordinator(nil)
	require.NoError(t, coordinator.initExtension(context.Background(), ext))
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- coordinator.shutdownExtension(ctx, ext) }()
	<-started
	cancel()

	var shutdownErr error
	select {
	case shutdownErr = <-result:
	case <-time.After(time.Second):
		releaseCallback()
		t.Fatal("canceled Shutdown owner did not return while callback remained blocked")
	}
	assert.Equal(t, extensionStateCleanupPending, coordinator.state(ext.name))
	releaseCallback()
	require.ErrorIs(t, shutdownErr, ErrCleanupPending)
	require.Eventually(t, func() bool { return coordinator.state(ext.name) == extensionStateStopped }, time.Second, time.Millisecond)
	assert.Equal(t, int32(1), ext.stopCalls.Load())
}

func TestPluginManagerShutdownDuringInitQuarantinesUntilLateInitCleansUp(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	releaseCallback := r7ReleaseOnce(release)
	defer releaseCallback()
	ext := &r7LifecycleExt{name: "late-init", init: func(_ context.Context, staging ExtensionRegistry) error {
		if err := staging.RegisterTool(context.Background(), testTool{name: "not-published"}); err != nil {
			return err
		}
		close(started)
		<-release
		return nil
	}}
	loader := newMgrTestPluginLoader("test")
	loader.SetResult("plugins", []Extension{ext})
	manager := NewPluginManager(loader)
	require.NoError(t, manager.Load(context.Background(), []string{"plugins"}))

	initResult := make(chan error, 1)
	go func() { initResult <- manager.Init(context.Background()) }()
	<-started
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	shutdownErr := manager.Shutdown(shutdownCtx)
	require.ErrorIs(t, shutdownErr, ErrCleanupPending)
	assert.Equal(t, extensionStateCleanupPending, manager.coordinator.state(ext.name))
	assert.Empty(t, manager.Tools())

	releaseCallback()
	require.ErrorIs(t, <-initResult, ErrCleanupPending)
	require.Eventually(t, func() bool { return manager.coordinator.state(ext.name) == extensionStateStopped }, time.Second, time.Millisecond)
	assert.Equal(t, int32(1), ext.stopCalls.Load())
	require.NoError(t, manager.Shutdown(context.Background()))
	assert.Equal(t, int32(1), ext.stopCalls.Load(), "retrying Shutdown must not repeat cleanup")
}

func TestPluginManagerShutdownContinuesAfterLateInitCleanup(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	releaseCallback := r7ReleaseOnce(release)
	defer releaseCallback()
	events := make(chan string, 2)
	cleanupHasDeadline := make(chan bool, 1)
	earlier := &r7LifecycleExt{name: "earlier", shutdown: func(context.Context) error {
		events <- "earlier"
		return nil
	}}
	late := &r7LifecycleExt{
		name: "late",
		init: func(context.Context, ExtensionRegistry) error {
			close(started)
			<-release
			return nil
		},
		shutdown: func(ctx context.Context) error {
			_, hasDeadline := ctx.Deadline()
			cleanupHasDeadline <- hasDeadline
			events <- "late"
			return nil
		},
	}
	loader := newMgrTestPluginLoader("test")
	loader.SetResult("plugins", []Extension{earlier, late})
	manager := NewPluginManager(loader)
	require.NoError(t, manager.Load(context.Background(), []string{"plugins"}))

	initResult := make(chan error, 1)
	go func() { initResult <- manager.Init(context.Background()) }()
	<-started

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, manager.Shutdown(shutdownCtx), ErrCleanupPending)
	manager.mu.RLock()
	shutdownOwner := manager.shutdownOp
	manager.mu.RUnlock()
	require.NotNil(t, shutdownOwner)
	assert.Equal(t, int32(0), earlier.stopCalls.Load(), "dependent shutdown waits for late Init cleanup")
	secondWaitCtx, cancelSecondWait := context.WithTimeout(context.Background(), 25*time.Millisecond)
	require.ErrorIs(t, manager.Shutdown(secondWaitCtx), ErrCleanupPending)
	cancelSecondWait()
	manager.mu.RLock()
	assert.Same(t, shutdownOwner, manager.shutdownOp, "concurrent callers join the original shutdown owner")
	manager.mu.RUnlock()

	releaseCallback()
	require.ErrorIs(t, <-initResult, ErrCleanupPending)
	require.Eventually(t, func() bool {
		manager.mu.RLock()
		defer manager.mu.RUnlock()
		return manager.shutdownComplete
	}, time.Second, time.Millisecond, "the original shutdown owner must resume without a caller retry")
	assert.True(t, <-cleanupHasDeadline, "late-Init cleanup must receive the first Shutdown caller's deadline")
	assert.Equal(t, int32(1), late.stopCalls.Load())
	assert.Equal(t, int32(1), earlier.stopCalls.Load())
	assert.Equal(t, []string{"late", "earlier"}, []string{<-events, <-events})
}

func TestPluginManagerShutdownCallbackGetsCallerDeadlineAndContinues(t *testing.T) {
	callbackRelease := make(chan struct{})
	defer close(callbackRelease)
	callbackDeadline := make(chan bool, 1)
	events := make(chan string, 2)
	earlier := &r7LifecycleExt{name: "earlier", shutdown: func(context.Context) error {
		events <- "earlier"
		return nil
	}}
	later := &r7LifecycleExt{name: "later", shutdown: func(ctx context.Context) error {
		_, hasDeadline := ctx.Deadline()
		callbackDeadline <- hasDeadline
		select {
		case <-ctx.Done():
			events <- "later"
			return ctx.Err()
		case <-callbackRelease:
			events <- "later"
			return nil
		}
	}}
	loader := newMgrTestPluginLoader("test")
	loader.SetResult("plugins", []Extension{earlier, later})
	manager := NewPluginManager(loader)
	require.NoError(t, manager.Load(context.Background(), []string{"plugins"}))
	require.NoError(t, manager.Init(context.Background()))

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	shutdownErr := manager.Shutdown(shutdownCtx)
	require.Error(t, shutdownErr)
	require.Equal(t, true, <-callbackDeadline, "the actual extension callback must receive the first caller's deadline")
	require.Eventually(t, func() bool {
		manager.mu.RLock()
		defer manager.mu.RUnlock()
		return manager.shutdownComplete
	}, time.Second, time.Millisecond, "a cooperative callback must let the owner continue reverse cleanup")
	manager.mu.RLock()
	ownerErr := manager.shutdownResult
	manager.mu.RUnlock()
	require.ErrorIs(t, ownerErr, context.DeadlineExceeded)
	assert.Equal(t, int32(1), later.stopCalls.Load())
	assert.Equal(t, int32(1), earlier.stopCalls.Load())
	assert.Equal(t, []string{"later", "earlier"}, []string{<-events, <-events})
}

func TestPluginManagerShutdownWaitsForUncooperativeCallbackBeforeEarlierExtension(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	releaseCallback := r7ReleaseOnce(release)
	defer releaseCallback()
	events := make(chan string, 2)
	earlier := &r7LifecycleExt{name: "earlier", shutdown: func(context.Context) error {
		events <- "earlier"
		return nil
	}}
	later := &r7LifecycleExt{name: "later", shutdown: func(context.Context) error {
		close(started)
		<-release
		events <- "later"
		return nil
	}}
	loader := newMgrTestPluginLoader("test")
	loader.SetResult("plugins", []Extension{earlier, later})
	manager := NewPluginManager(loader)
	require.NoError(t, manager.Load(context.Background(), []string{"plugins"}))
	require.NoError(t, manager.Init(context.Background()))

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, manager.Shutdown(shutdownCtx), ErrCleanupPending)
	<-started
	assert.Equal(t, int32(0), earlier.stopCalls.Load(), "the owner must keep reverse order while the callback is still active")
	manager.mu.RLock()
	assert.False(t, manager.shutdownComplete)
	manager.mu.RUnlock()

	releaseCallback()
	require.Eventually(t, func() bool {
		manager.mu.RLock()
		defer manager.mu.RUnlock()
		return manager.shutdownComplete
	}, time.Second, time.Millisecond)
	assert.Equal(t, int32(1), later.stopCalls.Load())
	assert.Equal(t, int32(1), earlier.stopCalls.Load())
	assert.Equal(t, []string{"later", "earlier"}, []string{<-events, <-events})
}

func TestPluginManagerCallbackCleanupPendingErrorIsCompleted(t *testing.T) {
	callbackErr := cleanupPending("sentinel", "Shutdown", errors.New("callback returned pending sentinel"))
	ext := &r7LifecycleExt{name: "sentinel", shutdown: func(context.Context) error { return callbackErr }}
	loader := newMgrTestPluginLoader("test")
	loader.SetResult("plugins", []Extension{ext})
	manager := NewPluginManager(loader)
	require.NoError(t, manager.Load(context.Background(), []string{"plugins"}))
	require.NoError(t, manager.Init(context.Background()))

	err := manager.Shutdown(context.Background())
	require.ErrorIs(t, err, ErrCleanupPending)
	manager.mu.RLock()
	complete := manager.shutdownComplete
	manager.mu.RUnlock()
	assert.True(t, complete, "a callback-returned pending sentinel is a completed shutdown result")
	assert.Equal(t, extensionStateStoppedWithError, manager.coordinator.state(ext.name))
	assert.Equal(t, int32(1), ext.stopCalls.Load())
}

func TestPluginManagerShutdownClosesCommitGateBeforePublication(t *testing.T) {
	loader := newMgrTestPluginLoader("test")
	stagingReady := make(chan *DefaultExtensionRegistry, 1)
	initReturned := make(chan struct{})
	ext := &r7LifecycleExt{name: "commit-race", init: func(ctx context.Context, registry ExtensionRegistry) error {
		staging, ok := registry.(*DefaultExtensionRegistry)
		if !ok {
			return errors.New("Init did not receive staging registry")
		}
		if err := registry.RegisterTool(ctx, testTool{name: "must-not-publish"}); err != nil {
			return err
		}
		stagingReady <- staging
		close(initReturned)
		return nil
	}}
	loader.SetResult("plugins", []Extension{ext})
	manager := NewPluginManager(loader)
	require.NoError(t, manager.Load(context.Background(), []string{"plugins"}))
	registry := manager.Registry().(*DefaultExtensionRegistry)
	registry.mu.Lock()
	var unlockOnce sync.Once
	unlockRegistry := func() { unlockOnce.Do(registry.mu.Unlock) }
	defer unlockRegistry()

	initResult := make(chan error, 1)
	go func() { initResult <- manager.Init(context.Background()) }()
	staging := <-stagingReady
	<-initReturned
	require.Eventually(t, func() bool {
		if staging.mu.TryLock() {
			staging.mu.Unlock()
			return false
		}
		return true
	}, time.Second, time.Millisecond, "Init owner should be waiting to publish its staging registry")

	shutdownResult := make(chan error, 1)
	go func() { shutdownResult <- manager.Shutdown(context.Background()) }()
	require.Eventually(t, func() bool {
		manager.mu.RLock()
		defer manager.mu.RUnlock()
		return manager.closing
	}, time.Second, time.Millisecond, "Shutdown should close admission while publication is blocked")
	unlockRegistry()

	initErr := <-initResult
	assert.Error(t, initErr, "an Init finishing after the shared close gate must not publish")
	require.NoError(t, <-shutdownResult)
	assert.Nil(t, registry.tool("must-not-publish"))
	assert.Equal(t, int32(1), ext.stopCalls.Load(), "successful but uncommitted Init must be cleaned once")
}

func TestPluginManagerShutdownErrorsPropagateWithoutRepeatingCallbacks(t *testing.T) {
	firstErr := errors.New("first shutdown failed")
	secondErr := errors.New("second shutdown failed")
	first := &r7LifecycleExt{name: "first", shutdown: func(context.Context) error { return firstErr }}
	second := &r7LifecycleExt{name: "second", shutdown: func(context.Context) error { return secondErr }}
	loader := newMgrTestPluginLoader("test")
	loader.SetResult("plugins", []Extension{first, second})
	manager := NewPluginManager(loader)
	require.NoError(t, manager.Load(context.Background(), []string{"plugins"}))
	require.NoError(t, manager.Init(context.Background()))

	err := manager.Shutdown(context.Background())
	require.ErrorIs(t, err, firstErr)
	require.ErrorIs(t, err, secondErr)
	assert.Equal(t, extensionStateStoppedWithError, manager.coordinator.state(first.name))
	assert.Equal(t, extensionStateStoppedWithError, manager.coordinator.state(second.name))
	assert.Equal(t, int32(1), first.stopCalls.Load())
	assert.Equal(t, int32(1), second.stopCalls.Load())

	retryErr := manager.Shutdown(context.Background())
	require.ErrorIs(t, retryErr, firstErr)
	require.ErrorIs(t, retryErr, secondErr)
	assert.Equal(t, int32(1), first.stopCalls.Load())
	assert.Equal(t, int32(1), second.stopCalls.Load())
}
