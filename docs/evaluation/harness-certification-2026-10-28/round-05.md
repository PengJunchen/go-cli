# 第5轮：Go工具环境修复与可选依赖补验

2026-10-29，记录截至本次文档提交时的状态。当前工作收敛为Go-only工程修复与新增标准题，Pi不再阻断Go验收。[第4轮Flipt配对](round-04.md)的版本、分数和原始记录保持不变；本轮尚未新增真实模型任务，也不把工程测试换算为模型得分。

## 已通过的基线与fd补验

已推送的`e1f5291…`在GitHub的13个检查全部完成并通过，包括race/coverage、E2E、Compliant、构建和静态检查。这是该源码版本的远端CI事实，不是下述环境修复已通过。此前本机整合`make verify`为9749 started、9740 pass、0 fail、9 skip，另log10/leak6通过；可选goimports跳过，详见[第4轮](round-04.md)。

其中三个依赖真实`fd`的分支此前因工具缺失跳过，本轮已补验。只在外部工具目录安装官方`sharkdp/fd v10.3.0`固定Darwin/ARM64发行包，对目标测试进程临时前插PATH；没有全局安装，也没有改变正式镜像、评测profile或测试源码。

三个既有目标加四个子测试在race下全部通过，0 skip、0 fail，目标命令墙钟约5.88秒。它们覆盖不同搜索根目录的结果归一化，以及将`--exec=touch`当文件名数据、避免变成命令选项。装配层测试还要求实际原生fd退出0、参数有`--`分隔、文件mtime未改变，避免仅凭fallback结果宣告原生调用成功。其它两个测试保留Find的fallback语义，不夸大为每次调用都导出了原生执行轨迹。

官方asset.digest与下载归档SHA均为`05702638…`，归档内fd与实际安装binary SHA均为`14134aad…`；1365份Go文件SHA及mode前后及独立复核均无变化。这关闭了上述三个目标的本机依赖缺失分支，不能外推全部可选工具或所有平台已验收。

## 新诊断：同镜像仍可能传入不同工具环境

0真实模型、无业务补丁的新鲜对照发现：直接启动的基础Bash可找到go/gofmt；冻结Go CLI实际Bash丢失了PATH中的`/go/bin`和`/usr/local/go/bin`，这两条命令变为missing。宿主`GOPATH=/go`在工具中变成unset，Go推导的模块路径从`/go/pkg/mod`变成`/root/go/pkg/mod`。

`/go/pkg/mod`实际存在，只有“31个直接条目”的有界目录观测，**不证明完整依赖覆盖，更不是Go build cache命中**。两边派生的GOCACHE目录均不存在。两臂各观察到一个Z进程表记录，均不是活后台任务；来源没有确定，也不能据此说是Go独有泄漏。两臂容器及网络最终均已清理。

这足以支持修复通用的宿主环境传递契约，不能倒填正式Flipt未导出的环境，更不能断言原题的所有依赖下载/DNS失败由某一个变量造成。两边都缺少rg，本次修复也不承诺安装rg或修复网络。

## 最小修复已通过实际环境复验

[修复设计](bash-host-environment-design.md)只做两件事：普通与流式Bash统一采用非登录`bash -c`，与受管启动gate同步；仅增加`GOPATH`、`GOCACHE`、`GOMODCACHE`三个固定路径键的宿主继承。保留unset、空值和实际值，不写死任何题目或镜像路径，不开放任意环境变量配置。

兼容变化是：工具不再自动读取用户登录初始化文件，而以受信启动者已准备的PATH为准。敏感变量过滤、调用方显式WithEnv能力、审批、沙箱、超时、取消、Unknown和资源证明规则保持原合同；不默认继承GOFLAGS、GOPROXY或环境启动脚本变量。

TDD红合同捕获了旧行为：两个工具实际仍是login shell，三个路径键的空值/非空值丢失，受管gate仍接收旧的`-lc`入口；unset、未列变量过滤、敏感后缀与显式覆盖的既有负例仍通过。该失败记录保留。最小实现只有三份生产文件及两份测试文件，实施提交为`a7ae0207e960bac4f49df39cf8f23176e4b88d38`，源码独审通过；新目标race合同及既有安全/生命周期回归也已通过，未删原安全断言。

唯一一次新鲜Linux无模型复验已独审通过。base与新Go导出的选定环境及neutral Go env完全相同：PATH保留上述宿主目录，裸go/gofmt恢复，GOPATH为`/go`、GOMODCACHE为`/go/pkg/mod`。配置、镜像、命令和driver环境策略保持；driver未额外注入路径。两臂均退出0（base为直接Bash，Go为一次真实CLI），Go一个action有资源完成证明与typed resolution、pending0；仅2次本地脚本HTTP、0真实模型。rg仍缺失、GOCACHE未预热、缓存覆盖未知；两边各1个Z观测仍保留。

新二进制SHA为`59748502…`，源身份为`e1f5291…`加完整patch `c88f5c40…`，不冒充旧f4产品。其payload原件继承了不受本次数据支持的“1362全树不变”字段，已另存纠错侧车：该次构建前后实际只逐文件核验5份已审文件，其余身份依据base加完整patch；原件不回写。relay已记录rm/inspect后的absence，但没有保存其精确ID，不能称独立ID级复核或整个namespace零残留。

随后主工作树整合`make verify`已PASS：485.373秒，9780 started、9774 pass、0 fail、6 skip、31包；另log10/leak6、lint0 issues、vet/build/scan通过，可选goimports仍跳过。本次1366份Go文件SHA及mode前后一致，并给该目标进程提供固定fd。它与上段5文件构建证据是两次独立记录，不能互相倒填。随后tagged补验也已完成：5/5通过（含父测试）、0 fail、0 skip，目标命令墙钟50.183秒；四个叶项分别为旧120轮5.92秒、PTY follow-up 1.30秒、PTY escape 6.32秒、R1b 25.74秒。它们是本地脚本模型的工程合同，不是自主真实模型长任务成绩，也不与旧版本时长拼成性能样本。

主树规范重建使用Go1.26.1、CGO关闭、Linux/amd64及固定构建参数，退出0、1.448秒；生成binary与实际Linux诊断的`59748502…`字节完全一致。1366份Go源SHA及mode在该次门前后不变，且与fullverify所验源码相同。这是新增的真实全树与二进制等价证据，**不回填旧payload的错误范围字段**。源码、完整门禁及R1b业务原件均已独立终核通过；上述实施提交已在本地完成，尚未push；本报告按文档提交时状态记录新远端CI待运行，不沿用e1的13项绿灯为新修复背书。

R2文件链保持已通过状态，Git真实二进制闭环仍待设计/执行；合成长链R1b不等于真实模型自主长项目。下一收费题与额外10题锁没有因此解封，后续无同期Pi时会明确记为单臂、非paired。

## 可审计来源

以下为本地证据库的安全工件内容SHA，不发布原始模型正文、补丁、隐藏评分输出或机器路径。fd来源为[官方v10.3.0发行页](https://github.com/sharkdp/fd/releases/tag/v10.3.0)，完整性依据官方GitHub asset.digest；没有独立签名附件，不称为签名认证。

| 证据 | SHA256 |
| --- | --- |
| e1远端13项CI快照 | `3d3edf22bbb0aa21d8131f862b7b4fbf381b327fe20719f0b5bd394b4eea62ce` |
| fd补验安全摘要 | `1b8cdf50b9a8e0c39a497ead6d13b4311674a983cb073d669329e6e2b676f444` |
| fd归档 / binary | `0570263812089120bc2a5d84f9e65cd0c25e4a4d724c80075c357239c74ae904` / `14134aadba85ab2cfe4494d0f44253145f897b77a23fd4d7df2cb4929b53c786` |
| fd补验独立审查 | `261b03caddb5cbe841d0824b4f21697a1fe9179f6b56ef72804098889d123369` |
| 环境对照validated / 独审 | `489be1a8ddc5cb2204624495c150fb7b0c41b25e65567007554e05fff4a06864` / `762b75d0233464f8592d3959b5f76cf81557bd45ab7e062b29cc4d1fccfd1a38` |
| 环境设计 / 设计准入 | `a07ae42e6b32270b4bd3b84c590604646343b04740051646cdb2e3aae6b61e11` / `030e57634b8286d244c08eb75f3f5fe6cd0aacb7d06737ba07f4cd002651fd8b` |
| 修复源码独审 | `c3c89c0bd62f59896db6f91a0b6fe13ac88abcc9d6b26bbcbe75599bcddc0038` |
| Linux复验validated / 独审 | `c15b592005f80e8578dd6dc161822f1b9ee61cb368e528ca9bd04576e8091b47` / `cdfef97cf86d436e3fec401af7ee2a420a8d54177acdd99c4b15dc54f38f2fe0` |
| 新Linux binary / payload | `59748502f52a06dc1169928a0f5e2bd3c493df5729d2b1c38205d537f25b41d6` / `f62a01d89ac5865475c0b39dfbf73444967e93fb23fa09dc8062594dfe75c835` |
| payload构建范围纠错 | `5950fdc347ec68ceb2aaff35a431d240272147c400329cf0e49a12f4616817f9` |
| 资源取证范围说明 | `1277bba423bfac016941f7e539e974bdac54a5fb888ec57cc564f3611ff4596e` |
| 本次整合fullverify摘要 / 日志 | `f44bd8e777a63cf8889cab447cfe6e12a6be606fd4bddad64e9d9d80498970a1` / `ae9aacb78bd6703bf908b5b94519d3c4969553463e9e46db18be57977c898533` |
| tagged与canonical摘要 / tagged日志 | `5cb6269e558a1650b350e0e9190a7987fe0fa687e7c2467752114b8301dc320b` / `76cb73eff317d64c72a34ea0b38067dda8b98df29f11ae00884a222a1c349ecc` |
| full/tag/canonical及R1b业务原件终核 | `75b0a80f21b43669b7e9ff9f4105e6a57918da50736296c29bc6c833d490ff9b` |
| 旧行为红合同日志 | `38d35339d8ffb023ca9d88a18e183fccaf5acfc49a7f96422192713ee59b2953` |
