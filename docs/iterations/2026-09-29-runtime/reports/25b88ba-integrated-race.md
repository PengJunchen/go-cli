# Durable CLI / entrypoint 集成 race

测试源码提交：`25b88ba890f59be488e2ea5defbf09186baf873d`，分支 `codex/runtime-repair-20260929`。执行期间至记录时的后续提交 `69d24d8`、`7c078ff` 只修改审查与设计文档，产品源码未变。环境为 Darwin arm64、Go 1.26.4；没有推送或生产验证。

实际命令（退出 0）：

```sh
rtk proxy env GOMAXPROCS=2 GOFLAGS='-p=2 -trimpath' NO_COLOR=1 TERM=dumb go test -json -race -count=1 ./internal/cli ./internal/config ./internal/session ./internal/core ./cmd/cli
```

Go JSON 事件记录 2,541 次 test run、2,541 次 pass、0 fail、0 skip（包括子测试）。五个包均有真实测试执行及 pass package event：

| 包 | package elapsed |
|---|---:|
| internal/config | 2.283s |
| internal/session | 5.798s |
| internal/core | 5.868s |
| internal/cli | 24.645s |
| cmd/cli | 99.457s |

完整事件流保存在 [gzip JSONL](../evidence/25b88ba-cli-core-session-race.jsonl.gz)，[stderr](../evidence/25b88ba-cli-core-session-race.stderr) 为空。此结果验证当前接受的 durable CLI、entrypoint、Memory 和 stream 修复的五包集成；不包含尚在孤立 worktree 开发的 compaction、attempt、safe-read、MCP generation、Hook/Skill/trust 与 UI 修复，不是最终 `make verify`、coverage、tagged E2E、PTY 或 R0–R8 全部完成证据。
