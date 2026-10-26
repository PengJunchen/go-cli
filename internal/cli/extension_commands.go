package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/tools"
)

type extensionCommand struct {
	name    string
	run     func([]string) error
	binding tools.ProjectTrustBinding
}

func (c *extensionCommand) Name() string     { return c.name }
func (c *extensionCommand) Synopsis() string { return "Extension command" }
func (c *extensionCommand) Run(ctx context.Context, _ Config, args []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.binding != nil && !c.binding.Valid() {
		return errors.New("cli: extension command trust revoked")
	}
	err := c.run(append([]string(nil), args...))
	if c.binding != nil && !c.binding.Valid() {
		return errors.New("cli: extension command trust revoked; external outcome may be unknown")
	}
	return err
}

// registerExtensionCommands loads only the extension control plane, before
// routing an unknown command. Provider/tool consumers share the same bootstrap
// contract when a full Assembly is needed.
func registerExtensionCommands(ctx context.Context, cfg Config, out io.Writer, registry CommandRegistry) (func(), error) {
	rc, ok := cfg.(*config.Config)
	if !ok || rc == nil || rc.Extensions.Enabled == nil || !*rc.Extensions.Enabled {
		return func() {}, nil
	}
	state, _ := newAssembleState(ctx, rc, "", "", out)
	if err := state.prepareExtensions(); err != nil {
		return nil, errors.Join(err, state.runCleanup())
	}
	if state.extensionManager != nil {
		for _, command := range state.extensionManager.CommandRegistrations() {
			if _, exists := registry.Get(command.Name); exists {
				return nil, errors.Join(fmt.Errorf("cli: extension command conflicts with registered command %q", command.Name), state.runCleanup())
			}
			if err := registry.Register(&extensionCommand{name: command.Name, run: command.Run, binding: state.extensionBinding}); err != nil {
				return nil, errors.Join(err, state.runCleanup())
			}
		}
	}
	return func() { _ = state.runCleanup() }, nil
}
