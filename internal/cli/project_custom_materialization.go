package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/pengjunchen/go-cli/internal/config"
)

// materializeProjectCustomCommand uses only the already verified entrypoint
// bytes. Interpreters, sibling files and other argv dependencies are not pinned.
func materializeProjectCustomCommand(cfg *config.Config, tool config.CustomToolConfig) (string, string, error) {
	verified := cfg.VerifiedProjectSources()
	if verified == nil || len(tool.Command) == 0 {
		return "", "", errors.New("verified custom command source is unavailable")
	}
	command := tool.Command[0]
	if !filepath.IsAbs(command) && !strings.ContainsAny(command, `/\`) {
		resolved, err := exec.LookPath(command)
		if err != nil {
			return "", "", fmt.Errorf("resolve captured custom entrypoint: %w", err)
		}
		command = resolved
	} else if !filepath.IsAbs(command) {
		working := verified.CanonicalRoot()
		if tool.WorkingDir != "" {
			if filepath.IsAbs(tool.WorkingDir) {
				working = tool.WorkingDir
			} else {
				working = filepath.Join(working, tool.WorkingDir)
			}
		}
		command = filepath.Join(working, command)
	}
	relative, err := filepath.Rel(verified.CanonicalRoot(), filepath.Clean(command))
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", "", errors.New("custom entrypoint is outside verified workspace")
	}
	data, present := verified.SourceBytes("custom-tools/" + filepath.ToSlash(relative))
	if !present {
		return "", "", errors.New("captured custom entrypoint bytes are unavailable")
	}
	directory, err := os.MkdirTemp("", "go-cli-custom-command-")
	if err != nil {
		return "", "", fmt.Errorf("create private custom executable directory: %w", err)
	}
	path := filepath.Join(directory, filepath.Base(command))
	if err := os.WriteFile(path, data, 0o500); err != nil {
		_ = os.RemoveAll(directory)
		return "", "", fmt.Errorf("materialize captured custom executable: %w", err)
	}
	return path, directory, nil
}
