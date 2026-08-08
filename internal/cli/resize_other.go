//go:build !darwin && !linux

package cli

// monitorResize is a no-op on non-Unix platforms. Terminal width updates rely
// solely on per-ReadLine invalidation in readLineTTY. The winchDone channel is
// closed so that Stop() does not block waiting for a goroutine that never ran.
func (le *DefaultLineEditor) monitorResize() {
	close(le.winchDone)
}
