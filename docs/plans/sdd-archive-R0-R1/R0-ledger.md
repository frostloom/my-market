# SDD ledger — plan: docs/plans/2026-11-18-R0-ai-gateway迁移与骨架.md（连带执行 R1 计划 docs/plans/2026-11-18-R1-商城垂直切片.md）

## Process rulings（执行前裁定）

- Ruling: 仓库由控制器以 `git init -b feat/r0-r1-foundation` 预先初始化（避免在 main 上直接实施）；R0-T1 Step 4 不再 init，直接 add+commit — 原因：技能禁止在 main/master 上实施 — 若错：代价是首个提交落错分支，可 branch -m 修复。
- Ruling: R0 与 R1 两份计划在同一分支连续执行、共用本台账（R1 任务行追加于此）；整分支终审一次覆盖两计划 — 原因：R1 直接依赖 R0 产物，分支级审查天然覆盖 — 若错：R0 缺独立终审，由 R1 终审+R0 冒烟补位。
- Ruling: 无既有 git 仓库可建 worktree，整个执行直接在 my-market 工作区进行（分支已隔离）— 若错：单工作区无并行隔离，本流程本就串行，无实际损失。

## Pre-flight conflict scan（预检表）

| 交互对 | 一方产出 / 另一方消费 | 结果 |
|---|---|---|
| R0-T2 go.work ↔ R0-T3 compose | T3 不触 T2 产物 | 一致 |
| R0-T3 compose ↔ R1-T7 追加 mall profile | T7 只追加服务组，端口 18090/9111/9112 与 PRD §3 一致 | 一致 |
| R0-T1 排除 .env ↔ R0-T3 需要环境变量 | T3 由 .env.example 模板化，值本地生成 | 一致 |
| R1-T2 proto/mallerr ↔ R1-T3/4/5/6 | 消费方按 T2 Interfaces；已修 Callback+Provider、cursor 包归属 | 一致 |
| R1-T4 GetOrder(tenant) ↔ R1-T6 越权隔离 | 签名一致（tenant 注入） | 一致 |
| R1-T5 MarkPaidTx(Callback) ↔ R1-T2 MarkPaidReq | Callback 含 Paid 字段而 MarkPaidReq 未列 — 见下行裁定 | 需裁定 |
| R1-T5 worker ↔ R1-T4 订单状态机 | 状态枚举同源 db-schema（tinyint 1-5） | 一致 |
| R1-T7 smoke ↔ api-contract 端点表 | 端点路径逐一对应 | 一致 |

- Ruling: MarkPaid gRPC 的请求需携带 `bool paid`（失败回调置 payment_records=failed、订单停留 created，对应 sequences.md §1 金额不匹配分支）——T2 生成 proto 时补该字段，T5/T6 按此实现 — 原因：计划文本笔误，spec（sequences.md）为权威 — 若错：失败回调无法表达，退化为仅成功回调。

## Task progress

### Task 4: complete (commit 288adf9, review clean；minor 缓办 6 条——失败分支 unbound 变量、redis SET 未检返回、死变量 hc、VALUES() 废弃语法、TID 空退化、acceptance.md 未落勾)

## R0 里程碑完成：4/4 任务 review clean，冒烟 PASS，acceptance 7/8（第 8 项旧仓归档=待用户操作）
- 实现者报告 DONE（commit 288adf9；smoke 两连跑 PASS；acceptance 7/8，第 8 项旧仓归档=待用户操作）。
- Ruling（环境事实，带入 R1）: 旧仓原生进程（PID 37786/37850-37858）仍占 127.0.0.1 回环端口（僵尸栈，其 DB 容器已停）；smoke 走 localhost(::1) 规避 — 用户手动清理后可恢复正常双栈 — 若错：无功能影响，仅环境噪音。
- Ruling（环境事实）: `CGO_ENABLED=0` 全仓构建零错误（绕开 SDKROOT 问题）——R1 服务构建默认带此参数 — 若错：无。
- minor (deferred→交付时告知): ①旧仓僵尸进程建议用户手动 kill；②旧仓三容器已 stop 未删；③git 身份待配置；④课程 PDF 公开前需移出。

### Task 3: complete (commits 7f6e83e..02b7681, fix round 1/5 已收敛：3 findings 全 ADDRESSED 零新破坏；minor 缓办 3 条)
- Task 3: fix round 1/5 (3 addressed, 0 open — 沉淀注释⑥⑦+KAFKA_BROKERS 误导注释; commits 7f6e83e..02b7681)

### Task 4（进行中）
- minor (deferred): 容器全 root 运行（无 USER 指令）；mysql 健康检查走 unix socket 可能提前放行；infra 三服务无 restart 策略与 ai 侧不一致——均记终审triage。
- 实现者报告 DONE（commit 7f6e83e，4 文件：compose + .env.example + 共享 Dockerfile + .dockerignore）。
- Ruling: 健康检查契约修正——gateway 无 /healthz（用 nc 18080 + GET / 200），billing 9101 纯 gRPC（用 nc），/healthz 实际在 9103/9102/9105 — 若错：T4 冒烟脚本沿用错误断言会假失败。此修正同步 acceptance.md R0 口径。
- Ruling（环境事实，带入 T4）: ⑥ DB 内 provider 地址指向宿主回环，容器网络内 router→mock-provider 不通；T4 冒烟前需把 providers 地址改为 compose 服务名（如 http://mock-a:9201）——经 admin API 或 SQL，属数据修正非代码 — 若错：/v1 冒烟会 502。
- 其余发现（LISTEN_HOST=0.0.0.0、Kafka LOG_DIRS 卷、无 syntax 行、depends_on required:false、agent 空 key 可启、旧容器已停未删）接受，已要求写入 compose 注释。
- minor (deferred): 旧仓遗留容器被 docker stop（未删，可 docker start 恢复）——交付时告知用户。

### Task 2: complete (commit af73168, review clean；⚠️×2 控制器采信：构建/测试证据自洽 + ai-gateway 零改动)

### Task 3（进行中）
- 实现者报告 DONE_WITH_CONCERNS（commit af73168，8 文件）。
- Ruling: go.work 的 go 指令为 `1.25.12` 而非计划文本的 `1.25`——Go 工作区硬约束（≥ 模块最高指令），接受 — 若错：无，这是工具链强制的唯一可行值。
- Ruling（环境事实，贯穿后续所有任务）: 本机默认 SDK（MacOSX27.0.sdk）与 clang 不匹配导致 cgo 链接失败（存量问题），**所有 Go 构建/测试须带 `SDKROOT=MacOSX26.sdk`** — 若错：后续构建莫名失败的排查成本。
- Ruling: internal/taskbench 71 个测试失败为存量（与旧仓差集为空），不属 R0 范围，接受 — 若错：终审或 R2 若依赖 taskbench 再处理。

### Task 1: complete (commit ae24b28, review clean；minor 缓办 3 条见上)
- 实现者报告 DONE_WITH_CONCERNS（commit ae24b28，17088 文件）。
- 控制器核验：无任何非 example 的 .env 入库（新旧仓一致）；旧仓 langfuse 密钥本就未跟踪。
- Ruling: web/dist 实际路径为 ai-gateway/internal/gateway/web/dist（brief 笔误，README 佐证）——后续任务沿用此路径 — 若错：embed 断言失败会在 R0-T2 构建暴露。
- Ruling: eval 与旧仓仅差 1 个文件（paired-pilots/.../rejected-check.log 被新 .gitignore 的 *.log 排除）——接受，评测如需该文件再 git add -f — 若错：一次 force-add 可补。
- Ruling: 首提交 diff 2.2M 行不可内联 → 清单式审查（package 含分布与只读比对命令）— 若错：审查覆盖面降为清单级，终审再兜底。
- minor (deferred): git 提交者身份为本机自动推导（1 <a1@...local>），公开前需用户配置 user.name/email；课程 PDF（华为版权材料）已入暂存历史，**公开仓库前必须移除或转私有**——已列入交付时告知项。
