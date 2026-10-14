# R4 Grep bounded-capture design handoff

**Design baseline:** `be08f808c69f8e7c464f068aea7d1cd326c2a929` (`codex/runtime-stabilization`).
**Status:** design only; no production changes or R4 acceptance claim.
**Scope:** Grep source capture, ripgrep framing, and the directly related tests. This design does not modify the concurrent R4 directory-capture lane.

## Source findings

`GrepTool.Execute` validates the requested root with `resolveAndValidatePath` before calling `search`, and then formats `[]match` into the existing `file:line:content` output. Keep that order and keep the existing whitelist option and metadata keys.

There are three independent source-capture gaps:

1. `grepWithRipgrepManaged` uses `runManagedSearchCommand`, which captures the entire stdout stream in a `bytes.Buffer`. `truncateMatches` applies `MaxMatches` and `MaxOutput` only after capture and parsing, so those public limits do not bound helper-output memory.
2. `grepPureGo` appends every matching line to `[]match`; it applies the same limits only after a complete walk. The 1 MiB `bufio.Scanner` token ceiling bounds one scanned line, but does not bound the result slice.
3. `parseRipgrepOutput` assumes every output row contains two colons. Ripgrep's real single-file `-n` format is `line:content`, so the row is discarded for lacking a path. Splitting at the first colon also misreads colon-bearing paths, and `Scanner.Err()` is ignored when the helper stream contains a row beyond Scanner's token limit.

The local helper is ripgrep 15.2.0. The real single-file behavior is the primary regression trigger; fake `rg` scripts remain useful for exact lifecycle and malformed-stream cases.

## Proposed capture and wire contract

Keep the existing public options and defaults: `MaxMatches` defaults to 1,000 and `MaxOutput` defaults to 1 MiB. Add no exported option or result type. Treat negative option values as an effective zero budget so they cannot cause a negative slice bound; zero retains no matches and reports truncation if a matching record is omitted. Use overflow-safe subtraction when checking byte budgets.

Use one private `grepMatchCapture` for both the Go scanner and the ripgrep stream. It stores at most the effective match count and the serialized bytes of retained complete records. The accounted record is the displayed path, `:`, decimal line number, `:`, content, and one newline byte. A record either fits whole or is omitted whole. On the first omitted matching record, set `truncated=true`; retain no later records. Continue walking Go input to preserve existing scan/error/cancellation behavior, but do not retain more matches. The ripgrep reader may stop decoding after it has proved truncation, but must continue draining stdout to EOF through the managed owner.

The external route should invoke ripgrep in JSON mode, for example with the fixed arguments `--no-config --json [-g=<glob>] -e <pattern> -- <validated-root>`. `--json` carries the input path and line number for both a file root and a directory root, so the parser no longer infers fields by splitting display text. Preserve the existing `RIPGREP_CONFIG_PATH` removal, inherit all other environment entries, preserve one-argument glob placement, keep the pattern behind `-e`, and keep `--` before the root. Tests should assert the exact argument sequence and environment behavior.

Read ripgrep's newline-delimited JSON events incrementally. A JSON string encodes a filename LF as `\\n`, so the event's outer LF remains an unambiguous frame even when the filename contains a newline or colon. Decode the JSON `path` and `lines` values in either their `text` or base64 `bytes` form, and take `line_number` from the event. Convert those values into the existing `match{File, Line, Content}` without cleaning, rebasing, or otherwise normalizing the path. Match `bufio.ScanLines` content semantics exactly: remove one trailing LF when present, then remove one trailing CR; at EOF without an LF, also remove one trailing CR. This preserves existing Go-scanner behavior for CRLF and final-CR lines.

For backward compatibility, ordinary output rows remain `file:line:content`, and colon-bearing paths remain byte-for-byte in `match.File` and in the display. The formatter should reversibly quote only filenames that contain line-breaking/control bytes or invalid UTF-8, using Go string quoting, so one actual match remains one output record. The quoting is a display encoding; the internal `match.File` continues to hold the decoded path bytes. Add tests for both the exact `match.File` and the one-record display. No structured downstream parser contract is added in this slice.

Keep `Metadata["matches"]` as the number of returned records and keep `Metadata["truncated"]` always present. Append the existing `\n[results truncated]` marker after retained records when the flag is true. `MaxOutput` counts the match records including each record's newline, as today; the marker remains outside that matched-output budget for compatibility. Exact-budget output is not truncated unless a later match is observed. If the first match cannot fit, return zero matches, `truncated=true`, and the marker. Empty searches return empty output, zero matches, and `truncated=false`.

Use a fixed per-JSON-frame safety limit, proposed at 8 MiB, independent of the match-output budget. This bounds the one-frame parser buffer while covering the default 1 MiB visible output budget with headroom for JSON escaping and framing. If a single encoded JSON event exceeds that frame limit, latch a typed `errGrepCaptureLimit`, discard its remainder and subsequent stdout, and keep draining. Do not misreport this case as ordinary truncation: after the managed helper joins, return the typed error. For pure Go, retain the existing 1 MiB scanned-line ceiling but wrap `bufio.ErrTooLong` in the same typed capture-limit error instead of returning partial success. The fixed frame/line limit is an explicit cap for unusually large single matching lines; it adds no configuration API.

Only `match` events add results. Validate the event fields needed for a match (`path`, `line_number`, and `lines`); malformed JSON or a malformed match event returns a typed `errGrepOutputProtocol`, rather than being silently skipped. Non-match ripgrep event types are parsed and ignored. Once a known match has been omitted by count or output-byte budget, stop decoding further events and drain them without retaining or interpreting them; `truncated=true` makes that omission explicit.

## Error, fallback, and ownership rules

The ripgrep route must use the writer-targeted managed command hook from directory-capture commit `ebfc9900cb1d5fd7313944553220f96994e910fc` only after that commit receives independent acceptance. That hook writes synchronously from the existing owned stdout reader inside `managedLocalCommand.collect`; it preserves process-group supervision, stderr draining, and reader joining. Do not add a second process supervisor or an untracked capture goroutine. If `ebfc990` is not accepted or integrated, keep this implementation blocked on that dependency rather than copying lifecycle ownership code.

The writer must never return a capture-limit error from `Write`, because doing so makes `io.Copy` stop reading before the helper and its readers join. Instead it records the typed error, clears the current frame, and returns `len(p), nil` for all later writes. Call `Finish` only after `managedSearchLifecycleError` confirms the readers joined and cleanup is known. If lifecycle is pending, canceled, or unknown, return no partial output and `errors.Join(lifecycleError, captureError)` when a capture error also exists. Preserve `errors.Is` for the caller context and typed capture/protocol errors, and preserve `errors.As` for `PendingExecutionError` / `OutcomeUnknown`; do not manufacture a new completion signal.

Fallback remains limited to a normally joined ripgrep process with an ordinary helper error, as it is today. A typed capture-limit or output-protocol error is terminal and must not fall through to Go. Cancellation, pending cleanup, and unconfirmed containment are terminal and must not fall through either. A clean capture limit is a user-visible error only for an overlong single encoded event/source line; ordinary configured count/byte exhaustion is successful, explicit truncation. For any helper failure eligible for fallback, do not start Go until the managed helper's readers and owned process group have joined.

Retain the existing root and descendant distinctions in the Go path, but stop swallowing meaningful incompleteness. Failure to open or enumerate the requested root is an error with the original cause preserved. An unreadable/disappearing descendant directory or file may be skipped best-effort, but sets `truncated=true`; cancellation is returned as `ctx.Err()` and is never converted into partial success. A scanner/read error while consuming a matched file remains an explicit error and returns no partial matches. Preserve the existing per-file whitelist check in the Go scanner and the pre-dispatch `resolveAndValidatePath` check for both backends; no helper dispatch occurs for a rejected root.

## TDD and acceptance matrix

Start with behavior tests that run against real ripgrep when installed and fake helpers for deterministic boundaries. The real-helper tests may skip only when `exec.LookPath("rg")` fails; the repository's local rig is ripgrep 15.2.0.

| Contract | Test and assertion |
| --- | --- |
| Single-file path survives ripgrep framing | Search a real fixture file with `grepWithRipgrep`; assert one `match` has the fixture's exact path, line number, and content. Baseline fails because plain `rg -n` emits `1:content` and `splitMatchLine` drops it. |
| Colon and LF filenames survive | Create filenames containing `:` and LF on POSIX; compare exact `match.File`, line, and content. Through `Execute`, assert one match remains one displayed record and metadata count is one. Baseline colon splitting corrupts the path; plain line framing cannot safely preserve LF names. |
| CRLF and final-CR content matches Go scanner | Search fixture lines ending in CRLF and a final line ending in CR without LF through both real rg and forced Go. Assert identical `Content` values, matching `bufio.ScanLines` (the terminal CR is removed in both cases). |
| Directory output and glob stay compatible | Search a multi-file tree with a glob using real rg and forced Go; assert existing path/content/line fields and safe-path output. Capture the helper argv and verify `--no-config`, `--json`, `--glob=<value>`, `-e`, and `-- <root>` positions. |
| Go capture is bounded during scanning | Unit-test `grepMatchCapture` with more than its configured count and with aggregate serialized bytes just below/at/above budget. Assert retained count and bytes never exceed their limits, whole records only, and truncation changes only after a real omitted match. Integration-test `WithForcePureGo`, count and byte limits, marker, and metadata. |
| Ripgrep saturation drains and joins | A PATH-local `rg` emits more JSON matches than the configured count, then writes a completion marker. Assert the marker exists before return, output contains only the accepted prefix plus the truncation marker, and no Go fallback occurs. |
| One event over the parser bound is explicit | A fake `rg` writes a valid oversized JSON event followed by a completion marker; place an ordinary matching file in the work tree so an accidental Go fallback would succeed. Assert the returned error matches the capture-limit sentinel, output is empty, the helper marker exists, and fallback success is impossible. Also unit-test a row split across many writer calls and a final event without a newline. |
| Malformed events are not dropped | Feed invalid JSON, missing match fields, invalid base64 bytes, and a frame beyond the hard cap. Assert protocol/capture errors instead of skipped matches. Preserve `errors.Is` to the sentinel. |
| Context and escaped writer remain owned | Retain `TestGrepTimeoutWithEscapedPipeWriterPreservesCancellationCause` and `TestGrepForcedPipeCloseStopsFallbackAndWithholdsProof`; adapt the fake helper to emit a valid JSON match before it starts the escaped writer. Assert `errors.Is(context.DeadlineExceeded/Canceled)`, no partial output, no fallback, no positive completion proof, and only the existing pending/unknown lifecycle result. |
| Root validation precedes spawn | With a configured whitelist, use PATH-local `rg` that writes a marker; request an outside absolute root. Assert validation error and absent marker. Add a symlink-escape variant. |
| Environment remains hardened and otherwise inherited | Extend `TestGrepSeparatesPatternAndIgnoresRipgrepConfig`: expect JSON-mode argv, assert child sees `RIPGREP_CONFIG_PATH` unset, and assert an unrelated sentinel environment variable remains inherited. |
| Root/descendant errors are honest | A missing/unreadable requested root is an error; descendant access failures are best-effort with `truncated=true`; a file-scanner error is returned, not silently converted into empty/complete output. Test cancellation closes the active file walk before returning. |

The initial RED should be a compile-successful behavior failure for the actual single-file ripgrep fixture and path framing, plus an integration failure showing configured limits still materialize all results. Do not count a test that only fails to compile due to a new private type as the primary RED. After implementation run the `internal/tools` race suite and the R4 `internal/approval`, `internal/core`, and configured-file-tools CLI race selections; also run `go vet ./internal/tools`, the repository linter for that package, and `git diff --check`.

## Scope limits and adjacent follow-up

This design bounds retained match memory and one parser/source record. It does not bound directory traversal time or total file bytes scanned; context cancellation remains the way to stop broad searches. It does not change Read/Ls/Find implementation, Grep registration/schema/effect classification, approval, RemoteBash, or R2 run ownership. `rg` helper failure may still use the existing Go fallback only after the real managed owner confirms join and containment.

The synthetic-path tests in the separate R4 directory-capture lane prove ordering/capture mechanics for their supplied path strings; they do not prove backend-wide Find semantic equivalence. Keep these separately audited Find questions visible and out of this Grep implementation:

- `fd` defaults to regex matching while the public Find description and Go implementation use glob matching. Verify the local `fd` contract and decide whether options can align them without changing public behavior.
- External `find` may include the search root while the Go walker omits a directory root. Verify single-file and directory-root behavior per installed backend.
- The `fd` adapter treats exit status 1 as no matches. Verify the supported local `fd` versions and the exact status contract.
- Independent fresh review of `ebfc9900cb1d5fd7313944553220f96994e910fc` reproduced LF-delimited capture splitting a legal `odd\nname` path into two records and whitespace trimming dropping an all-space filename. A separate directory-capture delta should use NUL framing (`fd --print0`, `find -print0`) where supported, ignore only truly empty records, and test an actual system `find` plus shims. Newline path display encoding must remain explicitly specified, and metadata must count actual retained records.

These are adjacent Find audit/follow-up items, not scope for this Grep design. Do not claim general cross-backend Find compatibility from synthetic path equality.
