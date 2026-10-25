# Operator-driven live PTY QA

This harness connects an actual `go-cli` binary to owned PTYs and exposes their bytes through a localhost xterm.js page. Browser inputs go to those PTYs. It uses the existing transparent Magpie forwarder and audit correlation from `scripts/e2e_live/run.py`; it never substitutes model responses. It does not launch Playwright, control a browser, or take screenshots. An operator uses the approved browser UI and captures the real page. Label screenshots **browser xterm rendering of actual PTY output**, not native Terminal screenshots.

The harness leaves `HOME` unchanged. Project configuration explicitly isolates `session.store_path` (a directory), `history.path`, `memory.store_path`, `model_registry.cache_path`, and enabled `production.audit.path` under the task root. Production auditing defaults to enabled, so interactive start rejects a missing or escaping audit path unless auditing is explicitly disabled. Preparation also disables the model registry and sets its task cache: a local false scalar alone may not clear an inherited true setting. The production binary must support these fields. This does not relocate the production workspace authority or trust store: only this run's exact workspace records are allowed there.

## Prepare

Install only the renderer's pinned production dependencies into a task-local cache. Do not run the old renderer or its Playwright dependency:

```sh
mkdir -p "$TASK_ROOT/cache/terminal-renderer"
cp scripts/e2e_terminal/package.json scripts/e2e_terminal/package-lock.json "$TASK_ROOT/cache/terminal-renderer/"
npm ci --omit=dev --ignore-scripts --prefix "$TASK_ROOT/cache/terminal-renderer" --cache "$TASK_ROOT/cache/npm"
```

Create a task-local scenario JSON. The supported role names are `ask`, `auto`, `deny`, and `dynamic`. All paths must be absolute, inside the task root; `enabled:false` switches a role off. `fixture_config` is optional and is copied only when `.go-cli.json` does not already exist. `args` are optional extra real interactive CLI arguments, not shell text. A dynamic fixture must supply its actual runtime/Skill/MCP/extension configuration; the harness invents none.

```json
{
  "roles": {
    "ask": {"workspace":"/absolute/task/workspaces/ask","enabled":true,"approval":"ask"},
    "auto": {"workspace":"/absolute/task/workspaces/auto","enabled":true,"approval":"auto"},
    "deny": {"workspace":"/absolute/task/workspaces/deny","enabled":true,"approval":"deny"},
    "dynamic": {
      "workspace":"/absolute/task/workspaces/dynamic-skill-hook",
      "fixture_config":"/absolute/task/artifacts/dynamic-config.json",
      "enabled":true,"approval":"ask","args":[]
    }
  }
}
```

The optional `--prepare-workspaces` step merges those storage paths into each existing JSON config. It preserves explicit `production.audit.enabled:false` and otherwise makes the default enabled state explicit. Do this **before granting project trust**. Never prepare or edit captured project sources after grant without using the real revoke/regrant lifecycle. YAML project config that shadows `.go-cli.json` is rejected at interactive start. Fresh harness workspaces must have no preexisting trust key or authority records.

```sh
python3 scripts/e2e_live_dynamic/run.py \
  --root "$TASK_ROOT" --binary "$TASK_ROOT/bin/go-cli" \
  --assets "$TASK_ROOT/cache/terminal-renderer/node_modules" \
  --scenario-config "$TASK_ROOT/artifacts/live-scenarios.json" \
  --ready-file "$TASK_ROOT/artifacts/live-ready.json" \
  --prepare-workspaces
```

Starting this server does not execute the CLI or contact a model. It prints only the loopback URL and private control-file path. The page contains a private per-run nonce, and control POSTs require both this nonce and the exact loopback Origin/Host. Keep `artifacts/control.json` private; do not publish its nonce. Only the three pinned xterm assets are served, never arbitrary task files. The server must remain loopback-only. No personal credentials are loaded: child environments preserve `HOME` and a small system allowlist, and use the existing local provider sentinel. Ambient cloud credentials, proxies, and SSH agents are excluded.

## Release and operate

After final production build and checks, the coordinator writes the ready JSON with `allow_start:true` and the exact binary `binary_sha256`. No start is allowed without this matching gate. The binary must be inside the task root. A coordinator can withdraw the gate to prevent new starts; existing processes are closed explicitly.

Open the printed loopback URL in the approved browser. For each role, **Start Trust grant terminal** runs the actual binary's `trust grant` in that workspace. Review its real captured source output, then enter `TRUST` using the visible text control. Trust input is sent as plain text; interactive input uses real bracketed paste. **Start go-cli interactive** requires this task's nonrevoked manifest-bearing trust key and valid isolated storage config. The frontend never grants trust itself. The second trust terminal shares the same canonical workspace with its interactive counterpart, allowing real revoke/regrant while the first PTY remains alive.

The textarea, Send, Send-and-Enter, and labeled key controls are test controls, separate from the product terminal. Enter/Esc/PageDown/y/n and Ctrl+C are actual PTY bytes. The xterm also accepts direct keyboard input. `screenReaderMode` exposes its real output to accessibility/DOM consumers. All live PTYs are resized to the same browser viewport. Output is polled in bounded byte chunks and replayed through `term.write(Uint8Array)`, retaining actual ANSI bytes. A session switch replays the owned spool; it does not synthesize terminal state. A 128 MiB spool cap is explicitly reported if reached and must not be confused with a product output limit.

The terminal container is fixed at 420 CSS pixels high with zero inner padding so the fit calculation and actual viewport agree. A full-page browser capture cannot change its row count through a temporary `vh` resize. The **scroll to latest output** button only changes the xterm viewport and sends no PTY bytes; prefer it to terminal key combinations for screenshot navigation. After a screenshot, refresh the browser's DOM observation before another control action. Reloading the page preserves backend processes; choose an existing terminal to replay its actual spool. If the coordinator replaces the binary during a run, preserve each earlier session's binary SHA and probe/final phase separately rather than attributing every attempt to the latest ready gate.

History replay suppresses xterm-generated device replies through each historical write callback. At terminal selection the controller captures the current PTY byte count as the replay boundary, then forwards `onData` only for live bytes beyond it; replaying an exited session never sends terminal replies. This prevents historical OSC color/CSI cursor queries from injecting replies into the current CLI input. Visible human input controls still use the normal PTY APIs. Initial browser-navigation/input calibration failures belong to the harness evidence and must not be described as model prompts or product failures.

Each interactive terminal gets a unique `X-Magpie-Session` and its own transparent loopback bridge to the existing `127.0.0.1:3425` Magpie instance. The requested model defaults to the full `codex/gpt-6.1-sol` ID. The bridge forwards original request/response bodies, status, SSE chunks and allowed headers, preserving local-sentinel Authorization without logging it. It accepts only authenticated `/v1/chat/completions` POSTs. Magpie itself retains account authentication; the harness never opens its credential stores. Optional `--model` changes the real requested model; `--gateway` accepts only the existing loopback port3425.

Set a turn label before sending a prompt. **Save and verify Magpie route/usage** records session-scoped, projected metadata and ID-based correlations. It can truthfully show missing or mismatched evidence; a requested/catalog model is not sufficient proof of the served model. The legacy correlator permits session-order fallback only when usage exposes no IDs, and rejects conflicting exposed identities. These metadata files contain no request/response bodies. Raw own-fixture PTY bytes and production traces are separate, private evidence and need review before publication.

Each request also retains its actual advertised function names, parameter-schema SHA-256 values, and a SHA-256 of the complete advertised tools array. Schema bodies and descriptions are omitted. Correlated tool receipts retain the bounded decision-source and decision-choice fields, so a visible keypress can be checked against its durable attribution. For an acceptance plan requiring exact response/route/session identities, any legacy ordered fallback remains diagnostic evidence and must not be counted as a verified model request.

A role may supply `private_fixture_facts`, a task-only map of labels to selected private fixture literals. The transparent bridge checks decoded JSON strings in memory and saves only `fixtureFactsChecked` plus the matching label array `fixtureFactsPresent`; it never saves those literal values or request bodies. A checked request with no matches proves only that the selected literals were absent from decoded JSON strings. It does not prove absence under other encodings or general information-flow security. Keep the scenario file private and use this facility only for owned fixture sentinels.

## Ownership and shutdown

`artifacts/owned-resources.json` records canonical workspace IDs, exact authority paths/sidecars, the shared trust-store location and only the owned trust-entry metadata/hash, plus owned process IDs and output byte hashes. Workspace identity is derived exactly as production: SHA256 of `go-cli/workspace-quarantine/v1\0` plus the canonical workspace path. On macOS, authority is under `~/Library/Application Support/go-cli/workspace-quarantine/` with `<workspaceID>.jsonl`, `<workspaceID>.lock` and related own-ID sidecars. The actual trust store is `~/.config/go-cli/trust.json`. No authority or trust creation is simulated or redirected.

Close each PTY or stop the harness with SIGTERM/Ctrl+C. It signals only its `start_new_session` process groups, waits, fences lingering descendants, drains output, closes local bridge servers and writes the final ownership manifest. Before deleting task data, the coordinator must verify all owned processes have exited, use the real CLI to revoke only these project keys, and atomically clean only the corresponding owned trust tombstones under the production lock if required. Delete only the manifest's exact own authority records after processes exit. Never remove/overwrite the shared trust store, its lock, other entries or its whole directory. The harness deliberately does not implement global cleanup.

This is an operator-driven live test facility, not an automatic assertion that a scenario passed. Screenshots, transcript state, real filesystem effects and independent route/usage evidence must be reviewed together. The harness itself does not launch browser automation; the operator controls the actual page through approved CUA and captures its real screenshots. It does not automate native Terminal, mock model responses, or replace global HOME/config.
