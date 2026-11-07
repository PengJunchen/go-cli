package tools

import (
	"errors"
	"fmt"
)

// ErrEditConflict identifies an edit whose captured target changed before commit.
var ErrEditConflict = errors.New("edit: target changed")

// EditConflictError describes an observable change to the edit target.
type EditConflictError struct {
	Path   string
	Reason string
	Cause  error
}

func (e *EditConflictError) Error() string {
	return fmt.Sprintf("edit: target %q changed (%s)", e.Path, e.Reason)
}

// Is reports whether target identifies an edit conflict.
func (e *EditConflictError) Is(target error) bool { return target == ErrEditConflict }

// Unwrap preserves the underlying path or I/O error, when present.
func (e *EditConflictError) Unwrap() error { return e.Cause }
