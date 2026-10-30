package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// resolveApprovalAuditDir keeps the implicit approval journal outside the
// canonical configured workspace. Explicit AuditTrail paths are not resolved here.
func resolveApprovalAuditDir(homeSource, workspaceSource func() (string, error)) (string, error) {
	if homeSource == nil {
		return "", errors.New("approval audit: home source is nil")
	}
	home, err := homeSource()
	if err != nil {
		return "", fmt.Errorf("approval audit: obtain home: %w", err)
	}
	if home == "" || !filepath.IsAbs(home) {
		return "", errors.New("approval audit: home must be a nonempty absolute directory")
	}
	home, err = canonicalExistingDirectory(home)
	if err != nil {
		return "", fmt.Errorf("approval audit: resolve home: %w", err)
	}
	workspace, id, err := resolveWorkspaceIdentity(workspaceSource)
	if err != nil {
		return "", fmt.Errorf("approval audit: resolve workspace: %w", err)
	}
	info, err := os.Stat(workspace)
	if err != nil {
		return "", fmt.Errorf("approval audit: inspect workspace: %w", err)
	}
	if !info.IsDir() {
		return "", errors.New("approval audit: workspace must be a directory")
	}
	if approvalAuditInside(workspace, home) {
		return "", errors.New("approval audit: home resolves inside workspace")
	}
	defaultRoot := filepath.Join(home, ".go-cli")
	for _, path := range []string{home, defaultRoot, filepath.Join(defaultRoot, "audit"), filepath.Join(defaultRoot, "audit", id)} {
		info, inspectErr := os.Stat(path)
		if errors.Is(inspectErr, os.ErrNotExist) {
			continue
		}
		if inspectErr != nil {
			return "", fmt.Errorf("approval audit: inspect namespace ancestor: %w", inspectErr)
		}
		if !info.IsDir() || info.Mode().Perm()&0o022 != 0 || info.Mode().Perm()&0o111 == 0 {
			return "", errors.New("approval audit: namespace ancestor is not a safe directory")
		}
	}
	target, err := resolveApprovalAuditMissingSuffix(filepath.Join(defaultRoot, "audit", id))
	if err != nil {
		return "", err
	}
	if approvalAuditInside(workspace, target) {
		return "", errors.New("approval audit: default directory resolves inside workspace")
	}
	return target, nil
}

func approvalAuditInside(workspace, target string) bool {
	relative, err := filepath.Rel(workspace, target)
	return err == nil && filepath.IsLocal(relative)
}

// Resolve an existing ancestor before appending missing path components. A
// dangling symlink or inspection failure is not treated as an absent directory.
func resolveApprovalAuditMissingSuffix(target string) (string, error) {
	ancestor := target
	var missing []string
	for {
		_, err := os.Lstat(ancestor)
		if err == nil {
			resolved, resolveErr := filepath.EvalSymlinks(ancestor)
			if resolveErr != nil {
				return "", fmt.Errorf("approval audit: resolve existing ancestor: %w", resolveErr)
			}
			info, err := os.Stat(resolved)
			if err != nil {
				return "", fmt.Errorf("approval audit: inspect resolved ancestor: %w", err)
			}
			if !info.IsDir() {
				return "", errors.New("approval audit: existing ancestor is not a directory")
			}
			if info.Mode().Perm()&0o022 != 0 {
				return "", errors.New("approval audit: existing ancestor is writable by group or others")
			}
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return resolved, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("approval audit: inspect default directory: %w", err)
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return "", errors.New("approval audit: no existing default-directory ancestor")
		}
		missing = append(missing, filepath.Base(ancestor))
		ancestor = parent
	}
}
