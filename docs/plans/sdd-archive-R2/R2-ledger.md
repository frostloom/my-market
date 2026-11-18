# SDD ledger — plan: docs/plans/2026-11-18-R2-充值闭环.md

分支 feat/r2-recharge（自 main@9d5370e 切出）。环境事实继承 R0/R1：CGO_ENABLED=0、compose 全家 11 容器在跑、billing gRPC 控制面要求 X-Control-Token、Kafka 宿主 29092、gateway 18080。

## 预检扫描

| 交互对 | 产出/消费 | 结果 |
|---|---|---|
| T1 proto 移动 ↔ T2/T3 billing_client | mall 唯一 import 面=ai-gateway/pkg/proto/billing/v1 | 一致 |
| T1 Credit/Refund ↔ api-contract §5/§7 | ref 幂等/min 口径；ADR-005 修订三处 | 一致 |
| T2 kind=recharge ↔ api-contract §5 {"kind":"recharge","goods_id"} | bff 分派→新 RPC | 一致 |
| T3 worker credit ↔ T1 CreditRequest | ref_type=mall/ref_id=order_no:line；CreditReply 字段实现者定（建议 balance_cent） | 需 T1 报告确认 |
| T4 compose 注入 ↔ ai profile 服务名 | BILLING_ADDR=billing:9101 + CONTROL_PLANE_TOKEN | 一致（billing 服务名核实过） |
| T1 改动面 ↔ D1"ai-gateway 既有代码零改动"原则 | proto 移动=机械 import 路径替换；Credit/Refund=纯新增；ADR-005 已修订授权 | 一致（D13 系既有授权） |

- Ruling: 在 feat/r2-recharge 分支执行（延续"不在 main 实施"），完成后经审查再并回 — 若错：一次 merge 可解。
- Ruling: T1 中 ai-gateway 的 proto 移动属机械重构（git mv + import 替换），允许触碰 ai-gateway 大量文件的 import 行，但语义改动仅限新增两 RPC — 若错：ai-gateway 全量测试门禁兜底。

## Task progress

### R2 Task 3（进行中：审查阶段）
- 实现者报告 DONE（commit 9bfec73；5 新测试绿含真 billing 断电重放/退款三态；spawn billing 子进程绕 import 边界做集成；80=80 门禁零回归）。
- 待裁定四 concern：窗口 B（billing 退成功 mall 翻转丢失→用户重试收敛无自动对账）、refunded 回放 refund_cent 口径、余额 0 退款透传、compose/smoke 留 T4。

### R2 Task 4: complete (commits f24e912+83e5681, review clean；minor 缓办 4 条：断电演练毫秒级 flake 竞态、401/403 归并 retryable、拨号样板 60 行重复待第三消费方收敛、topups 行形状契约未冻结)

## R2 里程碑完成：4/4 任务全部 review 收敛，SMOKE R2 PASS×2，验收 6/6，全仓测试绿

## R2 整体终审（2026-11-19，opus，386KB 分三遍）
- 判定：**Needs one docs-only fix wave → merge**。0 Critical；I-1=R1 窗口的镜像交错（Credit 已提交 done 不可达 × RefundPaid void）三层防护未覆盖、无补偿无日志——风险低（毫秒窗/无攻击原语/mock 支付）且 R4 对账设计恰好检出，但"全闭合"声明须修正入档。
- 六项指定面全部成立：billing 双向一致、资金不变量（除 I-1）闭合、容器/宿主同构（同源二进制+镜像 ID 前置校验）、import 边界分层（importguard+编译器+replace+Dockerfile 四层）、ai-gateway 零漂移（80=80 链）、SDD 纪律达标。
- triage：16 minor+7 局限全可带病合并；时限项——**topups wire 契约 R4 前必须冻结**、mapBillingError 永久拒绝 retryable R2.5 修、拨号样板第三消费方时收敛。

## R2 终审修复波（进行中，docs-only）：I-1 窗口分析补记 + M-1/T2-a/T3-b 注释修正
- 实现者报告 DONE（commits f24e912+83e5681；SMOKE R2 PASS 两遍；acceptance 6/6；全仓绿）。
- 待裁定三 concern：①容器化必要偏离（Dockerfile/go.mod/.dockerignore+billing 镜像重建）②billing Redis 投影 Kafka relay 异步滞后——smoke 以 ledger_balance 为权威 ③taskbench 单测新失败证明为 T1 前既有。

### R2 Task 3: complete (commits 9bfec73+da9b867+72c47ba, fix rounds 1+2 收敛：R1/R2 双窗口+守卫 TOCTOU 全闭合；minor 缓办 5 条；残窗已知局限=1.5 复查分支依锁语义推理、单线程不可直达构型，注释如实披露)

### R2 Task 4（进行中）
- 审查裁定四 concern：窗口 B 有条件接受（refund-vs-refund 面已闭合）；refunded 回放口径接受（R2+ 加列可修）；余额 0 退款透传接受（retryable 标注记 Minor）；compose 留 T4 接受。
- minor (deferred): mapBillingError 永久拒绝标 retryable、窗口 C 注释不精确、harness 清理顺序、stub 死辅助、注释函数名笔误。

### R2 Task 2: complete (commits 8d5f183+488edc7, review clean；minor 缓办 3 条：limited 翻转窗口注释软化、测试名过诺、cart 扣订阅行库存的契约措辞观察)

### R2 Task 3（进行中）
- 实现者报告 DONE（commits 8d5f183+488edc7；13 测试绿；ai-gateway 80=80 门禁；真 billing 幂等回放验证；自审修复 restockOrder 只回补实际扣减行）。
- 待裁定四 concern：restock 修复正确性（混单误判面）、宿主流浪 billing 进程遮蔽 9101（旧仓僵尸栈已备案）、compose 注入留 T4、bff ctx 收敛。

### R2 Task 1: complete (commits fae5f5b+2ccbf43+6317b8b, fix round 1/5 收敛：1 finding ADDRESSED 零新破坏；minor 缓办 4 条)
- Ruling（测试基建警示，带入后续任务）：ai_gateway_test 共享库会被 store 套件 setup 清库——**基础设施型测试套件禁止并发跑**（T1 修复轮曾因此作废一次门禁比对）。

### R2 Task 2（进行中）
- minor (deferred): RED 证据行号不可复现（测试实现同提交）、ref_type 未保留 refund/recharge 关键字（契约观察）、负 tenant_id 包装成 NotFound、credit_test 用 DB 0 未隔离。
- 审查正面：真 git rename+import 零遗漏、Credit 守卫序列与 payRechargeCash 逐项一致、RefundCent 与 RefundAmount 语义等价零 schema 改动、Reserve/Settle/Reverse 零改动经 diff hunk 证明、新 RPC 被全局 UnaryServer 拦截器鉴权覆盖。
- 实现者报告 DONE（commits fae5f5b+2ccbf43；TDD 4 测试绿；ai-gateway 失败集 80=80 与基线逐字节一致；Reserve/Settle/Reverse 字节级零改动脚本审计；CreditReply{balance_cent,created} 契约已定供 T3）。
- Ruling: Credit 重放无条件 no-op（不校验金额）vs Refund 保留金额一致性校验——语义不对称经 ADR-005 记录取舍（worker at-least-once vs 管理员防误操作），接受 — 若错：极端情况下 Credit 同 ref 异额重放不可检，但 worker 是唯一调用方且 ref 由订单行派生，风险受控。
- Ruling: 补 ai-gateway/buf.gen.yaml（monorepo 迁移丢了 proto-gen 配置）——属部署件新增非代码改动，接受。
- 待审查裁定：并发同 ref 双请求读回路径、新 RPC 是否被控制面鉴权覆盖、零改动断言可信度。
