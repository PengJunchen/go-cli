package cli

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/pengjunchen/go-cli/internal/acp"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/mcp"
)

const assemblyCloseBudget = 20 * time.Second

var (
	ErrAssemblyClosePending              = errors.New("cli: assembly close pending")
	ErrAssemblyRunAdmissionUnavailable   = errors.New("cli: assembly run admission cannot be closed")
	ErrAssemblyClosePanic                = errors.New("cli: assembly close owner panicked")
	ErrAssemblyClientCompletionUnknown   = errors.New("cli: client shutdown completion is unknown")
	ErrAssemblyResourceCompletionUnknown = errors.New("cli: assembly resource completion is unknown")
)

type assemblyClosePendingError struct {
	reason string
	cause  error
	done   <-chan struct{}
}

func (e *assemblyClosePendingError) Error() string {
	if e == nil {
		return ErrAssemblyClosePending.Error()
	}
	if e.reason == "" {
		return ErrAssemblyClosePending.Error()
	}
	return fmt.Sprintf("%s: %s", ErrAssemblyClosePending, e.reason)
}

func (e *assemblyClosePendingError) Unwrap() error {
	if e == nil {
		return ErrAssemblyClosePending
	}
	return errors.Join(ErrAssemblyClosePending, e.cause)
}

func (e *assemblyClosePendingError) Done() <-chan struct{} {
	if e == nil {
		return nil
	}
	return e.done
}

// AssemblyCloseCompletions returns the actual completion channels carried by
// Assembly close-pending errors in err's wrapping or join tree. Unrelated
// errors with a Done method are ignored. ok is false when no Assembly pending
// error is present or any such error has no valid completion channel.
func AssemblyCloseCompletions(err error) (channels []<-chan struct{}, ok bool) {
	found := false
	valid := true
	seen := make(map[<-chan struct{}]struct{})
	var visit func(error)
	visit = func(current error) {
		if current == nil {
			return
		}
		if pending, isPending := current.(*assemblyClosePendingError); isPending {
			found = true
			if pending == nil || pending.done == nil {
				valid = false
			} else if _, exists := seen[pending.done]; !exists {
				seen[pending.done] = struct{}{}
				channels = append(channels, pending.done)
			}
		}
		if many, hasMany := current.(interface{ Unwrap() []error }); hasMany {
			for _, cause := range many.Unwrap() {
				visit(cause)
			}
			return
		}
		if one, hasOne := current.(interface{ Unwrap() error }); hasOne {
			visit(one.Unwrap())
		}
	}
	visit(err)
	return channels, found && valid && len(channels) > 0
}

type assemblyCloseOwner struct {
	startOnce sync.Once
	done      chan struct{}

	guard              func() core.RunSlotGuard
	mcpResources       *assemblyMCPResources
	httpServer         *acp.HTTPServer
	cancel             context.CancelFunc
	requestStop        func()
	closeResources     func(context.Context, <-chan struct{}) error
	interactiveStop    func()
	beforeSessionClose func(context.Context) error
	afterMemoryJoin    func()

	mu       sync.Mutex
	deadline time.Time
	err      error
}

func newAssemblyCloseOwner(
	guard func() core.RunSlotGuard,
	cancel context.CancelFunc,
	requestStop func(),
	closeResources func(context.Context, <-chan struct{}) error,
) *assemblyCloseOwner {
	return &assemblyCloseOwner{
		done:           make(chan struct{}),
		guard:          guard,
		cancel:         cancel,
		requestStop:    requestStop,
		closeResources: closeResources,
	}
}

func (o *assemblyCloseOwner) Close(ctx context.Context) error {
	if o == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	o.startOnce.Do(func() {
		startedAt := time.Now()
		o.mu.Lock()
		o.deadline = startedAt.Add(assemblyCloseBudget)
		o.mu.Unlock()
		if o.mcpResources != nil {
			// Publish the MCP reload admission fence before the asynchronous owner
			// can block in any stop callback.
			o.mcpResources.fenceReloadAdmission()
		}

		// Cancel linked Harness/TurnRunner inputs synchronously, before the
		// asynchronous owner invokes any potentially blocking resource callback.
		if o.cancel != nil {
			o.cancel()
		}

		var idle <-chan struct{}
		var startupErr error
		func() {
			defer func() {
				if recover() != nil {
					startupErr = ErrAssemblyClosePanic
				}
			}()
			if o.guard != nil {
				guard := o.guard()
				if guard != nil {
					// The Assembly's default guard is known to have a short, local,
					// non-callback fence. Publish it before returning from Close so a
					// blocked stop callback cannot leave a new-run admission window.
					if defaultGuard, ok := guard.(*core.DefaultRunSlotGuard); ok {
						idle = defaultGuard.CloseAdmission()
					}
				}
			}
		}()
		func() {
			defer func() {
				if recover() != nil {
					startupErr = ErrAssemblyClosePanic
				}
			}()
		}()
		func() {
			defer func() {
				if recover() != nil {
					startupErr = ErrAssemblyClosePanic
				}
			}()
			o.mu.Lock()
			httpServer := o.httpServer
			o.mu.Unlock()
			if httpServer != nil {
				// BeginStop performs only the server's local admission fence and
				// launches its own join owner; it never waits for callbacks.
				httpServer.SealAdmission()
			}
		}()
		go o.start(startedAt, idle, startupErr)
	})

	o.mu.Lock()
	deadline := o.deadline
	o.mu.Unlock()
	remaining := time.Until(deadline)
	if remaining <= 0 {
		select {
		case <-o.done:
			return o.result()
		default:
			return o.pending("owner budget expired")
		}
	}
	budgetTimer := time.NewTimer(remaining)
	defer budgetTimer.Stop()
	select {
	case <-o.done:
		return o.result()
	case <-ctx.Done():
		select {
		case <-o.done:
			return o.result()
		default:
			return o.pending(ctx.Err().Error())
		}
	case <-budgetTimer.C:
		select {
		case <-o.done:
			return o.result()
		default:
			return o.pending("owner budget expired")
		}
	}
}

func (o *assemblyCloseOwner) start(startedAt time.Time, idle <-chan struct{}, startupErr error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			o.markPending(ErrAssemblyClosePanic)
		}
	}()

	if startupErr != nil {
		o.markPending(startupErr)
		return
	}
	if guard := o.guard(); guard != nil && idle == nil {
		closer, ok := guard.(core.RunAdmissionCloser)
		if !ok {
			o.markPending(ErrAssemblyRunAdmissionUnavailable)
			return
		}
		idle = closer.CloseAdmission()
		if idle == nil {
			o.markPending(ErrAssemblyRunAdmissionUnavailable)
			return
		}
	}
	if o.requestStop != nil {
		o.requestStop()
	}
	o.mu.Lock()
	interactiveStop := o.interactiveStop
	o.mu.Unlock()
	if interactiveStop != nil {
		interactiveStop()
	}
	closeCtx, cancel := context.WithDeadline(context.Background(), startedAt.Add(assemblyCloseBudget))
	defer cancel()
	var err error
	if o.closeResources != nil {
		err = o.closeResources(closeCtx, idle)
	}
	// Missing positive completion or a panic leaves the same owner retained;
	// it must not signal that downstream dependencies are safe to release.
	if errors.Is(err, ErrAssemblyClientCompletionUnknown) ||
		errors.Is(err, ErrAssemblyResourceCompletionUnknown) ||
		errors.Is(err, ErrAssemblyClosePanic) {
		o.markPending(err)
		return
	}
	o.finish(err)
}

func (o *assemblyCloseOwner) setInteractiveCloseHooks(
	stop func(),
	beforeSessionClose func(context.Context) error,
	afterMemoryJoin func(),
) error {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.deadline.IsZero() {
		return ErrAssemblyClosePending
	}
	o.interactiveStop = stop
	o.beforeSessionClose = beforeSessionClose
	o.afterMemoryJoin = afterMemoryJoin
	return nil
}

func (o *assemblyCloseOwner) setHTTPServer(server *acp.HTTPServer) error {
	if o == nil || server == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.deadline.IsZero() {
		return ErrAssemblyClosePending
	}
	if o.httpServer != nil && o.httpServer != server {
		return errors.New("cli: assembly already owns an ACP HTTP server")
	}
	o.httpServer = server
	return nil
}

func (o *assemblyCloseOwner) stopHTTPServer(ctx context.Context) error {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	server := o.httpServer
	o.mu.Unlock()
	if server == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	err := server.Stop(ctx)
	var pending interface{ Done() <-chan struct{} }
	if errors.As(err, &pending) {
		done := pending.Done()
		if done == nil {
			return errors.Join(err, ErrAssemblyResourceCompletionUnknown)
		}
		// The Assembly close context only bounds the first wait. Keep this
		// owner and all dependent resources until the server proves it joined.
		<-done
		err = server.Stop(context.Background())
		var stillPending interface{ Done() <-chan struct{} }
		if errors.As(err, &stillPending) {
			return errors.Join(err, ErrAssemblyResourceCompletionUnknown)
		}
	}
	if errors.Is(err, acp.ErrHTTPServerCleanupPending) {
		return errors.Join(err, ErrAssemblyResourceCompletionUnknown)
	}
	return err
}

func (o *assemblyCloseOwner) drainInteractiveSession(ctx context.Context) error {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	drain := o.beforeSessionClose
	o.mu.Unlock()
	if drain == nil {
		return nil
	}
	return drain(ctx)
}

func (o *assemblyCloseOwner) finishInteractiveMemory() {
	if o == nil {
		return
	}
	o.mu.Lock()
	afterJoin := o.afterMemoryJoin
	o.mu.Unlock()
	if afterJoin != nil {
		afterJoin()
	}
}

func closeAssemblyForCommand(ctx context.Context, assembly *AgentAssembly, command string) error {
	if assembly == nil {
		return nil
	}
	if err := assembly.Close(ctx); err != nil {
		return newExecutionError(command+": close assembly", err)
	}
	return nil
}

func (o *assemblyCloseOwner) result() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.err
}

func (o *assemblyCloseOwner) pending(reason string) error {
	o.mu.Lock()
	lastErr := o.err
	o.mu.Unlock()
	return &assemblyClosePendingError{reason: reason, cause: lastErr, done: o.done}
}

func (o *assemblyCloseOwner) markPending(err error) {
	o.mu.Lock()
	o.err = err
	o.mu.Unlock()
}

func (o *assemblyCloseOwner) finish(err error) {
	o.mu.Lock()
	o.err = err
	o.mu.Unlock()
	select {
	case <-o.done:
	default:
		close(o.done)
	}
}

type assemblyMCPResources struct {
	mu        sync.Mutex
	closing   bool
	clients   []mcp.MCPClient
	reloaders []mcp.HotReloader
	sources   map[string]*assemblyMCPSourceState
	staged    map[*assemblyMCPGeneration]struct{}
	retired   map[*assemblyMCPGeneration]struct{}
}

func (r *assemblyMCPResources) addClient(client mcp.MCPClient) {
	if client == nil {
		return
	}
	r.mu.Lock()
	r.clients = append(r.clients, client)
	r.mu.Unlock()
}

func (r *assemblyMCPResources) addReloader(reloader mcp.HotReloader) error {
	if reloader == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closing {
		return mcp.ErrHotReloadPending
	}
	r.reloaders = append(r.reloaders, reloader)
	return nil
}

func (r *assemblyMCPResources) fenceReloadAdmission() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.closing = true
	r.mu.Unlock()
}

func (r *assemblyMCPResources) requestStopReloaders() {
	r.mu.Lock()
	r.closing = true
	reloaders := append([]mcp.HotReloader(nil), r.reloaders...)
	r.mu.Unlock()
	for _, reloader := range reloaders {
		if lifecycle, ok := reloader.(mcp.HotReloadLifecycle); ok {
			lifecycle.RequestStop()
		}
	}
}

func (r *assemblyMCPResources) Reload(ctx context.Context) (int, error) {
	r.mu.Lock()
	if r.closing {
		r.mu.Unlock()
		return 0, mcp.ErrHotReloadPending
	}
	reloaders := append([]mcp.HotReloader(nil), r.reloaders...)
	r.mu.Unlock()
	var errs []error
	for _, reloader := range reloaders {
		if err := reloader.Reload(ctx); err != nil {
			errs = append(errs, fmt.Errorf("mcp reloader %q: %w", reloader.Name(), err))
		}
	}
	return len(reloaders), errors.Join(errs...)
}

func (r *assemblyMCPResources) waitReloaders(ctx context.Context) error {
	r.mu.Lock()
	r.closing = true
	reloaders := append([]mcp.HotReloader(nil), r.reloaders...)
	r.mu.Unlock()

	var errs []error
	for _, reloader := range reloaders {
		if lifecycle, ok := reloader.(mcp.HotReloadLifecycle); ok {
			done := lifecycle.Done()
			if done == nil {
				return errors.Join(errors.Join(errs...), fmt.Errorf("mcp reloader %q: %w", reloader.Name(), ErrAssemblyClientCompletionUnknown))
			}
			_ = lifecycle.Wait(ctx)
			select {
			case <-done:
			case <-ctx.Done():
				<-done
			}
			if err := lifecycle.Wait(context.Background()); err != nil {
				errs = append(errs, fmt.Errorf("mcp reloader %q: %w", reloader.Name(), err))
			}
			continue
		}
		stopErr := reloader.Stop()
		return errors.Join(
			errors.Join(errs...),
			fmt.Errorf("mcp reloader %q: %w", reloader.Name(), errors.Join(stopErr, ErrAssemblyClientCompletionUnknown)),
		)
	}
	return errors.Join(errs...)
}

func (r *assemblyMCPResources) closeClients(ctx context.Context) error {
	if err := r.closeMCPGenerations(ctx); err != nil {
		if assemblyCompletionUnknown(err) {
			return err
		}
	}
	r.mu.Lock()
	clients := append([]mcp.MCPClient(nil), r.clients...)
	r.mu.Unlock()
	var errs []error
	for _, client := range clients {
		if err := disconnectAssemblyMCPClient(ctx, client); err != nil {
			errs = append(errs, err)
			if assemblyCompletionUnknown(err) {
				return errors.Join(errs...)
			}
		}
	}
	return errors.Join(errs...)
}

func disconnectAssemblyMCPClient(ctx context.Context, client mcp.MCPClient) error {
	if client == nil {
		return nil
	}
	err := client.Disconnect(ctx)
	var pending interface{ Done() <-chan struct{} }
	if errors.As(err, &pending) {
		done := pending.Done()
		if done == nil {
			return errors.Join(err, ErrAssemblyClientCompletionUnknown)
		}
		select {
		case <-done:
		case <-ctx.Done():
			<-done
		}
		// Pending is a waiter result. Re-entering the idempotent client close
		// after its positive completion signal retrieves the terminal result.
		terminalErr := client.Disconnect(context.Background())
		var stillPending interface{ Done() <-chan struct{} }
		if errors.As(terminalErr, &stillPending) {
			return errors.Join(terminalErr, ErrAssemblyClientCompletionUnknown)
		}
		return terminalErr
	}
	if err != nil {
		if _, ok := client.(*mcp.HTTPClientAdapter); !ok {
			if _, ok := client.(*mcp.OfficialSDKAdapter); !ok {
				return errors.Join(err, ErrAssemblyClientCompletionUnknown)
			}
		}
		return err
	}
	// The default HTTP adapter has no background session worker, and the SDK
	// adapter's Disconnect synchronously closes and reaps its owned transport.
	// Other MCPClient implementations must expose a completion signal before
	// the Assembly can release their dependencies.
	switch client.(type) {
	case *mcp.HTTPClientAdapter, *mcp.OfficialSDKAdapter:
		return nil
	default:
		return ErrAssemblyClientCompletionUnknown
	}
}

func (s *assembleState) requestStopSources() {
	if s.memoryRuntime != nil && s.memoryRuntime.worker != nil {
		s.memoryRuntime.worker.Stop()
	}
	s.closeOwner.finishInteractiveMemory()
	if s.mcpResources != nil {
		s.mcpResources.requestStopReloaders()
	}
	if s.worktreeStopSignal != nil {
		s.worktreeStopSignal()
	}
}

func (s *assembleState) closeResources(ctx context.Context, idle <-chan struct{}) error {
	var errs []error
	if err := s.closeOwner.stopHTTPServer(ctx); err != nil {
		if errors.Is(err, ErrAssemblyResourceCompletionUnknown) {
			return err
		}
		errs = append(errs, fmt.Errorf("stop ACP HTTP server: %w", err))
	}

	if idle != nil {
		<-idle
	}
	if err := s.joinMemory(ctx); err != nil {
		errs = append(errs, err)
	}

	if s.modelSelector != nil {
		disposer, err := s.modelSelector.SealAndDetach(ctx)
		if err != nil && ctx.Err() != nil {
			// The owner budget is a waiter/cooperative deadline. Keep the same
			// owner on the selector gate until the active builder actually exits.
			disposer, err = s.modelSelector.SealAndDetach(context.Background())
		}
		if disposer == nil {
			return errors.Join(ErrAssemblyResourceCompletionUnknown, err, errors.New("model selector returned no disposer"))
		}
		s.modelDisposer = disposer
		if err != nil {
			errs = append(errs, fmt.Errorf("seal model selector: %w", err))
		}
	} else if s.primaryModelCleanup != nil {
		cleanup := s.primaryModelCleanup
		s.modelDisposer = func() error {
			cleanup()
			return nil
		}
	}

	if s.mcpResources != nil {
		if err := s.mcpResources.waitReloaders(ctx); err != nil {
			if assemblyCompletionUnknown(err) {
				return err
			}
			errs = append(errs, err)
		}
	}

	for i := len(s.pluginManagers) - 1; i >= 0; i-- {
		manager := s.pluginManagers[i]
		shutdownErr := manager.Shutdown(ctx)
		completed, waitErr := manager.WaitShutdown(ctx)
		if !completed {
			completed, waitErr = manager.WaitShutdown(context.Background())
		}
		if !completed {
			return errors.Join(append(errs,
				fmt.Errorf("plugin shutdown completion unavailable: %w", errors.Join(waitErr, shutdownErr, ErrAssemblyResourceCompletionUnknown)),
			)...)
		}
		if err := errors.Join(shutdownErr, waitErr); err != nil {
			errs = append(errs, err)
		}
	}

	// Extension shutdown callbacks may still use their registered MCP client
	// adapters, so only disconnect clients after every PluginManager joined.
	if s.mcpResources != nil {
		if err := s.mcpResources.closeClients(ctx); err != nil {
			if assemblyCompletionUnknown(err) {
				return errors.Join(errors.Join(errs...), err)
			}
			errs = append(errs, err)
		}
	}

	if s.acpAdapter != nil {
		s.acpAdapter.Close()
	}
	if s.acpClient != nil {
		if s.acpConnectErr != nil {
			if err := waitACPConnect(s.acpClient, s.acpConnectErr); err != nil {
				return errors.Join(errors.Join(errs...), fmt.Errorf("join ACP connect: %w", err))
			}
		}
		if err := disconnectACPClient(ctx, s.acpClient); err != nil {
			if assemblyCompletionUnknown(err) {
				return errors.Join(errors.Join(errs...), fmt.Errorf("disconnect ACP client: %w", err))
			}
			errs = append(errs, fmt.Errorf("disconnect ACP client: %w", err))
		}
	}

	for i := len(s.cleanupList) - 1; i >= 0; i-- {
		if err := safeAssemblyCleanup(ctx, s.cleanupList[i]); err != nil {
			if errors.Is(err, ErrAssemblyClosePanic) {
				return errors.Join(errors.Join(errs...), err)
			}
			errs = append(errs, err)
		}
	}

	if s.durableSession != nil {
		if err := s.durableSession.requestClose(ctx); err != nil {
			if errors.Is(err, ErrDurableSessionClosePending) {
				return errors.Join(errors.Join(errs...), fmt.Errorf("close durable CLI session: %w", errors.Join(err, ErrAssemblyResourceCompletionUnknown)))
			}
			errs = append(errs, fmt.Errorf("close durable CLI session: %w", err))
		}
	}
	if s.sessionStore != nil {
		if err := s.closeOwner.drainInteractiveSession(ctx); err != nil {
			errs = append(errs, fmt.Errorf("flush interactive session: %w", err))
		}
		if err := s.sessionStore.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	// Memory extraction may publish to the event bus while completing; join it
	// before closing either producer-side dependency, even when this Assembly
	// has no persistent memory store.
	s.memWG.Wait()
	s.closeOwner.finishInteractiveMemory()
	if s.memStore != nil {
		if err := s.memStore.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if s.eventBus != nil {
		s.eventBus.Close()
	}

	if s.modelDisposer != nil {
		if err := safeAssemblyDisposer(s.modelDisposer); err != nil {
			return errors.Join(errors.Join(errs...), fmt.Errorf("model disposal completion unknown: %w", errors.Join(err, ErrAssemblyResourceCompletionUnknown)))
		}
		s.modelDisposer = nil
	}
	if s.smallModelCleanup != nil {
		if err := safeAssemblyCleanup(ctx, func(context.Context) error {
			s.smallModelCleanup()
			return nil
		}); err != nil {
			if errors.Is(err, ErrAssemblyClosePanic) {
				return errors.Join(errors.Join(errs...), err)
			}
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func assemblyCompletionUnknown(err error) bool {
	return errors.Is(err, ErrAssemblyClientCompletionUnknown) || errors.Is(err, ErrAssemblyResourceCompletionUnknown)
}

func safeAssemblyCleanup(ctx context.Context, cleanup func(context.Context) error) (err error) {
	defer func() {
		if recover() != nil {
			err = ErrAssemblyClosePanic
		}
	}()
	return cleanup(ctx)
}

func safeAssemblyDisposer(dispose func() error) (err error) {
	defer func() {
		if recover() != nil {
			err = ErrAssemblyClosePanic
		}
	}()
	return dispose()
}

func waitACPConnect(client acp.ACPClient, err error) error {
	var pending interface{ Done() <-chan struct{} }
	if err == nil {
		return nil
	}
	if errors.As(err, &pending) {
		done := pending.Done()
		if done == nil {
			return ErrAssemblyClientCompletionUnknown
		}
		<-done
		if acp.IsBuiltInHTTPClientLocallyJoined(client) {
			return nil
		}
		if _, ok := client.(*acp.StdioAdapter); ok {
			return nil
		}
		return errors.Join(err, ErrAssemblyClientCompletionUnknown)
	}
	if _, ok := client.(*acp.StdioAdapter); ok {
		// The stdio adapter returns ordinary errors only after its connect
		// setup has unwound; any still-running cleanup is represented by the
		// typed Done-bearing pending error above.
		return nil
	}
	if acp.IsBuiltInHTTPClientLocallyJoined(client) {
		return nil
	}
	return errors.Join(err, ErrAssemblyClientCompletionUnknown)
}

func disconnectACPClient(ctx context.Context, client acp.ACPClient) error {
	if client == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	callCtx := ctx
	for {
		err := client.Disconnect(callCtx)
		var pending interface{ Done() <-chan struct{} }
		if errors.As(err, &pending) {
			done := pending.Done()
			if done == nil {
				return errors.Join(err, ErrAssemblyClientCompletionUnknown)
			}
			<-done
			callCtx = context.Background()
			continue
		}
		if _, ok := client.(*acp.StdioAdapter); ok || acp.IsBuiltInHTTPClientLocallyJoined(client) {
			return err
		}
		return errors.Join(err, ErrAssemblyClientCompletionUnknown)
	}
}
