# Find regular-file kind parity

## Scope and result

Branch `codex/repair-20260929-find-kind` starts at integrated base `5db266977b260ba0f1707e766f327bcba92671dd`. This change is limited to the Go Find kind predicate and its focused regression tests.

The Go walker now evaluates the actual `os.FileMode`: `type: "f"` includes regular files only, and `type: "d"` includes directories only. An empty type still includes every visible entry. The single-file root continues to use the already-resolved path and its `os.Stat` mode. This aligns Go fallback results with system `find -type f` and `fd --type f` semantics without changing descendant-link traversal or any other Find behavior.

## TDD evidence

The new Unix fixture includes a regular file, a nested regular file, child file and directory symlinks, a dangling symlink, and a FIFO when the filesystem supports it. It compares forced-Go results with the real system `find` backend for `type: "f"`, `type: "d"`, and an empty type. It also checks file-root behavior and top-K order/count/truncation metadata.

Before implementation, the focused test failed: Go `type: "f"` returned the three links and FIFO along with the two regular files, while real system `find -type f` returned only the two regular files. The directory and empty-type parity cases passed. After the predicate change, the same focused command passes all cases.

## Validation

Focused regression tests (GREEN):

```text
rtk env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb go test -p 2 -count=1 -run '^(TestFindTypeFMatchesOnlyRegularFilesAcrossBackends|TestFindKindFilterPreservesFileRootAndRegularFileTopK)$' -v ./internal/tools
PASS
```

Full package and scope checks:

```text
rtk env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb go test -race -p 2 -count=1 ./internal/tools
ok github.com/pengjunchen/go-cli/internal/tools 44.224s

rtk env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb go vet ./internal/tools
# exit 0

rtk env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb golangci-lint run --allow-serial-runners ./internal/tools/...
0 issues.

rtk git diff --check
# exit 0
```

`fd` is not installed in this environment (`rtk which fd` returned no path), so no real-fd execution is claimed. The backend comparison used the real system `find` executable through a PATH symlink and the forced-Go implementation.
