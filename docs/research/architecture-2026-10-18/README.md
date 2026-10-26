# Go CLI 动态能力架构研究

本目录给出 OpenCode v2 源码研究、go-cli 当前状态、目标架构与分阶段计划。建议方向是**typed Go 模块化单体**：控制面把已验证配置编译成不可变能力计划；运行面以 run lease 和明确 owner 管理资源；需要进程隔离时再启用可选 RPC 后端。不要把 Effect/TypeScript runtime 移植成 Go 框架，也不要把原子快照误当作资源生命周期。

## 结论与状态

- OpenCode 对 go-cli 最有价值的参考是 LayerNode/Location/Scope、按能力消费前渐进暴露、MCP 的独立生命周期，以及失败时可观察的状态。固定源码为 v2 分支 commit `4617210822bdbb31e5749751afbfb4584ccedac8`；该日期的 v2 分支头不是最新 release tag。研究链接固定到该 SHA，不混用 `dev` 或其他版本。
- go-cli 可以沿用现有 Assembly、MCP owner、prepared dispatch 与 runOwner 增量演进。目标应是一次 run 内能力一致、撤权后拒绝新的敏感调用和迟到结果、资源关闭有界且可诊断；不能承诺强杀忽略 context 的 Go goroutine，也不能假设独占资源总能双开回滚。
- 修复版 PoC 在独立 fresh copy 中通过 8 个初始生命周期/并发反例、跨 retiring/candidate 资源峰值与取消等待检查；从 [feasibility.md](feasibility.md) 恢复源码后，26 个顶层测试的普通、race、25 轮重复及 targeted 用例也通过，支持有限 generation/lease 机制的可行性。它没有覆盖全部 P0 合同，也没有接入生产 Assembly；P1–P7 仍未实施，详见 [review.md](review.md)。
- 基线 `make verify` 已完成：golangci-lint 0 issues、build 成功；test gate 8,867 started / 8,861 passed / 6 skipped / 0 failed；校验框架与 goroutine leak gate 通过，最后记录 `Verification complete`。环境为 Go 1.26.1 darwin/arm64 Apple M1，不等同于 Go 1.24 CI。

## 阅读顺序

1. [review.md](review.md)：当前审查结论、PoC 对抗点与未验证边界。
2. [architecture.md](architecture.md)：目标运行时、owner、权限、run pin 与资源策略。
3. [iteration-plan.md](iteration-plan.md)：阶段验收合同与回退策略。
4. [current-state.md](current-state.md)：go-cli 现状与基线证据。
5. [reference-projects.md](reference-projects.md)：跨项目一手源码对照。
6. [opencode-v2.md](opencode-v2.md)：OpenCode v2 专项源码研究与固定 SHA permalink 索引。
7. [feasibility.md](feasibility.md)：冻结 PoC、完整源码 fences、P0 覆盖矩阵与基线验证数据。

## OpenCode 学习优先级

先借鉴 Scope 所有权和显式 Location，再借鉴运行时按需投影工具/skills 的能力边界；随后评估 MCP 连接复用与配置 reload 的失败语义。把 OpenCode 的具体 TypeScript/Effect supervisor、全局 hook 注册表和 loader 形态视为其运行环境选择，不直接移植为 Go API。OpenCode 的 `generation` 整数只保护异步更新检查结果，不能当作通用 generation lease 或工具目录 revision。
