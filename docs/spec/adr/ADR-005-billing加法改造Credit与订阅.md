# ADR-005 billing 加法改造：Credit、Refund 与订阅两 RPC（新增 + 开门）

- 状态：已接受（对应 PRD D13；R2 T1 修订：两处改三处 + Refund 语义；R2.5 T1 修订：三处改五处 + 订阅两 RPC 语义）
- 日期：2026-11-18（依据代码核查：旧端点全部 403）；2026-11-19 修订；2026-11-19 R2.5 修订

## 背景

代码核查证实：充值/订阅的对外端点全部刻意 403（"真实支付接入后开放"），当前唯一加钱入口是兑换码。且 store 层 SubscribePlan 不收月费、无幂等 ref，`/admin/cancel-subscription` 是唯一开放的订阅端点（agent 客服停续费用）。商城入账与程序化开通需要入口。

## 决策

- 新增 gRPC `Credit(tenant_id, amount_cent, ref_type="mall", ref_id=order_no:line_no)`：billing 侧复用 topups 现成同步事务路径（topup 行 + ledger_balance 同事务；复合唯一键 uk_ref(ref_type,ref_id) 幂等，新 ref_type 与旧充值不互斥）；
- 新增 gRPC `Refund(tenant_id, amount_cent, ref)`（R2 T1 补充）：实际退额 = min(amount_cent, 当前余额)，绝不退负（与既有 RefundAmount 同口径）；ref 是幂等锚点——复用既有退款幂等机制（topups ref_type="refund"、ref_id=ref、requested_cent 记原始请求），零 schema 改动；同 ref 重放 no-op（refunded_cent=0），同 ref 不同金额/租户 → FailedPrecondition；
- 新增 gRPC `ActivateSubscription(tenant_id, plan_id, ref)`（R2.5）：模拟月费校验（不接真钱，只校验套餐在售且为定期订阅）+ 订阅记录。语义：无本期权益 → 开通本期；已有本期权益（active/退订未到期）→ **自本期期满顺延一个周期**（RenewDueSubscriptions 单租户等价：newStart=旧 cycle_end 无缝、cycle_num+1，显式续购复活 auto_renew）；已过期未续 → 从 now 起新一期；跨套餐续购 **fail-closed**（换套餐走既有 ChangePlan「下期生效」口径，不在购买路径静默变更）；
- 幂等锚（R2.5 T1 裁定，零 schema 改动）：订阅行无 ref 列，激活锚复用 topups uk_ref——与订阅变更**同事务**写一条 **0 元流水**（source=subscription, ref_type="mall-sub", ref_id=ref）作提交点；0 元行不进余额公式（Σtopups 不变），兼作程序化开通审计流水；同 ref 重放 no-op 返回当前订阅摘要（重放不校验载荷，与 Credit 同口径），跨租户 ref → FailedPrecondition；
- 新增 gRPC `CancelSubscription(tenant_id, ref)`（R2.5）：与既有 store.CancelSubscription/`/admin/cancel-subscription` 并存不改其行为；置 auto_renew=0、当期权益保留至期满、不退费（D15）；幂等来自状态定点收敛（已退订重放 no-op），ref 仅作审计关联锚（退订不动钱，不落账本行）；无订阅 → NotFound；
- **不打开旧 403 端点**（`/admin/subscribe`、`/admin/change-plan`、`/portal/*` 保持 403，console_policy/portal_closed 测试锚定）。billing 加法改造至此共**五处**：① proto 由 `internal/proto` 移至 `pkg/proto/billing/v1` 供 mall 共享（R2 T1，纯搬移零语义变化）；② `Credit`；③ `Refund`；④ `ActivateSubscription`；⑤ `CancelSubscription`。④⑤ 之外，Reserve/Settle/Reverse 与充值路径语义零改动。

## 理由

- 旧端点的关闭是有意设计（等真实支付），打开会引入与商城并行的第二售卖路径；
- 新 RPC 以 ref 幂等为第一原则，与 topups 账本天然对齐；Credit/Refund 直接收发分（amount_cent），不再过 RefundAmount 的「元」层换算；
- Credit 同 ref 重放统一 no-op（不校验金额一致），适配 worker 至少一次投递的重试语义；Refund 保留 RefundAmount 的金额一致性校验（管理员退款要防误操作，商城冲退 ref 独立于 Credit ref）；ActivateSubscription 重放同理不校验 plan（同 ref 重放是 worker 重试，不是新意图）；
- 订阅幂等锚选 0 元 topups 流水而非 subscriptions 加 ref 列：避免 schema 改动与迁移；uk_topup_ref 已是既定提交点模式；续期判定（active/cancelled × 到期 × 同套餐）在每租户现金锁内串行，与充值/退款/退订天然互斥；
- 顺延起点取旧 cycle_end 而非 now：当期权益无缝（用户不为续期损失剩余天数），周期边界与 cycle 档位配额窗口对齐（SubscriptionMoneyAllowance 以 cycle_start 为周期起点）；
- "新增 + 开门"的边界清晰，计费内核的风险面最小。

## 后果

- R2/R2.5 各含一段 billing 侧开发与单测（含在 ai-gateway 目录内，R2.6 起为 services/ai-gateway/）；
- gateway billing.proto 需扩展（加法）；api-reference 同步更新；
- proto 由此移至 `ai-gateway/pkg/proto/billing/v1`（R2 T1；R2.6 起目录为 `services/ai-gateway/pkg/proto/billing/v1`，模块路径不变），go.work 下 mall 各服务可直接 import `billingv1` 契约；
- 激活锚的 0 元流水会出现在租户入账流水列表（展示为 0 元「subscription」行）——这是有意的审计面；金额口径（余额/对账）不受影响。
