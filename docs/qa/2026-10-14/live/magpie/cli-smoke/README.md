# Real go-cli to Magpie CLI smoke

> Retention note (2026-10-24): generated build probes and JSONL run captures were removed after this README retained their summarized outcomes.


Captured 2026-10-14 using the existing executable `/Users/yinyuepeng/projects/go-cli/bin/go-cli` (SHA-256 `32680dee2e53eb44644986d6410a66a3280d5fb00c664c0ddf30de21f19081c2`), with Magpie already running on loopback at `http://127.0.0.1:3425/v1`. No source was changed or rebuilt. The child environment was an explicit allowlist with an isolated `HOME`, XDG config/cache/state roots, working directory, and temporary directory; `GO_CLI_PROVIDER_BASE_URL` was explicitly set to the loopback gateway. The only API key value was the dummy local string `magpie`. The isolated config enabled only the `read` builtin, constrained its configured path to the temporary fixture workspace, and enabled JSONL tracing at redaction level `redact`.

Two actual, non-mock CLI invocations completed with exit code 0 and empty stderr. The text run returned exactly `MAGPIE_TEXT_0b5da450b1206d80`. The second run invoked the real `read` tool on `fixture.txt`, received a successful tool receipt containing `MAGPIE_READ_b9295b8782a71ac5`, and returned exactly that nonce. See `runs.jsonl` and the redacted CLI stdout files.

Magpie's local usage endpoint returned HTTP 200. Its rows independently report provider `codex`, model `gpt-6.1-sol`, and HTTP 200. Text usage matches the CLI's 6,260 input/19 output. The read run has a 6,269/18 initial tool-call row and a 6,312/18 final generation row; Magpie recorded the latter as 168 uncached input plus 6,144 cached input and 18 output. See `gateway-usage.jsonl`. Correlation uses timestamps and matching usage values: this CLI does not expose a custom `X-Magpie-Session` header, and Magpie's `req` value is a model alias rather than a unique request ID. No account/session/email fields, auth headers, request bodies, or credentials were copied into the evidence.

`traces-redacted.jsonl` contains 21 spans for the text run and 31 for the read run. It keeps span timing/status and an allowlist of non-user-controlled attributes; prompt/tool arguments and other user-controlled attributes are omitted. Raw isolated traces, config, fixture, stdout/stderr staging files, and environment state remain outside the repository and were deleted after these redacted artifacts were written.
