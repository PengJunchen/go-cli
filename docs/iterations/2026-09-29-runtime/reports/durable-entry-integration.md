# Durable CLI / serve 集成记录

在已接受 entrypoint 源码 `33cf69d` 上集成独立审查的 durable CLI 源码链 `9a46672 → 4de967c → aeb9604 → bc75e4d → e393464 → 84f88f9`。集成等价提交为 `d9de5e4 → 429c946 → ae23790 → cbfd79f → fb8a96b → 695ac2c`。

`serve_cmd.go` 的冲突保留 entrypoint 的真实装配、早期 Ask 拒绝和失败启动行为；仅在已有非 nil config 检查后加入 durable session unsupported guard，仍早于模型装配、token 写入和监听。没有恢复旧的 nil-dispatcher echo fallback。

集成 SHA `695ac2c` 上，现有 `TestServeRejectsConfiguredDurableSessionBeforeListenerStart` 实际编译并失败：其未指定审批模式，新的早期 Ask 拒绝先返回 `UnsupportedServeApprovalError`，所以未到达该测试要验证的 durable unsupported 路径。修复只给该夹具增加 `--approve deny`；原 durable 错误链、零 provider 请求与消息断言全部保留。

RED 命令（退出 1）：`rtk env GOMAXPROCS=2 GOFLAGS='-p=2 -trimpath' NO_COLOR=1 TERM=dumb go test -race -count=1 -run '^TestServeRejectsConfiguredDurableSessionBeforeListenerStart$' ./internal/cli`。

GREEN 命令（退出 0，CLI 1.190s）：`rtk env GOMAXPROCS=2 GOFLAGS='-p=2 -trimpath' NO_COLOR=1 TERM=dumb go test -race -count=1 -run 'TestServeRejectsConfiguredDurableSessionBeforeListenerStart|TestServe.*(Unsupported|Ask|Runtime|Assembly|Nil)|TestDurableREPLBindingFailureStopsBeforeNextInput' ./internal/cli`。该结果是窄集成回归，不是最终全量门禁或 R2/R6 完成证据；完整 CLI/config/core/session/cmd 集成 race 和最终固定源码 gate 分别记录。
