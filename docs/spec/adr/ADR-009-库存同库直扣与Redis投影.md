# ADR-009 库存：同库直扣与 Redis 投影

- 状态：已接受（对应 PRD D17；复审 N3 裁定）
- 日期：2026-11-18

## 背景

mall-order 下单需扣 general 库存。mall-goods 提供了 DeductStock gRPC，但跨服务 gRPC 无法纳入订单本地事务；goods 表与 mall_orders 同在 market 库。

## 决策

- 下单/回补在 mall-order 的**本地事务内直接条件更新** `goods.stock`（`UPDATE goods SET stock=stock-? WHERE id=? AND stock>=?`，影响行数=0 即库存不足回滚）；
- Redis 计数仅作前置校验与列表展示投影，不作为扣减依据；与 MySQL 不一致时以 MySQL 为准回补对齐；
- `DeductStock`/`ReturnStock` gRPC 保留，仅供跨服务调用方（秒杀预热、管理端回补）。

## 理由

- 同库同事务是唯一零成本的原子方案；gRPC 两阶段或补偿引入复杂度无收益；
- "Redis 投影 + MySQL 权威"与 billing topups 的"Redis 投影 + MySQL 权威"原则一致。

## 后果

- mall-order 对 goods 表存在同库直写（跨域写，需在代码注释与 ADR 声明此约定）；
- 秒杀不使用 goods.stock（活动库存独立），二者不冲突。
