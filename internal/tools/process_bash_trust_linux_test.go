//go:build linux

package tools

import (
	"errors"
	"os"
	"testing"
)

func TestResolveTrustedLinuxBashPath(t *testing.T) {
	for _, test := range []struct {
		name    string
		paths   map[string]string
		failure error
		want    string
	}{
		{"merged usr", map[string]string{"/usr/bin/bash": "/usr/bin/bash"}, nil, "/usr/bin/bash"},
		{"alpine", map[string]string{"/bin/bash": "/bin/bash"}, nil, "/bin/bash"},
		{"system symlink", map[string]string{"/usr/bin/bash": "/bin/bash"}, nil, "/bin/bash"},
		{"untrusted target", map[string]string{"/usr/bin/bash": "/tmp/bash", "/bin/bash": "/bin/bash"}, nil, ""},
		{"inaccessible preferred path", map[string]string{"/bin/bash": "/bin/bash"}, os.ErrPermission, ""},
		{"missing", nil, nil, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls []string
			got, err := resolveTrustedLinuxBashPath(func(path string) (string, error) {
				calls = append(calls, path)
				if target, ok := test.paths[path]; ok {
					return target, nil
				}
				if test.failure != nil {
					return "", test.failure
				}
				return "", os.ErrNotExist
			})
			if got != test.want || (err != nil) != (test.want == "") {
				t.Fatalf("resolveTrustedLinuxBashPath() = %q, %v; want %q", got, err, test.want)
			}
			if test.name == "untrusted target" || test.name == "inaccessible preferred path" {
				if len(calls) != 1 {
					t.Fatal("unsafe preferred path must not fall back")
				}
			}
			if test.failure != nil && !errors.Is(err, test.failure) {
				t.Fatalf("lost error cause: %v", err)
			}
		})
	}
}
