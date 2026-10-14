# Durable CLI live-orphan crash matrix builder report

Date: 2026-09-30
Worktree: `go-cli-repair-20260930-crashmatrix`
Branch: `codex/repair-20260930-crashmatrix`
Base: `584d57b491c2af5222c6d67c67918930b1256abb`

## Scope and phase boundary

This stage adds actual-binary crash and restart evidence for the durable CLI path. The correctly named R2 input in this repository is `docs/iterations/2026-09-25-runtime/r2-cli-session-handoff.md`; there is no `r2-durable-session-design.md`. It is read alongside `docs/iterations/2026-09-29-runtime/reviews/durablecli-final-review.md` and `docs/iterations/2026-09-29-runtime/reports/durablecli-builder.md`.

The accepted scoped safe-read admission source is integrated at base `584d57b491c2af5222c6d67c67918930b1256abb`. This test commit covers live-orphan crash recovery, exact-session resume, and mutation quarantine only. It does not yet assert a real safe read during quarantine or verify that the older mutation marker remains after that read resolves; those assertions belong in a separate test commit.

## Actual CLI process scenario

The test builds `./cmd/cli` from this worktree, then runs that binary with isolated `HOME`, XDG config/cache, temp directory, session store, and a single workspace directory. A loopback OpenAI-compatible provider returns a call to the registered builtin `bash` tool. The Bash command is launched by the CLI after its session intent and global `dispatch_ready` row have been synced. A nonce-bound effect gate holds it until the test has read both durable files. Each Bash invocation appends a unique nonce/PID/random invocation ID to an append-only start ledger, then appends that same ID to an append-only workspace effect ledger after the gate opens; each append is followed by `sync`. The command advances a heartbeat containing its invocation ID while waiting on a fixture-specific release file.

The test kills only the owned CLI process via its `exec.Cmd.Process.Kill`. It does not signal a PID from durable data, kill a guessed process group, or run a helper before the durable dispatch. After the parent exits, the original invocation's heartbeat advances, proving the actual Bash process started by the CLI survived. The test rereads both append-only ledgers and asserts one start and one effect before the kill and after every exact reopen. At the crash point the original physical session has one Bash intent and no receipt or terminal; the workspace index has exactly one dispatch marker and no resolution.

The next CLI uses the exact physical session ID. Real `PrepareResume` recovery appends one `unknown`/`dispatched` receipt for the same action ID, the provider receives that historical call and receipt, and the run adds one successful terminal. The ledgers remain at one invocation and one effect, and the workspace index remains at one dispatch and zero resolutions.

A separate new physical session in the same working directory attempts a `write`. The real CLI returns exit code 1 with the workspace quarantine rejection, leaves the visible mutation absent, and records the assistant call and intent without a receipt or terminal. Its action receives no global dispatch marker, and both ledgers remain at one record, proving the distinct session caused zero effects. This baseline also exposes a CLI completion issue: after the admission error, terminal commit fails because the assistant tool-call group is still open and unreceipted. The test asserts this observed fail-closed result instead of manufacturing a receipt.

Reopening that new session by its exact ID consults the real workspace index, observes that this second action has no dispatch marker, and appends one `interrupted`/`not_dispatched` recovery receipt. The resumed run completes with a successful terminal and does not call the write executor. The original Bash child remains live through both restarts and the conflicting-write attempt. The fixture then writes the matching nonce to its release file, observes the exact invocation's exit proof and a stable final heartbeat, and confirms that child exit alone did not add a resolution or change the older `unknown` receipt.

Every observed physical session row has a unique source ID and version 1. The test checks exact physical IDs, action IDs, tool-call IDs, workspace ID, append-only start/effect counts, dispatch/resolution counts, receipt outcomes, terminal source IDs/run IDs/sequences, and the four provider request bodies/authorization headers. Fixture cleanup releases the nonce-bound process and waits, with a deadline, for an exit proof for every invocation in the start ledger; it never sends a process signal.

## Verification

- `rtk proxy go test -v -count=1 ./cmd/cli -run '^TestDurableCLIBinaryQuarantinesLiveOrphanedBashAndNeverReplays$'` — passed on Darwin after rebuilding the actual CLI from base `584d57b491c2af5222c6d67c67918930b1256abb`; no skips, `cmd/cli` reported 32.391s.
- `rtk go test -race -count=1 ./cmd/cli` — passed; 61 tests in one package, including the receipt-after-sync subprocess case and this live-orphan case.
- `rtk go vet ./cmd/cli` — passed.
- `rtk golangci-lint run ./cmd/cli` — no issues.
- `rtk git diff --check` — passed on the corrected test and report changes.

These checks are for the CLI package and the named actual-binary scenario on the current Darwin host. They do not establish full `make verify`, other-OS process behavior, or actual safe-read behavior under an unresolved orphan; that crash assertion remains for a separate test commit.
