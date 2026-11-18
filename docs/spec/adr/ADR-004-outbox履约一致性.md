# ADR-004 履约一致性：轻量事务性发件箱（outbox）

- 状态：已接受（对应 PRD 架构原则⑤）
- 日期：2026-11-18（v0.3 审核裁定 1）

## 背景

支付成功（market 库事务）与 billing 入账（ai 库事务）跨库跨服务，无法原子。备选：A 同步调用 Credit + 失败重试 + 对账兜底；B 事务性发件箱；C 全链路事件驱动。

## 决策

轻量 outbox：支付回调置 paid 的**同一 MySQL 事务**内写 `fulfillment_tasks(pending)`；后台 worker 拉取（`idx_status_next`）执行行履约（Credit/ActivateSubscription/发货），指数退避重试上限 5 次，失败标记交对账；全行 done 才置 fulfilled。退款时先作废 pending 行（status=voided），与 worker 竞态由幂等冲退兜底。

## 理由

- 崩溃安全有实据：paid 落库即意味着履约任务必存在，worker 重启续跑；
- Credit 自身幂等（billing uk_ref），重试安全，无需分布式事务；
- 相比 C 不引入全链路事件复杂度；相比 A "失败不丢任务"不依赖对账先行；
- outbox 是面试高频话题，有真实实现支撑。

## 后果

- 多一张表与一个 worker 进程；订单可见状态短暂停留 paid（App 轮询展示"履约中"）；
- 行 failed 的订单停留 paid 由对账 job 人工处置（不自动重试超过 5 次）。
