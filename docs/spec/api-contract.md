# mall-bff API 契约（spec/api-contract.md）

> 依据 [PRD.md](../PRD.md) v0.5（D14/D15/D17/D18）。mall-bff 的对外 REST 契约；调用方为**商城 Web 双前端**（用户端 + 管理端，R6）；变更先改本文档。
> 基地址：`http://<host>:18090/api/v1`

---

## 0. 通用约定

### 0.1 鉴权

| 项 | 约定 |
|---|---|
| 凭证 | `Authorization: Bearer <会话token>`（登录获得；**不是** API key，API key 仅用于 gateway /v1） |
| 验证 | mall-bff 经 gateway `:18080 /auth/*` 代理验证（透传为 agw_user cookie），派生 tenant_id |
| 无需鉴权 | `POST /auth/register`、`POST /auth/login`、`POST /payments/callback`（回调走验签） |
| 会话有效期 | 12 小时（billing 既有）；过期返回 MALL_10002 |

### 0.2 错误模型

所有非 2xx 响应体：

```json
{ "error": "人话描述", "code": "MALL_30003", "retryable": false }
```

### 0.3 分页（游标）

- 键集分页：排序键 `(created_at DESC, id DESC)`；
- `cursor` = Base64(`created_at,id`) 不透明令牌，首页不传；
- `page_size` 默认 20、上限 50；
- 响应：`{"items":[...], "next_cursor":"..." | null, "has_more":bool}`；`next_cursor=null` 即到底（App 显示"已经到底了"）。

### 0.4 幂等

| 操作 | 机制 |
|---|---|
| 下单 | `Idempotency-Key` 头必填；作用域 `(tenant_id, key)`；同 key 同 payload 返回原单（200），不同 payload 拒绝 MALL_30003 |
| 支付回调 | Provider 回调天然重试，`payment_records` + 订单状态条件更新幂等；重复回调返回当前状态 200 |
| Credit/冲退 | billing `uk_ref(ref_type, ref_id)` 唯一索引，重复 no-op |
| 秒杀抢购 | `(activity_id, tenant_id)` 唯一索引 |

---

## 1. 错误码全表

| code | HTTP | retryable | 含义 |
|---|---|---|---|
| MALL_10001 | 401 | false | 未登录 / 会话无效 |
| MALL_10002 | 401 | false | 会话过期（重新登录） |
| MALL_10003 | 409 | false | 用户名已被占用 |
| MALL_10004 | 401 | false | 用户名或密码错误 |
| MALL_10005 | 400 | false | 参数校验失败（error 内含字段说明） |
| MALL_10006 | 429 | true | 请求过于频繁（per-tenant 计数器：默认 120 req/min） |
| MALL_20001 | 404 | false | 商品不存在 |
| MALL_20002 | 409 | false | 商品已下架 |
| MALL_20003 | 409 | false | 库存不足 |
| MALL_20004 | 409 | false | 购物车行数超上限（20） |
| MALL_20005 | 400 | false | 商品数量超限（qty>99 或 <1） |
| MALL_20006 | 404 | false | 品类不存在 |
| MALL_30001 | 404 | false | 订单不存在 |
| MALL_30002 | 409 | false | 订单状态不允许该操作（如已支付再支付/已关闭再退） |
| MALL_30003 | 409 | false | 幂等键冲突（同 key 不同 payload） |
| MALL_30004 | 404 | false | 支付单不存在 |
| MALL_30005 | 400 | false | 回调验签失败 |
| MALL_30007 | 400 | false | 订单含不可购类型组合（如混入 recharge——recharge 仅直购） |
| MALL_30008 | 409 | true | 履约进行中，稍后再试 |
| MALL_40001 | 409 | false | 秒杀活动未开始/已结束 |
| MALL_40002 | 409 | false | 已抢完 |
| MALL_40003 | 409 | false | 不可重复抢购（每人限 1） |
| MALL_40004 | 429 | true | 秒杀限流（令牌桶） |
| MALL_40005 | 404 | false | 抢购凭证不存在 |
| MALL_50001 | 500 | true | 内部错误 |
| MALL_50002 | 504 | true | 下游超时 |
| MALL_50003 | 503 | true | 下游不可用（billing/goods 暂不可达） |

---

## 2. 认证

### POST /auth/register（无需鉴权）
请求 `{"username":"a-z0-9_- 3-32位","password":"8-72字节"}`
成功 200 `{"id":..,"username":"..","tenant_id":..,"session_token":".."}`
错误：MALL_10003（占用）/ MALL_10004 语义不适用 / MALL_10005（格式）/ MALL_10006（billing 槽位 4 并发满 → 透传 429）

### POST /auth/login（无需鉴权）
请求同上。成功 200 同 register。错误：MALL_10004 / MALL_10006。

### POST /auth/logout
成功 200 `{}`（撤销当前会话）。幂等：重复调用 200。

---

## 3. 商品

### GET /categories
200 `{"items":[{"id":1,"name":"精选","sort":1}, ...]}`（六个品类，精选为运营位）

### GET /goods?category_id=&cursor=&page_size=
200 `{"items":[Goods], "next_cursor":"...|null", "has_more":bool}`
`Goods`：`{"id":..,"type":"general|recharge|subscription","name":"..","subtitle":"..","price_cent":..,"orig_price_cent":..,"face_value_cent":..,"bonus_cent":..,"image_url":"..","stock":..,"ref_plan_id":..,"on_sale":true}`（recharge 附 `token_estimate`（按模型价换算的营销展示，不作入账依据））
错误：MALL_20006。

### GET /goods/{id}
200 `Goods`（含详情富文本 `detail`）。错误：MALL_20001。

---

## 4. 购物车

| 端点 | 语义 |
|---|---|
| GET /cart | 200 `{"items":[{"id":..,"goods":Goods,"qty":..}],"total_cent":..}` |
| POST /cart/items | body `{"goods_id":..,"qty":1}`，upsert（同商品合并 qty）；上限 20 行、qty≤99；recharge/秒杀商品返回 MALL_30007 |
| PATCH /cart/items/{id} | body `{"qty":..}`；qty=0 等价删除 |
| DELETE /cart/items/{id} | 200 `{}`；幂等 |

错误：MALL_20001/20002/20004/20005/30007。

---

## 5. 订单

### POST /orders（头：Idempotency-Key 必填）
请求二选一：
```json
{"kind":"recharge","goods_id":123}
{"kind":"cart","from_cart":true}
```
成功 201 `{"order_no":"M2026111812345612ab34","status":"created","amount_cent":..,"items":[行快照],"pay_params":{...Provider 创建支付单返回}}`
行为：recharge 直购建单；cart 结算建混合单（general+subscription；行级价格快照）；下单事务内同库直扣 general 行库存。
错误：MALL_10005/20001/20003/30003/30007。

### POST /orders/{no}/pay
触发 Provider 支付（mock：内部即刻回调）。200 `{"status":"paid|created","payment":{...}}`。幂等：已支付返回当前状态（MALL_30002 仅用于已关闭/已退）。

### POST /payments/callback（无需会话鉴权，走验签）
Provider 异步回调入口（mock/stripe test 均打到此处）。成功 200 `{"order_no":"..","status":"paid"}`（重复回调幂等返回当前状态）。验签失败 MALL_30005。

### GET /orders?cursor=
200 同分页结构；`Order`：`{"order_no","status","amount_cent","items":[...],"created_at","paid_at","fulfilled_at","closed_reason"}`

### GET /orders/{no}
200 `Order`（App 轮询履约结果用）。错误：MALL_30001。

### POST /orders/{no}/cancel
仅 `created` 可取消 → closed(user) + 回补库存。幂等：已 closed 返回 200。错误：MALL_30002。

### POST /orders/{no}/refund
规则（D15）：`paid`→直接退（先作废 pending 履约行）；`fulfilled`→recharge 行 min(行额度,当前余额) 冲退、general 行直退、subscription 行不退费=退订语义。200 `{"status":"refunded","refund_cent":..}`。错误：MALL_30002/30008。

---

## 6. 秒杀

### GET /seckill/activities
200 `{"items":[{"id":..,"goods":Goods,"seckill_price_cent":..,"remain":..,"start_at","end_at","status":"upcoming|ongoing|ended"}]}`

### POST /seckill/{id}/buy
成功 202 `{"voucher":"M2026111812345999xyz","status":"queued"}`（凭证=order_no，seckill 服务预生成）
错误：MALL_40001/40002/40003/40004（秒杀不占用普通幂等键，防重靠唯一索引）。

### GET /seckill/orders/{voucher}
200 `{"voucher","status":"queued|pending_payment|paid|fulfilled|timeout","order_no"}`
错误：MALL_40005。

---

## 7. 我的信息

### GET /me/overview
200 `{"balance_cent":..,"recent_topups":[...最近10条],"subscription":{"active":bool,"plan_name":"..","tier_remain":[...]}|null}`
实现：mall-bff 持 X-Control-Token 调 billing gRPC GetBalance + admin topups/订阅查询（D14）。

---

## 8. 限流分层（D12）

| 层 | 对象 | 机制 | 超限 |
|---|---|---|---|
| mall-bff 基础 | per-tenant 全接口 | Redis 计数器 120 req/min | MALL_10006 |
| 秒杀入口 | 抢购端点 | 令牌桶（R4 压测定容） | MALL_40004 |
| gateway 数据面 | /v1 推理 | billing 既有 Redis Lua（不动） | gateway 既有行为 |

## 9. 管理端 API（占位——R6 前随 spec 增补冻结）

管理端控制台（mall-admin）消费 `/api/v1/admin/*`：商品管理（增改/上下架/库存/定价）、订单管理（查询/退款处置）、秒杀活动管理（创建/预热/启停）、销售统计。鉴权方案（X-Admin-Token vs gateway 管理会话代理）与全量契约在 R6 启动前以本文档增补形式冻结（PRD D18）。
