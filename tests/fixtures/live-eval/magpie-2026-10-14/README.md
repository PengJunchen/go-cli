# Magpie live QA fixtures (2026-10-14)

These are the small reusable inputs retained from the final live CLI run, `20261014-193912-aafaafae`. The complete scenario results, earlier failures, fixes, caveats, and next steps are summarized in [`docs/research/2026-10-24-live-qa-digest.md`](../../../../docs/research/2026-10-24-live-qa-digest.md).

- `presentation-brief.md` is the original task brief migrated from the temporary run workspace.
- `create_live_deck.go` is the generated source used by the live task to create/edit its synthetic four-page PPTX. The generated PPTX itself was removed as a run output.
- `large-context.txt` is the exact synthetic file read during compaction testing (273,673 bytes). The fixture uses a run-specific nonce and contains no personal context.

SHA-256 and byte counts for the retained copies are recorded in `manifest.json`.
