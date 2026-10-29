# R2 下一小批：搜索文件闭环，再做本地 Git 交付

2026-10-29，Designer提案已通过独立源审；尚未实现或运行。本轮先收束既有候选与统一fullverify，本设计的新增Go测试另排下一批。先补第一条文件链，第二条Git链下批再做，不把它们扩成全部工具平台。依据本地 `r2-current-coverage-review.md` 源审；既有组件/CLI装配测试不能替代独立二进制最终产物验收。

## 共用最小入口

复用 `tests/e2e_binary_tools_test.go` 的实际race CLI构建、fresh HOME/工作区、HTTP peer、公开prompt入口；结合已集成R1b的durable action独立检查方式。新增测试文件即可，不改生产registry、threshold、工具权限或默认注册，不能通过禁用故障工具凑绿。

每次run固定binary/source/config/profile/hash；session放工作区外，memory-off、无small-model/扩展/MCP，明确这是机制隔离profile。测试目标是当前真实可达工具，不声称自主模型解题；零付费。复用现有peer的小扩展应保持旧用法默认兼容，不新建执行框架。

**实时广告门**：peer每次先解析当前实际请求中的tools/schema，只有待发调用在这一轮真实广告且参数匹配时才发送。记录请求hash、实际tool/schema hash和唯一tool_call_id；不能等调用成功后才反推“模型一定看得到”。不把Registry.List与wire广告混为一谈。

默认TopTools/threshold30保留。初始合成需求可明确请求本链的搜索/读改任务，不能使用测试内部白名单把全registry强塞给模型。需要发现时只调用当前广告的 `tool_search`，其真实schema是query/category，没有load参数，而且当前实现输出name/description。搜索后目标仍未广告应失败并记录可达性缺口，不直接调用内层registry、不编造load参数、不关生产threshold。不得把“曾经在旧请求出现”当本轮仍广告。

## 第一小批：文件搜索与读改回读

临时目录有空格路径、嵌套目录、两个txt及一个不匹配后缀的文件。目标txt第二行含本次随机token，其它文件是诱饵。外置grader持初始文件清单、内容hash和目标转换规则；peer不读宿主磁盘，只从真实工具结果获得路径/内容来推进后续调用。

建议正向顺序如下，全部使用当前实际schema：

| 步骤 | 真实工具/参数 | 后续推进依据 |
|---|---|---|
| 发现 | 若当前广告，`tool_search {category:"file"}` | 真实返回名称和描述，不伪称加载schema |
| 列目录 | `ls {path:"data space"}` | 归一化后目录项集合准确 |
| 找文件 | `find {path:"data space",pattern:"*.txt",type:"f"}` | 匹配文件集合准确，不误收log或目录 |
| 搜内容 | `grep {path:"data space",glob:"*.txt",pattern:<token>}` | 真正目标路径、行号2、匹配内容准确 |
| 读目标 | `read {path:<从实际结果取得的目标>}` | 下一轮收到对应真实tool result及内容 |
| 写结果 | `write {path:"result.txt",content:<由实际read结果派生>}` | 不由peer在磁盘直接写，不用预知最终答案代替工具数据流 |
| 错误反馈 | `edit {file_path:"result.txt",old_string:<不存在的片段>,new_string:...}` | 实际错误进入下一轮，产物未被错误修改 |
| 修正 | `edit {file_path:"result.txt",old_string:<实际存在且唯一的片段>,new_string:...}` | 正确修正结果由下一次read回读 |
| 回读/结束 | `read {path:"result.txt"}`→final | final之前已核真实回读；不只看最终自然语言 |

`edit`真实字段是file_path/old_string/new_string，不能把read/write的path错误套给它。可加一次grep无匹配，验证空结果仍按真实成功/错误语义进入后续；不要顺手扩全部路径、编码或权限矩阵。

### 第一链的可执行验收

1. 外置grader小合同先红→绿：最终磁盘文件集合、结果bytes/hash、输入文件未变，搜索实际集合及行号/内容都匹配初始fixture与规则。runtime日志/session只按预先声明路径排除，不把任意旁文件全部忽略。
2. 每一步的真实tool result在**下一次实际HTTP请求**中按call_id找到；不是peer自造tool回复或只验script步数。live广告guard拒绝缺失schema/未知参数，留明确失败，不注入绕过。
3. 正常唯一action逐项有intent、成功receipt、quiescent、适当非空正向proof、typed resolution，结束pending=0且public CLI退出0。错误edit按实际dispatch/receipt/no-dispatch分类核对，不能要求它mint一个没有发生的process proof，也不把错误包装成success。
4. 两个有区分力负对照只测独立grader：保留完整final/退出/证明证据但改错业务文件bytes，必须拒绝；从已捕获证据副本移除一个对应tool result，必须拒绝。后者不能用“peer脚本耗尽导致abort”冒充grader检验。原正向工件不改，负例在副本/独立沙盒执行，不声称两条完整CLI故障注入已运行。
5. 运行入口建议命名 `TestRealBinaryR2FileJourney`（待Builder落实际名称）。复用无tag现有binary fixture进入普通fullverify；首次只跑明确目标后交Reviewer，再统一fullverify，资源窗由根安排。若测试使用tag，必须明确普通make verify不会自动执行，不能二者混称。

## 第二小批：本地 Git 写入、推送和拉取交付

先做可验收的最小写链，不一次宣称17工具全认证。三个自有临时目录：workrepo、bare remote、collaborator。所有remote固定为临时bare的本地绝对路径，没有真实GitHub/凭据。准备阶段提交baseline、配置local user.name/email及pull.ff=only；CLI日志目录在fixture的正常ignore清单内，session放repo外，避免git add-all把运行证据当业务提交。

真实已注册工具没有 `git_add`：暂存使用 `git_commit {message:...,add_all:true}`，真实实现先运行git add-A。`git_remote {}`只列remote，不能用虚构add参数；origin在初态准备。

最小调用序列：status/diff→`git_create_branch {name:"journey"}`→`git_checkout {branch:"journey"}`→write业务文件→`git_commit {message:...,add_all:true}`→log→`git_push {remote:"origin",branch:"journey"}`→fetch/pull→status/read/final。模型每轮仍须通过实际广告门，不用Bash代替已注册的Git动作。

为避免fetch/pull只是空操作：在实际push成功结果和资源关闭被观察后，由**明确记录的外部collaborator fixture**只向bare推进一个额外commit，加入独立remote marker；不直接改agent workrepo。随后真实 `git_fetch {remote:"origin"}` 和 `git_pull {remote:"origin",branch:"journey"}` 必须将这个已知remote commit取回。外部actor动作单列，不能冒充agent完成的工具动作。

外置Git验收独立执行git fsck、读取HEAD/tree/本地branch/remote-tracking refs/bare refs、业务内容及工作区clean；最终HEAD应等于外部collaborator记录的commit，baseline后的两次业务commit应恰为agent提交及remote推进，原始文件未意外改动。逐唯一action检查proof/resolution/pending，不能只信工具输出中的commit hash或final文本。

第二批可先加重复branch造成真实Git错误后继续的反馈合同，以及wrong tree/out-of-date bare ref的grader负例。checkout/reset/revert/merge/stash等其它写操作、真实remote协议与Git取消仍单列partial，不能从这条最小链推为17项全部验收。强杀/重启/no-replay边界继续独立分组，不为本次两个链无限加场景。

## 准入与剩余范围

先第一链：独立grader负例、小peer广告/结果合同、真实binary正向旅程、Reviewer；通过后再第二链。出现产品bug保留红例后回设计，不为通过测试修改threshold或completion守卫。publicbinary、host OS、库组件和真实模型各层结果分别记录。

R2仍是partial：todo/task/goal、真实子Agent/MCP/审批拒绝/所有可选工具、全部Git写分支未因本设计完成。无费旅程通过后才升级对应覆盖行；当前没有执行结果，不改D/正式profile/新10锁，也不启动模型。
