package cli

import (
	"fmt"
	"sync"
)

// CommandRegistry is a collection of registered commands that can be resolved
// by name.
type CommandRegistry interface {
	// Register adds a command to the registry. Registering a command with a
	// name that already exists returns an error.
	Register(cmd Command) error
	// Get returns the command registered under name, reporting whether it is
	// present.
	Get(name string) (Command, bool)
	// List returns all registered commands in registration order.
	List() []Command
}

// DefaultCommandRegistry is the standard in-memory implementation of
// CommandRegistry. It is safe for concurrent use.
type DefaultCommandRegistry struct {
	mu    sync.RWMutex
	cmds  map[string]Command
	order []string
}

// NewDefaultCommandRegistry creates a new, empty default command registry.
func NewDefaultCommandRegistry() CommandRegistry {
	return &DefaultCommandRegistry{
		cmds: make(map[string]Command),
	}
}

// Register implements CommandRegistry.
func (r *DefaultCommandRegistry) Register(cmd Command) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	name := cmd.Name()
	if _, exists := r.cmds[name]; exists {
		return fmt.Errorf("command already registered: %s", name)
	}
	r.cmds[name] = cmd
	r.order = append(r.order, name)
	return nil
}

// Get implements CommandRegistry.
func (r *DefaultCommandRegistry) Get(name string) (Command, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	cmd, ok := r.cmds[name]
	return cmd, ok
}

// List implements CommandRegistry.
func (r *DefaultCommandRegistry) List() []Command {
	r.mu.RLock()
	defer r.mu.RUnlock()

	cmds := make([]Command, 0, len(r.order))
	for _, name := range r.order {
		cmds = append(cmds, r.cmds[name])
	}
	return cmds
}

var _ CommandRegistry = (*DefaultCommandRegistry)(nil)
