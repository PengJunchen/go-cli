//go:build !darwin

package tui

import "errors"

// termios is a placeholder on non-Darwin platforms. The keyboard loop probes
// support at runtime via makeRaw's error return and degrades gracefully.
type termios struct{}

// errRawModeUnsupported is returned by makeRaw on platforms without a
// confirmed termios layout so the caller can fall back to non-interactive
// (always-expanded) rendering.
var errRawModeUnsupported = errors.New("tui: raw mode not supported on this platform")

// makeRaw is a no-op stub on non-Darwin platforms.
func makeRaw(fd int) (*termios, error) { return nil, errRawModeUnsupported }

// restoreRaw matches the stub contract.
func restoreRaw(fd int, old *termios) error { return nil }
