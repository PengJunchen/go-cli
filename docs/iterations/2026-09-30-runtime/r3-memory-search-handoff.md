# R3 memory search: small corpora and Chinese substrings

**Status:** designer handoff; implementation and tests are not included here.

**Baseline:** `668076f8f5e18a95a8a01cd1dc68d42faa644cb5` (`codex/r3-memory-search-design`).
**Scope:** `FileMemoryStore.Search` and its private tokenizer only. This handoff does not change the R3 envelope work or memory-store API.

## Observed behavior

`internal/memory/file_store.go` currently tokenizes lowercased runs of Unicode letters and digits. A continuous Chinese phrase is therefore one token. `Search` indexes exact query-token matches, computes `idf = ln(N/(1+df))`, sums normalized term frequency times IDF, and discards scores `<= 0` before normalizing relevance to the best result. This creates two independently reproducible gaps:

- With one stored record and one matching term, `ln(1/2) < 0`, so the only hit is discarded.
- With two records and a term unique to one (`df=1`), `ln(2/2) = 0`, so that hit is discarded.
- A short CJK query such as `中文` does not match a longer continuous source token such as `本地中文检索`.

The existing English tests establish behavior to keep: `TestSearch_RelevanceRanking` checks exact relative and normalized scores, and `TestSearch_CommonTermNotDiscriminating` checks the three-record case where a term in two of three records has zero IDF and returns no hits. Existing empty-query, punctuation-only-query, and no-match tests also remain part of the contract. The execution plan `docs/iterations/2026-09-25-runtime/execution-tasks.json` explicitly names `TestMemorySearchOneAndTwoDocuments` and `TestMemorySearchCJKSubstring` under R3-BLD-03. The public `MemoryStore.Search` signature remains query/limit based, and `Memory.Relevance` is documented as a 0–1 value filled during retrieval; the execution plan, rather than the interface comment, supplies the small-corpus and CJK acceptance cases.

These are static predictions from the implementation and formula, not results from running tests in this handoff.

## Minimum design

Keep the existing scorer and its positive-score filter. Apply a corpus-size floor only when calculating IDF:

```text
effectiveN = max(N, 3)
idf        = ln(effectiveN / (1 + df))
score     += (termFrequency / documentTokenCount) * idf
```

For `N >= 3`, this is exactly the current formula, so the three-record common-term result and existing English scores/order stay unchanged. At `N=1`, a hit gets `ln(3/2) > 0`; at `N=2`, a term unique to one record also gets `ln(3/2) > 0`. In a two-record corpus, a term present in both still scores zero. The one-record corpus necessarily has no competing record, so a matching term is allowed to retrieve its sole record. Do not replace this with a generally smoothed IDF formula: that would make the established three-record common term positive and broaden the ranking change.

Change tokenization only for Han-script runs, leaving ordinary English tokenization byte-for-byte equivalent in behavior:

- Treat contiguous Han runes as one run. Emit overlapping two-rune tokens for runs of length two or more (`中文检索` → `中文`, `文检`, `检索`); emit the rune itself for a one-rune run.
- Continue to emit each contiguous non-Han Unicode-letter/digit run as the existing lowercased token. A Han/non-Han script transition ends one run and starts the other, even without punctuation.
- Whitespace, punctuation, symbols, and emoji end a run. Never form a CJK bigram across one of these boundaries.
- Use this one tokenizer for query terms, indexed document terms, term frequencies, and document lengths. Keep the existing OR-style query-term matching and IDF/TF accumulation; do not add phrase-search semantics.

This lets the two-rune query `中文` match that substring inside a longer Han run while limiting Han matching to pairs (or an exact one-rune run). A one-rune query is intentionally not a substring search through longer Han runs; if product acceptance requires that broader behavior, resolve it separately rather than adding unigrams to every run and silently changing the frequency and common-term model. The Unicode Han property includes ideographs used in Japanese as well as Chinese; Hiragana, Katakana, and Hangul retain current grouping. Tokenization uses Unicode code points, not grapheme clusters.

The store builds its term index from document content on `Add` and again on JSONL load, so changing the private tokenizer requires no persisted-index migration or file-format change. Do not add a new interface, configuration option, dependency, trace event, or per-search log. `FileMemoryStore` already satisfies `MemoryStore`; the change is local deterministic ranking/index behavior and introduces no new lifecycle boundary worth tracing.

## Builder TDD handoff

First add focused failing tests in `internal/memory/file_store_test.go`; record the pre-change failures for the tests that expose the two formulas and CJK exact-token mismatch. Then implement the two narrow changes above and rerun the focused tests. Do not infer that a predicted failure occurred until the builder runs it.

Required acceptance cases:

1. **One record:** add one memory with a unique English term, search for that term, and assert exactly that ID with relevance `1.0`.
2. **Two records, shared and unique terms:** make one term occur in both records and another occur only in one. The shared-term query returns no results; the unique-term query returns only its owner with relevance `1.0`.
3. **Chinese substring:** in an independent corpus of at least three records, query a two-rune phrase such as `中文` inside a longer Han run such as `本地中文检索`. Assert only its owning record is returned, so this checks tokenization without relying on the small-corpus floor.
4. **Single Han rune boundaries:** in an independent corpus of at least three records, a one-rune Han query succeeds when a document is that exact one-rune run. In a separate corpus of at least three records, querying the same rune does not match when it occurs only inside a longer Han run; multi-rune runs emit bigrams, not unigrams.
5. **No script conversion:** in a corpus of at least three records, query `汉字` against a document containing `漢字` and assert no hit because the pair has no shared bigram. Do not add simplified/traditional conversion. Conversely, use exactly four records where only `后台管理` and `後臺管理` contain `管理`. Query `后台管理` and assert the traditional document is returned through the shared `管理` bigram and existing OR term matching. That result is shared-token matching, not conversion.
6. **No accidental bridge:** with a three-or-more-record corpus, query `中文` and assert no hit for content containing `中，文` or `中🙂文`; the bigram must not cross punctuation or emoji. Keep these isolated boundary fixtures so corpus size does not confound the result.
7. **Empty and no match:** retain empty-query, punctuation-only-query, and unrelated-query cases returning no results.
8. **Compatibility and score range:** keep `TestSearch_CommonTermNotDiscriminating` unchanged (three records, term in two, no results) and `TestSearch_RelevanceRanking` unchanged (same order and relevance assertions). Every returned result must have strictly positive relevance in `(0,1]`, and the top result must be `1.0`; run the existing keyword and partial-match tests too, preserving English tokenization and OR matching.

Tests should assert IDs and score relationships, not ordering between equal-score records because the current sort does not define a tie-breaker. The focused gate is `go test ./internal/memory -run 'Test(MemorySearch|Search_)' -count=1` (adjust the regexp to the final test names), followed by the repository's normal required checks in the builder/reviewer stages.

## Risks and review points

- The effective corpus floor deliberately makes a one-record matching term retrievable even though it occurs in every record. Confirm this as the intended single-record behavior; keeping N=2 ubiquitous terms at zero preserves the common-term rule there.
- Han bigrams increase token count and change Han term frequencies/document lengths, so Chinese relevance values may differ. The English-only scoring path must remain identical when `N >= 3`.
- The tokenizer will split adjacent Latin and Han runs and will treat punctuation/emoji as hard boundaries. Cover those cases rather than relying only on the positive substring test.
- One-rune Han substring matching through longer runs, grapheme-aware segmentation, language-specific word segmentation, phrase/AND semantics, stemming, and ranking redesign are outside this narrow fix.
- The planned regex is only a focused check. The builder must also follow the repo's normal test/check instructions and report failures separately; this design handoff makes no claim that tests or full verification passed.
