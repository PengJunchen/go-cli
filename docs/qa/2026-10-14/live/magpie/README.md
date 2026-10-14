# Magpie local gateway evidence

Captured 2026-10-14 on macOS 26.5.2 (Apple Silicon) for the local go-cli live-provider run.

## Install and lifecycle

- Installed upstream Magpie release `v0.1.1231` from the official installer at `https://usemagpie.ai/install.sh`.
- Platform asset: `magpie-darwin-arm64.zip`; SHA-256 from the official `https://usemagpie.ai/api/latest` manifest: `c0a1ab3bc7b1b880289c10971cdd6e0853dba1a9305c30b8e8f67c24a8db6af6`. The inspected installer verifies this SHA-256 before extracting the app.
- Installed executable SHA-256 (`/Applications/magpie.app/Contents/MacOS/magpie`): `30fb35ec52186c7297d0cfc2034c58f5e9cf4a723b40d0e39b958a94d1fce39d`; `codesign --verify --deep --strict` passed.
- Installed paths: `/Applications/magpie.app` and `/Users/yinyuepeng/.local/bin/magpie`.
- `codesign` reports a valid Developer ID signature for `com.yetone.magpie` by Xipeng Guan (`LY7MVTUDZG`) and a stapled notarization ticket.
- Started with `open -a /Applications/magpie.app`. The app owns the gateway while it is running. No launch daemon or login autostart was installed.
- `GET /v1/models` succeeded. An earlier check of `/health` returned 404 because that is not a supported Magpie route; it was not a service failure. `lsof` showed the listener bound only to `127.0.0.1:3425`; no LAN interface was listening.

The install/start left the signed-in Codex account available through Magpie's account discovery. Discovery reported one Codex account. Upstream describes this mode as reading Codex's existing sign-in and storing only Magpie model picks; token refresh can rotate the token back into Codex's existing auth store. No auth JSON or credential value was copied or saved in this evidence. One early, unfiltered local diagnostic emitted a profile identity only in transient tool output; that value was not saved or repeated, and subsequent audit output was allowlist-filtered.

The native app window is running. The separate browser UI was not started; upstream's `magpie web` defaults to `127.0.0.1:3430` and generates a one-run `?k=` control key. Keep that key out of screenshots and logs. The Routing and Usage screens can show account email/login, so a screenshot should crop to the Gateway recent-call model/provider/status area or use the sanitized API evidence below.

Codex's `~/.codex/config.toml` SHA-256 was `ed36cd73e68fbfaae67465d9d13b9798aabee6e8b7fc8c3d258cdf7369dcfb9c` both before and after the run. Its contents were not recorded. OpenAI's [Codex Config Basics](https://developers.openai.com/codex/config-basic) documents this as the user-level Codex configuration path.

## Codex model catalog

The local `/v1/models` catalog listed these models from the signed-in Codex provider:

| Model ID | Context length | Max output |
| --- | ---: | ---: |
| `codex/gpt-6.1-sol` | 500,000 | 128,000 |
| `codex/gpt-6-astra` | 272,000 | 128,000 |
| `codex/gpt-6-sol` | 272,000 | 128,000 |
| `codex/gpt-6-luna` | 272,000 | 128,000 |
| `codex/gpt-5.6-sol` | 272,000 | 128,000 |
| `codex/gpt-5.6-terra` | 272,000 | 128,000 |
| `codex/gpt-5.6-luna` | 272,000 | 128,000 |

For this go-cli checkout, use the project launcher below, which sets the required `GO_CLI_*` provider and model variables. Setting only `OPENAI_BASE_URL` and `OPENAI_API_KEY` does not configure this binary. The catalog is a live Magpie response; IDs can change with the signed-in account and upstream availability. A catalog entry is not proof of account entitlement or guaranteed service availability.

## Real HTTP probe

One real, non-mock Responses API request was sent to the loopback gateway with session `magpie-live-20261014-01`. The Magpie route record time was `2026-10-14T14:40:21.131215+08:00`; it requested `codex/gpt-6.1-sol` and returned HTTP 200, `completed`, and `magpie-live-check-ok.`. The response ID was `resp_0d6ed0e321d3d811016acf23d86e40819bbbaa941712aa5686`.

The returned provider marker was `X-Magpie-Provider: codex`; the model marker was `X-Magpie-Model: codex/gpt-6.1-sol`. Magpie's route record independently reported `asked=codex/gpt-6.1-sol`, `served=gpt-6.1-sol`, status 200, and `done=true` (route ID `1791960021145`, 13,568 ms). Its usage record for the same session reported 138 uncached input tokens and 10 output tokens. The Responses body reported 4,234 input tokens including 4,096 cached, 10 output tokens, 4,244 total.

`live-request.jsonl` contains the sanitized request/response and route summary. It omits the authorization value and full request body. The gateway's local usage log also included the correlated row; no prompt text or account identity is recorded here.

## Streaming tool-call smoke

A second live check exercised OpenAI Chat Completions streaming and the tool-call round trip through the same Codex provider. The first stream finished with `tool_calls`, naming `emit_probe` with `{"value":"ok"}`. The local probe client returned the small tool result `probe returned: ok`; the follow-up stream completed with `emit_probe returned: ok.`. Both calls returned HTTP 200 and both response headers named provider `codex` and model `codex/gpt-6.1-sol`.

The response IDs were `chatcmpl-resp_0041c3dff2234883016acf25074bf4819bbcde6768ce6e285a` and `chatcmpl-resp_025a5e2b05fb8520016acf253c34188199be09be5d8e5bc582`. The first response reported 4,278 prompt tokens and 42 completion tokens. The second reported 4,268 prompt tokens (4,096 cached), 11 completion tokens, and 4,279 total; Magpie's usage log recorded 172 uncached input tokens and 11 output tokens. Both correlated usage rows and route outcomes were HTTP 200 on `gpt-6.1-sol`; details are in `stream-tool-roundtrip.jsonl`.

This evidence covers installation, account discovery, model enumeration, local binding, one real Responses request, and one streamed Chat Completions tool-call round trip. It does not establish multi-turn behavior beyond that one tool round trip or model availability beyond the observed catalog snapshot.

## Run this checkout against Magpie

From the workspace where go-cli should operate, invoke the launcher by its checkout path, for example `/path/to/go-cli/scripts/magpie/run-dot.sh`. It resolves and invokes only that checkout's `bin/go-cli`; it does not build or modify the binary and it keeps the caller's current directory as the workspace. With no arguments it starts the interactive TUI; arguments are passed through, for example `/path/to/go-cli/scripts/magpie/run-dot.sh prompt "Explain this workspace"`. Replace `/path/to/go-cli` with the actual checkout path.

Start Magpie first if the desktop app is not already open:

```sh
open -a /Applications/magpie.app
```

The launcher explicitly selects provider `openai`, loopback base URL `http://127.0.0.1:3425/v1`, dummy gateway key `magpie`, and both CLI model fields as `codex/gpt-6.1-sol`. This Magpie connection profile sets `production.model_timeout.total=10m` and `production.model_timeout.stream_idle=4m` through process-scoped environment variables; an explicitly supplied value for either variable is preserved. The longer profile accommodates slow, large artifact-generation streams through the local gateway. It does not edit `~/.codex/config.toml` or read Magpie/Codex credential files. The service must be listening on loopback port 3425 before the CLI request can succeed.

The timeout variables are `GO_CLI_PRODUCTION_MODEL_TIMEOUT_TOTAL` and `GO_CLI_PRODUCTION_MODEL_TIMEOUT_STREAM_IDLE`. They accept Go duration strings from greater than zero through `24h`; effective stream idle must not exceed total. These are explicit settings made only by this launcher profile. With no timeout variable configured, primary model middleware keeps its existing 30s total/10s stream-idle defaults and the small background model keeps its existing caller-context behavior. Setting either variable explicitly applies the resolved pair to the small model as well, including summary, title, and extraction calls; the 30s/10s framework defaults are unchanged.

## Earlier reviewed-checkout build and launcher probe (49dbf975)

The final reviewed source commit is `49dbf9755d860b877edd45ed9a73161ab5ebbb8f`. `rtk proxy make build` built the checkout's `bin/go-cli` using the repository Makefile profile (`go build -trimpath -ldflags "-s -w" -o ./bin/go-cli ./cmd/...`). The binary SHA-256 changed from `686c286534a95206bc714e21da82a8890e78537415c95ab68a5843ae08ded18e` before the build to `416b5a6ded71d2a389c989127420bf2a4a393126b6d66fb28e76542df13d9142` after it. The Go source diff was empty before and after (`e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855`). `bin/go-cli --version` returned `go-cli 0.1.0`; `scripts/magpie/run-dot.sh --help` exited 0 and printed the command/option help.

One genuine, non-TTY text request through the launcher completed with exit code 0. It ran from an isolated temporary HOME, XDG directories, TMPDIR, and working directory, with approval mode `deny`; the requested response was exactly `MAGPIE_FINAL_a0b9812e9dc55390`, stdout was 30 bytes, and stderr was empty. The isolated trace had 24 successful spans and no tool-named spans. CLI-created trace and log files stayed in that temporary directory and were discarded; the repository working directory was not used for the run.

Magpie's local usage CSV contained one matching row in the probe's time window: provider `codex`, asked `codex/gpt-6.1-sol`, served `gpt-6.1-sol`, HTTP 200, 7,210 input tokens (0 cache-read), 18 output tokens, 3,284 ms duration, 2,447 ms TTFT, route ID `1791960021359`, and response ID `chatcmpl-resp_0a64291a67a81fbd016acf571aef388199aa56bfa9cdd05728`. The gateway translated `/v1/chat/completions` to `/v1/responses`. Magpie's `request_id` field was empty; this correlation is by the narrow timestamp window, provider/model, and single matching route row, not a client-supplied session header. The CLI itself does not send an `X-Magpie-Session` header. No account fields, prompt bodies, auth headers, or credential values were included in the saved evidence; the response nonce is retained as proof of the live reply.

The Magpie app remained healthy after the probe at `127.0.0.1:3425`. The user-level Codex config SHA-256 was `ed36cd73e68fbfaae67465d9d13b9798aabee6e8b7fc8c3d258cdf7369dcfb9c` both before and after. A redacted, machine-readable build and request record is in [`cli-smoke/final-build-probe.jsonl`](cli-smoke/final-build-probe.jsonl).

## Historical reviewed-checkout build and launcher probe (ec267df)

At that time, the source checkout was `ec267df999405a03a09ed5485256aee6cc133aae`. The repository's `make verify` gate built `bin/go-cli` with the Makefile profile (`go build -trimpath -ldflags "-s -w" -o ./bin/go-cli ./cmd/...`). The binary SHA-256 at the probe was `24ea7ed9febb318adf3336d018677715967415c31e8b80d9b698aeee94f9467c`; `bin/go-cli --version` returned `go-cli 0.1.0`. `scripts/magpie/run-dot.sh --help` exited 0. The probe did not rebuild the binary. The local Go source diff remained empty (SHA-256 `e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855`). This is historical evidence for `ec267df`, not a probe of the later merged source.

One genuine non-TTY text request ran through the project launcher from an isolated temporary HOME, XDG configuration/cache/state directories, TMPDIR, and workspace, with approval mode `deny`. It ran from `2026-10-14T11:21:22Z` to `11:21:28Z`; stdout contained only the exact requested nonce `MAGPIE_FINAL_6794d33a-bfec-4ad8-a2df-9a59d8093747` plus a trailing newline (50 bytes), stderr was empty, and the CLI exited 0. The temporary trace contained 24 successful spans and no tool-named spans. Temporary CLI traces and logs were removed after extracting this redacted summary.

The local `GET /v1/magpie/usage/requests` audit returned HTTP 200. Its quiet probe window contained one gateway HTTP row matching the launcher call: provider `codex`, requested model alias `codex/gpt-6.1-sol`, served model `gpt-6.1-sol`, status 200, 7,195 input tokens, 31 output tokens, 4,816 ms TTFT, and 5,490 ms total duration. Magpie exposed route ID `1791960021489` and response ID `chatcmpl-resp_0525a02a274fb936016acf65b419008199b35c0c45220e6494`; the request traversed `/v1/chat/completions` to `/v1/responses`. There is no separate `request_id` field in this record, and the CLI does not send `X-Magpie-Session`. Two additional `session-unknown` Codex log-only rows appeared in the same time window with no route or response IDs; they are not joined to the HTTP request. The local gateway record's route and response IDs, provider/model, status, token usage, and isolated time window provide the audit evidence for the real CLI call.

The Magpie app remained healthy on `127.0.0.1:3425`, bound only to loopback. The user-level Codex config SHA-256 was `ed36cd73e68fbfaae67465d9d13b9798aabee6e8b7fc8c3d258cdf7369dcfb9c` before and after; its contents and all credential stores were left unread. A redacted machine-readable record is in [`cli-smoke/final-build-probe-ec267df.jsonl`](cli-smoke/final-build-probe-ec267df.jsonl). The earlier 49dbf build/probe record remains preserved in [`cli-smoke/final-build-probe.jsonl`](cli-smoke/final-build-probe.jsonl).

## Final merged-source launcher probe and gates (bdb76ef)

The final source commit is `bdb76efb0b31152e4c554268b43d39ef6285115a`. The existing `bin/go-cli` built by `make verify` was used without rebuilding; its SHA-256 was `3ac94e858fabeee47fb79ad1b00889f76f0def0ab845d537998f1955b34eb191`. `bin/go-cli --version` returned `go-cli 0.1.0`, and `scripts/magpie/run-dot.sh --help` exited 0. The Go source diff hash was the empty-tree SHA-256 `e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855`.

One genuine non-TTY text request went through the launcher from an isolated temporary HOME, XDG directories, TMPDIR, and workspace with approval mode `deny`. It ran from `2026-10-14T11:49:26Z` to `11:49:30Z`; stdout was exactly the unique nonce recorded in the sanitized JSONL plus a newline (53 bytes), stderr was empty, and the CLI exited 0. The isolated trace had 24 successful spans and no tool-named spans. The repository workspace was not used.

The narrow quiet-window audit matched one `Go-http-client` row for provider `codex`, requested alias `codex/gpt-6.1-sol`, served model `gpt-6.1-sol`, and HTTP 200. It reported 7,249 input tokens, 34 output tokens, 4,134 ms duration, and 3,223 ms TTFT; route ID `1791960021526` and response ID `chatcmpl-resp_0f32bd62d983745e016acf6c4805b0819aa331b6a94c7b329c` were present. The CLI does not send `X-Magpie-Session` and the gateway exposed no separate request ID, so this is a single matching provider/model/status row in an isolated 20-second window, not session-header correlation. The sanitized build, CLI, route, usage, service, and gate records are in [`cli-smoke/final-build-probe-bdb76ef.jsonl`](cli-smoke/final-build-probe-bdb76ef.jsonl).

The model catalog request returned the seven Codex model IDs listed above. Magpie remained active at `127.0.0.1:3425`, bound only to loopback; its executable version and SHA-256 are recorded in the JSONL evidence. The user-level Codex config SHA-256 was `ed36cd73e68fbfaae67465d9d13b9798aabee6e8b7fc8c3d258cdf7369dcfb9c` before and after, and its contents were not read. No credential or auth file was read or copied.

Final validation for this source commit passed: `make verify` completed in 407.634 seconds with 8,861 tests passed, 0 failed, 6 skipped, 0 lint issues, and 0 scanner errors (1,807 scanner warnings); the full race gate included TUI. Tagged E2E completed in 170.323 seconds with 4,856 passed, 0 failed, and 6 skipped. The full live Magpie run [`../terminal/20261014-193912-aafaafae/run-report.json`](../terminal/20261014-193912-aafaafae/run-report.json) passed all 194 checks with 36 real provider requests, 55 screenshots, CLI exit 0, and a clean race detector.

The earlier scoped package-wide TUI race run described below was interrupted while CPU-bound and did not produce a test result. It remains recorded as interrupted, not as a pass or failure; the final `make verify` above completed its full TUI race gate successfully.

## Remote integration and focused validation (bdb76ef)

The current source integration commit is `bdb76efb0b31152e4c554268b43d39ef6285115a`, a merge of local parent `ec267df999405a03a09ed5485256aee6cc133aae` and remote `origin/develop` parent `595d05999ffb506e36141d6e5cee21b669cb69d75`. The local compaction change already estimates provider-visible message metadata, while the remote request-preflight helper had been subtracting the same metadata again. The merge renames that helper to `preparedCompactionMessageBudget`: it still validates metadata serialization and estimates, but only subtracts the configured multimodal reserve. The post-compaction envelope estimate and fit guard remain authoritative.

The new exact-boundary regression exercises both prepared-request compaction and provider-overflow recovery. At budget 12 it removes the older tool-call group and keeps the mandatory system message, the latest complete tool-call/receipt group, and the current user input unchanged; the retained provider-visible estimate is exactly 12. The direct budget test verifies `30 - 7 = 23` and `12 - 7 = 5`, so the multimodal reserve is deducted once and old-history metadata is not pre-reserved. Invalid, negative, overflowing, and failed metadata estimates still fail closed.

Focused validation against the merged source:

- `rtk proxy go test ./internal/core ./internal/compaction ./internal/tui` passed.
- `rtk proxy go test -race ./internal/core ./internal/compaction ./internal/tui` passed core and compaction. The package-wide TUI race process remained CPU-bound and was manually interrupted after about 2 minutes 13 seconds with no test result; this is **not** recorded as a pass or a test failure.
- `rtk proxy go test -race ./internal/tui -run '^TestToolCallSpinnerCommand' -count=1` passed both focused spinner tests.
- `rtk proxy go vet ./internal/core ./internal/compaction ./internal/tui` passed.
- `rtk proxy /Users/yinyuepeng/go/bin/golangci-lint run ./internal/core ./internal/compaction ./internal/tui` reported 0 issues.

The scoped merged-source diff hash recorded for the three request-budget files is `cb4d0ae0c926fa8190f698eb50400d424b0cbf91fdf1b1ac665e6d59dd9ac558`. The final launcher probe and parent full-gate/live results are recorded above; the `ec267df` probe remains historical evidence only.

## Sources

- [Magpie upstream README](https://github.com/yetone/magpie) — official installer and subscription discovery behavior.
- [Magpie reference: Codex and gateway APIs](https://github.com/yetone/magpie/blob/main/docs/reference.md) — local gateway endpoints, request-route and usage inspection, and Codex model catalog behavior.
- [Magpie release v0.1.1231](https://github.com/yetone/magpie-releases/releases/tag/v0.1.1231) and its official [latest-release manifest](https://usemagpie.ai/api/latest) — release version and asset SHA-256.
- [OpenAI Codex Config Basics](https://developers.openai.com/codex/config-basic) — Codex user-level config location and precedence.
