package extension

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sync"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// This file defines the extensionCoordinator, which drives the Phase 4
// extension lifecycle (init -> run -> shutdown), emits tracing spans for each
// stage and hook invocation, and provides a process-wide plugin-loader
// registry with a safe nil-default (mirroring internal/production/registry.go).

var (
	// ErrCleanupPending means a lifecycle callback did not stop before its caller's context ended.
	ErrCleanupPending = errors.New("extension: cleanup pending")
	// ErrExtensionInitCanceled means Init returned successfully after its owner was canceled,
	// so its staged registrations were discarded and cleanup Shutdown was run.
	ErrExtensionInitCanceled = errors.New("extension: Init canceled before registry commit")
	// ErrExtensionNameConflict means another extension instance already owns the same name.
	ErrExtensionNameConflict = errors.New("extension: duplicate extension name")
	// ErrExtensionManagerClosing means the PluginManager no longer accepts new loads or init work.
	ErrExtensionManagerClosing = errors.New("extension: plugin manager is closing")
)

// CleanupPendingError reports an in-process callback that remains owned and may
// still be using its resources after the caller stops waiting.
type CleanupPendingError struct {
	ExtensionName string
	Phase         string
	Cause         error
}

func (e *CleanupPendingError) Error() string {
	if e.ExtensionName == "" {
		return fmt.Sprintf("extension manager %s cleanup_pending", e.Phase)
	}
	return fmt.Sprintf("extension %q %s cleanup_pending", e.ExtensionName, e.Phase)
}

func (e *CleanupPendingError) Unwrap() []error {
	if e.Cause == nil {
		return []error{ErrCleanupPending}
	}
	return []error{ErrCleanupPending, e.Cause}
}

func cleanupPending(name, phase string, cause error) error {
	return &CleanupPendingError{ExtensionName: name, Phase: phase, Cause: cause}
}

// extensionState is the lifecycle state of a managed extension.
type extensionState string

const (
	extensionStatePending          extensionState = "pending"
	extensionStateInitializing     extensionState = "initializing"
	extensionStateRunning          extensionState = "running"
	extensionStateStopping         extensionState = "stopping"
	extensionStateStopped          extensionState = "stopped"
	extensionStateStoppedWithError extensionState = "stopped_with_error"
	extensionStateFailed           extensionState = "failed"
	extensionStateCleanupPending   extensionState = "cleanup_pending"
)

type extensionOperation string

const (
	extensionOperationInit        extensionOperation = "init"
	extensionOperationInitCleanup extensionOperation = "init_cleanup"
	extensionOperationShutdown    extensionOperation = "shutdown"
)

// managedExtension retains each callback owner until it actually finishes.
type managedExtension struct {
	ext             Extension
	name            string
	cleanupCtx      context.Context
	state           extensionState
	operation       extensionOperation
	done            chan struct{}
	initErr         error
	shutdownErr     error
	cancel          context.CancelFunc
	cancelRequested bool
}

// extensionLifecycleGate linearizes manager shutdown admission against staged
// registry publication. Commit lock order is staging registry, parent registry,
// gate, then coordinator; shutdown takes the gate but never a registry lock.
type extensionLifecycleGate struct {
	mu      sync.Mutex
	closing bool
}

// extensionCoordinator drives extension lifecycle and hook dispatch while
// recording tracing spans for every stage. It is package-internal; the Phase 4
// lifecycle is exercised entirely by the package's own tests.
type extensionCoordinator struct {
	mu                  sync.Mutex
	reg                 ExtensionRegistry
	managed             map[string]*managedExtension
	gate                *extensionLifecycleGate
	shutdownCallbackCtx context.Context
}

// newExtensionCoordinator creates a coordinator over the given registry. When
// registry is nil it uses a fresh NewExtensionRegistry.
func newExtensionCoordinator(registry ExtensionRegistry) *extensionCoordinator {
	if registry == nil {
		registry = NewExtensionRegistry()
	}
	return &extensionCoordinator{
		reg:     registry,
		managed: make(map[string]*managedExtension),
		gate:    &extensionLifecycleGate{},
	}
}

// registry returns the underlying registry used by the coordinator.
func (c *extensionCoordinator) registry() ExtensionRegistry { return c.reg }

// requestShutdownCallbackContext records the first manager shutdown context
// for in-flight Init callbacks before their owner context is canceled. It does
// not invoke extension code while holding the coordinator lock.
func (c *extensionCoordinator) requestShutdownCallbackContext(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	if c.shutdownCallbackCtx == nil {
		c.shutdownCallbackCtx = ctx
	}
	for _, managed := range c.managed {
		if managed.operation == extensionOperationInit {
			managed.cleanupCtx = c.shutdownCallbackCtx
		}
	}
	c.mu.Unlock()
}

func (c *extensionCoordinator) clearShutdownCallbackContext() {
	c.mu.Lock()
	c.shutdownCallbackCtx = nil
	c.mu.Unlock()
}

// initExtension initializes an extension in a private registry and publishes
// all of its registrations atomically only after Init succeeds.
func (c *extensionCoordinator) initExtension(ctx context.Context, ext Extension) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if ext == nil {
		return errors.New("extension: nil extension")
	}
	parent, ok := c.reg.(*DefaultExtensionRegistry)
	if !ok || parent == nil {
		return ErrExtensionAtomicCommitUnsupported
	}
	name := ext.Name()

	c.mu.Lock()
	if current, exists := c.managed[name]; exists {
		if !sameExtensionInstance(current.ext, ext) {
			c.mu.Unlock()
			return fmt.Errorf("%w: %s", ErrExtensionNameConflict, name)
		}
		switch current.state {
		case extensionStateRunning:
			err := current.initErr
			c.mu.Unlock()
			return err
		case extensionStateFailed:
			err := current.initErr
			c.mu.Unlock()
			return err
		case extensionStateStopped, extensionStateStoppedWithError:
			err := current.initErr
			if err == nil {
				err = fmt.Errorf("extension %q has already been initialized", name)
			}
			c.mu.Unlock()
			return err
		case extensionStateInitializing, extensionStateCleanupPending, extensionStateStopping:
			done := current.done
			c.mu.Unlock()
			return c.waitForInit(ctx, current, done, false)
		default:
			c.mu.Unlock()
			return fmt.Errorf("extension %q has invalid lifecycle state %q", name, current.state)
		}
	}
	ownerCtx, cancel := context.WithCancel(ctx)
	staging, ok := NewExtensionRegistry().(*DefaultExtensionRegistry)
	if !ok {
		c.mu.Unlock()
		cancel()
		return ErrExtensionAtomicCommitUnsupported
	}
	staging.hookOwner = name
	managed := &managedExtension{
		ext:        ext,
		name:       name,
		cleanupCtx: ownerCtx,
		state:      extensionStateInitializing,
		operation:  extensionOperationInit,
		done:       make(chan struct{}),
		cancel:     cancel,
	}
	if c.shutdownCallbackCtx != nil {
		managed.cleanupCtx = c.shutdownCallbackCtx
	}
	c.managed[name] = managed
	c.mu.Unlock()

	go c.runInitOwner(ownerCtx, managed, parent, staging)
	return c.waitForInit(ctx, managed, managed.done, true)
}

func sameExtensionInstance(left, right Extension) bool {
	if left == nil || right == nil || reflect.TypeOf(left) != reflect.TypeOf(right) {
		return false
	}
	if !reflect.TypeOf(left).Comparable() {
		return false
	}
	return left == right
}

func (c *extensionCoordinator) runInitOwner(ctx context.Context, managed *managedExtension, parent, staging *DefaultExtensionRegistry) {
	name := managed.name
	span, spanCtx := tracing.SpanFromContext(ctx, "extension.init", tracing.SpanKindInternal)
	defer span.End()
	span.SetAttributes(tracing.Attribute{Key: "extension_name", Value: name})

	initErr := managed.ext.Init(spanCtx, staging)
	if initErr != nil {
		staging.seal()
		span.SetStatus(tracing.SpanStatusError, initErr.Error())
		c.finishInit(managed, initErr, nil, extensionStateFailed)
		return
	}

	// Do not hold the coordinator lock while waiting for either registry lock.
	// Shutdown can then close the shared gate while this publication is blocked
	// on the parent registry, and the final check and merge remain atomic.
	staging.mu.Lock()
	parent.mu.Lock()
	c.gate.mu.Lock()
	c.mu.Lock()
	staging.sealed = true
	aborted := managed.cancelRequested || ctx.Err() != nil || c.gate.closing
	cleanupCtx := managed.cleanupCtx
	unlockPublication := func() {
		c.mu.Unlock()
		c.gate.mu.Unlock()
		parent.mu.Unlock()
		staging.mu.Unlock()
	}
	if aborted {
		managed.state = extensionStateCleanupPending
		managed.operation = extensionOperationInitCleanup
		unlockPublication()
		cleanupErr := managed.ext.Shutdown(cleanupCtx)
		resultErr := errors.Join(ErrExtensionInitCanceled, cleanupErr)
		span.SetStatus(tracing.SpanStatusError, resultErr.Error())
		state := extensionStateStopped
		if cleanupErr != nil {
			state = extensionStateStoppedWithError
		}
		c.finishInit(managed, resultErr, cleanupErr, state)
		return
	}

	commitErr := parent.commitFromLocked(staging)
	if commitErr != nil {
		managed.state = extensionStateStopping
		managed.operation = extensionOperationInitCleanup
		cleanupCtx := managed.cleanupCtx
		unlockPublication()
		cleanupErr := managed.ext.Shutdown(cleanupCtx)
		resultErr := errors.Join(commitErr, cleanupErr)
		span.SetStatus(tracing.SpanStatusError, resultErr.Error())
		state := extensionStateStopped
		if cleanupErr != nil {
			state = extensionStateStoppedWithError
		}
		c.finishInit(managed, resultErr, cleanupErr, state)
		return
	}

	managed.state = extensionStateRunning
	managed.operation = ""
	managed.initErr = nil
	managed.cleanupCtx = nil
	cancel := managed.cancel
	managed.cancel = nil
	close(managed.done)
	unlockPublication()
	span.SetStatus(tracing.SpanStatusOK, "")
	if cancel != nil {
		cancel()
	}
}

func (c *extensionCoordinator) finishInit(managed *managedExtension, initErr, shutdownErr error, state extensionState) {
	c.mu.Lock()
	managed.initErr = initErr
	managed.shutdownErr = shutdownErr
	managed.state = state
	managed.operation = ""
	managed.cleanupCtx = nil
	cancel := managed.cancel
	managed.cancel = nil
	close(managed.done)
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (c *extensionCoordinator) waitForInit(ctx context.Context, managed *managedExtension, done <-chan struct{}, owner bool) error {
	select {
	case <-done:
		return c.initResult(managed)
	case <-ctx.Done():
	}
	select {
	case <-done:
		return c.initResult(managed)
	default:
	}
	if !owner {
		return ctx.Err()
	}
	c.mu.Lock()
	if managed.operation == extensionOperationInit {
		managed.cancelRequested = true
		managed.state = extensionStateCleanupPending
		if managed.cancel != nil {
			managed.cancel()
		}
	} else if managed.operation == extensionOperationInitCleanup && managed.state == extensionStateStopping {
		managed.state = extensionStateCleanupPending
	}
	c.mu.Unlock()
	select {
	case <-done:
		return c.initResult(managed)
	default:
		return cleanupPending(managed.name, "Init", ctx.Err())
	}
}

func (c *extensionCoordinator) initResult(managed *managedExtension) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return managed.initErr
}

// shutdownExtension shuts down an initialized extension exactly once. If Init
// is still running, Shutdown cancels its owner and waits for its cleanup path.
func (c *extensionCoordinator) shutdownExtension(ctx context.Context, ext Extension) error {
	if ctx == nil {
		ctx = context.Background()
	}
	return c.shutdownExtensionWithContexts(ctx, ctx, ext)
}

// shutdownExtensionWithContexts separates the callback context from the wait
// context. Manager-owned teardown can let its caller stop waiting at a deadline
// while preserving that original deadline for cooperative extension callbacks.
func (c *extensionCoordinator) shutdownExtensionWithContexts(waitCtx, callbackCtx context.Context, ext Extension) error {
	if waitCtx == nil {
		waitCtx = context.Background()
	}
	if callbackCtx == nil {
		callbackCtx = waitCtx
	}
	if ext == nil {
		return nil
	}
	name := ext.Name()
	c.mu.Lock()
	managed, exists := c.managed[name]
	if !exists {
		c.mu.Unlock()
		return nil
	}
	if !sameExtensionInstance(managed.ext, ext) {
		c.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrExtensionNameConflict, name)
	}
	switch managed.state {
	case extensionStateFailed:
		c.mu.Unlock()
		return nil
	case extensionStateStopped:
		err := managed.shutdownErr
		c.mu.Unlock()
		return err
	case extensionStateStoppedWithError:
		err := managed.shutdownErr
		c.mu.Unlock()
		return err
	case extensionStateRunning:
		shutdownCtx, cancel := context.WithCancel(callbackCtx)
		managed.state = extensionStateStopping
		managed.operation = extensionOperationShutdown
		managed.done = make(chan struct{})
		managed.cancel = cancel
		done := managed.done
		c.mu.Unlock()
		go c.runShutdownOwner(shutdownCtx, managed)
		return c.waitForShutdown(waitCtx, managed, done)
	case extensionStateInitializing, extensionStateCleanupPending, extensionStateStopping:
		if managed.operation == extensionOperationInit {
			managed.cancelRequested = true
			managed.cleanupCtx = callbackCtx
			managed.state = extensionStateCleanupPending
			if managed.cancel != nil {
				managed.cancel()
			}
		}
		done := managed.done
		c.mu.Unlock()
		select {
		case <-done:
			state := c.state(name)
			if state == extensionStateRunning {
				return c.shutdownExtensionWithContexts(waitCtx, callbackCtx, ext)
			}
			return c.shutdownResult(managed)
		case <-waitCtx.Done():
			select {
			case <-done:
				return c.shutdownResult(managed)
			default:
				return cleanupPending(name, "Shutdown", waitCtx.Err())
			}
		}
	default:
		c.mu.Unlock()
		return fmt.Errorf("extension %q has invalid lifecycle state %q", name, managed.state)
	}
}

func (c *extensionCoordinator) runShutdownOwner(ctx context.Context, managed *managedExtension) {
	span, spanCtx := tracing.SpanFromContext(ctx, "extension.shutdown", tracing.SpanKindInternal)
	defer span.End()
	span.SetAttributes(tracing.Attribute{Key: "extension_name", Value: managed.name})

	err := managed.ext.Shutdown(spanCtx)
	if err == nil {
		span.SetStatus(tracing.SpanStatusOK, "")
	} else {
		span.SetStatus(tracing.SpanStatusError, err.Error())
	}
	c.mu.Lock()
	managed.shutdownErr = err
	cancel := managed.cancel
	managed.cancel = nil
	managed.operation = ""
	managed.cleanupCtx = nil
	managed.state = extensionStateStopped
	if err != nil {
		managed.state = extensionStateStoppedWithError
	}
	close(managed.done)
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (c *extensionCoordinator) waitForShutdown(ctx context.Context, managed *managedExtension, done <-chan struct{}) error {
	select {
	case <-done:
		return c.shutdownResult(managed)
	case <-ctx.Done():
	}
	select {
	case <-done:
		return c.shutdownResult(managed)
	default:
	}
	c.mu.Lock()
	pending := managed.operation == extensionOperationShutdown
	if pending {
		managed.state = extensionStateCleanupPending
	}
	c.mu.Unlock()
	if !pending {
		select {
		case <-done:
			return c.shutdownResult(managed)
		default:
		}
	}
	return cleanupPending(managed.name, "Shutdown", ctx.Err())
}

func (c *extensionCoordinator) shutdownResult(managed *managedExtension) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return managed.shutdownErr
}

// runHook invokes a hook, emitting an `extension.hook` span with the
// `hook_name`, `event` and `action` attributes, and returns its HookResult.
func (c *extensionCoordinator) runHook(ctx context.Context, hook Hook, event HookEvent) HookResult {
	span, spanCtx := tracing.SpanFromContext(ctx, "extension.hook", tracing.SpanKindInternal)
	defer span.End()
	span.SetAttributes(
		tracing.Attribute{Key: "hook_name", Value: hook.Name()},
		tracing.Attribute{Key: "event", Value: event.Name},
	)

	result := hook.Handle(spanCtx, event)
	span.SetAttributes(tracing.Attribute{Key: "action", Value: string(result.Action)})
	span.SetStatus(tracing.SpanStatusOK, "")
	return result
}

// state returns the lifecycle state of the named extension, or Pending if it
// has not been initialized.
func (c *extensionCoordinator) state(name string) extensionState {
	c.mu.Lock()
	defer c.mu.Unlock()
	if m, ok := c.managed[name]; ok {
		return m.state
	}
	return extensionStatePending
}

// process-wide plugin loader registry (nil-default, like production/registry.go).
var (
	pluginLoaderMu sync.Mutex
	defaultLoader  PluginLoader
)

// registerPluginLoader sets the active PluginLoader. A nil value resets to a
// fresh DefaultPluginLoader.
func registerPluginLoader(l PluginLoader) {
	if l == nil {
		l = NewDefaultPluginLoader()
	}
	name := l.Name()
	pluginLoaderMu.Lock()
	defaultLoader = l
	pluginLoaderMu.Unlock()
	slog.Info("extension.register.plugin_loader", "name", name)
}

// getPluginLoader returns the active PluginLoader, lazily defaulting to a
// DefaultPluginLoader when none has been registered.
func getPluginLoader() PluginLoader {
	pluginLoaderMu.Lock()
	defer pluginLoaderMu.Unlock()
	if defaultLoader == nil {
		defaultLoader = NewDefaultPluginLoader()
	}
	return defaultLoader
}
