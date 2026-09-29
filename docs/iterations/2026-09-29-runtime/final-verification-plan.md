# 最终集成验证计划

本计划对应原 R0–R8 的剩余验收，不扩展 R9。它是执行安排，不是测试通过报告。每个 builder 的包级结果只证明其孤立提交；最终结果必须关联修复分支上同一个固定的产品源码提交。完成后合并至本地 `develop`，不推送。

## 集成前条件

- 源码切片有独立 review，拒绝项已修复并重新审查；保留原始失败证据。
- 持久化 CLI、压缩、attempt accounting、prepared loop detector、安全读取 proof、MCP generation、Hook/Skill/trust snapshot、默认能力与真实 TUI 已接入其生产入口。
- 原任务台账由指定 owner 按实际集成、测试和审查证据更新；设计接受、测试名存在和合并成功均不等于阶段完成。
- 额外的 `protected-changes-test-gate.patch` 尚未获授权、尚未应用。原补丁的授权不能代替这一补丁的批准。先完成所有已授权源码与验证，再提交确切差异供批准。

## 固定提交上的门禁

记录 branch、HEAD、工作树状态、Go/工具版本、环境、命令、完整日志和退出码；测试统计包括 run/pass/fail/skip，零匹配不能算通过。

1. 运行 `make verify`，覆盖 fmt、vet、lint、build、全仓 race、scanner、test-log、test-leak。临时工具目录 `/tmp/go-cli-runtime-verification-tools` 安装仓库依赖版本 `golang.org/x/tools/cmd/goimports@v0.38.0`，避免跳过 import 格式检查。共享 lint 锁用串行执行解决，不能改 lint 配置。
2. 运行 coverage 门禁（至少 65%）与 `-tags=e2e ./tests/...` 的真实测试执行门禁。全仓结果和 tagged E2E 分别记录；不以一者代替另一者。
3. 并行集成处按需要运行完整 tools/core/session/compaction/llm/production/CLI/TUI/MCP/ACP race。只有新变更、失败或未解决的问题才要求额外重复测试。
4. Scanner 的 ERROR 必须修源码或有严格可解释的扫描器修正，不靠忽略、降级或受保护配置绕过；WARN 的数量和性质在最终报告单列。
5. `make fmt` 若改变源码，先审查并提交，再在新的固定提交上执行受影响门禁；不能把格式化前的结果挂到格式化后的 SHA。

常规资源控制为 `GOMAXPROCS=2`、`GOFLAGS='-p=2 -trimpath'`。测试环境由原合同决定：普通非终端测试可用 `NO_COLOR=1 TERM=dumb`，ANSI 和 PTY 合同必须使用其规定的终端环境，不能统一禁色使断言失去意义。

## 用户入口与运行时证据

| 验收 | 必需证据与完成边界 |
|---|---|
| Durable restart | 真实 `cmd/cli` 二进制、隔离配置、loopback provider、真实 JSONL/index；创建、managed tool、正常退出、第二进程 exact physical session resume；receipt 同步后的 SIGKILL 不重复副作用。 |
| 未确认执行和遗留子进程 | 真实 CLI tool child 在 receipt 前已执行、根进程被杀且 child 仍存活；第二 session mutation 被隔离；同一 workspace 的受限 Read/LS 通过真实 sealed proof；旧 marker 不被新 read resolution 清除、不重放旧操作。测试 helper 用受控释放和退出证据回收，不能向未验证或复用 PID 发信号。 |
| Durable compaction | 真正的绑定 sink、不可伪造的 source provenance、首次和后续请求 preflight、相同 prepared handle refresh；失败零 provider dispatch、live/restore 一致、bound post-terminal hook 不再改写已发布历史。 |
| Provider policy | 真实 assembled chain 的初始、retry/fallback/overflow/cycler/summary 调用，共享 operation cap、一调用一 breaker settlement、实际 transport fence attempt identity、usage/cost known/unknown。Loopback 是协议与实现证据，不宣称线上供应商已验证。 |
| Loop detector | 普通 protected proxy 与 durable consumer 都观察同一 frozen trusted attempt；阈值调用零执行、无下一 provider 请求；并行 call 各有唯一 receipt，caller/detector first-cause 的 live 与 durable outcome 一致。 |
| MCP/ACP/serve | 真实 MCP 子进程的初始化失败、静默 read 取消、Disconnect/Wait；真实 peer/config generation 替换、撤销和 leases join；private HTTP profile 真实 agent、认证/隔离/限流、失败启动无 token/listener。Ask remote approval 与 remote TUI 仍按明确不支持合同验证零启动副作用。 |
| Trust/Skill/Hook | 持久 trust 的真实 CLI grant/list/revoke/restart；六个根配置候选及 executable source 的 present/absent、排序成员和精确 frozen bytes；变化拒绝加载且不按路径重读。Skill 有界加载/schema/args/XML，Hook detached DTO、明确支持 action 和 owner identity。 |
| 默认能力 | Manual memory 真实二进制 CRUD 与重启；模型不能调用 mode-control 绕过用户 Plan Mode；现有 help、tools 和配置错误反映实际能力。不得以 doctor pass 宣称功能已实现。 |
| TUI | 真正 App Update/View、canonical snapshot/overlay、owner publication、apply ACK 与 owned pumps join。10k rows 和 1MiB/1000 chunks 的实际 app latency/alloc/heap；1k/5k/10k 比较。 |
| 原生终端 | Darwin native PTY，宽度 60/80/120/200、resize、每 pane anchor、approval/modal/input/cancel/cleanup pending；完整旅程与 raw transcript。PTY 不证明 GUI、IME 或 accessibility。 |
| Docker | 可用 Docker daemon 上执行现有 Dockerfile 的 `linux/amd64` build 和 binary help smoke。此文件固定 amd64，不能宣称 ARM64；未执行的平台 gate 单独标为未验证。 |
| Trace/audit/shutdown | 真实 CLI/Assembly/REPL/serve 的 root/child span、错误和退出状态、prompt/secret privacy、disk/export failure、producer/owner join。单元 exporter 或 owner 结果不能代替完整入口。 |

## 结束标准

最终独立 release review 核对原任务的每项验收及其证据。仍有规定测试未执行、真实入口未接线、受保护补丁未批准或阻塞项未解决时，不标 R0–R8 完成。合并开发分支时记录准确 source/target SHA 和合并结果；没有推送或生产验证时明确不作相应声明。
