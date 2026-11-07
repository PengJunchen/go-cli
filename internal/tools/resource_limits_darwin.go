//go:build darwin

package tools

import "os/exec"

// applyRlimits is a compatibility no-op on Darwin.
func applyRlimits(_ *exec.Cmd, _ ResourceLimits) {}
