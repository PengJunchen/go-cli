//go:build linux

package tools

import (
	"errors"
	"fmt"
	"path/filepath"
	"syscall"
)

func trustedSystemBashPath() (string, error) {
	const path = "/usr/bin/bash"
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	if filepath.Clean(canonical) != path {
		return "", errors.New("Linux system Bash does not resolve to /usr/bin/bash")
	}
	return path, nil
}

func verifyTrustedSystemPathSecurity(path string, directory bool) error {
	attributes := []string{"system.posix_acl_access"}
	if directory {
		attributes = append(attributes, "system.posix_acl_default")
	}
	for _, attribute := range attributes {
		_, err := syscall.Getxattr(path, attribute, nil)
		if err == nil {
			return fmt.Errorf("trusted Bash path %q has an extended POSIX ACL (%s)", path, attribute)
		}
		if errors.Is(err, syscall.ENODATA) {
			continue
		}
		return fmt.Errorf("cannot verify trusted Bash path ACL %q (%s): %w", path, attribute, err)
	}
	return nil
}
