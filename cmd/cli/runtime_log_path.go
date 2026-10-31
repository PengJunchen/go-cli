package main

import (
	"errors"
	"path/filepath"
	"strings"
)

const runtimeLogFileEnv = "GO_CLI_LOG_FILE_PATH"

// resolveRuntimeLogFilePath preserves the historical cwd-relative default when
// unset. An explicit destination is an absolute literal, never an expansion.
func resolveRuntimeLogFilePath(lookup func(string) (string, bool)) (string, bool, error) {
	if lookup == nil {
		return "", true, errors.New("runtime log: environment lookup unavailable")
	}
	value, present := lookup(runtimeLogFileEnv)
	if !present {
		return filepath.Join(".go-cli", "logs", "go-cli.log"), false, nil
	}
	if value == "" || !filepath.IsAbs(value) || strings.ContainsRune(value, '\x00') {
		return "", true, errors.New("runtime log: invalid explicit destination")
	}
	return filepath.Clean(value), true, nil
}
