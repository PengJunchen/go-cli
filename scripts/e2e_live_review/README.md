# Live presentation fixture and reviewer

These scripts are for preparing input and inspecting an artifact produced by a separate, real Dot run. They never author or repair a PPTX.

Generate a per-run fixture JSON (it contains a fresh nonce, synthetic metrics, four exact distinct slide titles, and the two prompts). A live harness that already owns a run nonce should pass that nonce so all fixture paths and acceptance markers use the same value:

```bash
rtk python3 scripts/e2e_live_review/generate_fixture.py --out /path/to/run-evidence/fixture.json
```

```bash
rtk python3 scripts/e2e_live_review/generate_fixture.py --nonce A1B2C3D4E5F6 --out /path/to/run-evidence/fixture.json
```

Hash and retain that JSON before submitting its first prompt to Dot. Save the PPTX immediately after turn 1, before submitting the second prompt. Then run the independent structural/content check and render the final deck for visual review:

```bash
rtk python3 scripts/e2e_live_review/verify_pptx.py \
  --fixture /path/to/run-evidence/fixture.json \
  --before /path/to/run-evidence/after-turn-1.pptx \
  --after /path/to/run-evidence/after-turn-2.pptx \
  --out /path/to/run-evidence/pptx-review.json \
  --render-dir /path/to/run-evidence/rendered
```

The verifier checks the PPTX ZIP and internal OOXML relationship targets, the exact four-slide count, each exact editable slide title, required nonce/fact/status strings, and a text/shape-structure diff that allows only the requested status replacement. It also checks that the before/after SHA-256 values differ and that rendering produced exactly four page images. Rendering uses `soffice` and `pdftoppm` found on `PATH`; pass `--soffice` and `--pdftoppm` to select installed binaries explicitly. A rendered result still needs a human full-slide visual inspection.

This script report does not prove the LLM created or edited the deck. The separate live evidence ledger must bind each real user turn to completed upstream inference, the provider tool-call IDs, Dot dispatch/execution results, and the corresponding before/after file hashes. See `docs/qa/2026-10-14/live/review/acceptance-matrix.md`.
