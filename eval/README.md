# eval/ — 统一评测资产（顶层）

> R2.6-T2 起，评测资产从 `services/ai-gateway/eval` 提为仓库顶层 `eval/`，按评测域分目录。
> 本文件是资产的**存放规约**（放什么、怎么放、工具怎么定位）；各域内部的口径与运行手册见各域自己的 README。

## 目录按域划分

| 目录 | 域 | 状态 |
|---|---|---|
| `eval/ai-gateway/` | AI 网关存量评测（R2.6 前全部资产随迁） | 在用 |
| `eval/loadtest/` | 压测数据与报告（R4 秒杀+压测起归此） | 预留，R4 先建目录再产出 |
| `eval/e2e/` | 前端端到端评测（R6 双前端起归此） | 预留，R6 先建目录再产出 |

**新评测域先建目录再产出**：不要把新域的数据散放进 `eval/` 根或别的域目录；每个域一个一级子目录，域内自带 README 说明口径。

### eval/ai-gateway/ 存量内容（随迁自 services/ai-gateway/eval）

- **数据集**：`cases/`（客服 35 用例 × 6 维度）、`intent-suite/`（意图路由 1220 条 + 对抗子集）、`task-definitions/`（2026-10-12-full-dev-v1 冻结任务定义）、`runtime-profiles/`（2026-10-13 六档运行画像）、`task-suites/`、`contracts/`、`baselines/`
- **运行产物**：`reports/`（可再生，已 gitignore）、`campaigns/`、`paired-pilots/`、`reviews/`、`behavior-review/`、`experiments/`、`annotations/` 等（历史批次证据，只追加不回改）
- **langfuse 对接数据**：`langfuse-workbench/`（本地评测工作台数据）；langfuse **部署**（compose、.env 密钥）在 `deploy/langfuse/`（R2.7 起自服务树提为根 deploy/），二者分开——部署配置不进 eval/，评测数据不进 deploy/

## 资产分类与保留策略

- **数据集**（长期版本化）：用例、冻结定义、画像、契约、基线。改动走正常提交评审；冻结类目录（如 `task-definitions/2026-10-12-full-dev-v1`）一经评审冻结**只读**，修订版本新开日期目录。
- **运行产物**（可再生）：reports/、runs、本地评分缓存等。默认 gitignore（见根 `.gitignore` 的 `eval/ai-gateway/` 段）；确需留档的证据以压缩包+SHA 清单形式入库（沿用 behavior-review 的 byte-identical gzip 做法）。大文件（单文件 >10MB 的原始抓取物）不入库，归档策略在域 README 注明。

## 工具如何定位数据（env 优先于相对路径）

工具定位 eval 资产的优先级约定：**显式参数（flag/env）> 相对路径默认**。

- 顶层 Go 工具（R2.7 起位于 `tools/`：`tools/paired-eval`、`tools/conversation-eval`、`tools/paired-live`、`tools/full-customer-*`、`tools/full-window-*`、`tools/task-eval`、`tools/local-paired-eval` 等）：默认路径**相对仓库根为 cwd**（`cd <仓根> && go run ./tools/<name>`），统一为 `eval/ai-gateway/...`（R2.7-T2 起，原 `../../` 前缀随树迁缩短一层）。显式 flag（如 `-definitions`）可覆盖。
- 服务树内工具（`services/agent/tools/eval`）：以**服务树为 cwd**（`cd services/agent && go run ./tools/eval`），默认仍为 `../../eval/ai-gateway/...`——服务目录深度与旧布局一致，层数零改。
- Go 测试（包目录为 cwd）：`tools/<name>` 下为 `../../eval/ai-gateway/...`（距仓库根 2 层）；`tools/internal/*` 下为 `../../../eval/ai-gateway/...`（距仓库根 3 层）。
- git pathspec 类消费者（`tools/paired-live/guards.go` 的冻结输入审查）：以**仓库根为基准**写作 `eval/ai-gateway/task-definitions` 等。
- Node 工具（`services/gateway/web/scripts/gen-intent-suite.mjs` / `verify-intent-suite.mjs`）：以脚本自身位置推导，`resolve(here, '..', '..', '..', '..', 'eval', 'ai-gateway', ...)`（web/scripts 距仓库根 4 层，R2.7 树迁后深度不变）。
- 新工具若需要 cwd 无关的定位，约定用 **env 覆盖**（如 `EVAL_DIR` 指向 `eval/ai-gateway`），缺省回退上述相对路径；不要发明第三种默认值。
- Python/PowerShell 校验器（R2.7 起在 `tools/verify/`）：脚本位于仓库根下 2 层目录（与旧 `services/ai-gateway/scripts/` 同深，约束保持），`ROOT = parents[1]` 推导出树根（新位 `tools/`，旧位 `services/ai-gateway/`），`EVAL = ROOT.parent / eval / ai-gateway` 指向顶层资产；仅服务于**历史冻结证据**的工具（`tools/verify/tests/test_historical_evidence_checkout.py`、`tools/verify/helpers/*.go.txt`、window v6/v10 分类器与 `tools/internal/taskbench/window_actual_ready.go` 的冻结清单契约）保留旧布局路径原文——它们校验的是历史仓快照，不定位活树。

## 历史口径提示

`eval/ai-gateway/README.md` 与其下各批次 README 记录了各数据集的口径、运行命令与费用/授权注意事项；文中 `eval/...` 路径均为该资产目录内的相对引用。3660 对实验与 1220 条采集等历史批次**不能混算**，详见 assets 内指向的评测指南。
