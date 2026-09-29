package session

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
)

const workspaceIdentityDomain = "go-cli/workspace-quarantine/v1\x00"

// ErrInvalidCanonicalWorkspaceRoot reports a workspace root that cannot be
// used to derive a durable quarantine identity.
var ErrInvalidCanonicalWorkspaceRoot = errors.New("session: invalid canonical workspace root")

// WorkspaceIDForCanonicalRoot derives the domain-separated quarantine ID for
// an absolute, cleaned, symlink-canonical workspace root.
func WorkspaceIDForCanonicalRoot(root string) (string, error) {
	if root == "" || strings.ContainsRune(root, '\x00') || !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return "", ErrInvalidCanonicalWorkspaceRoot
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", errors.Join(ErrInvalidCanonicalWorkspaceRoot, err)
	}
	if canonical != root {
		return "", ErrInvalidCanonicalWorkspaceRoot
	}
	digest := sha256.Sum256([]byte(workspaceIdentityDomain + root))
	return "workspace-v1-" + hex.EncodeToString(digest[:]), nil
}
