# Injected tool cache privacy independent review

Reviewer: root; builder: design_grep_capture. Decision: **ACCEPT this bounded slice**.

Source target: `4bf8a784faddc7ff3a17e571c6c14c03e4250429`, base `17ab7b76ef0b7636abff1e5a055de7acf6f6bb31`.
Report-path-only follow-up: `91b450d2b9bf758bcf4477dc43d144045bc84c8f`.
Detached review checkout: `/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/repair-20260929-cache-review-4bf8`.

## Source judgment

The private key now hashes a versioned domain, tool name and JSON arguments, separated with NUL bytes; JSON encoding cannot contain an unescaped NUL separator. Stable JSON map ordering preserves deterministic keys. The returned key contains only the fixed version prefix and 64 hexadecimal digest characters. Neither raw tool names nor serialized arguments enter the FIFO cache's key, log or span fields.

Encoding failure returns an explicit non-cacheable result. The wrapper bypasses both Get and Set while still executing the tool, so unsupported arguments cannot alias under a bare tool name. Mutation tools still bypass the cache; a nil cache skips key generation. The default assembled wrapper still passes nil to this concern (`assembleProduction`), so this patch does not enable result caching or approve its broader identity/effect semantics.

Tests use the actual FIFO cache and trace exporter to check log/span secret absence, a spy cache to check keys and marshal-failure bypass, and execution counts to check repeat behavior. The patch is limited to the existing private helper and wrapper call site. It adds no protected configuration or new cache service.

## Independent verification

Environment: Darwin arm64; `GOMAXPROCS=2`, `NO_COLOR=1`, `TERM=dumb`, `GOFLAGS='-p=2 -trimpath'`.

```sh
rtk proxy env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb GOFLAGS='-p=2 -trimpath' go test -race -p 2 -count=5 -run 'TestToolCache|TestProductionToolWrapper' ./internal/cli
```

Exit 0; actual output: `ok github.com/pengjunchen/go-cli/internal/cli 1.169s`.
Log: `/tmp/go-cli-runtime-cache-root-review-4bf8.log`.

The builder separately records complete CLI race, vet and lint evidence in `reports/cache-builder.md`. Root's full integration gate remains a later step. This acceptance establishes injected-key privacy and marshal-failure behavior, not R3 phase completion, production cache activation, bounded argument serialization memory, or live financial accounting.
