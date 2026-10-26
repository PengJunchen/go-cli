package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/pengjunchen/go-cli/internal/config"
)

var projectExtensionStageCache = struct {
	sync.Mutex
	root   string
	leases map[string]int
}{leases: make(map[string]int)}

// materializeProjectExtensionPaths writes every verified project extension to
// a private, content-addressed path before any loader is called. The stable
// path lets Go's process-wide plugin cache reuse the same module after a fresh
// assembly; project paths are never resolved or reopened after verification.
func materializeProjectExtensionPaths(cfg *config.Config, configuredPaths []string) ([]string, func(context.Context) error, error) {
	type source struct {
		data []byte
		name string
	}
	sources := make([]source, 0, len(configuredPaths))
	for _, configuredPath := range configuredPaths {
		data, ok := cfg.ProjectExtensionBytesForVerifiedPath(configuredPath)
		if !ok {
			return nil, nil, fmt.Errorf("verified bytes are unavailable for project extension %q", configuredPath)
		}
		extension := filepath.Ext(configuredPath)
		if strings.HasSuffix(configuredPath, ".rpc.json") {
			extension = ".rpc.json"
			verified := cfg.VerifiedProjectSources()
			if verified == nil {
				return nil, nil, errors.New("RPC descriptor requires verified project sources")
			}
			var err error
			data, err = normalizeProjectRPCDescriptor(data, verified.CanonicalRoot())
			if err != nil {
				return nil, nil, err
			}
		}
		digest := sha256.Sum256(data)
		if strings.ContainsAny(extension, string(filepath.Separator)+string(filepath.ListSeparator)) {
			return nil, nil, fmt.Errorf("invalid project extension suffix for %q", configuredPath)
		}
		sources = append(sources, source{
			data: data,
			name: hex.EncodeToString(digest[:]) + extension,
		})
	}

	projectExtensionStageCache.Lock()
	defer projectExtensionStageCache.Unlock()
	root, err := ensureProjectExtensionStageRootLocked()
	if err != nil {
		return nil, nil, err
	}
	materialized := make([]string, 0, len(sources))
	leased := make([]string, 0, len(sources))
	seen := make(map[string]struct{}, len(sources))
	for _, source := range sources {
		path := filepath.Join(root, source.name)
		if _, exists := seen[path]; !exists {
			if err := writeOrVerifyProjectExtensionLocked(path, source.data); err != nil {
				var releaseErr error
				for _, prior := range leased {
					releaseErr = errors.Join(releaseErr, releaseProjectExtensionPathLocked(prior))
				}
				return nil, nil, errors.Join(err, releaseErr)
			}
			projectExtensionStageCache.leases[path]++
			leased = append(leased, path)
			seen[path] = struct{}{}
		}
		materialized = append(materialized, path)
	}

	var releaseOnce sync.Once
	cleanup := func(context.Context) error {
		var cleanupErr error
		releaseOnce.Do(func() {
			projectExtensionStageCache.Lock()
			defer projectExtensionStageCache.Unlock()
			for _, path := range leased {
				if err := releaseProjectExtensionPathLocked(path); err != nil {
					cleanupErr = errors.Join(cleanupErr, err)
				}
			}
		})
		return cleanupErr
	}
	return materialized, cleanup, nil
}

func ensureProjectExtensionStageRootLocked() (string, error) {
	if projectExtensionStageCache.root == "" {
		dir, err := os.MkdirTemp("", "go-cli-project-extensions-*")
		if err != nil {
			return "", fmt.Errorf("create private extension directory: %w", err)
		}
		absolute, err := filepath.Abs(dir)
		if err != nil {
			_ = os.RemoveAll(dir)
			return "", fmt.Errorf("make private extension directory absolute: %w", err)
		}
		canonical, err := filepath.EvalSymlinks(absolute)
		if err != nil {
			_ = os.RemoveAll(dir)
			return "", fmt.Errorf("canonicalize private extension directory: %w", err)
		}
		projectExtensionStageCache.root = filepath.Clean(canonical)
	}
	root := projectExtensionStageCache.root
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(root, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("create stable private extension directory: %w", err)
		}
		info, err = os.Lstat(root)
	}
	if err != nil {
		return "", fmt.Errorf("inspect private extension directory: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return "", errors.New("private extension directory has unexpected type or permissions")
	}
	if info.Mode().Perm() != 0o700 {
		if err := os.Chmod(root, 0o700); err != nil {
			return "", fmt.Errorf("set private extension directory permissions: %w", err)
		}
	}
	return root, nil
}

func writeOrVerifyProjectExtensionLocked(path string, data []byte) error {
	info, err := os.Lstat(path)
	if err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0o400 {
			return fmt.Errorf("private extension materialization %q has unexpected type or permissions", path)
		}
		current, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read private extension materialization: %w", err)
		}
		if !bytes.Equal(current, data) {
			return errors.New("private extension materialization does not match its content digest")
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect private extension materialization: %w", err)
	}
	if projectExtensionStageCache.leases[path] > 0 {
		return errors.New("leased private extension materialization disappeared")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create private extension file: %w", err)
	}
	written, writeErr := file.Write(data)
	if writeErr == nil && written != len(data) {
		writeErr = io.ErrShortWrite
	}
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("write private extension file: %w", err)
	}
	if err := os.Chmod(path, 0o400); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("make private extension file read-only: %w", err)
	}
	return nil
}

func releaseProjectExtensionPathLocked(path string) error {
	count := projectExtensionStageCache.leases[path]
	if count <= 1 {
		delete(projectExtensionStageCache.leases, path)
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove private extension materialization: %w", err)
		}
		return nil
	}
	projectExtensionStageCache.leases[path] = count - 1
	return nil
}

func normalizeProjectRPCDescriptor(data []byte, projectRoot string) ([]byte, error) {
	var descriptor map[string]json.RawMessage
	if err := json.Unmarshal(data, &descriptor); err != nil {
		return nil, fmt.Errorf("cli: RPC descriptor: %w", err)
	}
	var cwd string
	if raw, ok := descriptor["cwd"]; ok {
		if err := json.Unmarshal(raw, &cwd); err != nil {
			return nil, errors.New("cli: RPC descriptor cwd must be a string")
		}
	}
	if cwd == "" {
		cwd = projectRoot
	} else if !filepath.IsAbs(cwd) {
		cwd = filepath.Join(projectRoot, cwd)
	}
	encoded, err := json.Marshal(cwd)
	if err != nil {
		return nil, err
	}
	descriptor["cwd"] = encoded
	var command []string
	if err := json.Unmarshal(descriptor["command"], &command); err != nil || len(command) == 0 {
		return nil, errors.New("cli: RPC descriptor command is required")
	}
	if !filepath.IsAbs(command[0]) {
		command[0] = filepath.Join(projectRoot, command[0])
	}
	encoded, err = json.Marshal(command)
	if err != nil {
		return nil, err
	}
	descriptor["command"] = encoded
	return json.Marshal(descriptor)
}
