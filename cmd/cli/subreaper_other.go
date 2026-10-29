//go:build !linux

package main

import (
	"errors"
	"os"
)

func standaloneSubreaperDispatch() (int, bool) {
	private, err := readSubreaperPrivateEnvironment(os.Environ())
	if err != nil || len(private) != 0 {
		return subreaperFailure(errors.New("private Linux supervisor bootstrap is unsupported")), true
	}
	enabled, err := parseStandaloneSubreaperMode(os.Getenv(subreaperEnvironment))
	if err != nil {
		return subreaperFailure(err), true
	}
	if enabled {
		return subreaperFailure(errors.New("opt-in supervisor is only available on Linux")), true
	}
	return 0, false
}
