# Independent PPTX review: live run 20261014-182412-40ef8373

## Result

The model-created deck and separate-turn status edit are attributable and preserve the requested slide text/shape structure, but the PPTX package is not interoperable with LibreOffice. **The presentation acceptance subcase fails; visual review is not possible for this artifact.** Both the original and revised deck fail to load, and no PDF or slide PNGs were produced. Do not count the harness's prior structure/content pass as a complete PPTX pass.

## Evidence

- Frozen v1: `terminal/20261014-182412-40ef8373/presentation-before.pptx`, 8,545 bytes, SHA-256 `4f2fed385c0acd3b1e0b322f2544db30ca99f93b6ba5c29d726bffb02baab414`.
- Frozen v2: `terminal/20261014-182412-40ef8373/presentation-after.pptx`, 8,561 bytes, SHA-256 `797a9fbfe45b1021d4c9bd71e549ea0cb98d3446a5b4e2e2d8ed20dd324123cc`.
- The captured model-authored helper changed only `releaseStatus` from `INITIAL` to `REVISED` in the second-turn source snapshot. The different frozen deck hashes and exact status-only OOXML comparison establish that a separate turn modified the existing file. Helper snapshots: create SHA-256 `dffde03729feeb0ca119e5c3cf854879f0ee1ec30bdeb92475a9e09332f0615e`; edit SHA-256 `1c519e53a523a99250d48af31a7b866dcba4689483dca1559f614944a524aefd`.
- The harness's original `pptx-review.json` reports a four-slide deck with 13 editable text shapes, expected fixture strings, and status-only shape/text preservation. Its LibreOffice render step failed with `Error: source file could not be loaded` and created no PNGs.
- Direct conversion of both v1 and v2 with the bundled LibreOffice executable and separate unique user profiles also failed with `Error: source file could not be loaded`. Repeating the same executable/profile approach with the known-working 174155 control deck succeeded, so the Fontconfig warning is environmental noise rather than the reason these files are rejected.
- The successful control PDF is `review/182412-control-render/live_review_3A61DCA80B0C.pdf`, SHA-256 `964232d96aa711d81bb5b556606ab72ca0db3b4592f0e47c2fc4c4189d0c9172` (37,174 bytes). Both current deck render output directories are empty.
- Independent report: `182412-independent-pptx-review.json`, SHA-256 `81ce7ced74f496525efe06e946ec3aa1b0fcd9caf23ec3d7b28a7b73d22f4b58`.
- Working-package control report: `174155-content-type-control.json`, SHA-256 `1126f0365da021f680b578125c6f1505d7d70a8d3a6b806af103b57960645cd4`.

## Package defect

Both new deck versions declare the main presentation part in `[Content_Types].xml` as:

```text
application/vnd.openxmlformats-officedocument.presentationml.presentation+xml
```

The main presentation part requires:

```text
application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml
```

The 174155 control has the correct `.presentation.main+xml` type and loads in LibreOffice. The 182412 model-authored helper constructs the wrong type by concatenating `presentationml.` + `presentation` + `+xml`. Its structural verifier checked for a content-types part but did not check the canonical content type, which allowed its package check to pass.

I added that narrow canonical content-type validation to the owned `scripts/e2e_live_review/verify_pptx.py`. The current 182412 independent report now fails with the exact before/after MIME mismatch, while the 174155 package control passes. I did not modify either model-authored deck.

## Visual assessment

**Not reviewable.** No unmodified slide render was produced for either frozen 182412 version. XML text, slide count, and geometry are not a substitute for inspecting all four slides at full size. A fresh live creation must produce a package with the canonical MIME type before rendering and visual QA can pass.
