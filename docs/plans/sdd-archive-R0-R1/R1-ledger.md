# SDD ledger — plan: docs/plans/2026-11-18-R1-商城垂直切片.md
（连续执行于 R0 台账之后；R0 台账：.superpowers/sdd/2026-11-18-R0-ai-gateway迁移与骨架/progress.md）

## 环境事实（继承自 R0 台账，所有 R1 任务适用）
- 本机 Go 构建/测试须带 SDKROOT=MacOSX26.sdk（默认 SDK cgo 链接失败，存量问题）
- ai 栈运行于 deploy/compose（infra+ai profile，8 容器）；market 库将建在同一 MySQL 3307 实例
- billing 9101 纯 gRPC；/healthz 在 9103/9102/9105；gateway 18080 无 /healthz（GET / 探活）
- 禁止 import github.com/frostloom/ai-gateway/**（internal 隔离）

## Task progress

### R1 Task 1: complete (commit ab2bc30, review clean；minor 缓办 5 条：pipefail/引号值/密码 argv 可见/文案笔误/无独立包装 sh)

### 中期代码审计（2026-11-18，判定：地基可靠，0 Critical / 2 Important / 9 Minor）
- **Important-1 → 并入 T5 第 0 步**：MALL 码与 retryable 在 gRPC 边界丢失（toGRPCError 只发标准码+人话）——T5 须在 pkg/mallerr 增 ToGRPC/FromGRPC 载体（errdetails.ErrorInfo 或 metadata 携带 code+retryable），mall-goods 与 mall-order 统一接线，配单测（20001 vs 20006 可区分、5xxx 三态不坍缩） — 若错：bff 层"码表与 §1 一致"验收必挂。
- **Important-2 → 部分并入 T5**：order.proto 增补 MarkPaidReq.tenant_id（field 6，服务端租户重校验，sequences §6）；**不**加 PayOrder RPC（维持裁定：bff 持 Provider，order 只收终态）；kind 字段 R2 随 recharge 直购再加。
- Minor 打包进 T5：GoodsItem 补 category_id/limited（goods 填充）；goods 键集索引（db-schema.sql 补 (on_sale,created_at,id) 索引 + 对已建库出 ALTER 脚本随 T5 提交）；go mod tidy；mall-gen README 一行。
- minor (deferred→终审)：PRD 9201-9203 口径与 9110/9120 未登记（本条已随手修）、compose mysql healthcheck 密码入 argv 取舍注、schema-apply 半建无自愈、token_estimate 固定常数契约注记、E() retryable 只放宽不收窄（T5 知悉）。

### 文档第四轮复审（2026-11-18，修复已提交）
- 5 Major 全修（提交见 git log "round-4 review fixes"）：纯API残留句、PRD 6处App措辞、R0计划 app/harmony、PRD§7 加"以 DDL 为准"横幅、R6 管理端秒杀依赖补声明。
- 顺手修：§5.6 限定词、R1 计划 pay 测试名 200、db-schema 版本注、voucher 示例 21 字符、arch/api-contract App 措辞 3 处、acceptance R0 勾选回填（7/8，归档待用户）。
- Ruling: 计划缺 PayOrder rpc 不补——按 T6 设计 bff 自持 PaymentProvider（CreatePayment）+ 仅调 order.MarkPaid，order 服务不参与支付编排 — 若错：T5/T6 实现时如需 order 侧支付入口再加 rpc（非破坏）。
- minor (deferred→终审): api-contract 秒杀枚举缺 cancelled 态、MALL_30006 空号注记、R1 种子"六品类"措辞、payment_records 无 UNIQUE(order_no,provider_trade_no)（T5 须以条件更新影响行数为幂等门）、arch §9 DEV 标签未含 R6、D5 决策名历史注记。

### R1 Task 6: complete (commits c43b7a3+f5f4048, fix round 1/5 收敛：3 findings 全 ADDRESSED 零新破坏；minor 缓办 4 条 + 2 实现裁定 accept)

### R1 Task 7: complete (commits 2ef6e0f+37fa17b, review clean 附 2 Important follow-up：①重放状态码 201 vs 契约§0.4 的 200（契约内部自相矛盾，裁定=修文档改 201 与实现/冒烟一致）②根缺 .dockerignore（257MB 上下文×3 构建）——均入终审修复波；minor 缓办 5 条)

## R1 里程碑完成：7/7 任务全部 review 收敛，SMOKE R1 PASS，11/11 容器健康

## 整分支终审（2026-11-18，opus，535KB 分七遍）
- 判定：**Needs one fix wave → Ready to merge as R0+R1**。0 Critical。
- 新发现：I-1 金额不匹配分支未实现（sequences 明文三分支缺失，裁定必修入波）；M-1 cart goods 四字段保真（入波）；M-2 契约 §3 缺两字段（入波）；M-3 cartLines 无超时（入波）；M-4 acceptance R1 未回填（入波）。
- 正面：错误码链路端到端、四层对齐、修复波零回归、SDD 纪律（仅 M-2 一处实现先于文档）、安全终检零发现。
- triage：28 条缓办——6 条入波、2 条公开前门槛（git 身份/PDF）、3 条 R3 前（SKIP LOCKED、reflection 关闭等）、其余 R2/R4 顺手或可延后（全表见终审报告）。

## 终审修复波（进行中）：六项一次性修复（I-1/I-2/I-3/M-1/M-2+M-3/M-4）
- 审查判定 Approved 但两 Important 便宜且真实，主动修复收敛。超范围变更裁定：proto signature_verified=7 增补=合理必要；五 concern（callback 限流/多副本缓存 30s/不清车/XFF/粗映射）与两裁定（10001/10002 拆分、pay_params=null）均 accept。
- minor (deferred): 上游体解码失败忽略+id 非零校验缺失、空车结算码语义 R2 议、go.mod indirect 归类、报告测试计数误差。
- 实现者报告 DONE（commit c43b7a3，19 文件；14/14 测试 + 连真实服务手测全过；logout 缓存逐出为手测发现的自修复）。超 brief 变更待裁定：proto MarkPaidReq.signature_verified=7 增补；五 concern 两裁定待裁定。

### R1 Task 5: complete (commits ec0efc3..c6eeb5f 共 9 笔, fix round 1/5 收敛：5 findings 全 ADDRESSED 零新破坏；minor 缓办 3 条：测试钟口径、INSERT IGNORE 吞错类、多副本 SKIP LOCKED)
- Task 5: fix round 1/5 (5 addressed, 0 open — 退避 off-by-one/promote 窗口/cancel 竞态码/验签如实/注入接缝+两钉死测试; commits d598c67..c6eeb5f)

### R1 Task 6（进行中）
- 审查裁定 concern：①TZ 同源配对实证自洽（可接受，残余脆弱性移交）；②不能简单接受（无测试分支正是 bug 所在，随本轮注入测试闭环）；③paid=false 实现正确可接受（bff 接线时补测）。
- minor (deferred): 测试钟口径小疵（worker_test NOW(3) 写 Go 族列）、INSERT IGNORE 吞错类、多副本无 SKIP LOCKED（ship 幂等时安全，R3 多副本前评估）。
- 审查正面结论：增补包 A-D 全绿（含独立查库实证）、幂等矩阵逐分支推演、回补/退款竞态面扎实。
- 实现者报告 DONE（commits ec0efc3..d598c67 共 8 笔：增补包 4 + 支付域主体 + 自审修复 3 + gofmt；8 命名测试全 PASS；grpcurl 实证 ErrorInfo 载体与 worker 自动履约）。
- 实现者 concern 待审查裁定：①MySQL UTC vs Go 墙钟差 8h 同源配对规避 ②ship 退避分支无专测 ③paid=false 回调无专测。

### R1 Task 4: complete (commits 8813031+1ac101f, fix round 1/5 收敛：1 finding ADDRESSED 零新破坏；minor 缓办 4 条)
- Task 4: fix round 1/5 (1 addressed, 0 open — 秒杀不可加购校验+测试+耦合注释; commits 8813031..1ac101f)

### R1 Task 5（进行中）
- minor (deferred): 归并后 qty 无上界（防御未来调用方）、clamp/toProto/tokenEstimate 与 goods 重复待收敛 pkg、AddCart 校验在事务外（下游覆盖）、并发测试无 barrier（断言集对任意交错成立）。
- 审查正面结论：事务回滚完整、RR 条件更新无丢失更新/死锁（固定加锁序）、幂等读回闭环（含并发首建竞态）、cleanup 幂等且失败路径执行——质量高于一般垂直切片水准。
- 实现者报告 DONE（commit 8813031；9/9 测试、三项 Review Focus 实证、grpcurl 冒烟、cleanup 复位验证）。
- 实现者自报五条边界语义待审查裁定：cart 20 行 TOCTOU、订单行 qty 越界用 MALL_10005、商品名无快照列、PATCH 不存在行 MALL_10005 防探测、支付三 RPC Unimplemented（T5 范围）。

### R1 Task 3: complete (commits fdb5ef3+a002300, review clean；minor 缓办 5 条：下架品类仍可翻页口径、DSN loc 运维备忘、测试硬种子锚点+无DB时 Fatalf、行值比较索引下推、health 端口退出顺序)

### R1 Task 4（进行中）
- 实现者报告 DONE（commits fdb5ef3+a002300；mallpage TDD + 12 项集成测试全绿 + grpcurl 冒烟；detail=13/token_estimate=14 接口债已清）。
- Ruling（范围，2026-11-18 用户裁定）：鸿蒙作业取消；v0.4 曾裁"纯 API"，用户纠正为**必须有前端（用户端+管理端）**——PRD v0.5/D18/R6 落档，独立 web/ 工程（Vue3 双入口）由 mall-bff go:embed 托管，不动 ai-gateway web/；admin 契约 R6 前 spec 增补冻结 — 若错：前端形态或托管方式不合用户预期，R6 前可改。
- minor (deferred): token_estimate 常数口径（1分≈1000token）接 billing 后校准；gRPC reflection 生产关闭。

### R1 Task 2: complete (commit de7bac6, review clean；go 1.26.0 偏离证据链成立；minor 缓办：Makefile 版本钉死、E() retryable 两个测试 case)
- Ruling（接口债，并入 T3）：GoodsItem 补 `detail=13` + `token_estimate=14` 两字段并重新生成——契约 §3 要求，T2 草图遗漏 — 若错：T6 BFF 序列化时再返工，代价一轮任务。

### R1 Task 3（进行中）
- 实现者报告 DONE（commit ab2bc30；两遍幂等 + 自检 7/7）。
- Ruling: 规格缺陷——docs/spec/db-schema.sql 声称幂等但表定义无 IF NOT EXISTS、品类 INSERT 无 IGNORE；实现者以 schema-apply.sh 的"9 张表存在则跳过"守卫解决且未改规格文件，接受 — 若错：终审可顺手把规格文件改幂等（deferred minor）。
- Ruling: recharge/subscription 商品 category_id=0（不进浏览品类），接受，SQL 注释已说明。
- minor (deferred): seed 无独立 .sh 包装（头注释说明应用命令）；规格文件幂等化留给终审。
