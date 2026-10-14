# Runtime entrypoint truth repair — builder report

Base revision: `1144e1ebc9f339a04d502f0bdca04069c9d30ff5`

Source commit: `77b7c948e095085f482e5a0227a2f1a87b328e35`

Branch: `codex/repair-20260929-entrytruth`

## Delivered behavior

- `serve` now rejects default or explicit `--approve ask` with a typed unsupported-capability error that directs callers to choose `auto` or `deny`. The rejection happens before config assembly, token generation or writing, and listener startup. Existing `--approve auto` authentication and loopback/wildcard checks remain in force.
- `serve` requires a loaded config and a successfully assembled dispatcher. Missing runtime and assembly failures return errors; any assembly returned with a failure is closed through its existing owner. The token is generated and persisted only after assembly succeeds, and the CLI no longer starts with a nil-dispatcher fallback.
- Interactive TUI `mode: remote` returns a typed unsupported error in `interactiveCmd.Run`, before REPL setup can create providers, tools, or sessions. The library `ACPStreamAdapter` remains a read-only SSE consumer; this change does not claim remote prompt dispatch.
- TUI `diff_style` now accepts only `unified`. Loaded defaults set `unified`; explicit `split` and `auto` produce a config validation error before provider assembly.

The tests were first added and run against the baseline. They reproduced ask-mode startup and remote TUI returning success, and the config cases confirmed that `split`/`auto` were accepted and the loaded default was empty. The token/runtime fixtures were then tightened to use private directories so they distinguish an intended fail-start from unrelated token-file permission errors.

## Verification

- `rtk go test ./internal/cli -run 'TestServe|TestInteractiveRemoteModeRejected' -count=1` — 28 passed.
- `rtk go test ./internal/config -run 'TestTUIConfig' -count=1` — 10 passed.
- `rtk go test -race ./internal/cli ./internal/config` — 1,175 passed across both packages.
- `rtk go vet ./internal/cli ./internal/config` — passed.
- `rtk proxy golangci-lint run --allow-serial-runners ./internal/cli ./internal/config` — 0 issues.
- `rtk git diff --check` — clean before the source commit.

This report and source commit cover the scoped CLI/config slice only. They do not establish full R6 completion, official ACP interoperability, or a remote approval round-trip.
