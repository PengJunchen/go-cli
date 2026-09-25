//go:build linux

package cli

import "os"

func cancelReaderSupportsTTYFile(*os.File) bool { return true }
