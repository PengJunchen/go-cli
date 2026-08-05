//go:build !darwin && !linux

package tools

import "os/exec"

type rlimitSnapshot struct{}

func saveRlimits() rlimitSnapshot       { return rlimitSnapshot{} }
func restoreRlimits(_ rlimitSnapshot)   {}
func applyRlimits(_ *exec.Cmd, _ ResourceLimits) {}
