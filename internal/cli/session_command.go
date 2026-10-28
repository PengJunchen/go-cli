package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/session"
)

type sessionCmd struct{ out io.Writer }

var _ Command = (*sessionCmd)(nil)

func newSessionCmd(out io.Writer) *sessionCmd { return &sessionCmd{out: out} }
func (*sessionCmd) Name() string              { return "session" }
func (*sessionCmd) Synopsis() string {
	return "Inspect saved sessions and explicitly import legacy history"
}

func (c *sessionCmd) Run(ctx context.Context, cfg Config, args []string) error {
	if len(args) == 0 {
		return newUsageError("session: use list, inspect-legacy <file>, or import-legacy --source <file> --store <directory> [--id <physical-id>]")
	}
	if args[0] == "inspect-legacy" {
		if len(args) != 2 {
			return newUsageError("session inspect-legacy requires one file")
		}
		inspection, err := session.InspectLegacySession(ctx, args[1])
		if err != nil {
			return newExecutionError("session: inspect legacy history", err)
		}
		_, err = fmt.Fprintf(c.out, "Legacy transcript: %d entries, %d branches.\nSHA256: %s\nSource was inspected read-only.\n", inspection.EntryCount, inspection.LeafCount, inspection.SHA256)
		return err
	}
	fs := flag.NewFlagSet("session "+args[0], flag.ContinueOnError)
	fs.SetOutput(c.out)
	var source, store, id string
	if rc, ok := cfg.(*config.Config); ok {
		store = rc.Session.StorePath
	}
	fs.StringVar(&store, "store", store, "durable session directory")
	fs.StringVar(&source, "source", "", "legacy JSONL source file")
	fs.StringVar(&id, "id", "", "new physical session ID (defaults to a fresh ID)")
	if err := fs.Parse(args[1:]); err != nil {
		return newUsageError("session: %v", err)
	}
	if len(fs.Args()) != 0 || store == "" {
		return newUsageError("session: --store <directory> is required and trailing arguments are not accepted")
	}
	if err := validateDurableSessionStorePath(store); err != nil {
		return err
	}
	switch args[0] {
	case "list":
		entries, err := session.ListSessionCatalog(ctx, store)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintln(c.out, "Physical sessions:"); err != nil {
			return err
		}
		for _, entry := range entries {
			if _, err := fmt.Fprintf(c.out, "  %s [%s] %s\n", entry.SessionID, entry.Status, truncatePreview(entry.Preview)); err != nil {
				return err
			}
		}
		return nil
	case "import-legacy":
		if source == "" {
			return newUsageError("session import-legacy: --source is required")
		}
		physical, err := session.ImportLegacySession(ctx, source, store, id)
		if err != nil {
			return newExecutionError("session: import legacy history", err)
		}
		_, err = fmt.Fprintf(c.out, "Imported physical session: %s\nResume with interactive --resume-session %s. Original source retained.\n", physical, physical)
		return err
	default:
		return errors.New("session: unknown subcommand")
	}
}
