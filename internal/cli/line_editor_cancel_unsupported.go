//go:build !linux && !darwin && !windows

package cli

import "os"

func cancelReaderSupportsTTYFile(*os.File) bool { return false }
