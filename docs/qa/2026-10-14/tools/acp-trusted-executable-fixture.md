# Trusted ACP executable fixture: red/green evidence

**Evidence class:** race-enabled Go package test using a temporary trusted project and local shell executable
**Comparison point:** `develop` at `e77e8058c51e5370a2ecc3eacb5fce0419829bcf`
**Scope:** the captured trusted-project ACP executable receives the initial ACP `connect` frame; changed-path rejection remains covered by the same test.

## Red: baseline reproduction

The unmodified `develop` baseline failed during `make test` in
`TestTrustedProjectACPCommandUsesCapturedExecutableAndRejectsChangedPath`:

```text
=== RUN   TestTrustedProjectACPCommandUsesCapturedExecutableAndRejectsChangedPath
acp_project_command_trust_test.go:58: Error: Condition never satisfied
Messages: the captured executable must consume ACP's connect handshake
--- FAIL: TestTrustedProjectACPCommandUsesCapturedExecutableAndRejectsChangedPath (1.04s)
```

This was reproduced in the pure-baseline run; the full run reported 8,686
passed, 3 failed, and 6 skipped out of 8,695 tests. The original fixture wrote
its only marker after reading stdin, so the failure did not distinguish an
executable that never started from one that started but did not consume the
frame. The log contained no race report or ACP connect event for this test.
Therefore the exact failing stage and its cause remain undetermined; this is not
attributed to host load.

Raw baseline log on the validation host:
`/Users/yinyuepeng/Library/Application Support/rtk/tee/1791951885_make_test.log`.

## Green: fixture correction

The test now asserts the effective trusted config's `stdio` transport and
captured relative command, registers `assembly.Cleanup` immediately after
successful assembly, and has the fixture write a start marker before reading
stdin. A bounded 10-second helper waits for the actual `connect` frame and
reports separately whether the executable did not start or started without
consuming the frame. No production ACP handshake code changed.

```text
rtk go test -race ./internal/cli -run '^TestTrustedProjectACPCommandUsesCapturedExecutableAndRejectsChangedPath$' -count=20
PASS, 20 runs, 12.029s (before the cleanup-registration ordering-only adjustment)

rtk go test -race ./internal/cli -run '^TestTrustedProjectACPCommandUsesCapturedExecutableAndRejectsChangedPath$' -count=1
PASS, 2.328s (after cleanup was registered immediately after assembly)
```

The race-enabled `internal/cli` package also passed in 61.685s before that
cleanup-ordering adjustment. The final repository `make verify` result is
tracked separately by its runner; these targeted green runs do not substitute
for that gate.
