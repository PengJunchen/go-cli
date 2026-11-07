//go:build darwin || linux

package tools

import (
	"fmt"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestEditDirectoryCapabilityFallbackPolicy(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "invalid_search_flag", err: unix.EINVAL, want: true},
		{name: "unsupported", err: unix.EOPNOTSUPP, want: true},
		{name: "unimplemented", err: unix.ENOSYS, want: true},
		{name: "wrapped_unsupported", err: fmt.Errorf("open: %w", unix.EINVAL), want: true},
		{name: "permission_denied", err: unix.EACCES},
		{name: "missing_path", err: unix.ENOENT},
		{name: "io_error", err: unix.EIO},
		{name: "unknown", err: fmt.Errorf("unknown")},
		{name: "no_error"},
	} {
		t.Run("open/"+tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, unsupportedEditRootOpen(tc.err))
		})
	}

	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "old_linux_path_fd", err: unix.EBADF, want: runtime.GOOS == "linux"},
		{name: "permission_denied", err: unix.EACCES},
		{name: "io_error", err: unix.EIO},
		{name: "unknown", err: fmt.Errorf("unknown")},
		{name: "no_error"},
	} {
		t.Run("stat/"+tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, unsupportedEditRootStat(tc.err))
		})
	}
}
