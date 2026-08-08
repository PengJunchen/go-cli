package config

import (
	"os"
	"path/filepath"
	"sync"
)

// lazyPath defers the resolution of a path string until first access and
// caches the result. It is useful for paths that are expensive to compute
// (e.g. they involve environment lookups, user-home resolution, or filesystem
// stat calls) and that are not always needed at startup.
//
// The resolve closure runs at most once, protected by sync.Once, so repeated
// Get calls are cheap after the first.
type lazyPath struct {
	once    sync.Once
	value   string
	resolve func() string
}

// newLazyPath returns a lazyPath whose value is computed by resolve on the
// first Get call.
func newLazyPath(resolve func() string) *lazyPath {
	return &lazyPath{resolve: resolve}
}

// Get returns the resolved path, computing it on the first call.
func (lp *lazyPath) Get() string {
	lp.once.Do(func() { lp.value = lp.resolve() })
	return lp.value
}

// resolveHistoryPath computes the default history file path under the user's
// home directory. It is wrapped in a lazyPath so the os.UserHomeDir call (and
// any future filesystem probing) is deferred until the path is actually
// requested.
func resolveHistoryPath(cfgPath string) *lazyPath {
	return newLazyPath(func() string {
		if cfgPath != "" {
			return cfgPath
		}
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return ""
		}
		return filepath.Join(home, ".go-cli", "history.jsonl")
	})
}
