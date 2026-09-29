package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pengjunchen/go-cli/internal/approval"
	"github.com/pengjunchen/go-cli/internal/config"
)

type trustCommand struct {
	manager    approval.TrustManager
	in         io.Reader
	out        io.Writer
	isTerminal func() bool
}

const (
	trustActionGrant  = "grant"
	trustActionList   = "list"
	trustActionRevoke = "revoke"
)

var _ Command = (*trustCommand)(nil)

// NewTrustCommand creates the trust grant/list/revoke command. Grant requires
// an interactive terminal; list and revoke are safe to run non-interactively.
func NewTrustCommand(manager approval.TrustManager, input io.Reader, output io.Writer, isTerminal func() bool) Command {
	if input == nil {
		input = strings.NewReader("")
	}
	if output == nil {
		output = io.Discard
	}
	return &trustCommand{manager: manager, in: input, out: output, isTerminal: isTerminal}
}

func (c *trustCommand) Name() string     { return "trust" }
func (c *trustCommand) Synopsis() string { return "Manage trusted project source manifests" }

func (c *trustCommand) Run(ctx context.Context, cfg Config, args []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(args) == 0 {
		return newUsageError("usage: go-cli trust <grant|list|revoke>")
	}
	switch args[0] {
	case trustActionGrant:
		if len(args) != 1 {
			return newUsageError("usage: go-cli trust grant")
		}
		return c.grant(ctx, cfg)
	case trustActionList:
		if len(args) != 1 {
			return newUsageError("usage: go-cli trust list")
		}
		return c.list()
	case trustActionRevoke:
		if len(args) != 2 {
			return newUsageError("usage: go-cli trust revoke <canonical-root>")
		}
		return c.revoke(ctx, args[1])
	default:
		return newUsageError("unknown trust action: %s", args[0])
	}
}

func (c *trustCommand) grant(ctx context.Context, cfg Config) error {
	if c.isTerminal == nil || !c.isTerminal() {
		return errors.New("trust grant requires an interactive terminal")
	}
	loaded, ok := cfg.(*config.Config)
	if !ok || loaded == nil {
		return errors.New("trust grant requires the startup project capture")
	}
	snapshot := loaded.CapturedProjectSourcesForGrant()
	if snapshot == nil || snapshot.CanonicalRoot() == "" || snapshot.ManifestDigest() == "" {
		return errors.New("trust grant is unavailable because project source capture failed")
	}
	frozen, ok := c.manager.(approval.FrozenProjectSourceTrust)
	if !ok {
		return errors.New("trust grant requires a frozen-source trust manager")
	}
	if err := c.writeGrantSummary(snapshot); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(c.out, "Type TRUST to confirm grant:"); err != nil {
		return fmt.Errorf("cli: write trust grant prompt: %w", err)
	}
	line, readErr := bufio.NewReader(c.in).ReadString('\n')
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return fmt.Errorf("cli: read trust grant confirmation: %w", readErr)
	}
	if strings.TrimSpace(line) != "TRUST" {
		_, err := fmt.Fprintln(c.out, "Grant canceled")
		if err != nil {
			return fmt.Errorf("cli: write trust cancellation: %w", err)
		}
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := frozen.TrustProjectSourceSnapshot(ctx, snapshot); err != nil {
		return fmt.Errorf("cli: persist project source grant: %w", err)
	}
	if _, err := fmt.Fprintf(c.out, "Saved source manifest for %s. It becomes active only after a later invocation verifies it.\n", snapshot.CanonicalRoot()); err != nil {
		return fmt.Errorf("cli: write trust grant result: %w", err)
	}
	return nil
}

func (c *trustCommand) writeGrantSummary(snapshot *approval.ProjectSourceSnapshot) error {
	if _, err := fmt.Fprintf(c.out, "Project root: %s\nManifest SHA-256: %s\nSources:\n", snapshot.CanonicalRoot(), snapshot.ManifestDigest()); err != nil {
		return fmt.Errorf("cli: write trust source summary: %w", err)
	}
	sources := snapshot.SourceRecords()
	sort.Slice(sources, func(i, j int) bool {
		leftOrder, leftIsConfig := trustConfigSourceOrder(sources[i].RelativePath)
		rightOrder, rightIsConfig := trustConfigSourceOrder(sources[j].RelativePath)
		if leftIsConfig != rightIsConfig {
			return leftIsConfig
		}
		if leftIsConfig && leftOrder != rightOrder {
			return leftOrder < rightOrder
		}
		return sources[i].ID < sources[j].ID
	})
	for _, source := range sources {
		state := "absent"
		if source.Present {
			state = "present"
		}
		if _, err := fmt.Fprintf(c.out, "  file %s (%s, %s)", source.ID, source.RelativePath, state); err != nil {
			return fmt.Errorf("cli: write trust source summary: %w", err)
		}
		if source.Digest != "" {
			if _, err := fmt.Fprintf(c.out, " sha256=%s", source.Digest); err != nil {
				return fmt.Errorf("cli: write trust source summary: %w", err)
			}
		}
		if _, err := fmt.Fprintln(c.out); err != nil {
			return fmt.Errorf("cli: write trust source summary: %w", err)
		}
	}
	for _, directory := range snapshot.DirectoryRecords() {
		state := "absent"
		if directory.Present {
			state = "present"
		}
		if _, err := fmt.Fprintf(c.out, "  directory %s (%s, %s)\n", directory.ID, directory.RelativePath, state); err != nil {
			return fmt.Errorf("cli: write trust directory summary: %w", err)
		}
		members, _ := snapshot.DirectoryMembers(directory.ID)
		for _, member := range members {
			if _, err := fmt.Fprintf(c.out, "    %s\n", member); err != nil {
				return fmt.Errorf("cli: write trust directory members: %w", err)
			}
		}
	}
	return nil
}

func trustConfigSourceOrder(path string) (int, bool) {
	switch path {
	case ".go-cli.yaml":
		return 0, true
	case ".go-cli.yml":
		return 1, true
	case ".go-cli.json":
		return 2, true
	case "go-cli.yaml":
		return 3, true
	case "go-cli.yml":
		return 4, true
	case "go-cli.json":
		return 5, true
	default:
		return 0, false
	}
}

func (c *trustCommand) list() error {
	reader, ok := c.manager.(approval.TrustEntryReader)
	if !ok {
		return errors.New("cli: trust manager does not expose stored trust entries")
	}
	entries, err := reader.LoadTrustEntries()
	if err != nil {
		return fmt.Errorf("cli: list trust entries: %w", err)
	}
	if len(entries) == 0 {
		_, err := fmt.Fprintln(c.out, "No trust entries.")
		if err != nil {
			return fmt.Errorf("cli: write trust list: %w", err)
		}
		return nil
	}
	paths := make([]string, 0, len(entries))
	for root := range entries {
		paths = append(paths, root)
	}
	sort.Strings(paths)
	now := time.Now()
	for _, root := range paths {
		entry := entries[root]
		if _, err := fmt.Fprintf(c.out, "%s\t%s\n", root, trustEntryListStatus(root, entry, now)); err != nil {
			return fmt.Errorf("cli: write trust list: %w", err)
		}
	}
	return nil
}

func trustEntryListStatus(root string, entry approval.TrustEntry, now time.Time) string {
	if entry.ManifestVersion == 0 {
		return "legacy path-only entry; does not authorize executable sources"
	}
	if entry.ManifestVersion != 1 {
		return fmt.Sprintf("unsupported manifest version %d; not authorized", entry.ManifestVersion)
	}
	if entry.Path != root || entry.CanonicalRoot != root || entry.ManifestDigest == "" {
		return "invalid manifest binding; not authorized"
	}
	if entry.ExpiresAt != "" {
		expiresAt, err := time.Parse(time.RFC3339, entry.ExpiresAt)
		if err != nil {
			return "invalid expiry; not authorized"
		}
		if now.After(expiresAt) {
			return "expired manifest; not authorized"
		}
	}
	return fmt.Sprintf("manifest v%d %s; current sources still require an exact match", entry.ManifestVersion, entry.ManifestDigest)
}

func (c *trustCommand) revoke(ctx context.Context, rawRoot string) error {
	if c.manager == nil {
		return errors.New("cli: trust manager is unavailable")
	}
	root, err := canonicalRootArgument(rawRoot)
	if err != nil {
		return err
	}
	if err := c.manager.RevokeTrust(ctx, root); err != nil {
		return fmt.Errorf("cli: revoke trust for %s: %w", root, err)
	}
	if _, err := fmt.Fprintf(c.out, "Revoked trust for %s\n", root); err != nil {
		return fmt.Errorf("cli: write trust revoke result: %w", err)
	}
	return nil
}

func canonicalRootArgument(raw string) (string, error) {
	if !filepath.IsAbs(raw) || filepath.Clean(raw) != raw {
		return "", errors.New("trust revoke requires an absolute canonical root")
	}
	canonical, err := filepath.EvalSymlinks(raw)
	if err == nil {
		if filepath.Clean(canonical) != raw {
			return "", errors.New("trust revoke path is not the canonical root")
		}
		info, statErr := os.Stat(canonical)
		if statErr != nil {
			return "", fmt.Errorf("cli: inspect revoke root: %w", statErr)
		}
		if !info.IsDir() {
			return "", errors.New("trust revoke path is not a directory")
		}
		return canonical, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("cli: canonicalize revoke root: %w", err)
	}
	ancestor := filepath.Dir(raw)
	for {
		canonicalAncestor, ancestorErr := filepath.EvalSymlinks(ancestor)
		if ancestorErr == nil {
			if filepath.Clean(canonicalAncestor) != ancestor {
				return "", errors.New("trust revoke path is not the canonical root")
			}
			info, statErr := os.Stat(ancestor)
			if statErr != nil {
				return "", fmt.Errorf("cli: inspect revoke root parent: %w", statErr)
			}
			if !info.IsDir() {
				return "", errors.New("trust revoke parent is not a directory")
			}
			return raw, nil
		}
		if !errors.Is(ancestorErr, os.ErrNotExist) {
			return "", fmt.Errorf("cli: canonicalize revoke root parent: %w", ancestorErr)
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return "", errors.New("trust revoke path has no existing canonical ancestor")
		}
		ancestor = parent
	}
}
