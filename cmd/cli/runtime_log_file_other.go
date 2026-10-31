//go:build !darwin && !linux

package main

import (
	"errors"
	"os"
)

func openExplicitRuntimeLogFile(_ string) (*os.File, error) {
	return nil, errors.New("runtime log: safe explicit append unsupported")
}
