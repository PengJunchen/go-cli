# Test execution gate addendum

Status: proposed only. This is a separate sequential addendum; apply `protected-changes.patch` first, then this file. Neither patch has been applied to protected originals. This addendum was generated in memory with `difflib` from the primary proposal's output. The pre-change protected-file Git blobs remain Makefile `32a78c9bde2af05f105bd32d771a178bfd9d78f5`, CI `2e886a904d6447928c3c6a1cb9ff9c999cd407df`, and PR workflow `8e529516c1d436f48f0961c67d2e9badf1c29fb2`.

The new non-protected `internal/verify/cmd/test-gate` command runs `go test -json`, parses the actual event stream, and requires at least one test function to emit `run` and `pass` events. A successful Go command with no matching test functions or no test files fails with the package patterns, observed package count, and Go's own output. Malformed JSON and process launch failures fail; a non-zero `go test` exit status is returned unchanged, while the configured timeout returns 124. Package patterns are explicit `-package` arguments, so a separated value such as `-run TestName` cannot be mistaken for a package.

The addendum routes Makefile `test`, `test-cov`, `test-cov-check`, `test-log`, and `test-leak` through the gate. CI uses the same `make test-cov` path, gates E2E tests with the `e2e` build tag, and the PR workflow uses `make test`. It removes `test-cov-check`'s stderr suppression so diagnostics remain visible. The runner needs no new Go dependency.

`test-log` and `test-leak` still select only the existing log-capture and goroutine-checker helper tests in `internal/verify/...`. The gate proves that the selected helper tests actually ran; it does not establish that the full Runtime lifecycle is covered by leak checks. This patch makes no claim of full Runtime leak-gate coverage.

The gate reports aggregate events across the selected package patterns; it does not require every matched package to contain tests. The full `./...` targets still run all packages and fail on any Go test error. E2E continues to require its existing Docker/test environment.

Rollback: revert this addendum's Makefile and workflow hunks as one unit, leaving the primary scanner/package-coverage proposal independent. The new CLI can remain unused or be removed separately if it is not adopted.
