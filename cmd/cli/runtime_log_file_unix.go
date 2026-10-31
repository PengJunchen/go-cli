//go:build darwin || linux

package main

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// openExplicitRuntimeLogFile uses the same descriptor-bound no-follow pattern
// as physical session append, scoped only to the opt-in runtime log sink.
func openExplicitRuntimeLogFile(path string) (*os.File, error) {
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return nil, err
	}
	directory, err := unix.Open(parent, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = unix.Close(directory) }()
	var info unix.Stat_t
	if err := unix.Fstat(directory, &info); err != nil {
		return nil, err
	}
	if info.Mode&unix.S_IFMT != unix.S_IFDIR || info.Uid != uint32(os.Geteuid()) || info.Mode&0o777 != 0o700 {
		return nil, errors.New("runtime log: parent is not an owned private directory")
	}
	var current unix.Stat_t
	if err := unix.Lstat(parent, &current); err != nil {
		return nil, err
	}
	if current.Mode&unix.S_IFMT != unix.S_IFDIR || current.Dev != info.Dev || current.Ino != info.Ino {
		return nil, errors.New("runtime log: parent identity changed")
	}
	return openRuntimeLogAt(directory, filepath.Base(path), path)
}

func openRuntimeLogAt(directory int, name, path string) (*os.File, error) {
	file, err := unix.Openat(directory, name, unix.O_WRONLY|unix.O_APPEND|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, err
	}
	reject := func(err error) (*os.File, error) { _ = unix.Close(file); return nil, err }
	var info unix.Stat_t
	if err := unix.Fstat(file, &info); err != nil {
		return reject(err)
	}
	if info.Mode&unix.S_IFMT != unix.S_IFREG || info.Uid != uint32(os.Geteuid()) || info.Mode&0o777 != 0o600 {
		return reject(errors.New("runtime log: destination is not an owned private regular file"))
	}
	var current unix.Stat_t
	if err := unix.Fstatat(directory, name, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return reject(err)
	}
	if current.Dev != info.Dev || current.Ino != info.Ino {
		return reject(errors.New("runtime log: destination identity changed"))
	}
	return os.NewFile(uintptr(file), path), nil
}
