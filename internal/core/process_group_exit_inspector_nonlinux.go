//go:build !linux

package core

import "errors"

var errPIDNamespaceTokenUnsupported = errors.New("PID namespace tokens are supported only on Linux")
