# 动态能力接线复核

状态：最后源完整make verify final3、串行package E2E final4及正式测量均已完成；默认并行E2E的一次既有fixture超时及复验记录保留。主代理已完成临时数据清理，源码 worktree 保留供 review。本页不替代最终验证报告。日期：2026-10-22。

## 最后源完整gate

`/tmp/go-cli-iteration-20261022.to3oRg/logs/make-verify-final3.exit` 为0，完整日志 `make-verify-final3.log` 以 Verification complete 结束。lint为0 issues；race gate为8962 started、8956 passed、6 skipped、0 failed（31包）；AST扫描466文件，0 ERROR、1866 WARN；log gate为10/10，leak gate为6/6。相比final2多4项WARN，仅为新snapshot getter的test-only SCAN006。扫描规则、阈值和保护文件未为本轮结果放宽。WARN与SKIP均保留，不写成零告警或全平台覆盖。

六项skip为ownership未知平台分支、stdio不支持containment的平台分支、三个依赖已安装fd的用例及内存OOM用例；具体原因与环境条件保留在原始日志中。性能数据、真实edge/Rust等价对照和额外平台本机结果不能从此gate推断。

最后源完整E2E以原Make target、`GOFLAGS=-p=1 make test-e2e` 运行，`logs/test-e2e-final4-p1.exit` 为0：4877 started、4871 passed、6 skipped、0 failed（8包）。这不是默认并行重跑成功；此前默认并行final3的失败见下一节。更早final2完整verify/E2E成功仍留作过程记录，不替代最后源结果。

票01–07状态为verified-local；新补AC-01-1与AC-05-2已进入完整gate。公开SDK外部consumer实际构建与依赖检查已通过：69依赖，仅公开v1 API，无internal依赖（`upstream/public-sdk-consumer/result.json`）。票08的整gate与owner交错AC已勾选，测量和最终清理已完成，票08全部AC均已验证。所有票仍unpublished-to-tfs，产品规格仍review v0.1。

## E2E失败与控制复验

`logs/test-e2e-final3.log` 保留一次失败：`TestDefaultGitDiffCancellationJoinsDescendantHoldingPipes` 在未改的 `git_diff_lifecycle_test.go:46` 等待fake git启动后代的1秒标记超时，4877 started、4870 passed、6 skipped、1 failed，Make exit 2。主代理核对原fixture和相关production文件与baseline SHA相同。安静条件下以原预算运行 `-race -tags=e2e -count=5`，current和baseline各5/5通过（`runtime/quiet-gitdiff-current-e2e-race.log`、`quiet-gitdiff-baseline-e2e-race.log`）。

随后原完整Make target只以GOFLAGS限制package并行度，final4实际exit 0；没有修改源码、断言、timeout或gate。该结果保留负载敏感的验收限制，不用成功重跑抹去前次失败，也不将package串行结果写成默认并行条件保证。

## 正式构建与测量

已读取持久 [results.md](../../research/iteration-2026-10-22/results.md) 与 [measurements.json](../../research/iteration-2026-10-22/measurements.json)。六build全部exit 0；新隔离pilot10/10，正式5 warmup+30 measured、五arm与两个场景共350/350调用成功。另独立读取JSON核对350个tuple唯一、50 warmup/300 measured、10 cohort、全部exit 0且无timeout；主代理已核对1233项Go与go.mod/sum manifest和实测源码一致。

headless为14,213,890B，current full-UI CGO0为22,651,666B。full/headless交互诊断各三种历史规模均exit 0，包含B/op与allocs/op；功能和历史body不同，不作精确等价UI倍率。edge真实入口与预算、公开SDK消费等功能证据独立保留；Go性能arm全部为default compatibility，不能将headless标签表现写成edge profile性能。

正式数据没有建立冷OS cache、实物edge、Rust、千能力规模或真实模型质量/网络对照。OS cache未flush；独立新进程/工作目录与5次warmup是实际协议。RSS为50ms进程树采样，可能漏峰或重复共享页；schema重编码字节也不同于wire/token。最终清理尚未执行，原始临时文件不能被提前描述成已删除。

## 真实消费路径

- 主入口把原 `loadConfig` 闭包通过 `WithRuntimeConfigLoader` 传入上下文，保持 CLI 原有配置优先级与项目捕获/验证。REPL 的 slashContext 持有实际 Assembly CapabilityBackend；/capabilities、/skills、/skills load、/reload、/mcp list|load|reload server 由默认 slash registry 路由。
- Harness 与 TurnRunner 取得同一个 runtimePlan，core 根据上下文选择真实 prepared registry、HookChain 与 pre-tool interceptor；runtimePromptBuilder 从该 plan 取 PromptOptions。显式刷新等到 run owner 真正释放 lease，而非 UI 等待者返回。
- MCP 首次激活使用 append publication 与独立 catalogRevision；既有请求的 authority fence不因纯追加变化。core 下一模型请求刷新定义，并检查 response 调用名确实在本 request 声明。新工具随后仍走原 prepared dispatch，不提供任意 MCP 调用旁路。
- 标准 Skill 的目录前言读取只读到结束分隔线并有至多 1KiB 预读；正文按需载入并缓存。项目目录只使用已验证捕获字节；Skill load 的实际正文摘要单独记录，目录 identity 不伪称已经读过未物化正文。

## 已执行的定向验证

原始日志位于本轮临时区 `spec/logs/` 等子目录，供验收期间定位。任务结束由主代理保留摘要、命令、版本/输入边界及必要可复现结果，并按用户要求清理原始临时日志、下载、缓存与测试资源；这些临时路径不作为永久可读链接。

| 执行 | 结果 | 说明 |
| --- | --- | --- |
| 首轮 Skill/MCP/CLI 新用例 | 失败 | llm 扩展 provider 的 Role→string 编译问题交原作者修复；Skill description 的尾换行断言修正为规范化结果；MCP 新用例通过。 |
| 第二轮同组 | Skill/MCP 通过，CLI 失败 | 发现全局 Hooks JSON 被 YAML subset decoder 解析为空；修为正确 format。MCP CLI fixture 缺少 scoped credential，改为标准本地 mock provider 配置。 |
| 第三轮 CLI 新用例 | 通过，1.271s | 真实 Loop Skill正文进入后续request；同run MCP discovery→下一request schema→prepared call；prompt idle刷新与 cold配置拒绝；HookPlan保持旧对象不可变；RPC captured materialization正确。 |
| 最终 MCP 原子性小集 | 通过，0.954s | 首次激活/指定服务器刷新对过大schema都在发布前拒绝，generation、catalogRevision与owner保持原值；候选连接已关闭，旧执行器真实Loop仍可调用；同run下一request与跨grant拒绝也通过。日志 `mcp-atomicity-final.log`。 |
| 最终 CLI 合同 race 小集 | 通过，2.821s | 覆盖dynamic/cold/source identity/eager合同、预算/空启动、真实MCP与Skill、Assembly DAG cleanup及独立review用例。日志 `final-cli-contract-race.log`。 |
| MCP slash 路由与补全 race | 通过，2.000s | 单一子命令注册表驱动路由、usage与completion；默认list、load、legacy/named reload、非法参数和backend错误传递均覆盖。日志 `slash-mcp-final-race.log`。 |
| 排列等价性与真实SDK CLI入口（default） | 三包通过，1.250s / 1.608s / 2.661s | `TestSnapshotIdentityIgnoresInputTraversalOrder`、成本归属正负例及真实CLI SDK command/provider journey；日志 `runtime/final-entry-default-race-3.log`。 |
| 同组headless/no_plugin及lint | 三包通过，1.474s / 1.547s / 2.573s；两tag lint均0 issues | `runtime/final-entry-headless-race.log`、`final-entry-default-lint-2.log`、`final-entry-headless-lint.log`；后续完整最终源gate已通过。 |

单独 MCP 用例还验证了 `exec.Command` 的 process owner 生命周期：取消成功 setup 的 context 之后，两轮 ListTools/CallTool 使用同一个 PID，显式 Disconnect 才退出；原有取消握手、失败清理、关闭合约保留，由全量测试继续验证。分页测试覆盖 cursor推进与重复cursor拒绝。

随后增加的 prompt byte budget 候选拒绝、预算更新、空启动与cold配置回归已在最终CLI合同小集及完整gate通过。审核修复期间按主代理安排串行使用短验证窗口，避免与统一 make verify 争用宿主资源。

## 审核发现与修复

- RPC 项目 descriptor 的 `.rpc.json` 后缀在私有 materialization 中被缩成 `.json`，已保留完整后缀，并将缺省/相对 cwd 与相对 worker 路径确定性规范到已验证项目根。派生后的字节自身内容寻址，不重新读取项目可变文件。
- 原 MCP stdio `CommandContext` 把已发布 worker 绑定到短期 setup/errgroup context；改为由已有 process owner 关闭，握手/请求仍由各自 context 限制。
- 全局 hook 配置加入稳定 JSON source identity；MCP Env 私有 fingerprint 不进入日志，快照仅有随机 process namespace 与单调 opaque revision；秘密跨重启不承诺内容复放。
- `/reload` 对尚未批准的项目捕获返回 needs-trust；涉及已加载项目扩展/MCP 的 trust generation 变更返回 restart-required，不能给旧回调套新批准。
- headless 仍复用原 REPL/run ownership/session/approval 流程。Windows 的 Unix `Stat_t` 引用已拆到平台 helper；无法证明私有文件 ownership 的平台返回 ErrOwnershipUnsupported，保留 0600要求。交叉构建不代表 Windows 本机运行已验证。
- 真正SDK入口验收经过外部public-only worker构建、持久trust grant、原loader/CLI执行command与provider模型。首次真实provider消费发现cost tracker只接受HTTP origin；已限定允许extension-process与extension-rpc的精确组合，负向组合仍拒绝。两tag定向race及修后完整final3 gate均通过。

## 剩余验收

已完成整命令、两tag定向、六build与正式测量结果见本页首部及持久研究记录；最终清理也已实际完成，状态见 verification。没有用静态检查或分项通过冒充未执行项目。TFS票据仍unpublished-to-tfs；产品规格保持review v0.1。


## 收尾审查修复（已纳入最终 gate）

- 扩展 Requires/Provides 从公开 manifest 进入全模式 CLI 的 Init前图验证与实际初始化次序，snapshot含同一节点/manifest来源；新增缺失、循环、重复 provider 三组 Assembly 拒绝与 cleanup 回归。
- named MCP refresh 跨项目 grant 返回 restart-required，避免局部成功却不能提交的假象；新增断言拒绝发生在额外网络请求之前。
- prompt renderer 改用 pinned plan 的实际 compiler/预算，修复预算上调后仍套用启动阈值；连接数量预算变更明确要求重启。
- 空启动保留 Skill/MCP 控制工具与空 owner，新增首个目录/source 后可在真实 Loop 搜索/加载/调用，避免仅状态页变更而模型缺少控制入口。

这些用例在前述第三轮之后新增，已由主代理最终完整gate与串行定向复跑覆盖。

静态复核额外发现：PluginManager.Load后、Init前图验证失败时，尚未进入coordinator的资源可能被原Shutdown流程遗漏。生命周期owner已修复，让这类exactinstance进入原Shutdown状态机而非跳过；最终完整gate的manifest与Assembly DAG回归已验证未Init的loaded资源也确实关闭。

MCP交互发布另补两阶段准备：先构建完整候选、合并全工具metadata并验证最终prompt与schema总预算；再一次发布materialized definitions与精确执行器，不留提交后的List/Load失败点。named refresh通过Manager的host内部commit步骤同步切换资源与runtime snapshot，所有可预判的编译/快照校验在提交之前完成，提交拒绝只Abort且保留current。超预算回归同时检查候选关闭、旧owner指针、metadata identity、catalogRevision及真实旧执行器调用。

最后静态收口将runtime.dynamic模式开关与活跃MCP的source身份切换明确标为cold；真实reload表回归已在最终完整gate通过。同source的lazy刷新没有新增限制。

另将dynamic+eager MCP明确收为cold：这种装配禁止旧后台watcher绕过run pin替换registry；`/reload`存在当前/新MCP源或既有owner即返回restart-required，包含同路径新bytes与源文件删除。dynamic=false/eager仍使用原兼容路径。真实四项表回归检查无watcher、拒绝发生在新server连接之前、metadata保留及旧executor仍可用；已进入最终CLI合同race小集并通过。

统一AST扫描发现的五处新增假key字面量已改为复用原approved测试fixture Provider；没有修改规则或增加例外。MCP slash另改为规范Subcommand注册表，让list/load/reload路由、帮助与现有completion共用元数据，避免业务字符串硬路由。完整make verify已实际重跑AST：final2为0 ERROR、1862 WARN；最后源final3为0 ERROR、1866 WARN，不以源修复代替扫描结果。
