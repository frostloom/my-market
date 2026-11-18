# ADR-005 billing 加法改造：Credit 与 ActivateSubscription（新增 + 开门）

- 状态：已接受（对应 PRD D13）
- 日期：2026-11-18（依据代码核查：旧端点全部 403）

## 背景

代码核查证实：充值/订阅的对外端点全部刻意 403（"真实支付接入后开放"），当前唯一加钱入口是兑换码。且 store 层 SubscribePlan 不收月费、无幂等 ref。商城入账需要程序化入口。

## 决策

- 新增 gRPC `Credit(tenant_id, amount_cent, ref_type="mall", ref_id=order_no:line_no)`：billing 侧复用 topups 现成同步事务路径（topup 行 + ledger_balance 同事务；复合唯一键 uk_ref(ref_type,ref_id) 幂等，新 ref_type 与旧充值不互斥）；
- 新增 gRPC `ActivateSubscription(tenant_id, plan_id, ref)`：幂等 ref + 模拟月费校验（不接真钱）+ 订阅记录；已有未到期订阅=立即续一期；
- **不打开旧 403 端点**；这两个 RPC 是 billing 仅有的两处加法改造，Reserve/Settle/Reverse 语义零改动。

## 理由

- 旧端点的关闭是有意设计（等真实支付），打开会引入与商城并行的第二售卖路径；
- 新 RPC 以 ref 幂等为第一原则，与 topups 账本天然对齐；
- "新增 + 开门"的边界清晰，计费内核的风险面最小。

## 后果

- R2/R2.5 各含一段 billing 侧开发与单测（含在 ai-gateway 目录内）；
- gateway billing.proto 需扩展（加法）；api-reference 同步更新。
