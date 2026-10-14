# Independent PPTX review: 20261014-184011-290bc55c

**PPTX subcase: PASS.** The live run as a whole failed later during context compaction; this document accepts only the artifact subcase.

## Provenance and structure

- Fixture: `docs/qa/2026-10-14/live/terminal/20261014-184011-290bc55c/presentation-fixture.json`, SHA-256 `958fa40c46d3afa667ef31fcb64efdf0c7c0e1458cf8688941a2d803b51f5757`.
- Before deck SHA-256: `75e7b11c8f37af414aa5d69b86d892a4e10416a0eb1adc5f0355e367e9d13404`.
- After deck SHA-256: `e01ea49a2a3637c942882432559c1eb000d9774cf35c8d33645d582b3b902aac`.
- Dot's workspace artifact and the frozen after-turn copy are byte-identical. The run report records the target absent before turn 1, the first-turn snapshot before turn 2, and a distinct second user turn.
- The separately captured model-authored Go helper changes by one line only: `releaseStatus` from `Release status: INITIAL` to `Release status: REVISED`. The before/after helper snapshots have SHA-256 `b59270576ef7234e3450ea943586e60f9e15785addea140126f577fed51f3001` and `906f0454e4b402abc1bf0166b0910d851ce1abcef292a98bfde9b15621572b07`.
- Provider sequences 19–21 contain the turn-1 Write/Bash creation path; model call IDs `call_b94b789bb3f341edbad0fdd537e91f9f` and `call_06dfd13aa84741528223d99dfb6b7ad1` have later success receipts and durable quiescent completion proof. Sequences 22–24 contain the separate turn-2 Edit/Bash path; IDs `call_380d23b5e35f4718ad9ba78fb3f352e5` and `call_7030bb33be144549b08b2e6c206a740c` also have later success receipts and durable quiescent proof.
- Both PPTX packages contain four slides, 13 editable text shapes, no raster-only pictures, and the canonical main presentation content type `application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml`. The title nonce, four distinct slide titles, all synthetic metric strings, and the “Synthetic QA inputs only - not product measurements” disclosure are present. Slide 4 alone changes from `INITIAL` to `REVISED`; shape order, names, geometry, other slide text, and package relationships remain stable.

The reviewer's independent verifier result is [184011-independent/pptx-review.json](184011-independent/pptx-review.json), SHA-256 `646f0f9009ecec118bd19dfff904c0c8743a5f0ecab3dc84e590830f657ee29b`. LibreOffice and `pdftoppm` rendered four 1601×900 pages. The independent PNG hashes match the harness-rendered PNG hashes exactly.

## Full-size visual inspection

All four unmodified after-turn PNGs were opened at original size. The template is sparse with broad whitespace, but text is readable and the hierarchy is consistent. No clipping, overlap, empty slide, or broken image appeared. Each slide displays the synthetic-data disclosure at the bottom; the requested `REVISED` status is legible on slide 4.

| Slide | Image and SHA-256 | Finding |
| ---: | --- | --- |
| 1 | `pptx-rendered/slide-1.png`, `dfa77231b8355496d052b62e35e328639e117e1e958115a259ec210856991684` | Nonce title and subtitle are legible; disclosure fits along the bottom. No clipping or overlap. |
| 2 | `pptx-rendered/slide-2.png`, `67c21a16cc65d92d49ee4bc091014c74a375d9ac912ee4e42fb4956b20d591db` | “What Was Checked” and the 68-of-100 synthetic metric fit on one line; disclosure is visible. |
| 3 | `pptx-rendered/slide-3.png`, `4576df60c157384cb85a36341328d46dbc9dd29dcadcb144c8ed221d8f54b1d2` | Both synthetic metrics fit and remain readable beneath the title; disclosure is visible. |
| 4 | `pptx-rendered/slide-4.png`, `ef91393d3d22aba5c39810f134325fe601541075ff703079811bac8d3fdbe484` | The `REVISED` status is legible; disclosure is visible. No clipping or overlap. |

The artifact demonstrates model-directed CLI tool creation and a separate model-directed edit of the same file. Its figures are deliberately synthetic and must not be described as product measurements.
