# Live Magpie terminal E2E

This harness runs the real `go-cli interactive` binary (built with Go's race
detector) on a PTY and renders the original PTY bytes through the pinned xterm.js
renderer used by `scripts/e2e_terminal`. Its localhost bridge forwards every
model request and response to the installed Magpie gateway. It does not contain
or return model fixtures.

The CLI gets an isolated `HOME`, config directory, and workspace. By default
the isolated config does not define `sandbox.allowed_paths`, so the run tests
the normal unscoped registration path. The report records the exact allowed
paths, their hash, the isolated config hash, and observed per-tool registration
source/scope metadata. Magpie keeps using its existing signed-in Codex account.
The CLI's local OpenAI-compatible
API key is the non-secret sentinel `magpie`; the bridge adds a unique
`X-Magpie-Session` header and records only request IDs, route/model/provider,
usage, tool names/IDs, safe path basenames, and hashes of request/result data.
Authorization headers, request bodies, prompts, and full model output are never
written to bridge evidence. The raw PTY transcript separately contains the
actual interactive session for review and replaces the local API sentinel if
it appears.

The default run builds the real CLI with `-race` and exercises one continuous
interactive session. It checks ordinary text turns; real `ls`, literal `grep`,
`read`, `write`, `edit`, and Bash tool calls; a denied write followed by an
approved write/edit; one exact Python Bash command rejected by default policy
even after any normal required approval; and an approved 20-second Bash task
with a queued follow-up. It then creates and separately edits a model-generated
PPTX, verifies real context compaction and recall, and finishes with Escape
cancellation followed by an exact unknown-outcome warning at the idle prompt,
matching quiescent durable call IDs, a fenced ordinary prompt with no provider
request, explicit `/session new`, and a no-tool text turn in a different
physical session. The run performs no workspace mutation after cancellation.
For `/session new`, the runner first matches the CLI-announced ID to the newly
created `s_<id>.jsonl` path; that file may still be empty until the first fresh
user turn is committed. After that turn, it separately requires a durable user
row and successful terminal record bound to the announced physical session.
The PPTX is independently checked as editable OOXML and rendered for human
visual review. The compaction check requires production UnifiedCompactor trace,
log, and durable-session evidence, then verifies the real post-compaction
request still carries the early memory fact and newest question.

Run from the repository root with the locally installed Magpie gateway available
on loopback:

```sh
rtk proxy python3 scripts/e2e_live/run.py --max-tokens 128000 --model-timeout-total 10m --model-timeout-stream-idle 4m
```

To run only the initial text/read/search/list/deny/allow smoke prefix:

```sh
rtk proxy python3 scripts/e2e_live/run.py --smoke-only
```

To exercise presentation generation/editing and real context compaction while
reusing a fresh early-memory seed, without repeating the separately reviewed
smoke/approval/long-task/Escape scenes:

```sh
rtk proxy python3 scripts/e2e_live/run.py --model codex/gpt-6.1-sol --max-tokens 128000 --artifact-focus
```

This mode reports `live-artifact-focus`, identifies its selected scenes in the
JSON/Markdown report, and does not claim full-suite acceptance. Both PPTX
versions are copied from the file produced/edited by Dot's actual CLI tool
calls; the harness only freezes and verifies those bytes.
The local compaction threshold is 128,000 tokens so the real model-requested
large read can fit intact and become a durable source item. Six subsequent
provider-backed prompts each carry about 30 KB of generated QA pressure; they
must trigger a production summary projection whose source boundary covers that
specific read receipt, while the latest prompt and early canary remain available.
The harness records the original receipt hash separately from later compacted
provider requests and does not claim that the full uncompressed receipt was
forwarded after compaction.

When another agent is actively changing production source and a diagnostic does
not need those changes, `--reuse-binary <path-to-a-previous-run>/bin/go-cli-race`
copies a previously recorded race binary byte-for-byte and records its original
run report/hash. This is for focused diagnostics only; the report explicitly
states that the current checkout is not the reused binary's source. Final
acceptance uses a fresh race build from the reviewed production revision.

The default local compaction threshold is 128,000 tokens. It leaves room for
the complete >=200 KB model-requested read before six later real provider turns
apply deliberate large-context pressure; actual UnifiedCompactor evidence is
still mandatory.

The runner exposes `--model-timeout-total` and
`--model-timeout-stream-idle`; final long-call acceptance explicitly uses
`production.model_timeout.total: 10m` and
`production.model_timeout.stream_idle: 4m` for provider calls and genuine
non-streaming compaction summaries. The chosen values are written into the
isolated YAML config and the run report.
The repository's Streaming Bash tool has a fixed 30-second execution limit,
so the long-task fixture sleeps for 20 seconds to retain time for a human-style
follow-up and tool receipt while proving an actual in-flight interval. The
runner captures the visible TUI progress marker only while its `.started` file
exists and `.finished` is absent; it does not infer activity from transcript
phrasing. User turns are delivered through the CLI's enabled xterm bracketed-
paste input mode, followed by a separate Enter byte. The nonblocking PTY writer
drains output while sending and enforces a 120-second deadline; evidence keeps
input byte counts and SHA-256 values, never prompt text.
The turn waiter also checks newly persisted session-store terminal checkpoints,
so a CLI error followed by an interactive `>` prompt is recorded as a failed
turn immediately instead of being mistaken for successful idle or waiting on a
missing output artifact.

For a separate scoped comparison, opt in explicitly with
`--scoped-workspace`. This adds only that run's isolated workspace to
`sandbox.allowed_paths`; it is diagnostic evidence and does not replace the
default unscoped acceptance run.

The runner installs the existing pinned terminal renderer dependencies if they
are absent, builds `./cmd/cli` with `-race`, creates a per-run isolated workspace,
and writes screenshots, PTY bytes, gateway metadata, and a Markdown/JSON report under
`docs/qa/2026-10-14/live/terminal/`. Generated workspaces are retained in
`docs/qa/2026-10-14/live/workspaces/` for independent artifact review. The
presentation is authored by model-requested Write/Edit and Bash tool calls. The
model uses Go's standard library and `go run` to create and edit OOXML because
the real CLI explicitly blocks script interpreters such as Python and Node in
its Bash tool. The harness copies the first-turn deck to evidence before
submitting the edit turn; it does not author or repair a presentation.
Each isolated workspace is also an empty Git root. The parent evidence folder
ignores generated children, and ripgrep would otherwise inherit that ignore
rule and miss the synthetic fixture files during the real `grep` tool call.
This boundary affects only the isolated test workspace; it does not add
`sandbox.allowed_paths` or change CLI ignore behavior.
The generated workspace parent also has a small `go.mod` boundary so model-
authored `.go` helper files remain outside the product module during `go test
./...`; the runner ensures it exists and records its hash before building.

The bridge distinguishes provider request kinds. A streamed request must carry
the current user prompt's expected marker (when the prompt has one) to count as
an interactive turn. Non-streaming calls are recorded as internal inference,
which keeps a production compaction-summary request from being mistaken for a
completed user turn merely because it ran while that turn was active. Turn
labels with no expected provider response (the post-cancel fence and
`/session new` command) are excluded from the interactive-turn completion
checks. Each internal request still needs its own completed upstream response,
route, and usage proof.
The per-request route snapshot must report the unique Magpie session; the
runner separately records a local bridge capture tag to show which inference
triggered that snapshot. The gateway does not return that local tag.
Usage rows must agree with every response ID, route ID, and session field they
expose; the harness never falls back to order after an exposed ID conflict.
Session-order usage matching is reserved for gateway versions whose usage
records omit both response and route IDs entirely.

Screenshots are genuine xterm.js renders of bytes read from the real CLI PTY in
headless Chromium. They are not native Terminal screenshots. Provider evidence
comes from Magpie route and usage audit endpoints and the upstream response
metadata observed by the transparent forwarder. A passing run requires all
scenario assertions, an upstream Codex route, and no Go race report.

Harness static checks used for this revision:

```sh
rtk proxy python3 -m py_compile scripts/e2e_live/run.py
rtk proxy env PYTHONPATH=/private/tmp/dot-qa-pyflakes-20261014 python3 -m pyflakes scripts/e2e_live/run.py
```
