# R6 Root Early Help/Version Review

**Verdict: ACCEPT for the root early-dispatch slice only.** Reviewed exact commit `ad1d889eac58d3ec2ffc6b2c5d4c71ac27c1240d` (`dbc0cfdf171698477e45ab050e062cf73e9015d2` parent).

The implementation follows the frozen contract in [`r7-remaining-design.md`](../r7-remaining-design.md#L59), its acceptance criteria at lines 97 and 105, and the original root contract in [`r6-entrypoint-design.md`](../../2026-09-25-runtime/r6-entrypoint-design.md#L52). `main.go` creates one `CommandRegistry` and registers built-ins before config loading. `IsGlobalHelpOrVersion` uses the same `newGlobalFlagSet` parser used by `RunWithRegistry`; only a successfully parsed global help/version request takes the minimal config path. The same registry is passed to normal dispatch, where built-in registration is a no-op once its entries exist. The registered `help` or `version` command handles early output; other invocations still call the full config loader and retain its failure exit.

The early config explicitly disables tracing with exporter `none`, so help/version output stays on stdout without trace records or config-dependent tracing. Tests also cover parser boundaries (subcommand help, disabled flags, and invalid flags), prove the injected loader is skipped only for recognized root help/version, and prove malformed config still returns exit 1 for non-early invocations. The real-binary test covers broken global and project config, successful help/version output, no interactive/session dispatch, and config validation failure for an unknown command. I found no blocking or non-blocking findings for this slice.

Validation on this checkout:

- `rtk go test -count=1 -run '^TestCLIBinaryGlobalHelpAndVersionIgnoreBrokenConfig$' ./cmd/cli` — passed; 4 test/case entries in 1 package, including a built CLI subprocess.
- `rtk go test -race ./cmd/cli ./internal/cli` — passed; 864 tests across 2 packages.
- `rtk make vet` — passed.
- `rtk make scan` — exit 0; 375 files scanned, 0 errors and 1,570 warnings.

This acceptance does not close R6 trust wiring. `main()` still constructs `NewDefaultTrustManager(nil)` and uses legacy `config.LoadTrusted`; this commit does not add the persistent source manifest, single `FileTrustStore` manager wiring, byte-snapshot config loading, or `trust grant/list/revoke` commands required by the frozen design. Those remain prerequisites before claiming the broader R6 trust slice is complete.
