package cli

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// SlashCommandHandler handles a single slash command. Handlers are registered
// in a SlashCommandRegistry and dispatched by handleSlashCommand.
type SlashCommandHandler interface {
	// Name is the canonical command name (without the leading "/").
	Name() string
	// Description is a short, user-facing summary shown by /help.
	Description() string
	// Handle executes the command. args are the tokens following the command
	// name. deps carries the runtime dependencies the handler needs via
	// domain accessor interfaces. The returned string is the pendingInput
	// (non-empty when the handler injects a prompt for the REPL loop).
	Handle(ctx context.Context, args []string, deps Dependencies) (string, error)
}

// SlashCommandRegistry manages slash command handlers. It is safe for
// concurrent use.
type SlashCommandRegistry struct {
	mu       sync.RWMutex
	handlers map[string]SlashCommandHandler
	aliases  map[string]string // alias -> command name
}

// NewSlashCommandRegistry returns an empty registry.
func NewSlashCommandRegistry() *SlashCommandRegistry {
	return &SlashCommandRegistry{
		handlers: make(map[string]SlashCommandHandler),
		aliases:  make(map[string]string),
	}
}

// Register adds a handler keyed by its Name. It returns an error when the
// handler is nil, has an empty name, or a command with that name is already
// registered.
func (r *SlashCommandRegistry) Register(h SlashCommandHandler) error {
	if h == nil {
		return fmt.Errorf("slash: cannot register nil handler")
	}
	name := h.Name()
	if name == "" {
		return fmt.Errorf("slash: handler name is empty")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.handlers[name]; exists {
		return fmt.Errorf("slash: command %q already registered", name)
	}
	r.handlers[name] = h
	return nil
}

// RegisterAlias maps alias to the registered command name. Aliases are
// resolved by Lookup.
func (r *SlashCommandRegistry) RegisterAlias(alias, name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.aliases[alias] = name
}

// Lookup returns the handler for name, resolving aliases first. The second
// value is false when no matching command or alias is registered.
func (r *SlashCommandRegistry) Lookup(name string) (SlashCommandHandler, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if h, ok := r.handlers[name]; ok {
		return h, true
	}
	if realName, ok := r.aliases[name]; ok {
		if h, ok := r.handlers[realName]; ok {
			return h, true
		}
	}
	return nil, false
}

// List returns all registered handlers sorted by name. It is intended for
// /help output.
func (r *SlashCommandRegistry) List() []SlashCommandHandler {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]SlashCommandHandler, 0, len(r.handlers))
	for _, h := range r.handlers {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Name() < out[j].Name()
	})
	return out
}

// Names returns the canonical names of all registered handlers, sorted.
func (r *SlashCommandRegistry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.handlers))
	for name := range r.handlers {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
