# R4 durable compaction correction review

**Target blob:** `82f02f89b7ff2c7a4d6af1cc3eff4e168aac870f`. **Source HEAD:** `1144e1ebc9f339a04d502f0bdca04069c9d30ff5`. The target file's working-tree blob matches the supplied frozen hash.

**Verdict: ACCEPT for the frozen handoff.** The requested correction closes the post-terminal path: the source-snapshot-bound history is now the durable discriminator, and `AgentImpl.Run` skips the legacy hook after its canonical loop has committed. Direct `AgentImpl.Compact` on that bound history returns `ErrDurableCompactionUnsupported` before invoking the hook or copying/mutating history. Non-durable hook and manual-compaction behavior remains available.

The durable CLI `/compact` path explicitly retains the 4de candidate's pre-dispatch rejection guard, marks it as not yet integrated in the current root source, and forbids substituting a second manual projection path. The new owner rows and REDs cover the bound run hook, direct `Agent.Compact`, and durable slash command; they also make request preflight the sole bound durable strategy/summarizer entry point.

Static review only. No tests were run and no product source was changed.
