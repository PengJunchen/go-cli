//go:build darwin

package tools

import (
	"errors"
	"path/filepath"
)

func trustedSystemBashPath() (string, error) {
	const path = "/bin/bash"
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	if filepath.Clean(canonical) != path {
		return "", errors.New("Darwin system Bash does not resolve to /bin/bash")
	}
	return path, nil
}

func verifyTrustedSystemPathSecurity(_ string, _ bool) error {
	// Darwin admission relies on /bin being part of the SIP-protected system
	// volume. verifyTrustedSystemExecutable separately checks root ownership and
	// rejects group/other write bits on every path component. This intentionally
	// does not accept Homebrew or user-managed Bash installations.
	return nil
}
