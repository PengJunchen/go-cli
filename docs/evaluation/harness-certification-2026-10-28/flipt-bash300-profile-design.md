# 下一批有限 profile：Bash300、supervisor关闭

2026-10-29，最小additive方案已准入，Python候选已实现并提交0962aa7，最终25源/64合同包已通过safe audit与独审冻结（14.444秒，源manifest f14539b5…）；实际新profile双CLI有效态及模型仍未放行。基线是public runner提交 `098c350`；相邻执行区旧D runner暂不同步。此文不授权模型调用，不改变旧profile或10题锁。

## 唯一新增选择

保留现有 `PROFILE_ID=main-5736e88-personal-memory-off-v1`、现有无参数调用的 `go_config(endpoint)`、Pi配置和argv行为。只增加有限allowlist中的第二项：`main-bash300-personal-memory-off-v2`。

新增纯 `resolve_profile(profile_id)`，未知值立即报错；`run_trial`追加keyword参数，默认旧ID，入口在 `out.mkdir`、Docker、题目读取前解析。CLI增加 `--profile`，choices只能是这两个ID，默认仍旧ID。不增加自由duration、自由环境变量、任意配置patch或隐式按模型/题目选择profile的入口。

新profile的Go配置只增加 `tools.bash.timeout="300s"`。small_model、个人memory、extensions/MCP/skills、native工具、压缩、模型/effort、HTTP/整题3000秒与80请求等保持原值；Pi原生配置与argv不变，不把Pi伪称同样固定300秒。pair两arm记录共同的新profile_id。

supervisor必须关闭：**只对新profile**固定追加容器启动参数 `--env GO_CLI_LINUX_SUBREAPER=0`，create后inspect实际 `Config.Env`，要求该key唯一且有效值为0；缺失、重复、非0或不能inspect均在启动CLI前拒绝。不是检查宿主os.environ，也不把“声明off”冒充实际容器配置。旧profile不增加该参数，保留原默认兼容性。不要为了这个profile加一个可由用户自由填充的env通道。未来outer-on须另外profile、平台门和真实CLI取消验收，不借此绕开amd64/Rosetta限制。

## 记录来自实际配置和请求

保留每arm已有实际config SHA记录；明确区分Go配置文件和Pi实际使用的models/settings文件。当前driver可能同时写出双方配置，Pi arm中未使用的go.json变化不等于Pi配置发生了变化。

使用明确字段 `configured_go_bash_execution_window_seconds=30/300`、`go_supervisor_mode="off"`；它们属于共同protocol中的Go配置说明，不能在Pi行里误称Pi的实际单命令时限。记录实际启动环境核验结果；没有运行证据时不能把configured字段命名成“已测得每次真实deadline”。

工具描述/schema是动态harness输出。需要其哈希时，从真实CLI送到relay的请求中对实际tools数组、实际bash function.description等做规范化hash并绑定request/来源；没有字段时为unknown/null，子Agent工具子集可产生不同哈希。不得在driver中另造一份工具schema或硬写“5m0s”description来冒充观察值。fixture应核对真实请求中的30/300窗口描述及无timeout模型参数。

## 先红后绿的合同

1. 旧调用 `go_config(endpoint)` 输出与旧快照完全相同；默认CLI解析仍选旧profile，Pi配置/argv不变。
2. 新Go config与旧config的语义差异只有 `tools.bash.timeout=300s`；新profile的Pi实际使用文件与argv逐字相同。配置构造每次返回独立对象，不污染下次默认调用。
3. Go/Pi两arm记录同一个新ID；configured Go窗口与supervisor-off字段来源清楚、actual config SHA正确；新profile的Docker create参数固定env0，inspect缺失/重复/非0拒绝，旧profile不变。工具hash只从实际请求提取，不可由profile常量伪造。
4. 函数调用和CLI传未知profile均在任何out/Docker/题目读取前拒绝；验证标记out不存在、Docker stub未调用。不接受任意duration或额外env参数。
5. 现有本地fixture可覆盖两profile，检查实际Go请求的Bash描述、原schema和有效模型/effort/cap；不因此启动真实模型。现有budget/network/source/payload门仍执行。

通过Python小合同和独立源审后，才将新runner另冻导出到相邻执行区；不得覆盖D job或用旧source manifest回填。Flipt模型pair还需fresh empty0/reference1、最终产品fullverify、二进制/runner/payload/profile冻结及根显式派发。
