# Extension API v1

The public package imports only the standard library. Compatible hosts accept
`host_api` values `1`, `1.0`, and `^1.0`. Unknown capabilities, duplicate names,
unsupported versions and undeclared contributions fail before publication.
Fields can be added within v1; incompatible semantic changes require another
major package and handshake. The host supports tools, existing hook events,
commands, complete-response providers and prompt contributions. Skill DTOs are
reserved; this host rejects RPC skill negotiation and uses standard SKILL.md.

## Host execution support

A declared contribution and a registered tool do not by themselves establish
support for durable tool execution. Generic worker tools currently lack the
request-completion contract required by the durable CLI and are rejected before
dispatch in that mode. Ordinary custom tools have the same limitation. This is
separate from the reviewed completion contracts for the CLI's built-in MCP
transports and managed child agents. See the [runtime guide](../../../docs/runtime-guide.md)
for the current loading, approval, execution, and restart boundaries.

## Protocol and descriptor

`Serve` reads newline-delimited requests and writes matched responses. The first
request must be `initialize` with `{"version":"1.0"}`. Frames including newline
are at most 1 MiB. Methods are `tool.call`, `hook.handle`, `command.run`, and
`provider.generate`; a provider's Stream adapter emits a single final response,
not token deltas. Workers must reserve stdout for protocol frames. The host
does not replay failed calls: transport loss or deadline after dispatch means
the external outcome may be unknown. Workers must honor request deadlines.

Build `tests/external-extension` as an independent executable, compute its
SHA-256, and place a trusted `.rpc.json` descriptor in `extensions.plugin_paths`:

```json
{
  "command": ["/absolute/path/to/worker"],
  "artifact_sha256": "sha256-of-executable-bytes",
  "cwd": "/explicit/working/directory",
  "env": {},
  "max_inflight": 8,
  "timeout_millis": 30000
}
```

A self-contained binary is recommended because the captured entrypoint runs
from a private temporary directory. Launchers depending on libraries adjacent
to their original executable may fail there. Absolute-shebang wrappers can
work when their interpreters and referenced assets use explicit paths. Those
interpreters, extra argv files, scripts and assets remain separately trusted
local dependencies; hashing the captured entrypoint does not pin their contents
or authenticate their supply chain. The example uses a standalone Go binary.

The descriptor grants execution of the named local program. Artifact hashing
checks integrity and does not authenticate its publisher. The host captures the verified executable into a private temporary owner directory
(up to 128 MiB) and executes those captured bytes, removing them after process
exit. The child receives only the explicit environment. It shares the host user's OS permissions;
process isolation alone does not impose filesystem, network, CPU or RSS limits.
Unix teardown kills the worker process group; other platforms currently kill
the direct worker. Native `.so` compatibility remains trusted in-process code,
with no unload guarantee. SDK provider requests never implicitly carry host
API keys, base URLs, credential handles or process environment.
