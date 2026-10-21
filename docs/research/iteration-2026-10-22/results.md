# 2026-10-22 实测与评价

最终同条件构建中，headless 为 **14,213,890字节**，比 current full-UI CGO0 小 **37.3%**。固定 read→edit→read CLI 旅程里，baseline 与 current full 的 median 为102.9 / 102.6ms，视为接近；headless 为94.7ms。它是同主机、此fixture、此配置的观察值，不能外推完整项目或动态能力满载成本。

[机器记录 measurements.json](measurements.json)保存350次调用的数值、参数、版本、package integrity、二进制SHA256与Go源文件hash；不保存workspace、模型response正文或二进制。原始日志产生于task temp，最终清理见[verification](../../iterations/2026-10-22-dynamic-runtime/verification.md)。[上游研究](upstream.md)和[操作指南](../../iterations/2026-10-22-dynamic-runtime/usage.md)分别记录设计证据与使用方法。

## 固定版本与协议

- 主机：Apple M1，8 logical CPU，16GiB RAM，macOS26.5.2 arm64；Go1.26.1。正式开始时间 `2026-10-21T18:39:59.351356+00:00`。本任务重型校验结束后串行测量，未冻结其他桌面应用或控制电源/热状态。
- baseline：git archive `38239888f6275f9a329825a9bde791b449280227`，独立目录构建。current：同一base上的迭代工作树；全部Go源manifest SHA256 `93b734b5d92ec91e67dffac3a0185a7c118b837be8add2d065f347d0eb7b339a`。测量时尚未commit，不能用base SHA冒充current实现版本。
- 三个Go性能arm均是 **default compat profile**，extensions/git/tracing关闭。headless在此只表示build tags，**不是edge profile性能实测**。原始入口edge功能另列。
- 官方发行PI `@earendil-works/pi-coding-agent@1.3.0` + Node22.22.2，print/no-session/no-extensions/no-skills/no-mcp/offline；OpenCode `@opencode/cli-darwin-arm64@2.0.32`，每次新状态目录与私有 `run --standalone --auto` 服务。它不代表复用已常驻OpenCode服务的单轮延迟。npm包来源不冒充已核验与研究Git SHA逐字节相同。
- 相同loopback scripted OpenAI-compatible provider、相同任务输入。ready为完整prompt装配后一次模型请求；tools为read→edit→read→final四请求，验证41/42工具结果、最终文件42和成功输出。模型决策预设，未测真实模型任务质量、token成本或真实网络首token。
- 每arm每场景5 warmup+30 measured，AB/BA顺序交替，共 **350/350调用成功**；warmup/measurement任一失败会阻止对应cohort headline。新隔离functional pilot10/10通过；旧pilot是校准，未混入统计。首轮泛化prompt使Go既有Top30筛掉read；锁定实际完整任务后重试阶段由receipt推进。随后补齐OpenCode官方资源根隔离并重新pilot。
- HOME未改；仅metadata检查Go个人config、canonical Skills/MCP/hooks/auth/models/commands不存在；既有memory文件0字节、未改。PI独立资源目录；OpenCode XDG/config与官方 OPENCODE_TEST_HOME隔离兼容Skills，同时确认系统/MDM配置不存在。完整prompt、flags与隔离参数在JSON。
- wall从Popen到主进程wait4；post-exit verification/cleanup另计，grace=0。350次全部无残留进程组/已观察PID，无RSS采样错误。检查只清理本次自身PGID。

## 真实CLI任务耗时

单位ms，单元格为median / p95。这不是语言性能排名，也不说明工具集、会话持久化和后台服务模型等价。

| arm | ready wall | read→edit→read wall | measured + warmup通过 |
|---|---:|---:|---:|
| baseline CGO0 | 29.6 / 30.6 | 102.9 / 110.6 | 30/30 + 5/5 |
| current full-UI CGO0 | 29.6 / 30.9 | 102.6 / 110.7 | 30/30 + 5/5 |
| current headless,no_plugin CGO0 | 20.6 / 21.7 | 94.7 / 104.4 | 30/30 + 5/5 |
| PI 1.3.0 / Node 22.22.2 | 277.3 / 287.4 | 341.5 / 361.6 | 30/30 + 5/5 |
| OpenCode 2.0.32 / fresh standalone | 579.2 / 629.2 | 660.6 / 693.7 | 30/30 + 5/5 |

Go full与baseline差0.3ms，不解读为性能提升。headless此旅程约低8%，只作本场景观察。PI四个基础工具、OpenCode十二个默认工具与Go Top30 schema集合不同；OpenCode还包含新私有服务/状态初始化，不能由表格推导产品整体优劣或复用daemon的耗时。

## 内存、进程与schema

只列tools旅程的 **50ms PID/PPID树RSS采样峰值**，MiB，median / p95；它可能错过间隔内峰值，共享页可能相加多次。每次采样数和观察PID范围同时列出。三个Go ready仅约20–30ms、各只有一次早期采样，故 **不做ready树RSS比较**。JSON仍保留这些原始数值与单独wait4指标，不把后者当整个树总量。

| arm | sampled tree RSS MiB | 每次sample数 | 观察PID数范围 | tool数 / schema重编码B |
|---|---:|---:|---:|---:|
| baseline CGO0 | 30.8 / 31.3 | 2–2 | 1–1 | 30 / 14,610 |
| current full-UI CGO0 | 30.9 / 31.8 | 2–2 | 1–2 | 30 / 14,610 |
| current headless,no_plugin CGO0 | 23.7 / 24.2 | 2–2 | 1–2 | 30 / 14,610 |
| PI 1.3.0 / Node 22.22.2 | 108.0 / 116.1 | 5–5 | 1–1 | 4 / 2,956 |
| OpenCode 2.0.32 / fresh standalone | 459.1 / 463.8 | 9–10 | 2–5 | 12 / 13,990 |

schema字节是tools数组经Python `json.dumps(ensure_ascii=False)` 默认分隔符重编码的UTF-8长度，不是原wire请求字节或token数。表内schema在该arm所有tools请求相同；具体名称与每次长度保存在JSON。CPU仅保留wait4报告值，不冒称完整进程树CPU。进程采样自身也有开销；所有arm使用同样方法。

## 构建体积与可部署边界

六个产物均Go1.26.1、CGO=0、`-trimpath -buildvcs=false -ldflags='-s -w'`；headless三目标额外 `-tags headless,no_plugin`，同一cmd入口/真实核心。native Make产物使用受保护Make原flags，单独记录，不作为纯tag变量对照。

| 产物 | 平台 | 字节 | build exit |
|---|---|---:|---:|
| headless | darwin/arm64 | 14,213,890 | 0 |
| linux-amd64 | linux/amd64 | 15,151,266 | 0 |
| linux-arm64 | linux/arm64 | 13,828,258 | 0 |
| windows-amd64 | windows/amd64 | 15,501,824 | 0 |
| full-ui-cgo0 | darwin/arm64 | 22,651,666 | 0 |
| baseline-cgo0 | darwin/arm64 | 22,222,530 | 0 |

current full比baseline大1.9%；不能隐去新增runtime/SDK合同成本，只突出headless。受保护Make默认产物为25,043,906B，实际CGO=1，完整build metadata见JSON。final headless import graph224包，无Bubble Tea/Glamour/Lip Gloss/Chroma/stdlib plugin。`no_plugin`只排除native.so；SDK JSONL worker与标准MCP仍可使用。

PI独立安装目录111,525,896B（不含Node），其中package dist18,675,263B；160B cli.js只是entrypoint。Node完整运行时footprint未测，不把其动态launcher尺寸当整个运行时。OpenCode平台binary186,957,968B。安装目录与Go单binary不是同一口径。Linux/Windows只完成交叉编译，未在目标板实测；Windows私有ownership验证目前fail closed，不等于Windows完整运行支持。

## edge与SDK功能证据

原始entry-loader→可信临时project→edge profile→Harness，default与headless两标签race均通过：4次read/edit/read/final、文件42、Skill正文未披露、未选MCP0请求；原wire tools schema每请求17,419B，显式限额4 Skills / 1 MCP / 131072 prompt+schema B。改1024B后在模型dispatch前拒绝。该17,419B口径与上表重编码schema不同。

metadata-first指目录/模型投影与materialization；verified project为trust完整捕获有界正文，不能称全部项目正文启动零IO。用户完整Skill正文才lazy读缓存。此证据不代表0/100/1000能力规模或dynamic满载性能已测。

公开SDK示例在真正独立 `example.org/consumer` module、GOWORK=off/CGO0构建通过；69个依赖仅public `pkg/extensionapi/v1`来自本仓库，无internal依赖。真实cmd入口还完成外部command与provider消费、worker回收验收；它发现并修复了cost tracker旧HTTP-origin白名单，现仅允许extension-process与extension-rpc准确组合。完整主机校验见verification。

标准prompts/resources全链路、现代MCP订阅、逐工具语义tool_search、RPC Skill与逐token provider stream未实现；本轮实际采用/后续范围见upstream映射表。Honeycomb官方归属仍未证实，不将DeepSeek Harness冒充Honeycomb。

## 交互诊断

下表为原有真实full renderer与headless event consumer的单次Go benchmark操作（`-benchtime=1x`）。历史行数1k/5k/10k、流1000×1KiB相同；历史body分别为 `loaded canonical row N` / `history`，且full逐chunk绘制、headless仅drain与snapshot，因此 **不计算精确等价UI速度倍率**。它也不是30次process cohort。

| 实现 | 历史行 | ms/op | B/op | allocs/op |
|---|---:|---:|---:|---:|
| full | 1000 | 31.864 | 26,450,416 | 82,940 |
| full | 5000 | 37.468 | 44,220,840 | 122,967 |
| full | 10000 | 42.684 | 66,516,392 | 172,998 |
| headless | 1000 | 0.637 | 1,056,592 | 17 |
| headless | 5000 | 1.775 | 5,143,328 | 12 |
| headless | 10000 | 4.181 | 10,272,448 | 17 |

full额外每chunk诊断p50为33/29/29μs、p95为50/37/34μs；已有断言验证1,000个chunk没有SetRows/sameRows全历史重扫。headless大输出不积累chunk、取消join、durable身份/clone-before-ACK、幂等关闭合同通过。以上不替代人工UI可用性评价，headless不提供rich modal/theme/in-flight keyboard steering。

## 验证与暂存数据

最终 `make verify` exit0：8962 started /8956 pass /6 skip /0 fail；AST0ERROR/1866WARN。最近默认并发E2E一次在未改fixture的1秒marker等待超时；相关source current/baseline一致且各原参数quiet count5通过，不能据此证明负载是唯一原因。随后 `GOFLAGS=-p=1 make test-e2e` 完整exit0：4877/4871/6/0，仍保留race、count和全部断言，限制包级竞争。未改保护规则、阈值或超时来消除失败。

详细验收、初轮失败摘要及最终清理状态以[verification](../../iterations/2026-10-22-dynamic-runtime/verification.md)为准。临时原始输出、fixture、信任store、安装目录和二进制已由主代理实际清理；本报告与JSON独立保留评价所需参数/数值/provenance，不依赖已清理文件继续存在。
