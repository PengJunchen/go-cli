# Find regular-file filter independent review

Decision: **ACCEPT** the narrow source change at `ea1f94e08fb2747dda50b9f3e54c5ed2d1a2868d`. No actionable P1/P2 found. The root reviewed the exact diff and ran the two behavior tests in a fresh detached worktree, `repair-20260929-find-kind-review-ea1f`.

The walker now passes the actual `os.FileMode` from its existing non-following entry inspection into the predicate. `IsRegular` excludes child symlinks (including dangling links), directories and special files; `IsDir` and the empty filter preserve their previous contracts. Requested file roots continue through the existing resolved-root path. The change does not alter traversal, sorting, output framing, caps, cancellation or descendant-link policy.

The regression compares forced-Go results with a real system `find` executable and observes both output and match/truncation metadata. It exercises regular/nested files, three kinds of symlink, and a FIFO when supported. The root independently ran `rtk proxy env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb GOFLAGS='-p=2 -trimpath' go test -race -count=5 ./internal/tools -run '^(TestFindTypeFMatchesOnlyRegularFilesAcrossBackends|TestFindKindFilterPreservesFileRootAndRegularFileTopK)$'`: exit 0, package 1.372s. The builder separately records the compiled behavior RED, full tools race, vet and scoped lint in its report.

Real `fd` is absent on this host, so neither this review nor the builder proves real-fd interoperability. This acceptance is for the regular-file filter, not the whole R4 phase or an integrated repository gate.
