//go:build !darwin && !linux

package privatefile

func fingerprintFile(string) (FileFingerprint, error) {
	return FileFingerprint{}, ErrFingerprintUnsupported
}
