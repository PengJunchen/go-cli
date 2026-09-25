# R6 private-file and scoped-auth code review

Review date: 2026-09-25  
Review target: `codex/runtime-r6-private-files` at `cde6c2bee2eda89f6084a48c8527177f5e037183` (follow-up to `9368af4a413c311be649b22bab91bb4e209f87e1`; base `98a6930639179d648a709df229a396674d4cdc90`)  
Review type: independent source review, disposable boundary probes, focused tests, affected-package race tests, and vet. No product source files were changed by the reviewer.

## Decision

**Accepted for this bounded slice.** The initial findings against `b9e709c` and the follow-up dangling-parent-symlink classification bug are fixed in the reviewed head. The private-file/auth tests and focused config race pass. This is not a whole-R6 implementation or release sign-off; broader R6 gates remain separate.

The review is limited to the private-file helper, the auth readers, and the trust-store, onboarding, and serve-token writers.

## Fixed findings from the initial review

At `b9e709c`, three read-path failures were reproduced with disposable tests:

1. `lookupAuthFile("openai")` followed `$HOME/.config` when it was a symlink and returned the target key. The final reader rejects a non-directory `.config` entry for the actual default auth path; `TestLookupAuthFileRejectsDefaultConfigDirectorySymlink` covers this.
2. With no usable `$HOME`, `modelsConfigPath` fell back to the project-relative `.config/go-cli/models.json` and returned a scoped key. The final path resolver returns an unavailable path instead; `TestLookupScopedModelsJSONUsesNoProjectRelativePathWithoutHome` proves the relative credential is ignored and the next scoped source is used.
3. `models.json` read failures and invalid credential documents were converted to `ErrAuthNotFound`. The final reader distinguishes genuine missing files from read/parse/shape failures, so a corrupt source stops lower-priority fallback. Maintained coverage is in `TestLookupScopedModelsJSONReadAndParseFailuresAreSourceErrors`.

The final parser rejects top-level `null`, `credentials: null`, malformed credential arrays, null records, missing/invalid scope fields, empty or whitespace keys, and malformed records anywhere in the array. It validates the entire credential list before returning a match, rejects conflicting keys for one normalized scope, accepts identical duplicates idempotently, and rejects duplicate JSON member names for `credentials` and the record identity/key fields. A valid object without scoped credentials and a valid scope mismatch remain `ErrAuthNotFound`.

`models.json` is handled conditionally: 0644 metadata with no scoped records (including an empty `credentials` array) remains readable and unmatched, while a non-empty scoped credential source must pass the private auth reader's regular-file, 0600-or-narrower file, 0700-or-narrower parent, and symlink checks before its key is returned. No general private-mode rule was applied to metadata-only files.

## Follow-up blocker closed

**Blocking finding on `9368af4` — `isMissingScopedCredentialFile` followed dangling parent symlinks while deciding whether ENOENT means “missing.”** I reproduced this in a temporary test, then removed the test from the review worktree:

```text
HOME/.config is a real directory
HOME/.config/go-cli -> HOME/missing-config   (dangling symlink)
lookupScopedModelsJSON(openai scope) -> ErrAuthNotFound
```

The reproduction command was:

```text
rtk proxy go test -count=1 -run '^TestReviewDanglingModelsParentSymlinkIsSourceError$' -v ./internal/config
```

It failed because `errors.Is(err, ErrAuthNotFound)` was true. `os.Lstat(models.json)` reports ENOENT through the dangling ancestor; the classifier then uses `os.Stat` while walking upward, follows the symlink, and reaches the existing `.config` directory, which it interprets as proof that the credential file is genuinely absent. The resolver therefore continues to lower-priority sources.

The `cde6c2b` follow-up changes the nearest-ancestor inspection from `Stat` to `Lstat`, so it sees an existing symlink entry rather than following it to a missing target. The maintained `TestLookupScopedModelsJSONRejectsDanglingSymlinkPathComponents` covers a dangling final file, immediate parent, and `.config` ancestor; each is now a source error. A genuinely absent file under a real directory remains `ErrAuthNotFound`.

## Reviewed write and read boundaries

`internal/privatefile.WriteAtomic` creates a unique sibling temporary file, explicitly applies mode 0600, loops to complete short writes, syncs and closes before rename, then syncs the containing directory. Write and rename failure tests verify that old bytes remain and temporary files are removed. A directory-sync failure reports that replacement already occurred and the new bytes remain. Existing broad parent directories are rejected without chmod; newly created parents are private. Parent and destination symlink fixtures verify rejection or replacement of the link itself without modifying its target.

The onboarding writer routes through `WriteAtomic`. Its default-path hardening rejects `.config` and `go-cli` symlinks before creating files or changing an external target's mode; custom broad directories fail closed. The trust-store and serve-token writers use the same helper. A serve-token write failure returns before listener startup. The auth reader rejects broad permissions, non-regular files, corrupt JSON, generic parent/final-file symlinks, and the default `.config` symlink without leaking fixture secrets or permitting fallback.

## Verification evidence

The reviewed head is `cde6c2bee2eda89f6084a48c8527177f5e037183`, with merge base `98a6930639179d648a709df229a396674d4cdc90`.

The focused model/parser selection passed independently on `9368af4`, before the path-classifier follow-up:

```text
rtk proxy go test -count=1 -run 'TestLookupScopedCredentialDataRejectsDuplicateMembers|TestLookupScopedModelsJSON' ./internal/config
ok github.com/pengjunchen/go-cli/internal/config
```

The full affected-package race suite passed independently on `9368af4`:

```text
rtk env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb go test -race -p 2 -count=1 ./internal/privatefile ./internal/config ./internal/approval ./internal/cli
ok github.com/pengjunchen/go-cli/internal/privatefile
ok github.com/pengjunchen/go-cli/internal/config
ok github.com/pengjunchen/go-cli/internal/approval
ok github.com/pengjunchen/go-cli/internal/cli
```

The focused race selection for the path-classification fix passed independently:

```text
rtk env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb go test -race -p 2 -count=1 -run '^TestLookupScopedModelsJSON(RejectsDanglingSymlinkPathComponents|MissingFileIsNotFound|ReadAndParseFailuresAreSourceErrors)$' ./internal/config
ok github.com/pengjunchen/go-cli/internal/config
```

Affected-package vet passed on the immediately preceding source head (`9368af4`):

```text
rtk proxy go vet ./internal/privatefile ./internal/config ./internal/approval ./internal/cli
```

Config-package vet also passed on the final source head:

```text
rtk proxy go vet ./internal/config
```

## Scope limits

This review does not cover the broader R6 trust/backend/protocol or real-entrypoint gates. It does not establish OS-sandbox behavior or a complete defense against same-user concurrent filesystem replacement; the reviewed path checks are fail-closed checks within this slice, not a dirfd/openat sandbox. Do not report the full R6 phase as complete based on this review.
