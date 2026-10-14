# MCP close admission independent review

Reviewer: root; builder: review_existing_seams. Decision: **ACCEPT this bounded slice**.

Fixed review target: `28646ed0d9ca64fc3841fefb865a57b85afe173d`, base `17ab7b76ef0b7636abff1e5a055de7acf6f6bb31`.
Detached review checkout: `/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/repair-20260929-mcp-review-2864`.

## Source judgment

The production constructor captures the existing `*assemblyMCPResources` in its close owner. The first `Close` synchronously sets `closing` under the same mutex used by `Reload` admission and `addReloader`, before starting the asynchronous stop owner. This is a known private mutex-and-bit operation, not an arbitrary callback held in `Close`.

The patch retains the existing single owner and does not move potentially blocking `RequestStop` calls into synchronous admission fencing. An already admitted reload remains subject to its reloader's actual completion; `waitReloaders` still precedes client disconnection. Repeated `Close` keeps the original stop and resource-close callbacks. Nil resources are valid for assemblies without MCP.

The new test holds an admitted reload and the asynchronous stop callback with separate barriers. A post-`Close` reload returns `mcp.ErrHotReloadPending` without entering the reloader; releasing the stop callback does not disconnect clients until the admitted reload has actually joined. The test then observes client completion and one completed owner. Existing blocked-stop and panic-pending tests cover adjacent ownership behavior.

## Independent verification

Environment: Darwin arm64; `GOMAXPROCS=2`, `NO_COLOR=1`, `TERM=dumb`, `GOFLAGS='-p=2 -trimpath'`.

Command, exit 0:

```sh
rtk proxy env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb GOFLAGS='-p=2 -trimpath' go test -race -p 2 -count=20 -run 'TestAssemblyClose(FencesMCPReloadBeforeAsyncStopAndJoinsAdmittedReload|FencesBeforeBlockedStopCallback|PanicRetainsPendingOwner)$' ./internal/cli
```

Actual output: `ok github.com/pengjunchen/go-cli/internal/cli 2.442s`.
Log: `/tmp/go-cli-runtime-mcp-root-review-2864.log`.
`git diff --check 17ab7b76 28646ed0d9ca64fc3841fefb865a57b85afe173d` also exited 0.

The builder separately reports the complete CLI race suite, vet and lint in `reports/mcp-fence-builder.md`; those are builder evidence, not another root full-suite run. Root will run the combined integration gates after all accepted slices are assembled. This acceptance does not close R6/R7 phase tasks or establish remote-peer/platform coverage.
