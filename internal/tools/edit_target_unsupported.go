//go:build !darwin && !linux

package tools

import "errors"

func captureEditTarget(_ string, _ PathWhitelist) (editTarget, error) {
	return nil, errors.New("edit: safe target handling is unsupported on this platform")
}
