# Harness 评测源包

当前基于main `5736e8828c80913fd4a32dc7b6e22eb82e5ec2b1`：迁入Thinking修复与通用CLI回归，保留上游Git生命周期。[当前审核](reports/release-review.md) · [profile与版本边界](reports/main-migration-review.md) · [入门指南](../../docs/evaluation/README.md)。旧379/B/Pi/C记录不混为当前成绩。

## 导出到新的外部工作区

以下准备不调用模型。目标目录必须不存在，Python使用3.12+。

```bash
export HARNESS_EVAL_WORKSPACE="$PWD/../go-cli-eval-reproduce"
(
set -e
test ! -e "$HARNESS_EVAL_WORKSPACE"
mkdir "$HARNESS_EVAL_WORKSPACE"
cp -R scripts/harness_eval/runner/. "$HARNESS_EVAL_WORKSPACE/"
cd "$HARNESS_EVAL_WORKSPACE"
python3 -m venv .venv
.venv/bin/python -m pip install -r requirements.lock
mkdir third_party
git clone https://github.com/scaleapi/SWE-bench_Pro-os.git third_party/swe-bench-pro
git -C third_party/swe-bench-pro checkout 66f92766bba642462d4bbe5479e83f91f9211862
.venv/bin/python -m unittest discover -s tests -v
)
```

## 准备共同payload

目录为 `artifacts/payload/{go-cli,manifest.json,node/,pi/}`，不提交二进制或依赖目录。

- Go：从被测源码执行 `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -o <工作区>/artifacts/payload/go-cli ./cmd/cli`。
- Node：从[官方22.22.2目录](https://nodejs.org/dist/v22.22.2/)下载 `node-v22.22.2-linux-x64.tar.xz`，核对SHA256 `88fd1ce767091fd8d4a99fdb2356e98c819f93f3b1f8663853a2dee9b438068a`，解压到payload/node。
- Pi：复制 `runner/locks/pi/` 的两份JSON到payload/pi，使用该package-lock执行 `npm ci`，版本固定1.4.0。
- manifest至少填实际 `go_cli_source_revision`（40位）与 `go_cli_sha256`；同时记录dirty patch SHA、编译器、平台及Node/Pi/Python锁身份。driver核对binary哈希；新构建不得复用旧manifest。

## 运行顺序

先拉取 `dataset-lock.json` 中的精确镜像digest，再按 [RUNNER.md](runner/RUNNER.md) 执行网络probe、无费single/parallel fixture、独立评分控制。当前profile显式 `memory.enabled=false`，不同于产品默认true；原生session、工具与压缩保留。镜像缺失先恢复资产，不能降级跟随tag。

**去掉 `--fixture-smoke` 会调用真实模型并可能计费。** 模型批次由协调者统一冻结与调度；CLI退出成功不代表任务通过。官方成绩取fresh clean grader，失败与超时保留。[D单题开发回归](../../docs/evaluation/harness-certification-2026-10-28/round-02.md)官方1分但关停失败，完整交付分0；更多未见题与全工具/长任务认证均未完成。[辅助策略审计](reports/auxiliary-policy-audit.md)公开当前无LLM摘要的配置边界。

公开包仅有自有代码、合同、依赖锁、任务/镜像元数据与安全报告。原题、gold、隐藏验收、raw轨迹、镜像、私钥和旧Git历史留在包外；第三方许可仍须遵守。详见 [评测方法](../../docs/evaluation/methodology.md) 和 [AA参考边界](reports/aa-reference.md)。
