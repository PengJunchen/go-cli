# 宿主可配置 Bash 执行窗口：最小设计

2026-10-29，设计已准入，**候选已实现并完成局部合同与31秒真实CLI边界独审，组合fullverify仍待完成**，见[第3轮](round-03.md)。字段存在于当前未发布候选，不能据此声称旧D已有该入口。 D5aa/profile与历史结果冻结；[现状核查](bash-timeout-audit.md)说明D默认30秒且没有现有CLI延长入口，并未证明NavD遭遇过该超时。本设计独立于outer-reaper修复，不阻塞它。

## 接口与语义

新增宿主配置 `tools.bash.timeout`，值为正数Go duration字符串，例如：

```json
{"tools":{"bash":{"timeout":"300s"}}}
```

同一字段支持既有JSON/YAML配置路径；可增加对应环境入口 `GO_CLI_TOOLS_BASH_TIMEOUT=300s`。首版不加CLI flag、不增加模型工具参数、不增加async/worker API、不启用timeout tier。

建议 `ToolsConfig.Bash BashToolConfig`、`BashToolConfig.Timeout *string`。未设置/nil沿用既有工具默认30秒；指针只用于区分未设置与显式无效空串。JSON null按既有nil-overlay规则等同未设置，不能表达无限时间或覆盖成0。显式空串、空白、无单位数字、0/负数、不可解析或ParseDuration溢出均报 `tools.bash.timeout` 配置错误；正数小数duration允许。不额外套用模型HTTP的24小时限制，不引入无穷大/0代表关闭的语义。

环境层用 `LookupEnv` 保留presence：不存在不覆盖，存在但为空应拒绝。继续使用现有合并/项目来源信任规则，不改全局配置优先级；有效值以最终合并结果为准。特别是当前LoadTrusted先有global/env、再合入已验证project overlay，不能在新文档里承诺env一定压过所有project配置。评测记录最终有效值及配置来源/哈希。

避免把30秒再复制为另一个默认常量：配置解析对未设置返回“未配置”，装配不传timeout覆盖，原 `NewBashTool` 仍是默认值来源。

## 最小源码接入点

| 位置 | 候选改动 |
|---|---|
| `internal/config/types.go` | ToolsConfig新增Bash子配置；新类型只有Timeout，不顺带加tier或async字段。 |
| 配置纯解析函数/方法 | 建议 `BashToolConfig.ExecutionTimeout() (time.Duration, bool, error)`：未配置返回0,false,nil；配置有效返回duration,true,nil；错误携带字段路径。Validator和assembly复用它，避免两套语法/范围规则。具体命名可由Builder按本包风格调整。 |
| `internal/config/validator.go` | 校验最终merged字段；正常Loader和LoadTrusted路径在返回Config前失败。 |
| `internal/config/loader.go` | 增加一个presence-aware环境读取；复用既有pointer整体覆盖机制，不修改通用merge算法。 |
| `internal/cli/assemble.go` | AssembleAgent最早的纯preflight再次解析，放在 `prepareExtensions`、`assembleModel`、工具注册之前，覆盖直接传Config绕过Loader的调用方。把已验证duration/configured标记存入本次assembly私有状态，不能到assembleTools才发现无效配置。prompt/interactive及正常子Agent共用装配路径。 |
| `internal/tools/builtins.go` | 增加 `WithRegisteredBashTimeout(time.Duration)` host注册选项；RegisterDefaults只有显式设置时append现有 `WithTimeout`，供真实CLI装配传入。直接调用新注册选项的非正值也须拒绝，不能当未设置；用独立presence标记避免0被吞。 |
| `internal/tools/bash.go` | 复用已有WithTimeout和执行路径；工具Description可准确展示有效宿主执行窗口以及同步契约，schema仍只有command。 |

默认注册使用 `NewStreamingBashTool`，其内部嵌入 `BashTool`，非stream `Execute` 同样转到 `ExecuteStreaming`。因此正确注册同一个WithTimeout即可覆盖真实CLI streaming与非stream调用，不要复制两套timeout选择逻辑。单独创建 `NewBashTool(WithTimeout(...))` 的库行为不变。

## 执行时限不是关停预算

时间选择仍为：宿主已配置的Bash执行窗口（未设30秒）→现有host-only RuntimeExecutionTimeout取更短值→父context更早取消/deadline优先。RuntimeExecutionTimeout=更长值不能反向提高普通工具窗口。tier零值false不动；不因go/make或审批模式动态改变本字段。

达到执行deadline后仍进入既有TERM/KILL、leader/reader join、group ESRCH及PendingExecution/Unknown路径。**不得改 `processTerminateGrace`、`processKillWait`、reader grace、assembly close或invocation producer预算。** 可配置300秒只是允许一条同步命令正常运行更久，不能使未知效果变成功，也不能使外层3000秒任务预算失效。

不创建默认worker工具；`owned_worker_tool.go`只是动态SDK/RPC包装，当前profile未提供固定worker。Bash提示应准确说明：同步执行，宿主窗口上限及上层取消可能更短；只接受command；脚本内部并行须wait收尾，不能留下未归属后台任务后立即退出。合法 `cmd & wait`、重定向仍可用，不用正则封禁 `&`。不要写“传入timeout参数”，也不要让模型为一次调用自行改宿主配置。

如果库调用方显式开启既有TimeoutTier，工具Description不能把t.Timeout写成所有命令的固定上限；可按tier状态描述分类策略。真实CLI本提案仍不启用tier。描述变化属于被测harness可见提示的一部分，profile冻结应保存工具schema/description哈希，不能声称只有不可见计时器发生变化。

## 可执行验收条件

下列为Builder拟实现的TDD合同，不表示测试现已存在或运行。

| 门 | 最小可验证结果 |
|---|---|
| C1 配置红绿 | 未设置得到configured=false且工具实际Timeout30s/TimeoutTier=false；JSON/YAML/env显式300s/5m得到300s；普通工具schema仍只有command。 |
| C2 拒绝无效输入 | 显式空、空白、0s、-1s、bare300、坏字符串、overflow均失败；数字JSON/YAML类型不误转默认。含坏值的实际CLI在模型/工具调用前退出，本地HTTP peer计数0、工具marker不存在；直接AssembleAgent坏Config也在extension/model前拒绝。 |
| C3 装配真生效 | 捕获真实prompt与interactive assembly中的默认StreamingBash：显式host值确实进入其BashTool.Timeout；auto/no-sandbox不改变该值。不能只测解析或WithTimeout本身。 |
| C4 stream/nonstream | 真实OS短窗合同（如200ms窗口执行`exec sleep 0.5`失败，1s窗口同命令成功），同时覆盖Streaming.ExecuteStreaming、其Execute、普通Bash.Execute；结果核对deadline分类和资源join，不用“模型回复成功”代替。时限间隔要足够，避免依赖临界调度。 |
| C5 只收紧 | host1s+runtime100ms仍在短窗取消；host100ms+runtime1s不能延长；父context更短仍优先。原unknown/forced-reader-close、cancel/后代清理负例继续有效，不改core门。 |
| C6 公共CLI边界 | 资源窗允许时一次零付费本地HTTP/SSE真实CLI：默认执行`exec sleep 31`出现约30s执行deadline；新host300s同命令正常完成并持久化completion/resolution/正常关停。独立新OUT与source/profile哈希，不覆盖D。可先用C4缩时OS合同开发，但不能把它宣称已经实测30→300的真实CLI。 |
| C7 发布/评测 | 小合同先红后绿，Reviewer独审装配与无效值副作用门，全make verify；再冻结新二进制和单独profile ID、实际有效窗口、所有其它预算。未批准前不修改D，不付费重跑或解封holdout。 |

建议先实现config/assembly合同，再执行缩时OS合同，最后做一次31秒边界对照。root统一安排资源窗；无需为纯描述文案写镜像字符串断言。

## 公平对照的下一步

新版本的30s与300s是两个明确profile，固定模型/effort/环境/整题3000秒/请求上限/工具集合/压缩策略，仅改变本字段及其真实描述。先开发题回归，再冻结未见验证；报告deadline次数、成功与失败、Agent/工具墙钟、tokens/cache和官方补丁score及完整交付。

这不意味着Pi与Go已经有共同单命令硬限。Pi原生可选timeout策略保持产品对照；若下一阶段要隔离共同硬限，须分别在可审计执行边界实施并单独命名profile，不能只用提示词让模型选300秒。

## 现有源依据（D5aa）

- [ToolsConfig](https://github.com/PengJunchen/go-cli/blob/5aa063ee647f753f7f0fc7282a3baf5b6e691932/internal/config/types.go#L207)、[merged校验](https://github.com/PengJunchen/go-cli/blob/5aa063ee647f753f7f0fc7282a3baf5b6e691932/internal/config/loader.go#L170)、[pointer覆盖机制](https://github.com/PengJunchen/go-cli/blob/5aa063ee647f753f7f0fc7282a3baf5b6e691932/internal/config/loader.go#L415)。
- [装配先后顺序](https://github.com/PengJunchen/go-cli/blob/5aa063ee647f753f7f0fc7282a3baf5b6e691932/internal/cli/assemble.go#L404)、[默认工具构造](https://github.com/PengJunchen/go-cli/blob/5aa063ee647f753f7f0fc7282a3baf5b6e691932/internal/tools/builtins.go#L135)。
- [已有WithTimeout](https://github.com/PengJunchen/go-cli/blob/5aa063ee647f753f7f0fc7282a3baf5b6e691932/internal/tools/bash.go#L40)、[stream选择窗口](https://github.com/PengJunchen/go-cli/blob/5aa063ee647f753f7f0fc7282a3baf5b6e691932/internal/tools/streaming_bash.go#L184)、[runtime只收紧](https://github.com/PengJunchen/go-cli/blob/5aa063ee647f753f7f0fc7282a3baf5b6e691932/internal/tools/bash_runtime_input.go#L38)。
