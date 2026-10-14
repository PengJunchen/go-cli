//go:build linux

package tools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func trustedSystemBashPath() (string, error) {
	return resolveTrustedLinuxBashPath(filepath.EvalSymlinks)
}

func resolveTrustedLinuxBashPath(resolve func(string) (string, error)) (string, error) {
	for _, path := range []string{"/usr/bin/bash", "/bin/bash"} {
		canonical, err := resolve(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		switch filepath.Clean(canonical) {
		case "/usr/bin/bash", "/bin/bash":
			return filepath.Clean(canonical), nil
		default:
			return "", errors.New("tools: Linux system Bash resolves outside the trusted system paths")
		}
	}
	return "", fmt.Errorf("locate trusted Linux system Bash: %w", os.ErrNotExist)
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
