# R3 optional nonblocking run claim review

Accepted source `921ce33f1b2fc3bc54cd698c9693778fab79049c`, integrated as `8f54b3b`. The parent reviewer inspected both changed files independently of the builder. This acceptance covers the optional guard operation only; it does not accept model selector or Assembly lifecycle integration.

`TryClaimRun` uses the same mutex, current claim, and notification state as `ClaimRun`, `Release`, and `CloseAdmission`. It tests closed admission before busy, publishes exactly one accepted claim under that mutex, and does not reserve a slot on either error. Existing `RunSlotGuard` implementers remain source-compatible. The operation does not use a canceled context or spawn a late claimant to simulate a try operation.

The reviewer enumerated all four new tests, then ran them on the integrated tree with race detection and `-count=20`. All passed in 1.252s; see `../evidence/r3-run-slot-try-root-race.log`. The builder separately reported full core race, vet, lint, and diff checks passing. The previous guard-only integration at `4c3351c` passed full core race and repository lint; those results are not represented as a full-repository test of `8f54b3b`.

The initial builder RED was an unavailable-API compilation failure, which is not behavioral TDD evidence. A behavior-only baseline with API stubs has been requested; until its output is received, this report claims regression execution and source review, not a completed behavioral RED/GREEN record.

Model switching must still acquire its nonqueue mutation gate before building a candidate, use this exact shared guard, retain generation cleanup responsibility, and fence admission after a failed publication callback. Those obligations are outside this small guard commit.
