package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/llm"
)

// modelsCmd implements Command and lists models from the model registry.
type modelsCmd struct {
	out      io.Writer
	registry llm.ModelRegistry
}

// newModelsCmd creates a models command writing to out. When registry is nil
// the command builds its own ModelsDevRegistry on demand from configuration.
func newModelsCmd(out io.Writer) *modelsCmd {
	return &modelsCmd{out: out}
}

// Name implements Command.
func (c *modelsCmd) Name() string { return "models" }

// Synopsis implements Command.
func (c *modelsCmd) Synopsis() string { return "List available models from the model registry" }

// Run implements Command. With the "list" sub-argument (or no arguments) it
// prints every provider and its models in a table. When no registry is
// configured on the command, a ModelsDevRegistry is created from configuration
// and refreshed best-effort.
func (c *modelsCmd) Run(ctx context.Context, cfg Config, args []string) error {
	fs := flag.NewFlagSet("models", flag.ContinueOnError)
	fs.SetOutput(c.out)
	if err := fs.Parse(args); err != nil {
		return newUsageError("models: %v", err)
	}

	rest := fs.Args()
	if len(rest) > 0 && rest[0] != "list" {
		return newUsageError("models: unknown subcommand %q (use: models list)", rest[0])
	}

	reg := c.registry
	if reg == nil {
		reg = c.buildRegistryFromConfig(ctx, cfg)
	}

	providers := reg.Providers()
	if len(providers) == 0 {
		fmt.Fprintln(c.out, "No model registry data available. Enable model_registry in config or check network connectivity.")
		return nil
	}

	w := tabwriter.NewWriter(c.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "PROVIDER\tMODEL\tCONTEXT\tMAX OUT\tINPUT $/M\tOUTPUT $/M\tMODALITY")

	for _, p := range providers {
		models := reg.ModelsForProvider(p.ID)
		if len(models) == 0 {
			fmt.Fprintf(w, "%s\t(none)\t-\t-\t-\t-\t-\n", p.Name)
			continue
		}
		for _, m := range models {
			ctxStr := "-"
			if m.ContextWindow > 0 {
				ctxStr = fmt.Sprintf("%d", m.ContextWindow)
			}
			maxOutStr := "-"
			if m.MaxOutputTokens > 0 {
				maxOutStr = fmt.Sprintf("%d", m.MaxOutputTokens)
			}
			modality := m.Modality
			if modality == "" {
				modality = "-"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%.2f\t%.2f\t%s\n",
				p.Name, m.Name, ctxStr, maxOutStr, m.InputPrice, m.OutputPrice, modality)
		}
	}
	return w.Flush()
}

// buildRegistryFromConfig creates a ModelsDevRegistry using the cache path and
// TTL from the loaded configuration. Lazy loading (via Providers/Lookup) handles
// cache and network access, respecting the configured TTL.
func (c *modelsCmd) buildRegistryFromConfig(ctx context.Context, cfg Config) llm.ModelRegistry {
	var cachePath string
	var ttlHours int
	if rc, ok := cfg.(*config.Config); ok {
		cachePath = rc.ModelRegistry.CachePath
		ttlHours = rc.ModelRegistry.TTLHours
	}
	ttl := time.Duration(ttlHours) * time.Hour
	return llm.NewModelsDevRegistry(cachePath, ttl)
}

var _ Command = (*modelsCmd)(nil)
