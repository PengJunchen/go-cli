package privatefile

import (
	"errors"
	"fmt"
)

// ErrFingerprintUnsupported means the platform cannot provide stable file
// identity and change-time metadata. Callers must verify the complete file
// instead of trusting an in-memory cursor when this error is returned.
var ErrFingerprintUnsupported = errors.New("privatefile: file fingerprints are unsupported")

// FileFingerprint identifies a regular file version using its filesystem
// identity, size, and change time. It is intended for cache invalidation, not
// as a cryptographic integrity proof.
type FileFingerprint struct {
	Exists         bool
	Device         uint64
	Inode          uint64
	Size           int64
	ChangeTimeSec  int64
	ChangeTimeNsec int64
}

// FingerprintFile returns stable identity, size, and ctime metadata for path.
// A missing path has the zero fingerprint and no error. Callers must fall back
// to full content verification whenever metadata cannot be read reliably.
func FingerprintFile(path string) (FileFingerprint, error) {
	if path == "" {
		return FileFingerprint{}, errors.New("privatefile: fingerprint path is empty")
	}
	fingerprint, err := fingerprintFile(path)
	if err != nil {
		return FileFingerprint{}, fmt.Errorf("privatefile: fingerprint %q: %w", path, err)
	}
	return fingerprint, nil
}
