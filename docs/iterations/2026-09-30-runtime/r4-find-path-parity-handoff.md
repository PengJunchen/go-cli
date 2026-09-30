# R4 Find path parity handoff

**Scope:** normalize Find helper results for directory roots to the existing pure-Go output contract. This is a bounded compatibility fix within R4 directory capture; it does not close R4 or change directory traversal, match selection, capture budgets, or helper lifecycle ownership.

## Compatibility decision

For a directory search, the canonical displayed path is the cleaned path relative to the searched directory. The searched directory itself is excluded. Apply this shape to `fd`, system `find`, and the pure-Go walker before a helper candidate enters the shared top-K collector. For example, searching `.` or an absolute directory for a nested file displays `sub/file.txt`.

This preserves the existing pure-Go behavior in `findPureGo`: it skips the root callback and adds `filepath.Clean(relativePath)` for descendants. The external helper route keeps its current root exclusion for system `find` (`-mindepth 1`) and normalizes helper path spellings at the streaming capture boundary, so normalization does not add an unbounded intermediate result slice. Relative helper records that are already root-relative remain supported for shims. Absolute helper records are converted with `filepath.Rel` against the directory search root. A normalized `.` record is omitted; a record that escapes the search root is rejected as a capture error. The existing NUL framing, per-path capture limit, continued pipe draining, managed join, and no-fallback behavior for capture errors remain in force.

For a file root, preserve the existing resolved path spelling exactly, whether it is relative or absolute. `FindTool.Execute` detects existing non-directory roots before external directory helpers and the pure-Go file-root branch returns `searchPath` unchanged. With an absolute workdir, the current regression `TestFindSystemFindExcludesDirectoryRootAndFileRootIsIncluded` asserts the absolute result; with the default relative `.` workdir, the equivalent result remains relative.

## Source evidence and acceptance

- `resolveAndValidatePath` returns the spelling from `resolveWithinWorkdir`: it joins and cleans a relative path against `Workdir`, preserving a relative spelling when `Workdir` is relative (`internal/tools/write.go`).
- `findPureGo` returns cleaned root-relative paths for directory descendants and does not add the root (`internal/tools/find.go`). Its file-root branch returns `searchPath` unchanged.
- `find` detects existing non-directory roots before probing helpers (`internal/tools/find.go`); `findWithFindManaged` adds `-mindepth 1` for directory roots. The file-root spelling remains unchanged by this parity fix.
- Existing backend tests compare synthetic basename records, so they do not validate actual `find` output for `.` or absolute roots. Add exact-output tests using the real installed system `find` and fd when available, plus controlled `fd` output, covering dot, absolute, and nested directory roots. Preserve and assert the existing absolute file-root result.

This handoff resolves R4-DC-3 for this bounded implementation slice only. It is not evidence of the remaining R4 schema, effect, entrypoint, dependency, or full integration gates.
