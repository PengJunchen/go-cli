# 真实运行轮次索引

> Retention note (2026-10-24): raw per-run reports and terminal artifacts were removed after the outcomes and failure sequence were distilled into the live QA digest. This index preserves status and counts only.


完整最终193912在合并bdb76ef通过；191314是ec267历史通过。失败、诊断模型、局部smoke/artifact-focus保留原报告。请求数不等于用户轮数；184011第30请求是内部summary，不能当pressure4主响应。源码身份及diff以每轮报告为准。

| 运行 | 请求模型 | 状态 | 用户轮记录 | 请求记录 | 截图记录 | 源码 |
| --- | --- | --- | ---: | ---: | ---: | --- |
| [20261014-150122-7ef3b912](terminal/20261014-150122-7ef3b912/run-report.json) | 未保留 | failed | 0 | 0 | 0 | `e0493e5f` |
| [20261014-150427-60ef4e9b](terminal/20261014-150427-60ef4e9b/run-report.json) | codex/gpt-6.1-sol | failed | 1 | 3 | 4 | `e0493e5f` |
| [20261014-152358-63990f30](terminal/20261014-152358-63990f30/run-report.json) | codex/gpt-6.1-sol | failed | 1 | 2 | 3 | `e0493e5f` |
| [20261014-153101-ef080409](terminal/20261014-153101-ef080409/run-report.json) | codex/gpt-6-luna | failed | 1 | 3 | 4 | `f827a91e` |
| [20261014-153502-05113d31](terminal/20261014-153502-05113d31/run-report.json) | codex/gpt-6-luna | failed | 2 | 6 | 3 | `f827a91e` |
| [20261014-155103-a11d7e7c](terminal/20261014-155103-a11d7e7c/run-report.json) | codex/gpt-6-luna | live-smoke-only | 5 | 13 | 8 | `6f2762e8` + 工作区差异 |
| [20261014-155705-a891a2fe](terminal/20261014-155705-a891a2fe/run-report.json) | codex/gpt-6-luna | failed | 4 | 13 | 7 | `6f2762e8` + 工作区差异 |
| [20261014-161209-82245643](terminal/20261014-161209-82245643/run-report.json) | codex/gpt-6.1-sol | failed | 0 | 0 | 0 | `508c785d` |
| [20261014-161230-dbf24f1e](terminal/20261014-161230-dbf24f1e/run-report.json) | codex/gpt-6.1-sol | failed | 1 | 3 | 2 | `508c785d` |
| [20261014-162026-42a11637](terminal/20261014-162026-42a11637/run-report.json) | codex/gpt-6.1-sol | live-smoke-only | 5 | 12 | 8 | `508c785d` |
| [20261014-162310-59f1ebd1](terminal/20261014-162310-59f1ebd1/run-report.json) | codex/gpt-6.1-sol | failed | 5 | 14 | 8 | `508c785d` |
| [20261014-162904-ce3be8d0](terminal/20261014-162904-ce3be8d0/run-report.json) | codex/gpt-6.1-sol | failed | 5 | 14 | 8 | `508c785d` |
| [20261014-163504-7c08979c](terminal/20261014-163504-7c08979c/run-report.json) | codex/gpt-6.1-sol | failed | 5 | 13 | 10 | `508c785d` + 工作区差异 |
| [20261014-163836-56a49dbf](terminal/20261014-163836-56a49dbf/run-report.json) | codex/gpt-6.1-sol | failed | 5 | 14 | 10 | `508c785d` + 工作区差异 |
| [20261014-164221-a3e607ba](terminal/20261014-164221-a3e607ba/run-report.json) | codex/gpt-6.1-sol | failed | 5 | 13 | 11 | `2c58f995` + 工作区差异 |
| [20261014-164632-5efefab2](terminal/20261014-164632-5efefab2/run-report.json) | codex/gpt-6.1-sol | failed | 5 | 14 | 12 | `2c58f995` + 工作区差异 |
| [20261014-165012-9dc46718](terminal/20261014-165012-9dc46718/run-report.json) | codex/gpt-6.1-sol | failed | 5 | 14 | 12 | `2c58f995` + 工作区差异 |
| [20261014-165617-51d002f0](terminal/20261014-165617-51d002f0/run-report.json) | codex/gpt-6.1-sol | failed | 7 | 16 | 17 | `2c58f995` + 工作区差异 |
| [20261014-170449-4818728d](terminal/20261014-170449-4818728d/run-report.json) | codex/gpt-6.1-sol | failed | 1 | 1 | 4 | `2c58f995` + 工作区差异 |
| [20261014-171811-82c75abe](terminal/20261014-171811-82c75abe/run-report.json) | codex/gpt-6.1-sol | failed | 1 | 2 | 9 | `3a20cc23` |
| [20261014-172353-ae18536c](terminal/20261014-172353-ae18536c/run-report.json) | codex/gpt-6.1-sol | failed | 1 | 3 | 12 | `3a20cc23` + 工作区差异 |
| [20261014-173000-9c8ba10a](terminal/20261014-173000-9c8ba10a/run-report.json) | codex/gpt-6.1-sol | failed | 2 | 2 | 16 | `3a20cc23` + 工作区差异 |
| [20261014-174155-f895248a](terminal/20261014-174155-f895248a/run-report.json) | codex/gpt-6.1-sol | failed | 3 | 12 | 36 | `7d5dfd09` |
| [20261014-181548-4087fce4](terminal/20261014-181548-4087fce4/run-report.json) | codex/gpt-6.1-sol | failed | 8 | 19 | 16 | `49dbf975` |
| [20261014-182412-40ef8373](terminal/20261014-182412-40ef8373/run-report.json) | codex/gpt-6.1-sol | failed | 11 | 26 | 43 | `49dbf975` |
| [20261014-184011-290bc55c](terminal/20261014-184011-290bc55c/run-report.json) | codex/gpt-6.1-sol | failed | 14 | 30 | 42 | `78a7f483` |
| [20261014-190417-930a46d9](terminal/20261014-190417-930a46d9/run-report.json) | codex/gpt-6.1-sol | failed | 19 | 35 | 53 | `ec267df9` |
| [20261014-191314-3ac94c9e](terminal/20261014-191314-3ac94c9e/run-report.json) | codex/gpt-6.1-sol | passed | 20 | 36 | 54 | `ec267df9` |
| [20261014-193912-aafaafae](terminal/20261014-193912-aafaafae/run-report.json) | codex/gpt-6.1-sol | passed | 20 | 36 | 55 | `bdb76efb` |
