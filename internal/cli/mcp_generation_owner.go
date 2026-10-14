package cli

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/pengjunchen/go-cli/internal/mcp"
	"github.com/pengjunchen/go-cli/internal/tools"
)

type assemblyMCPSourceState struct {
	generation uint64
	current    *assemblyMCPGeneration
}

// assemblyMCPGeneration owns one source's client set and exact lease handles.
// Once retired, its clients are disconnected only after all committed calls
// that acquired one of those handles have completed.
type assemblyMCPGeneration struct {
	owner      *assemblyMCPResources
	sourceID   string
	generation uint64
	clients    []mcp.MCPClient
	leases     []*mcp.MCPClientGeneration
	done       chan struct{}
	cleanup    sync.Once
	cleanupErr error
}

func newAssemblyMCPGeneration(sourceID string, generation uint64) *assemblyMCPGeneration {
	return &assemblyMCPGeneration{
		sourceID:   sourceID,
		generation: generation,
		done:       make(chan struct{}),
	}
}

func (r *assemblyMCPResources) nextMCPGeneration(sourceID string) uint64 {
	if r == nil {
		return 1
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.sources[sourceID]
	if state == nil {
		return 1
	}
	return state.generation + 1
}

func (r *assemblyMCPResources) stageMCPGeneration(candidate *assemblyMCPGeneration) error {
	if r == nil || candidate == nil {
		return errors.New("cli: MCP generation owner is unavailable")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closing {
		return mcp.ErrHotReloadPending
	}
	if r.staged == nil {
		r.staged = make(map[*assemblyMCPGeneration]struct{})
	}
	candidate.owner = r
	r.staged[candidate] = struct{}{}
	return nil
}

// publishMCPGeneration follows the generation lock order: the caller already
// owns DTR through update, then this method takes the owner lock while Publish
// advances the shared fence and backing registry. It performs no network work.
func (r *assemblyMCPResources) publishMCPGeneration(candidate *assemblyMCPGeneration, update *tools.MCPGenerationUpdate) error {
	if r == nil || candidate == nil || update == nil {
		if update != nil {
			update.Abort()
		}
		return errors.New("cli: MCP generation publication is unavailable")
	}
	r.mu.Lock()
	if r.closing {
		update.Abort()
		delete(r.staged, candidate)
		r.mu.Unlock()
		return mcp.ErrHotReloadPending
	}
	if err := update.Publish(); err != nil {
		// A checked publication rejection must fail closed for this source. The
		// held DTR transaction makes disabling the old generation atomic with
		// respect to deferred materialization and ordinary registration.
		disableErr := update.Disable()
		if disableErr != nil {
			update.Abort()
			r.mu.Unlock()
			return errors.Join(fmt.Errorf("cli: publish MCP generation: %w", err), fmt.Errorf("cli: disable rejected MCP source: %w", disableErr))
		}
		state := r.sourceStateLocked(candidate.sourceID)
		state.generation = candidate.generation
		old := state.current
		state.current = nil
		r.retireMCPGenerationLocked(old)
		r.mu.Unlock()
		return fmt.Errorf("cli: publish MCP generation; previous source generation disabled: %w", err)
	}
	state := r.sourceStateLocked(candidate.sourceID)
	old := state.current
	state.generation = candidate.generation
	state.current = candidate
	delete(r.staged, candidate)
	r.retireMCPGenerationLocked(old)
	r.mu.Unlock()
	return nil
}

func (r *assemblyMCPResources) disableMCPGeneration(sourceID string, generation uint64, update *tools.MCPGenerationUpdate) error {
	if r == nil || update == nil {
		if update != nil {
			update.Abort()
		}
		return errors.New("cli: MCP source disable is unavailable")
	}
	r.mu.Lock()
	if r.closing {
		update.Abort()
		r.mu.Unlock()
		return mcp.ErrHotReloadPending
	}
	if err := update.Disable(); err != nil {
		update.Abort()
		r.mu.Unlock()
		return fmt.Errorf("cli: disable MCP source: %w", err)
	}
	state := r.sourceStateLocked(sourceID)
	state.generation = generation
	old := state.current
	state.current = nil
	r.retireMCPGenerationLocked(old)
	r.mu.Unlock()
	return nil
}

func (r *assemblyMCPResources) sourceStateLocked(sourceID string) *assemblyMCPSourceState {
	if r.sources == nil {
		r.sources = make(map[string]*assemblyMCPSourceState)
	}
	state := r.sources[sourceID]
	if state == nil {
		state = &assemblyMCPSourceState{}
		r.sources[sourceID] = state
	}
	return state
}

func (r *assemblyMCPResources) retireMCPGenerationLocked(generation *assemblyMCPGeneration) {
	if generation == nil {
		return
	}
	if r.retired == nil {
		r.retired = make(map[*assemblyMCPGeneration]struct{})
	}
	generation.owner = r
	r.retired[generation] = struct{}{}
	generation.startCleanup()
}

func (g *assemblyMCPGeneration) startCleanup() {
	if g == nil {
		return
	}
	g.cleanup.Do(func() {
		for _, lease := range g.leases {
			lease.Retire()
		}
		go func() {
			for _, lease := range g.leases {
				<-lease.Retire()
			}
			var errs []error
			for _, client := range g.clients {
				if err := disconnectAssemblyMCPClient(context.Background(), client); err != nil {
					errs = append(errs, err)
				}
			}
			g.cleanupErr = errors.Join(errs...)
			if g.owner != nil {
				g.owner.mu.Lock()
				delete(g.owner.retired, g)
				g.owner.mu.Unlock()
			}
			close(g.done)
		}()
	})
}

func (g *assemblyMCPGeneration) wait() error {
	if g == nil {
		return nil
	}
	g.startCleanup()
	<-g.done
	return g.cleanupErr
}

func (r *assemblyMCPResources) cleanupMCPCandidate(candidate *assemblyMCPGeneration) error {
	if candidate == nil {
		return nil
	}
	if r != nil {
		r.mu.Lock()
		delete(r.staged, candidate)
		r.mu.Unlock()
	}
	return candidate.wait()
}

func (r *assemblyMCPResources) closeMCPGenerations(_ context.Context) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	r.closing = true
	groups := make(map[*assemblyMCPGeneration]struct{})
	for _, state := range r.sources {
		if state != nil && state.current != nil {
			groups[state.current] = struct{}{}
			r.retireMCPGenerationLocked(state.current)
			state.current = nil
		}
	}
	for group := range r.staged {
		groups[group] = struct{}{}
		group.startCleanup()
		delete(r.staged, group)
	}
	for group := range r.retired {
		groups[group] = struct{}{}
	}
	r.mu.Unlock()

	var errs []error
	for group := range groups {
		if err := group.wait(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
