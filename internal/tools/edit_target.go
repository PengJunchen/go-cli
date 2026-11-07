package tools

import (
	"context"
	"os"
)

// editTarget owns the captured target and commits only within its parent handle.
type editTarget interface {
	snapshot() ([]byte, os.FileMode)
	revalidate() error
	write(context.Context, []byte) error
	close() error
}
