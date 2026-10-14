//go:build darwin

package privatefile

import (
	"errors"
	"os"
	"syscall"
)

func fingerprintFile(path string) (FileFingerprint, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return FileFingerprint{}, nil
	}
	if err != nil {
		return FileFingerprint{}, err
	}
	if !info.Mode().IsRegular() {
		return FileFingerprint{}, errors.New("path is not a regular file")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return FileFingerprint{}, ErrFingerprintUnsupported
	}
	return FileFingerprint{
		Exists:         true,
		Device:         uint64(stat.Dev),
		Inode:          uint64(stat.Ino),
		Size:           info.Size(),
		ChangeTimeSec:  stat.Ctimespec.Sec,
		ChangeTimeNsec: stat.Ctimespec.Nsec,
	}, nil
}
