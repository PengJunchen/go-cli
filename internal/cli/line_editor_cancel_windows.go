//go:build windows

package cli

import "os"

// The Windows cancelreader backend cannot guarantee that every active
// overlapped console read is interruptible, so it cannot safely hand input
// ownership back before restoring terminal mode.
func cancelReaderSupportsTTYFile(*os.File) bool { return false }
