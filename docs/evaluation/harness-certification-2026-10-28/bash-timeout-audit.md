# D 的 Bash 执行时限核查

2026-10-29，只读核查。源码固定为 D `5aa063ee647f753f7f0fc7282a3baf5b6e691932`，profile 为 `main-5736e88-personal-memory-off-v1`。本次没有改源码、配置或原结果，没有执行模型、Docker任务或测试。此核查不阻塞独立 outer-reaper 实现。

## 结论

**D 的真实 CLI 普通 Bash 工具是固定30秒，不是按命令自动选5/30/300秒。** `TimeoutTier` 存在于库实现，但本 CLI 的装配没有启用它；即使命令以 `go` / `make` 开头，也不因此获得300秒。`--approve auto`、`--no-sandbox` 均不改变这个选择。

模型看到的 Bash schema 只有 `command`，不存在可传入的 `timeout`。当前公开 CLI/config 也没有给默认 Bash 延长该时限的入口。库调用方可以构造 `NewStreamingBashTool(WithTimeout(...))`，但这不等于固定 CLI 已经支持该配置，更不等于模型可以自行延长。

该限制会影响需要单次较长同步编译、测试或安装的工程任务。它不证明“Go不能完成长任务”：很多短工具调用仍可组成长任务；也没有证据表明 D 本次 Nav 失败由30秒超时引发。

## 固定源码选择链

| 层 | D 实际行为与固定源码 |
|---|---|
| 评测argv | runner以 `prompt --approve auto --no-sandbox --output json` 启动；[eval_core.py](https://github.com/PengJunchen/go-cli/blob/5aa063ee647f753f7f0fc7282a3baf5b6e691932/scripts/harness_eval/runner/eval_core.py#L29) |
| prompt装配 | approval与no-sandbox分别加入assemble选项，没有Bash timeout选项；[prompt.go](https://github.com/PengJunchen/go-cli/blob/5aa063ee647f753f7f0fc7282a3baf5b6e691932/internal/cli/prompt.go#L216) |
| 注册默认工具 | assemble只给RegisterDefaults传sandbox/resource/git等；[assemble.go](https://github.com/PengJunchen/go-cli/blob/5aa063ee647f753f7f0fc7282a3baf5b6e691932/internal/cli/assemble.go#L1094) |
| Bash构造选项 | RegisterDefaults仅加sandbox及resource limits，没有WithTimeout或WithTimeoutTier；注册的是StreamingBash；[builtins.go](https://github.com/PengJunchen/go-cli/blob/5aa063ee647f753f7f0fc7282a3baf5b6e691932/internal/tools/builtins.go#L135) |
| 默认实例 | Timeout=30s，TimeoutTier零值false；Streaming构造器复用它；[bash.go](https://github.com/PengJunchen/go-cli/blob/5aa063ee647f753f7f0fc7282a3baf5b6e691932/internal/tools/bash.go#L123)、[streaming_bash.go](https://github.com/PengJunchen/go-cli/blob/5aa063ee647f753f7f0fc7282a3baf5b6e691932/internal/tools/streaming_bash.go#L134) |
| 最终deadline | 先取t.Timeout，只有TimeoutTier=true才分类；再接受host-only收紧，最后context.WithTimeout；[streaming_bash.go](https://github.com/PengJunchen/go-cli/blob/5aa063ee647f753f7f0fc7282a3baf5b6e691932/internal/tools/streaming_bash.go#L184) |
| runtime收紧 | WithBashRuntimeExecutionTimeout只在更小时替换，不能增加；用于runtime配置的post-tool action；[bash_runtime_input.go](https://github.com/PengJunchen/go-cli/blob/5aa063ee647f753f7f0fc7282a3baf5b6e691932/internal/tools/bash_runtime_input.go#L38)、[post_tool_actions.go](https://github.com/PengJunchen/go-cli/blob/5aa063ee647f753f7f0fc7282a3baf5b6e691932/internal/core/post_tool_actions.go#L135) |

完整生产源码搜索没有 CLI 调用 `WithTimeoutTier`。若未来宿主显式启用该库选项，分类器也只是以 `strings.Fields(command)` 的第一个词再取basename：直接go/make/npm/cargo/docker为300秒，部分读命令为5秒，其它30秒；`cd ... && go test`、`env ... go test` 等未必被识别为慢命令。这是未启用策略的实现特征，不是 D 的实际选择。[分类源码](https://github.com/PengJunchen/go-cli/blob/5aa063ee647f753f7f0fc7282a3baf5b6e691932/internal/tools/bash.go#L410)

## 哪些“timeout”不是这项设置

- `--approve auto`选择AllowAllClassifier，不调整工具时限；[审批装配](https://github.com/PengJunchen/go-cli/blob/5aa063ee647f753f7f0fc7282a3baf5b6e691932/internal/cli/assemble.go#L1407)。
- `--no-sandbox`选择AllowAllSandbox；该分支也不加载Sandbox.MaxCPU/MaxMemory，但D仍有外部Docker1CPU/4GiB。这些资源限额不是Bash墙钟deadline；[沙箱装配](https://github.com/PengJunchen/go-cli/blob/5aa063ee647f753f7f0fc7282a3baf5b6e691932/internal/cli/assemble.go#L1010)。
- `production.model_timeout.total=5m / stream_idle=2m`是模型HTTP调用的限制，不会把Bash延长为5分钟。任务外层3000秒是整次Agent预算，也不覆盖Bash自己的30秒。
- `tools.custom_tools[].timeout`以秒配置**自定义命令工具**，不是默认Bash的覆盖入口；换成自定义工具会改变schema、注册与completion资格，不能当相同原生Bash对照。[ToolsConfig/CustomToolConfig](https://github.com/PengJunchen/go-cli/blob/5aa063ee647f753f7f0fc7282a3baf5b6e691932/internal/config/types.go#L207)
- Bash schema不接受其它属性；普通工具参数中添加timeout不会变成host配置。[schema](https://github.com/PengJunchen/go-cli/blob/5aa063ee647f753f7f0fc7282a3baf5b6e691932/internal/tools/bash.go#L145)
- 在command里包 `timeout 600 ...` 也不能超过外层Bash30秒；`nohup ... &`并不是合法的原生异步任务接口。

## 证据等级与实测边界

上面的“固定30秒”是由冻结源码、真实profile配置及装配路径推出的有效配置，不是此次额外运行了31秒探针。D记录的配置没有tools/sandbox/approval/hooks覆盖；production只有模型timeout项。源码核查时上述文件与5aa无差异。

只读取 D 的安全receipt标量，未读取任务gold、隐藏断言或Pi答案：三个durable session合计15个Bash intent可匹配到receipt，intent→receipt最长1.143秒；另3个没有匹配intent的receipt不纳入执行时长。所检查的18个Bash receipt没有显式“timed out/deadline exceeded”文本。现有trace没有逐次导出effective Bash timeout，因此不能声称trace直接测得每次30秒，也不能把普通错误或最后后台cleanup unknown算成timeout。

本地原始依据是相邻 `go-cli-eval/jobs/nav-D-20261029T013619-e60464/instance_go/` 中的configs、result及durable session；不随本公开文档发布raw内容。

只读抽查冻结Pi1.4.0安装资产：其 `dist/core/tools/bash.js` 声明可选 `timeout`，单位秒，省略没有工具默认timeout；实现上限2,147,483.647秒，且仍受外层Agent预算约束。实际bundle主chunk也包含该schema。对应未打包文件SHA256为 `f32e87a42f66276b2eafaaf0848babf52523d37ab0fd448439586376747f03c4`，包metadata指向 [earendil-works/pi](https://github.com/earendil-works/pi)。这是冻结安装代码核查，不是说本次Pi实际用了某个timeout，更不是保证服务端或其它版本行为一致。

因此当前比较应披露：同一外部3000秒预算下，两者保留了不同原生单命令能力。它可以用于产品默认策略比较，不能称“单命令时限也已对齐”的隔离实验。

## 最小后续对照

**现有D没有可用配置能只把默认Bash改成更长。** 不伪造一个配置字段，也不为使D过题偷偷换工具或重新启用tier。outer修复与timeout改进分开冻结。

1. 先在资源窗做零付费真实CLI的边界合同：`exec sleep 29`应正常完成，`exec sleep 31`应触及约30秒工具deadline；`exec`让sleep就是leader，尽量避免把后台孤儿回收混入时限测量。保存真实elapsed、deadline错误及资源收尾，使用新OUT；保持原D记录。此处只是拟运行，当前未执行。
2. 若需要长命令支持，另立通用API提案：提供host侧 `tools.bash.timeout` duration（默认仍30s），接入RegisterDefaults→WithTimeout，明确校验及上层ctx更短时仍优先。先不增加模型可调参数，也不启用基于首token的tier。该字段目前**不存在**，必须单独TDD与产品验收后才可配置。
3. 新版本可做30s与300s两profile的同题开发回归，除该host配置外固定模型、effort、工具、网络、环境、外部3000秒/80请求；同时记录timeout次数、工具wall、tokens、完整交付与官方score。模型看见的实际工具描述也要记录，避免文案变化被误称仅timeout单变量。
4. Pi保留其原生可选timeout做产品对照；如果另设共同300s硬上限，需要在双方有可审计执行边界上实施并另命名profile，不能仅靠提示词要求模型选择300。小样本只作诊断，先回归再未见验证。

没有默认 `worker_start/worker_wait` 工具：owned-worker是动态SDK/RPC包装，`worker_echo`只是测试插件。当前extensions关闭，因此不能在长任务教程或Bash描述中假装这个profile已有通用worker。
