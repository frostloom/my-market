# 核心时序（spec/sequences.md）

> 依据 PRD v0.5 + architecture.md 八图；本文件补**错误分支**，为实现与验收的依据。图为 Mermaid，可渲染。
> 调用方说明（2026-11-18）：鸿蒙作业已取消，图中 App/调用方 = **商城 Web 前端**（用户端 mall-user；管理端走 /admin/*，R6 契约增补）——见 PRD §5.4/D18。

---

## 1. 充值直购闭环（含错误分支）

```mermaid
sequenceDiagram
    autonumber
    participant A as 商城前端
    participant B as mall-bff
    participant O as mall-order
    participant P as PaymentProvider
    participant W as 履约 worker
    participant L as billing
    participant M as MySQL market

    A->>B: POST /orders（recharge，Idempotency-Key）
    alt 幂等键已存在
        O-->>B: 同 payload→返回原单 200；异 payload→MALL_30003
    else 正常
        O->>M: 事务：INSERT mall_orders(created)+行快照
    end
    A->>B: POST /orders/{no}/pay
    O->>P: CreatePayment（payment_records=created）
    P--)B: HandleCallback（验签·幂等）
    alt 验签失败
        B-->>P: MALL_30005，订单停留 created
    else 验签通过且金额匹配
        O->>M: 事务：payment_records=paid + status=paid<br/>+ INSERT fulfillment_tasks(pending)
    else 金额不匹配
        O->>M: payment_records=failed，订单停留 created
    end
    loop worker（指数退避，上限 5 次）
        W->>M: 拉取 pending 任务（next_run_at 到期）
        W->>L: Credit(ref_type=mall, ref_id=order_no:line)
        alt billing 超时/不可用
            W->>M: retry_count+1，next_run_at 后移；5 次后 status=failed 交对账
        else 成功或 ref 已存在（no-op）
            W->>M: 行 done；全行 done → order fulfilled
        end
    end
    A->>A: 轮询 GET /orders/{no} 至 fulfilled
```

**断电恢复推演**：paid 已落库但 outbox 行未执行 → worker 重启后按 `idx_status_next` 继续拉取；Credit 幂等（uk_ref）保证重试不重复入账；行 failed → 对账 job 标记人工处置。

---

## 2. 混合购物车订单（含超时/取消）

```mermaid
sequenceDiagram
    autonumber
    participant A as 商城前端
    participant B as mall-bff
    participant O as mall-order
    participant M as MySQL market

    A->>B: POST /orders（kind=cart，Idempotency-Key）
    O->>M: 事务：订单 created + 行快照<br/>+ 同库直扣 goods.stock（stock>=qty 条件更新）
    alt 库存不足（条件更新影响行数=0）
        O-->>B: MALL_20003，事务回滚
    end
    alt 用户支付
        A->>B: POST /orders/{no}/pay → 回调置 paid + outbox（同图1）
    else 超时 30 分钟未支付
        M->>M: 扫描 job：status=created AND created_at<now-30min<br/>→ closed(timeout) + 回补库存（条件更新 stock+qty）
    else 用户主动取消
        A->>B: POST /orders/{no}/cancel → closed(user) + 回补库存
    end
```

---

## 3. 退款（D15，含竞态）

```mermaid
sequenceDiagram
    autonumber
    participant A as 商城前端
    participant B as mall-bff
    participant O as mall-order
    participant L as billing
    participant M as MySQL market

    A->>B: POST /orders/{no}/refund
    alt status=paid（未入账）
        O->>M: 先作废 pending 履约行(status=voided)<br/>→ status=refunded（资金未动）
        note over O,M: 与 worker 竞态：若 worker 恰在作废前完成 Credit<br/>→ 订单可能已 fulfilled，本次退款按下一分支幂等冲退
    else status=fulfilled
        O->>M: 逐行处置
        O->>L: recharge 行冲退 min(行额度,当前余额)（幂等 ref）
        L-->>O: ok / ref 已存在 no-op
        O->>M: general 行退回+回补库存<br/>subscription 行不退费=退订语义（当期保留）<br/>→ status=refunded
    else 其他状态
        O-->>B: MALL_30002
    end
```

---

## 4. 月卡开通（R2.5）

```mermaid
sequenceDiagram
    autonumber
    participant W as 履约 worker
    participant L as billing
    participant MA as MySQL ai 库

    W->>L: ActivateSubscription(tenant, plan_id, ref=order_no:line)
    alt ref 已存在
        L-->>W: no-op（返回已生效订阅）
    else 正常
        L->>MA: 事务：模拟月费校验（不接真钱）<br/>+ 订阅记录（本期窗口起止）
        L-->>W: 生效 → 行 done
    else 租户已有未到期订阅
        L-->>W: 按「续一期」处理（手动续订=立即续一期）
    end
    note over L,MA: 额度消费语义（既有）：每 5h 滚动窗口<br/>额度内 funding=subscription、pre=0；超额 402
```

---

## 5. 秒杀（含全部失败分支与对账）

```mermaid
sequenceDiagram
    autonumber
    participant A as 商城前端
    participant B as mall-bff
    participant S as seckill
    participant R as Redis
    participant K as Kafka
    participant C as 消费者
    participant M as MySQL market

    A->>B: POST /seckill/{id}/buy
    B->>B: 令牌桶限流（超限 MALL_40004）
    alt 活动未开始/已结束
        B-->>A: MALL_40001
    end
    B->>S: Buy(activity, tenant)
    S->>R: Lua：判重+判库存+DECR（原子）
    alt 已抢完
        S-->>A: MALL_40002（快速失败）
    else 重复抢购（Lua 判重 or 消费者撞唯一索引）
        S-->>A: MALL_40003
    else 成功
        S->>K: produce seckill.orders（voucher=预生成 order_no）
        S-->>A: 202 queued + voucher（produce 后同步返回）
    end
    K->>C: 消费（at-least-once）
    C->>M: 落库 seckill_orders(queued→pending_payment)<br/>+ mall_orders(created)（DB stock>0 兜底 + 唯一索引防重）
    alt 消费者撞 UNIQUE(activity,tenant)（极小概率重复投递）
        C->>C: 幂等跳过
    end
    A->>A: 轮询 GET /seckill/orders/{voucher}
    alt 15 分钟内支付
        A->>B: 正常 /orders/{no}/pay → 回调 + outbox 履约
    else 超时未支付
        M->>M: 扫描：status=pending_payment AND created_at<now-15min<br/>→ timeout + 回补 Redis 库存（INCR，幂等：仅 status 成功转 timeout 时执行）
    end
    note over M: 对账 job（周期）：Redis 余量 vs sold_stock+在途订单<br/>不一致→记录差异+滞留默认保留待核查（D16）
```

---

## 6. 会话与越权防护

```mermaid
sequenceDiagram
    autonumber
    participant A as 商城前端
    participant B as mall-bff
    participant G as gateway :18080
    participant L as billing :9103

    A->>B: 任意请求 + Authorization: Bearer 会话token
    B->>G: GET /auth/me（cookie 透传，gateway 注入 X-Control-Token）
    alt 会话无效/过期
        G-->>B: 401 → B 返回 MALL_10001/10002
    else 有效
        G-->>B: tenant_id → B 以 tenant 维度执行业务
    end
    note over B,L: 所有按租户作用域的查询/写入强制 tenant 注入<br/>（参数服务端重校验，客户端传 tenant 一律忽略）
```

---

## 7. 错误→HTTP 映射（实现备忘）

| gRPC/内部错误 | HTTP | code |
|---|---|---|
| 会话 401（gateway 转发） | 401 | MALL_10001/10002 |
| billing Unavailable/Deadline | 503/504 | MALL_50003/50002（retryable） |
| goods 记录不存在 | 404 | MALL_20001 |
| 条件更新影响行数=0 | 409 | MALL_20003 / MALL_30002（按场景） |
| 唯一索引冲突（idem/秒杀防重） | 409 | MALL_30003 / MALL_40003 |
| 参数校验失败 | 400 | MALL_10005 |
