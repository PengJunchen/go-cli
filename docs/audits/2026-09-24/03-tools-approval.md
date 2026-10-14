# 工具与审批源码审计

日期：2026-09-24  
范围：`internal/tools`、`internal/approval` 全部生产 Go 源及对应测试；另追踪 CLI 装配、工具分发、MCP 重载、配置和核心计划模式接线。  
审计目标：功能契约与实际 executor 一致、工具调用安全边界清晰、取消和输出有界、审批决定可解释。

## 结论

工具层已有不少合理的安全与可靠性基础：Bash 默认要求配置 sandbox；文件写入使用临时文件加 rename；写/编辑操作有按路径串行队列和可选备份、diff；Web Fetch/MCP HTTP 默认有 DNS 拨号时的 SSRF 检查；MCP、远程 Bash 等默认归为需要审批的非只读工具；审批和工具注册都有明确接口及并发保护。

主要问题集中在四处：审批 `y` 的一次授权被缓存为重复授权；计划模式以工具名白名单猜测副作用，漏掉 Git 和扩展工具；Streaming Bash 达输出上限后不再 drain 管道，有限命令可卡到超时；Bash `allowed_paths` 实际只检查进程 cwd，命令读写目标路径仍不受限，配置表述与能力边界需要澄清。另有 MCP 重载没有重新读取配置或替换已加载定义、文件工具对缺失内容参数静默按空字符串执行，以及工具 schema/结果上限覆盖不完整等问题。

这里的“sandbox”是应用层 cwd 检查和命令黑名单，不是 OS 文件系统或进程隔离。审计未把允许绝对路径读取、用户配置本机 MCP、黑名单漏项本身描述为独立漏洞；风险按代码实际承诺和用户是否主动配置区分。

证据等级：**A** = 源码接线与契约直接可证；**B** = 源码路径可证，未做运行时复现；**R** = 有限运行时复现；**C** = 扩展 API/未接线功能上的条件性或设计缺口。

## 工具层发现

### TOOL-001 — `sandbox.allowed_paths` 只检查 Bash 工作目录，不隔离命令访问的目标路径

**严重度：P2（配置语义与能力边界需澄清）｜证据：A**

`SandboxConfig.AllowedPaths` 注释称把“command execution”限制在配置目录及子目录（`internal/config/types.go:245-250`），可能被理解为限制命令文件访问范围。CLI 把它传给 Bash sandbox（`internal/cli/assemble.go:789-805`）；但 `PathWhitelist`、`BashSandbox.Validate` 的源码注释明确说它校验的是 working directory，且 `Validate` 只比较 cwd 与命令过滤器结果（`internal/tools/bash_sandbox.go:83-115,346-356`）。`BashTool.Execute` 传给它的是 `Workdir`（`internal/tools/bash.go:168-196`）。它不分析命令参数、重定向目标、子进程或路径解析，所以在白名单目录中执行 `cat /etc/passwd`、`printf x > /tmp/x` 这类未被黑名单拦截的命令时，cwd 检查仍会通过。

当前源码能确认的能力边界是“限制启动命令的 cwd”，不能据此确认产品承诺过文件系统 confinement，也不能把它称为已实现的 OS sandbox。建议将配置注释和用户文档明确改为 “allowed working directories”；若产品需要限制命令能读写的文件，再将平台级 filesystem/process sandbox 作为单独明确的迭代目标，不靠 shell 文本匹配声称覆盖路径。

建议新增测试固定当前行为合同：检查允许的 cwd 及拒绝列表外 cwd；如果未来新增文件系统隔离，再增加从 repo 读外部文件、写外部重定向、经符号链接越界等负向用例。现有安全测试主要验证 cwd/词法 `..` 路径，并未证明命令目标受限。

### TOOL-002 — Streaming Bash 达 `MaxOutput` 后不 drain，导致有限输出命令挂至超时

**严重度：P1（工具可用性）｜证据：R**

`readPipe` 达到保存输出上限后追加截断标记并 `break`，不再读取 OS pipe（`internal/tools/streaming_bash.go:202-219`）；`ExecuteStreaming` 要等两个 reader goroutine 退出后才调用 `cmd.Wait`（`:138-150`）。生产者继续写满管道后会阻塞，reader 已退出，最终只有 context timeout 能结束工具。CLI 注册 `StreamingBashTool`（`internal/tools/builtins.go:143-150`），且 AgentLoop 在有 EventStream 时明确走 `ExecuteStreaming`（`internal/core/loop.go:996-1019`），所以这是用户主路径，不是孤立 API。

有限复现以 Bash builtin `printf` 生成 200,000 字节、`MaxOutput=20`、deadline 1.2 秒；执行耗时 1.204 秒并以 timeout 失败。日志和归档源分别在 [streaming-bash-output-cap-repro.log](/Users/pengjunchen/Documents/github/go-cli/docs/audits/2026-09-24/evidence/streaming-bash-output-cap-repro.log) 与 [streaming-bash-output-cap-repro.go.txt](/Users/pengjunchen/Documents/github/go-cli/docs/audits/2026-09-24/evidence/streaming-bash-output-cap-repro.go.txt)。临时测试文件运行后已删除；`.go.txt` 不会被 Go 测试发现。现有 `TestStreamingBashTruncation` 只写一条很短的命令输出（`internal/tools/streaming_bash_test.go:129-142`），没有超过管道容量，因此漏掉故障。

修复应在保存上限后继续读并丢弃数据，保留截断状态；同时检查 `Scanner.Err()`。回归测试要用超过 OS pipe buffer 的多行有限输出，断言 deadline 前成功、返回内容有界、sink 只收到约定范围。

### TOOL-003 — 缺失 `write.content` 会把现有文件覆盖为空文件

**严重度：P1（数据完整性）｜证据：A**

`WriteTool.Execute` 只强制校验 `path`；如果 `content` 缺失、类型错误或非字符串，局部变量保留为空串（`internal/tools/write.go:193-205`），之后正常写盘。CLI 把 `write` 包在 `MutationQueueWrapper` 内（`internal/cli/assemble.go:1074-1079`）；队列也把缺失内容转换为空串（`internal/tools/middleware.go:30-48`），再由 `WithOverwrite(true)` 创建的执行器应用（`internal/tools/mutation.go:292-311`）。因此仅包含路径的损坏/不完整调用可以静默将已有文件原子替换为 0 字节文件。显式 `content: ""` 作为清空文件是另一个有效操作，应与字段缺失区分。

同一处对 `edit.new_string` 缺失也静默采用空串（`internal/tools/edit.go:103-105, 134-149`），使已匹配的文本块被删除。CLI 未给 `WriteTool`/`EditFileTool` 提供 `Parameterized` schema，运行时 schema validator 对没有 schema 的工具直接跳过（`internal/tools/middleware.go:205-223`），所以不能补上这层缺失字段校验。

建议将 `content`、`new_string` 声明为 required，并在 executor 和 mutation queue 两处拒绝缺失/错误类型，继续允许显式空字符串。测试要经过实际 middleware queue，保留旧文件内容并断言 malformed call 不落盘；再单独覆盖显式空串。

### TOOL-004 — ReadTool 的中间目录符号链接可越过词法 workdir

**严重度：P2（路径边界实现不完整）｜证据：A**

`ReadTool.resolvePath` 声称阻止越出 workdir（`internal/tools/read.go:206-210`），但通用 `resolveWithinWorkdir` 只 clean 并做词法前缀判断，不解析符号链接（`internal/tools/write.go:66-90`）。ReadTool 仅对最终分量做 `Lstat`；若最终分量不是链接，即使它的某个父目录是链接，也会 `os.Open(abspath)` 跟随父目录链接（`internal/tools/read.go:104-126,166-170`）。触发条件是 workdir 内存在 `linkdir -> /outside`，调用读取 `linkdir/secret`。最终分量检查不会发现越界。

这是“相对路径必须留在 workdir”契约与实现不一致；默认 CLI ReadTool 没有工作区读取白名单配置，因此不据此声称任意绝对路径读取是漏洞。建议对现存路径解析真实父路径，并使用安全的 `openat`/不跟随链接方式处理竞态；增加父目录 symlink 指向 workdir 外的 read、write、edit、grep、find、ls 测试。`resolveAndValidatePath` 对写工具在显式 whitelist 下会解析中间链接（`internal/tools/write.go:93-138`），但无 whitelist 时跳过该层检查。

### TOOL-005 — 搜索/目录输出截断发生在完整采集之后，无法限制中间内存和工作量

**严重度：P2（大型/恶意工作区的可用性）｜证据：A**

`grep` 的 `MaxMatches`/输出截断在 `grepWithRipgrep` 完整执行 `cmd.Output()`、解析全部匹配，或 PureGo 收集全部匹配后才应用（`internal/tools/grep.go:188-225,262-298`）。`find` 同样先用 `cmd.Output()` 或 PureGo 收集全部 paths，之后才裁到 `findMaxResults`（`internal/tools/find.go:123-166,169-230`）。`ReadTool` 文件内容有限制，但对目录执行 `os.ReadDir` 后将全部名字拼进字符串（`internal/tools/read.go:137-163`）；`LSTool` 同样先收集全部 immediate children 再构建输出（`internal/tools/ls.go:126-199`）。这些限制最多约束最终输出，不能作为峰值内存或扫描量边界。

建议使用流式扫描/有限 writer，达到条数/字节/时间上限即停止遍历并明确标记 truncated；外部 `rg`、`fd`、`find` 应由限量 reader 消费。测试用大量结果与极长行检查峰值/返回上限，并验证可取消。

### TOOL-006 — PureGo Grep 在 context 取消后返回部分结果并报成功

**严重度：P2（结果正确性与取消语义）｜证据：A**

`grepPureGo` 遇到 `ctx.Done()` 时把 `ctx.Err()` 返回给 `WalkDir`，但随后忽略 walk error、返回已有匹配；`GrepTool.search` 对该结果总是返回 `nil` error（`internal/tools/grep.go:198-199,262-297`）。当 ripgrep 因取消报错时，代码也会回退到 PureGo（`:189-199`），同样可能把取消转成成功的部分结果。调用者无法区分“没有更多匹配”与“扫描被取消”。

建议保留区分取消的 error，不要把 `context.Canceled`/`DeadlineExceeded` 当作后端不可用回退；测试取消后的 PureGo、ripgrep 两条路径都返回 context error，部分结果不可被误报为完整成功。

### TOOL-007 — MCP 热重载无法应用新配置、替换已有工具或刷新模型工具定义

**严重度：P1（已公开能力未闭环）｜证据：A**

CLI 对自动发现的配置文件建立 reloader（`internal/cli/assemble.go:1651-1661,1725-1767`），`/mcp reload` 会触发它（`internal/cli/slash_mcp_reload.go:28-35`）。但初次装配时 client 已持有 `MCPServerConfig` 副本：HTTP adapter 把 config 存入 `cfg` 字段（`internal/mcp/http_adapter.go:29-35,69-77`），stdio adapter 的 `adapterCore` 也把 config 存在字段中（`internal/mcp/adapter.go:202-208`）。配置变动时 `runCycle` 只是断开并重连同一 client，再从同一个配置调用 `ListTools`（`internal/mcp/hot_reload.go:382-419`）；它不重新解析文件或构造新 client。

即便 server 的工具列表发生变化，`RegisterDeferred` 只替换 loader、删除 deferred registry 的 `loaded` 项（`internal/tools/deferred.go:66-81`），而 adapter `Get` 先返回 underlying registry 已加载定义（`:245-263`），旧工具定义/schema 仍会执行。新工具 loader 也不会递增底层 `DefaultToolRegistry.Version()`；AgentLoop 的 tool-definition cache 以 version 判断刷新，无法识别该变化（`internal/tools/middleware_registry.go:69-79`、`internal/core/loop.go:264-301`）。

因此编辑 endpoint/command、增删 server 工具或更新 schema 后，界面显示“触发 reload”不等于新配置已生效；该回路需要原子重建 client/工具定义、清理删除项、递增注册表版本并失效模型 schema 缓存。建议集成测试覆盖已 materialize 的工具、配置端点变化、工具 schema/名称变化和已删除工具。

### TOOL-008 — 远程 Bash 输出上限只裁最终 Output，SSH 缓冲和 Metadata 仍保存完整 stdout/stderr

**严重度：P2（远程输出可造成高内存占用）｜证据：A**

SSH client 把 stdout、stderr 写入无界 `bytes.Buffer`（`internal/tools/ssh_client.go:136-160`）。`RemoteBashTool` 再将内容写入 limited writer 裁剪最终 `Output`，但 `Metadata` 同时持有未裁剪的完整 `stdout` 和 `stderr`（`internal/tools/remote_bash.go:154-172`）。对配置的远端执行大量输出的命令时，`MaxOutput` 不构成捕获内存上限，原文还会一直挂在结果元数据上。

建议在 SSH 客户端使用限量 writer/流式接口，工具结果不携带重复的完整原始输出；增加可控 mock SSH client 输出超过上限的测试，断言峰值捕获和 Metadata 都有界。远程工具默认走 Ask 审批、用户配置远端执行本身不作为漏洞定性。

### TOOL-009 — 主文件工具没有模型可见参数 schema，运行时也不会验证这些参数

**严重度：P2（模型调用可靠性）｜证据：A**

AgentLoop 只在 `ToolDefinition` 实现 `tools.Parameterized` 时把 schema 传给 LLM（`internal/core/loop.go:292-301`），validator 对无 `Parameterized` 实现直接跳过（`internal/tools/middleware.go:205-223`）。`ReadTool`、`StreamingBashTool`、`WriteTool`、`EditFileTool`、`GrepTool`、`FindTool`、`LSTool` 仅实现 `ToolDefinition`；相反 git、remote Bash、custom、LSP、subagent、ImageRead 等部分工具有 schema。前述关键文件操作因而只把口头 Args 描述给模型，且不能靠统一 validator 拒绝缺字段、类型错误或多余参数。

这足以证明契约暴露不一致，不推断某个具体 provider 一定拒绝请求。建议给每个内置工具补完整 JSON Schema 并将执行器校验与 schema 共享；按每个 provider 的真实序列化格式做合约测试，确保模型定义、执行校验和 UI 审批预览一致。

### TOOL-010 — ImageReadTool 与可持久化 Goal/Task/Todo 的默认产品接线不完整

**严重度：P2（功能不可达/进程重启丢失）｜证据：A**

`ImageReadTool` 实现存在且带参数 schema，但生产 CLI 源里 `NewImageReadTool`/`image_read` 没有注册点；`rg` 命中仅限实现及测试。用户通过默认 CLI 不能调用它。Goal store 支持传入文件路径持久化，但 CLI 用 `NewDefaultGoalStore("")`，Todo/Task 则分别 `NewTodoStore()`、`NewTaskStore()` 创建进程内对象（`internal/cli/assemble.go:1220-1225`），相应会话数据不会跨进程保存。

需决定这些是有意未发布的库能力还是应在 CLI 中可用。若面向产品，应接通工具注册及媒体输入协议，并用应用数据目录配置持久化路径；加命令重启后的读回测试。若未计划支持，应在 CLI help/文档中避免暗示能力已可用。

### TOOL-011 — 默认 `web_search` 返回 Mock 数据，配置错误/漏配时功能退化但不失败

**严重度：P2（搜索功能可靠性）｜证据：A**

无 provider 选项时 `NewWebSearchTool` 明确构造 `MockSearchProvider`（`internal/tools/web_search.go:14-39`）。CLI 只在配置 `brave` 且有 API key，或配置 `fetch` 时装配真实 provider；其他情况保留默认 mock（`internal/cli/assemble.go:1227-1241`）。工具虽会写日志和 metadata `mock=true`（`internal/tools/web_search.go:77-89`），但模型收到的是成功格式化的搜索结果，而非“未配置真实搜索”。

建议生产 CLI 默认 fail closed 并返回明确 provider 未配置提示；MockProvider 留给测试或显式 demo 选项。新增无配置、brave 缺 key、未知 provider、真实 provider 各自的 CLI 集成测试。

## 审批与权限发现

### APR-001 — `y` 的“只允许一次”决定被写入通用会话/store 缓存

**严重度：P1（授权语义）｜证据：A；独立 Reviewer 复核通过**

UI 将 `y` 映射为 `ApprovalAllow`（一次允许），`a` 映射为 `ApprovalAlwaysAllow`（`internal/tui/app.go:1636-1651`；枚举注释 `internal/approval/callback.go:21-29`）。但 `ApprovalMiddleware.decide` 在 Ask 回调解析后，不论用户选的是 Allow 还是 AlwaysAllow，都会将结果写入 session map 和 `ApprovalStore`（`internal/approval/middleware.go:182-198`）；相同 mode/tool/args 的后续调用先命中这些缓存，跳过 callback（`:169-177`）。只有 `AlwaysAllow` 才写单独的 `ApprovalCache`，但 `Allow` 已被较早的通用缓存遮蔽。

所以同一 middleware/store 生命周期内，用户按一次 `y` 后，相同工具+参数的第二次调用不会再次问用户，实际行为等同重复授权。当前 CLI 的 `ApprovalStore` 是每次组装创建的内存 store（`internal/cli/assemble.go:1050-1071`）；不应夸称 `a` 当前会跨进程永久授权。已有测试验证通用 Allow 会缓存（`internal/approval/middleware_test.go:133-150`），并验证 AlwaysAllow 跳过后续 callback（`:306-325`），但没有覆盖 `ApprovalAllow` 的第二次调用语义。

应只缓存 classifier 自身的静态 Allow/Deny 或显式 AlwaysAllow；一次性 callback 决定只放行当前调用，不写跨调用 store/session。测试 classifier=Ask、callback 固定返回 ApprovalAllow，连续两次同参调用应触发两次 callback；ApprovalAlwaysAllow 则触发一次。

### APR-002 — Plan mode 的只读闭包以四个工具名实现，绕过多种有副作用的工具

**严重度：P1（只读承诺不成立）｜证据：A**

CLI `/plan` 描述为 “read-only exploration”（`internal/cli/slash_handlers.go:350-358`）；EnterPlanModeTool 也说代理应提出方案而不执行写工具（`internal/tools/plan_mode_tools.go:21-40`）。但 `writeToolNames` 只包含 `write`、`edit`、`bash`、`mutation`（`internal/core/plan_mode.go:28-35`），并由统一 interceptor 检查（`internal/core/plan_mode_middleware.go:12-22`；CLI 接线 `internal/cli/assemble.go:1380-1384`）。内置 Git 工具中 `git_commit`、`git_checkout`、`git_push`、`git_create_branch`、`git_merge`、`git_stash`、`git_stash_pop`、`git_reset`、`git_revert`、`git_pull` 等在 plan mode 下不会被该拦截器识别（注册清单 `internal/tools/builtins.go:153-173`）。`remote_bash`、MCP/custom/extension 工具也不在表内；其中任意外部工具只要实际产生副作用，仍可执行。工具审批会再 Ask 非只读工具，但人工点 y 不能替代“计划模式所有副作用都禁止”的契约。

修复方向是工具 capability/effect 元数据，而不是继续维护脆弱名称表。工具注册时声明 read-only、workspace-write、git-state、network-write、external-side-effect 等权限；plan mode 对写效果拒绝，未知副作用默认拒绝或要求离开计划模式。测试枚举所有 git 写操作、MCP/custom/extension 未知 effect，并包含通过 subagent 调用同一工具注册表的闭包测试。

**产品决策 D-01：** `exit_plan_mode` 同时暴露为模型可直接调用工具（`internal/cli/assemble.go:1255-1258`），执行后立即解除阻拦（`internal/tools/plan_mode_tools.go:61-98`）。工具描述明确写着解除计划模式，因此不能仅因代理能调用它就判作漏洞；产品需明确退出由模型决定、经 UI 确认，还是只能用户 `/plan exit`。若 UI 模式承诺“强制只读直到用户确认”，当前模型自主退出不符合该承诺；若设计上代理完成方案后可自主退出则行为合理。无论选哪种策略，建议写清系统提示和测试。

### APR-003 — TUI 动态 resolver 忽略静态策略传入的 Bash forbidden 名称

**严重度：P2（入口策略契约不一致，需产品定案）｜证据：A**

`assembleApproval` 在 TUI 路径构造 `SafetyPolicyClassifier([]string{"bash"})`，明确把 `bash` 放进 outright-forbidden 列表；但该路径随后给 middleware 注入 `AuditResolver(DefaultPermissionModeResolver)`（`internal/cli/assemble.go:1008-1013,1059-1067`）。Middleware 有 resolver 时优先使用它而非已构造的静态 classifier（`internal/approval/middleware.go:236-244`）；默认 resolver 的 `PermissionDefault` 分支创建 `SafetyPolicyClassifier(nil)`（`internal/approval/resolver.go:34-45`），于是 Bash 从 Deny 变为 Ask；TUI 人工批准后它可以执行。普通交互路径没有 resolver，仍以静态 `forbidden=[bash]` 执行（`assemble.go:1014-1028`）。

这是明确的两套入口策略不一致和 dead config，不单凭此判定为权限绕过：TUI 中用户仍然看见审批并可主动批准，产品可能本就希望允许人工判断。产品需决定 Bash 是人工确认后允许运行，还是所有模式禁用；再统一 resolver 输入/静态策略与描述，测试 TUI、非 TUI 交互、`--approve auto` / `--approve deny`。如果坚持绝对禁止，应让 resolver 保留 forbidden 集合并在 callback 前 Deny。

### APR-004 — 自定义 Classifier 返回未知枚举值时按 Allow 放行

**严重度：P2（扩展点 fail-open）｜证据：C**

`WrapToolCall` 只在 classification 恰等于 `Deny` 时短路；`Classification(99)` 既不是 Deny，也不会进入 Ask 分支，会直接执行 `next`（`internal/approval/middleware.go:152-159,182-188`）。此外 `Classification.String()` 对任何未知值返回 `"allow"`（`internal/approval/classification.go:28-38`）。内置 classifiers 均只返回合法常量，因此当前默认路径不触发；但 `ApprovalClassifier` 是公开接口/扩展点，插件错误或 enum 演进可静默 fail-open。

执行器边界应显式接受 Allow/Deny/Ask，未知值按 Deny 并记录 classifier 名与非法值；增加自定义 classifier 返回 99 时 executor 不会被调用的测试。

### APR-005 — 决策缓存没有绑定实际 executor、capability 与版本

**严重度：P2（扩展/动态注册的一致性问题）｜证据：C，默认 MCP 热重载已有独立故障**

`sessionKey` 只由 mode、tool name、canonical args 的哈希构成（`internal/approval/middleware.go:130-140`）；缓存命中发生在重新取 classifier/executor 之前（`:169-185`）。`DefaultToolRegistry.Register` 支持同名 tool last-write-wins 替换（`internal/tools/registry.go:20-22,39-60`）。在真实扩展代码通过同名替换 executor/capability 而未改变 name+args 的场景，旧决策可以作用到新 executor；缓存 key 不能证明它授权的是当前工作目录、实际参数解释、远程端点、MCP server/tool schema 或 executor 版本。

目前 MCP 热重载本身因为 TOOL-007 的 stale-definition bug 并不能造成“有效替换后旧授权放行”；不要把这种未发生链路描述成已复现利用。后续修好热替换前，应让审批前后的同一解析结果绑定 immutable tool identity/capability/version/workdir/endpoint，工具定义更新时失效旧审批决策。加 executor 变更前后相同 name/args 的回归测试。明确的用户 `AlwaysAllow` 需定义授权 scope，不应使用不能区分实现版本的通用名字。

### APR-006 — Project trust 入口未接通，fingerprint 保存但信任检查从不比较

**严重度：P2（受信配置功能不完整）｜证据：A/C**

`TrustProject` 保存项目 `.go-cli/mcp.json` 的 fingerprint（`internal/approval/trust.go:85-99`），但 `IsTrusted` 只查路径与过期时间，没有重新计算/比较 fingerprint（`:52-72`）。CLI `main` 为 `LoadTrusted` 构造独立的内存 TrustManager（`cmd/cli/main.go:22-29`），没有持久化 store，也没有对它调用 `TrustProject`；`assemble.go` 中 auto-discovered 项目 MCP/skills/custom 又通过进程全局的另一个 in-memory TrustManager 检查（`internal/cli/assemble.go:1794-1803,1867-1875,1937-1943`；全局默认也是内存 store `internal/approval/registry.go:20-28`）。生产命令源码未接入 TrustProject/撤销入口。结果是默认 CLI 没有可完成的项目授权/撤销流程，项目配置始终被当未信任；即使调用 API 建立 trust，配置内容变更也不会按 comment 描述使其失效。

建议统一 CLI 和 assembler 使用同一个显式 TrustManager、提供明确的交互授权/撤销路径、对 project path 规范化，并在每次加载受信配置时比对 fingerprint；写测试覆盖全局配置安全校验、项目批准、持久化、文件变更撤销和 CLI 重启后的结果。全局配置已实施 owner/权限验证（`internal/approval/trust.go:159-185`），这是合理保护。

### APR-007 — Durable approval JSONL 记下分类结果 Ask，没有记录用户最终 y/n/a

**严重度：P2（审计可追溯性缺口）｜证据：A**

`AuditClassifier` 在 `Classify` 返回时写入 `allow/deny/ask`（`internal/approval/classifier.go:141-179`）；TUI callback 的实际批准结果在之后由 `resolveAsk` 映射到 Allow/Deny（`internal/approval/callback.go:145-172`、`middleware.go:201-233`），没有写回 durable `AuditTrail`。因此用户按 y/a 后，JSONL 留下的事件仍是 `ask`，不能回答最终是否运行；Span 上的分类属性是最终结果，但并非该 append-only 文件。

当前 AuditEntry 类型名称确实限定为“classification decision”，如果它只要求审计分类器输出，这不是实现错误；但 CLI 注释将该文件描述为 approvals 的 append-only audit trail（`internal/cli/assemble.go:1044-1048`），用它复盘人工授权会缺核心事实。建议审计事件区分 classifier_decision 与 approval_response，并记录一次性/AlwaysAllow、调用 ID、执行结果；参数需脱敏/限长，避免将秘密写入持久日志。测试确认 y/n/a 与最终执行结果一一对应。

### APR-008 — `PermissionAuto` 未知工具默认 Allow，且 CLI 没有接入该运行时模式

**严重度：P2（未接线策略的潜在 fail-open）｜证据：C**

`DefaultPermissionModeResolver` 的 Auto 分支传空 safe/dangerous set（`internal/approval/resolver.go:34-45`）；`AutoClassifier` 对 safe 与 dangerous 未知工具都默认 Allow，只有列入 dangerous 才 Ask（`:70-105`）。这与 Auto 注释“安全工具自动允许、危险工具确认”相比，是对未知工具的开放式默认。当前 `SetPermissionMode` 在生产 CLI 没有调用点（`rg SetPermissionMode` 命中只有定义及测试），也没有用户可选 PermissionAuto 的接线，所以这是未触达策略 API，不是当前 CLI 可利用路径。

在添加该模式之前，应明确 safe/dangerous capability registry；未知 capability 默认 Ask/Deny。再通过 CLI 入口测试实际工具类别与各模式的决策表，不能以单测默认未知 Allow 作为安全保障。

## 合理设计与当前测试盲区

- **合理且已有接线：** Bash 与 Streaming Bash 默认拒绝 nil sandbox；`--no-sandbox` 是显式 opt-out；命令过滤和资源限制有独立组件。Bash 命令黑名单是减少误操作的 defense-in-depth，不是安全容器。
- 文件路径工具对词法 `..`、绝对路径 whitelist 和最终符号链接做了基础校验；写入使用 atomic replace；Write/Edit 通过 per-file queue 排序，queue `Close` 会等待 sender/worker 完成（`internal/tools/mutation.go:364-394`）。diff 与 backup 错误当前记录后继续写，应明确它们是提示/补救功能，不是事务回滚保证。
- Web Fetch 对 URL scheme/解析 IP 做校验，再在拨号时再次拦截私网、链路本地和 metadata 地址，降低 DNS rebinding 风险（`internal/tools/url_validator.go:63-107,110-165`；调用处 `web_fetch.go:97-123`）。MCP HTTP 特意允许 loopback 用于本机服务，但仍拦截其他私网地址，这符合本地 MCP 配置场景。
- MCP、custom、remote Bash 未列在内建只读审批白名单中，普通 Ask 流程要求确认；用户显式 `--approve auto` 则授权所有工具，此为明确的产品权限选择。SSH 使用 `StrictHostKeyChecking=yes`，密码经 `SSHPASS` 环境变量而不是 argv 传递（`internal/tools/ssh_client.go:102-128,168-190`）。
- MCP 工具结果带 prompt-injection 启发式扫描；它适合作为警告/降权措施，不能作为注入安全边界。HTTP response 有大小上限（`internal/mcp/http_adapter.go:49-66`）。
- 工具层有 55 个 Go 测试文件、审批层有 12 个；路径词法拒绝、基础 truncation、Timeout、审批 Allow/Deny、AlwaysAllow、线程安全等已覆盖。主要盲区是复合契约：一次批准再重复、真实 pipe buffer 以上截断、计划模式每个 side-effect tool、配置变动后的 MCP tool replacement、父目录 symlink、恶意超大搜索结果和 CLI trust 闭环。**本轮未执行全量测试或 make verify**；唯一运行时测试是 TOOL-002 的有限定时复现。

## 建议迭代顺序

| 阶段 | 目标 | 交付与验收 |
|---|---|---|
| **T0：权限语义和授权安全** | 修正 `ApprovalAllow` 缓存；梳理每个入口的分类/审批契约；明确 Bash forbidden 与 TUI Ask 的产品语义 | 一次批准后第二次仍确认；AlwaysAllow 只在明确 scope 内重用；合法 enum 全覆盖；权限策略表对齐 TUI、非 TUI、remote、MCP、custom、git |
| **T1：Capability Policy / Plan Mode** | 用 executor capability/effect 替换 `writeToolNames` 和仅工具名审批 key；未知扩展工具采取保守默认 | 对 git、custom、MCP、subagent、remote 和动态注册全量 side-effect 测试；Plan mode 期间不存在写/网络副作用执行路径；授权决策绑定 executor 版本与工作目录/endpoint |
| **T2：Runtime 进程、输出和取消** | 修复 Streaming Bash drain；统一 Bash/SSH/custom/Grep/Find 的 bounded capture 和进程取消清理 | 大于 pipe buffer 的有限命令在 timeout 前完成；超限输出只保存固定额度；远程 Metadata 不保留原文；取消搜索返回 context error；CLI EventStream 端到端可断开 |
| **T3：文件工具约束与参数契约** | 澄清 `allowed_paths` 是 cwd 配置；若需文件访问隔离则作为单独 OS sandbox 目标；修复 symlink parents 和必需参数；对所有工具注册 JSON Schema | 当前 cwd 合同测试；若新增 OS sandbox，再测外部读写、重定向与符号链接；缺失 `content/new_string` 拒绝且不变更文件；每个模型 provider 的工具 schema 与执行验证合同测试 |
| **T4：MCP/trust/审计闭环** | Trust UI/API 与 loader 共用一个 manager；指纹变化撤销；热重载重建 client、原子替换工具和缓存；记录最终人工审批 | 项目配置可批准、持久化、撤销；改 endpoint/schema/add/remove server tool 后模型和 executor 同步；审批 JSONL 能回放“分类—用户决定—执行结果” |
| **T5：功能完整度与产品默认** | 决定暴露 image read 与 persistence；真实搜索 provider 缺失时明确提示 | image_read 默认可发现/注册；Goal/Task/Todo 是否跨重启保持有产品合同和重启测试；搜索缺 provider 时显式提示而非返回伪结果 |

## 源码覆盖清单

本次逐文件覆盖全部 `internal/tools` 生产 Go 源（70 个）和 `internal/approval` 生产 Go 源（11 个），并检查这两处所有测试源码（分别 55、12 个）。CLI/边界调用链按发现所涉源文件追踪，未把下列源码清单中的未执行测试描述成通过。

`internal/tools`：

`ask_user_tool.go`, `bash.go`, `bash_sandbox.go`, `builtins.go`, `custom_tool.go`, `deferred.go`, `diff.go`, `edit.go`, `error.go`, `file_tracker.go`, `find.go`, `git.go`, `git_blame.go`, `git_branch.go`, `git_checkout.go`, `git_commit.go`, `git_create_branch.go`, `git_diff.go`, `git_fetch.go`, `git_log.go`, `git_merge.go`, `git_pr_create.go`, `git_pull.go`, `git_push.go`, `git_remote.go`, `git_reset.go`, `git_revert.go`, `git_stash.go`, `git_stash_pop.go`, `git_status.go`, `goal_store.go`, `goal_tools.go`, `grep.go`, `html_converter.go`, `image_read.go`, `ls.go`, `lsp.go`, `lsp_client.go`, `lsp_multi.go`, `lsp_tool.go`, `middleware.go`, `middleware_registry.go`, `mutation.go`, `mutation_middleware.go`, `plan_mode_tools.go`, `read.go`, `registry.go`, `remote_bash.go`, `resource_limits.go`, `resource_limits_darwin.go`, `resource_limits_linux.go`, `resource_limits_other.go`, `result_masking.go`, `search.go`, `snapshot_manager.go`, `ssh_client.go`, `streaming_bash.go`, `subagent_tool.go`, `task_store.go`, `task_tools.go`, `todo_store.go`, `todo_write.go`, `tools.go`, `truncate.go`, `url_validator.go`, `web_fetch.go`, `web_search.go`, `web_search_provider.go`, `worktree_manager.go`, `write.go`。

`internal/approval`：

`audit_trail.go`, `callback.go`, `classification.go`, `classifier.go`, `middleware.go`, `permission.go`, `registry.go`, `resolver.go`, `store.go`, `trust.go`, `trust_store.go`。

主要接线文件：`internal/cli/assemble.go`, `internal/cli/slash_handlers.go`, `internal/cli/slash_mcp_reload.go`, `internal/core/loop.go`, `internal/core/plan_mode.go`, `internal/core/plan_mode_middleware.go`, `internal/tools/middleware.go`, `internal/tools/deferred.go`, `internal/tools/middleware_registry.go`, `internal/config/types.go`, `internal/config/config.go`, `cmd/cli/main.go`, `internal/mcp/hot_reload.go`, `internal/mcp/http_adapter.go`, `internal/mcp/adapter.go`, `internal/llm/llm.go`。
