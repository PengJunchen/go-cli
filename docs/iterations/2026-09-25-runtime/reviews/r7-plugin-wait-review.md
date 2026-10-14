# R7 exact PluginManager shutdown wait review

Accepted source `c7843be83afcd55772caea9f615a3664744b9e9a`, integrated as `bf8fbc0`. The parent independently reviewed the implementation and its three tests. This accepts only the observation seam, not Assembly shutdown integration.

`WaitShutdown(ctx) (completed bool, err error)` snapshots the existing operation under the manager mutex. It never creates, replaces, or restarts that operation. A missing operation returns the typed not-started error without closing admission. An operation writes its stored result before closing its fixed `done` channel; waiting on that close establishes synchronization before reading `op.err`. No later writer mutates the result. An already-completed operation takes precedence over an already-canceled waiter, and the cancellation branch rechecks completion.

The separate `completed` result is necessary: an extension can itself return `ErrCleanupPending`, so `errors.Is` alone cannot identify an active callback. A canceled wait returns `completed=false`; the callback remains owned by the original shutdown operation. Later and repeated waits retrieve that same operation's outcome without another callback invocation. Nil contexts normalize to Background; a nil manager reports not-started.

The parent listed all three `TestPluginManagerWaitShutdown*` tests before execution. Twenty race repetitions passed in 1.020s, and the full extension package race run passed in 1.411s. Evidence is in `../evidence/r7-plugin-wait-root-race.log` and `../evidence/r7-plugin-wait-main-extension-race.log`. The builder additionally reported behavioral RED with API stubs, package lint, and repository compile-only success. Compile-only evidence is not end-to-end shutdown acceptance.

Assembly must still start shutdown with its own cooperative callback context, then join this exact operation while retaining dependent resources. A waiter timeout must not cause Assembly to rerun Shutdown or advance to resource disposal before `completed=true`.
