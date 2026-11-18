# my-market 产品需求文档（PRD）

| 项 | 内容 |
|---|---|
| 文档版本 | v0.5（范围调整②：Web 双前端） |
| 日期 | 2026-11-18 |
| 状态 | 规格五件套已产出；R0 已完成、R1 执行中；admin 契约随 R6 前增补 |
| 配套文档 | [architecture.md](architecture.md)（架构与服务流转图，八图） |
| 关联代码 | ai-gateway（整仓迁入，见 §12） |

**修订记录**

| 版本 | 日期 | 说明 |
|---|---|---|
| v0.1 | 2026-11-17 | 初稿 |
| v0.2 | 2026-11-17 | 三轮拷问定稿（D1-D12） |
| v0.3 | 2026-11-18 | 双 agent 审核修复（D13-D17） |
| v0.4 | 2026-11-18 | 鸿蒙作业取消：R5 移除（曾误裁为纯 API 形态） |
| v0.5 | 2026-11-18 | **Web 双前端回归**：用户端商城 + 管理端运营控制台，新增 R6 里程碑 |

---

## 1. 项目概述

### 1.1 一句话定位

**my-market = 一个「卖算力的商城」**：用户在**商城 Web 前端**完成购买（普通商品 / 按量充值 / 月卡订阅），运营者在**管理端控制台**管理商品/订单/秒杀；充值余额与月卡额度供 AI 网关（OpenAI 兼容接口）推理消费——形成「**购买 → 入账 → 推理 → 计量 → 扣费**」的完整商业闭环。ai-gateway 的 Web portal/dashboard 保留为计费侧控制台（余额、流水、订阅、API key 管理；其自带充值入口当前为关闭状态，见 §3）。

### 1.2 背景与来源

1. **学校作业（已取消，2026-11-18）**：原计划以鸿蒙 ArkTS 商城客户端交付《移动应用开发》大作业，老师取消后不再交付任何鸿蒙产物；mall 保持纯 API 形态（此条保留作历史背景）。
2. **ai-gateway（已开发完成）**：OpenAI 兼容多租户模型网关 + 计费平台，7 个 Go 微服务，核心是计费正确性（Redis 原子预扣、幂等结算/冲正、对账、Kafka 事件流、充值/订阅、AI 客服、Vue3 控制台）。
3. **个人目标**：高完成度面试作品——Go 微服务、gRPC、Redis/Kafka/MySQL、K8s 云原生、秒杀高并发。

### 1.3 目标与非目标

**目标**
- 在 ai-gateway 之上构建商城域（商品/购物车/订单/秒杀），融合为单一体系；
- 全体系容器化：docker-compose（开发）→ K8s/k3s（Windows 机 server + Mac 机 agent）多副本，七项云原生能力（§10.2）；
- 秒杀场景落地（Redis Lua 扣减 + Kafka 削峰 + 异步落库 + 对账），产出带数字的压测报告；
- **交付商城 Web 双前端**（v0.5）：用户端商城 + 管理端运营控制台（原鸿蒙 App 取消后的客户端形态）。

**非目标（不做）**
- 真实支付——**普通商品不做**；**AI 充值侧为远期接入目标**（三档路径见 §5.5，架构按支付网关抽象 + 回调幂等设计）；
- 异地多活（多副本 + 跨节点调度 + 故障自愈即止）；
- 对外运营；
- 重写 ai-gateway 已验证的计费内核（只在边界做增量改造）。

---

## 2. 商业模型与商品类型（核心业务裁定）

AI 售卖沿用 gateway 现状，**只有两种模式**：

| 模式 | 说明 | 商城形态 | 入账/生效方式 |
|---|---|---|---|
| **按量计费** | 充值余额（分口径），推理按各模型单价从余额扣 | `recharge` 充值包（面值制，可配赠送） | billing `Credit` 新增 gRPC 幂等入账（ref_id=订单号:行号） |
| **月卡订阅** | 月费换「各模型档位每 5 小时滚动免费额度」（billing Plan 体系） | `subscription` 月卡商品 | billing `ActivateSubscription` 新增 gRPC（幂等 ref） |

**商品类型与购物规则（已裁定）**：

| 类型 | 进购物车 | 结算方式 | 履约 |
|---|---|---|---|
| `general` 普通商品（手机/服饰等五品类） | ✅ | 混合结算 | 发货 mock |
| `recharge` 充值包 | ❌ **仅直购**（收银台式一步买） | 单独直购 | Credit 入账 |
| `subscription` 月卡 | ✅ | 混合结算 | ActivateSubscription 开通/续订 |
| 秒杀活动商品（充值包或普通商品） | ❌ 仅秒杀直购 | 秒杀下单 | 按类型履约 |

- **没有"token 包"商品**：不存在"固定 token 数商品"，token 数只是商品页按模型单价的营销换算展示；
- 混合订单（一单含 general + subscription）按行履约，行级幂等；
- 余额唯一权威在 billing，金额单位统一为**分**。

---

## 3. 现状盘点：ai-gateway 已有能力（融合底座）

> 本节经代码逐条核查（文件:行号级证据），描述以代码现状为准。

| 能力 | 现状（已核实） | 对本体系意义 |
|---|---|---|
| 计费正确性 | Redis Lua 原子「判余额≥N 再扣」；`uk_request_phase` 幂等结算/冲正 | 秒杀扣减同构复用 |
| **推理 bills 批量落账** | Redis 实时余额权威 + `BILLING_LEDGER_BATCH_SECONDS`（默认 5，0-60，0 关闭）定时批量刷 MySQL；Kafka 只承载明细不改余额 | bills 链路机制，**保持不动** |
| **充值 topups 落账** | **同步 MySQL 事务**：topup 行 + tenants.ledger_balance 同事务提交（MySQL 为权威账本与提交点），Redis 为事后投影对齐 | Credit 复用的正是这条路径 |
| **充值/订阅对外入口** | **全部刻意 403 封死**（"充值入口暂时关闭，请使用余额兑换码""月卡开通与套餐变更尚未开放，真实支付接入后开放"）；当前唯一加钱入口为兑换码 `/portal/redeem` | Credit 与 ActivateSubscription 是「**新增 RPC + 正式开门**」，非调用现成接口（D13） |
| 对账 | reconciler 扫滞留 pending 三分类；**默认 `-no-auto-reverse`（滞留保留待核查，不自动全退）** | 秒杀对账沿用此默认口径（D16） |
| Kafka 事件流 | 计费明细事件（segmentio/kafka-go；Producer 单 topic 绑定，新 topic 需新实例） | 复用 pkg/kafka，新增 seckill.orders topic |
| 充值订单 | RechargeOrder 状态机（store 层完整，幂等 order_no）；退款 `min(订单额度, 当前余额)` | 状态机与退款口径的参照范式 |
| 订阅 | GPT Plus 式月卡：每 RefreshHours（默认 5h）滚动窗口各档 token 额度；额度内 funding=subscription、pre=0；超额 402；**程序化开通/续订接口不存在**（端点 403，store 层 SubscribePlan 不收月费、无幂等 ref） | R2.5 的 ActivateSubscription 为全新开发 |
| **门户账号体系** | `/auth/register` `/auth/login` `/auth/me` `/auth/logout`（用户名密码、bcrypt、注册同事务建租户、12h 会话、HttpOnly cookie agw_user；登录注册并发槽位 4） | App 注册登录直接复用（mall-bff 经 gateway 代理调用，D14） |
| 路由熔断 | 内存模式：5 连败跳闸→10s 冷却→半开 1 探测；Redis 模式：**60 秒窗口 20 次平台失败**；AutoBan 写 providers.status=2 持久化；**失败计数已 Redis 共享，状态机仍每实例内存** | 上游熔断已有；多副本仅状态机待共享（P1 表述已修正） |
| AI 客服 | agent 服务，执行层不信任 LLM；内部凭证 X-Agent-Internal + X-Agent-Tenant（未配置该 token 时内部路径关闭） | 不动；P6 商城工具集依赖内部凭证（部署须配置） |
| 可观测 | Prometheus 指标 + OTel trace | 商城接入同一套 |
| Web 控制台 | Vue3 双入口；gateway 同时托管数据面与页面 | 保留，与 App 并存 |
| 评测资产 | eval/ 220M（含 langfuse-workbench） | **随仓迁移**，评测在新仓继续 |

**服务与端口（dev，已核实）**：gateway **代码默认 8080，dev 由 `GATEWAY_PORT=18080` 显式设定（迁移后 compose 必须带）** · billing 9101(gRPC)/9103(admin HTTP，**全端口要求 X-Control-Token**，含 /auth/*) · router 9102 · agent 9105 · mock-provider 9201-9203 · MySQL 3307 · Redis 6381 · Kafka **容器内 9092 / 宿主映射 29092**（宿主进程接入用 29092）。
**新增分配**：mall-bff 18090 · mall-goods 9111 · mall-order 9112 · seckill 9113 · registry 5000（K8s 期）。

---

## 4. 整体体系

分层结构与全部服务流转图见 **[architecture.md](architecture.md)**（八图：全景架构、充值闭环〔含支付回调与 outbox〕、混合订单、月卡生效、退款、秒杀、App 会话鉴权、部署演进）。此处仅列业务域：

| 业务域 | 服务 | 状态 | 职责 |
|---|---|---|---|
| 商品域 | mall-goods（新） | 新建 | 商品/品类/分页/普通商品库存 |
| 交易域 | mall-order（新）、mall-bff（新） | 新建 | 购物车、订单状态机、支付回调、outbox 履约、App REST 入口与会话适配 |
| 营销域 | seckill（新） | 新建 | 秒杀活动、库存预热、Lua 扣减、削峰、回补、对账 |
| 计费域 | billing（已有） | **加法改造** | 新增 `Credit` 与 `ActivateSubscription` 两个幂等 gRPC；内核语义不动 |
| AI 网关域 | gateway、router、mock-provider（已有） | 不动 | 推理数据面 |
| 智能域 | agent（已有） | 不动 | AI 客服 |
| 对账域 | reconciler、event-consumer（已有）+ 履约 worker、秒杀消费者、对账 job（新） | 扩展 | 最终一致兜底 |
| 客户端域 | 商城 Web 双前端（新，web/）、ai-gateway Web（已有） | 新建/保留 | 用户端商城 + 管理端控制台（R6）/ 计费侧控制台 |

**架构原则**：① 余额/入账唯一权威在 billing（任何加钱动作只走 Credit，ref 幂等）；② 订单系统按业务域隔离：billing 内 RechargeOrder（充值，同步 MySQL 事务）与推理 bills（Redis 实时权威 + 批量落账）各自既有机制**均不动**；mall_order 为 market 库的独立订单体系（支付同步事务 + outbox 履约），秒杀走 Kafka 异步落库——分离理由是**业务域与演进隔离**，非持久化机制差异（v0.3 修正）；③ 对外仅 mall-bff 与 gateway 两个数据面入口；④ 同构模式复用（预扣/幂等/对账三模式多处落点）；⑤ 跨服务履约一致性用**轻量事务性发件箱**：支付置 paid 的同一事务写 fulfillment_tasks → 后台 worker 执行 Credit/ActivateSubscription（幂等可重试）→ 全行 done 才 fulfilled → 对账 job 扫滞留兜底；⑥ mall 侧稳定性三板斧 = 超时 + 重试 + 对账，不新增服务间熔断框架（上游熔断 router 已有）。

---

## 5. 新增体系设计

### 5.1 mall-goods（商品域）

- 商品三类型：`general` / `recharge` / `subscription`（月卡映射 billing Plan，数据源在 billing，本域做展示缓存）；
- 品类：精选（运营位，跨品类聚合）/ 手机 / 服饰 / 穿搭 / 家居 / 福利（秒杀入口）；
- gRPC：`ListCategories` / `ListGoods(category, cursor, page_size)` / `GetGoods` / `DeductStock` / `ReturnStock`；
- 普通商品库存（D17 裁定）：**下单事务内扣减**——goods 与 mall_orders 同在 market 库，mall-order 在下单本地事务内**直接条件更新** `goods.stock`（`stock >= qty`，同库同事务，不经 gRPC）；Redis 计数做前置校验与展示，与 MySQL 不一致时**以 MySQL 为准回补对齐**；closed / refunded 时同库回补。`DeductStock` / `ReturnStock` gRPC 仅供秒杀预热/回补等跨服务调用方使用。充值包/月卡无库存（可配限量）。

### 5.2 mall-order + mall-bff（交易域）

**订单状态机**（D15 裁定退款语义）：

```
created ──支付回调(验签·幂等)──► paid ──outbox worker 按行履约──► fulfilled
   │                             │                                 │
   ├─超时 30 分钟未支付──► closed  ├─退款(未入账)──► refunded         └─退款(已入账)──► refunded
   └─用户主动取消────────► closed
```

- `closed` = 超时或主动取消（含 general 行库存回补）；
- `paid → refunded`：资金未动直接退；**退款前作废该订单全部 pending 履约行**（与 worker 的竞态由幂等冲退兜底）；
- `fulfilled → refunded`：**recharge 行冲退 = min(行额度, 当前余额)**（走 billing 幂等冲退，ref 复用入账 ref）；general 行直接标记退回 + 回补库存；**subscription 行不退费——等价退订语义**：当期权益保留至窗口期满、不再续订（与 billing 现有「退订=当期不退只停下期续费」口径一致）；
- 订单 fulfilled 判定：全部履约行 done；行失败重试 5 次后标记 failed，订单停留 paid 交对账处置。

**支付抽象（回调驱动）**：`PaymentProvider` 接口（CreatePayment / HandleCallback / QueryPayment）+ `MockPaymentProvider`。支付流程：下单 → CreatePayment（写 payment_records=created）→ **异步回调** HandleCallback（验签占位、幂等）→ payment_records 更新 + 同一事务内置 paid + 写 fulfillment_tasks（outbox）。切真实支付只换 Provider，订单链路不动。

**购物车**：登录后可用；MySQL `cart_items` 持久化；可进购物车 = general + subscription；上限 20 行、单行 qty ≤ 99；结算生成混合订单，价格按行快照。

**mall-bff**：App 唯一 REST 入口。**会话适配（D14 裁定）**：App 发 `Authorization: Bearer <会话token>`，mall-bff 经 **gateway :18080 的 `/auth/*` 代理**验证会话（gateway 已实现代理注入 X-Control-Token 并透传 agw_user cookie），mall-bff **不直连 billing 9103**——billing 零改动。**余额/流水查询（N9）**：mall-bff 以内部服务身份（持 X-Control-Token）调 billing gRPC `GetBalance` 与 admin topups 查询。**基础限流（D12 落点，N11）**：per-tenant Redis 计数器，默认 120 req/min，写接口更严。API 契约见 spec/api-contract.md。

**Credit（billing 加法改造之一，D13 裁定）**：新增 gRPC `Credit(tenant_id, amount_cent, ref_type="mall", ref_id=order_no:line_no)`——billing 侧走 topups 现成**同步 MySQL 事务**路径（topup 行 + ledger_balance 同事务，MySQL 为提交点，Redis 投影对齐；复用 `uk_ref(ref_type, ref_id)` 复合唯一索引幂等），重复调用 no-op。**不打开旧 403 充值端点**，本 RPC 成为唯一程序化入账入口。

**ActivateSubscription（billing 加法改造之二，R2.5 全新开发）**：gRPC `ActivateSubscription(tenant_id, plan_id, ref=order_no:line_no)`——幂等 ref + 模拟月费校验（不接真钱）+ 落订阅记录；手动续订 = 立即续一期（与既有自动续费器并存）。

### 5.3 seckill（营销域）

活动 = 充值包或普通商品 × 秒杀价 × 独立活动库存 × 每人限购 1。

```
请求 → bff 鉴权+令牌桶限流 → seckill:
 ① Redis Lua 原子「判重+判库存+扣库存」→ 失败快速返回
 ② 成功 → 写 Kafka(seckill.orders，order_no 由 seckill 服务预生成并随消息传递) → produce 后同步返回「排队中 + 凭证(=order_no)」
 ③ 消费者：落库 seckill_orders(status=pending) + 创建 mall_order(created)
    → 引导支付 → 正常支付/履约链路（recharge→Credit / general→发货）
 ④ 超时未支付（15 分钟）：延迟任务回补 Redis 库存 + 关闭订单（幂等）——对账处置中唯一的自动回补场景
 ⑤ 对账 job：Redis 余量 vs DB 计数比对；滞留处置默认保留待核查（沿用 reconciler 口径，D16）
```

防超卖三道防线：Lua 原子扣减（主）→ DB `stock>0` 条件更新兜底 → `(activity_id, tenant_id)` 唯一索引防重。

### 5.4 客户端域（v0.5：Web 双前端，R6）

- **用户端商城**：注册/登录、品类/商品列表（游标分页 + 到底提示）、详情、购物车、下单/模拟支付、我的订单、充值包直购、余额/流水卡、（R4 后）秒杀入口与抢购结果轮询；
- **管理端控制台**：商品管理（增改/上下架/库存/定价）、订单管理（查询/退款处置）、秒杀活动管理（创建/预热/启停）、销售统计概览；
- **技术裁定（D18）**：独立前端工程 `my-market/web/`（Vue3 + Vite + TS，与 ai-gateway web 同栈；设计系统 tokens.css/组件风格移植复用），双入口构建（mall-user / mall-admin），产物 **go:embed 由 mall-bff 托管**（与 gateway 托管自有 web 同模式）——**不改动 ai-gateway 的 web/**，保持两域边界；
- 鉴权：用户端走会话 token（§5.2）；管理端走 mall-bff `/admin/*`（admin 凭证方案与全量契约在 R6 前的 spec 增补中冻结——初定 X-Admin-Token 头或经 gateway 管理会话代理二选一）；
- API key 定位不变：仅用于外部程序调 /v1；
- Web AI 客服导购（原 P6）挂用户端商城：agent 商城工具集，经 X-Agent-Internal 凭证路径，**部署须配置 AGENT_INTERNAL_TOKEN**。

### 5.5 支付接入路径（AI 充值侧远期目标，普通商品维持模拟）

| 档位 | 方案 | 资质要求 | 定位 |
|---|---|---|---|
| 档一（现在） | MockPaymentProvider | 无 | 回调/验签/幂等/对账全按真实语义 |
| 档二（演示级） | **Stripe test mode / PayPal sandbox** | 个人账号即可 | 真实 webhook 回调全流程（假钱），面试演示"完整支付链路" |
| 档三（真实收款） | 微信支付 Native（扫码）或支付宝当面付 | **个体工商户/企业执照**（个人可办） | 最终接入形态 |

合规提醒：真实对外收款售 AI 服务涉及 ICP/生成式 AI 备案；面试阶段以档二为宜。触发时机见 §15 P5。

### 5.6 实现参数裁定（D17，细节入 spec）

| 参数 | 裁定值 |
|---|---|
| 普通订单支付超时 | 30 分钟（定时扫描关闭 + 库存回补） |
| 秒杀支付超时 | 15 分钟（唯一自动回补场景） |
| 游标分页 | 键集 `(created_at, id)` 降序，不透明 Base64 游标；page_size 默认 20、上限 50 |
| 幂等键作用域 | `(tenant_id, key)`；同 key 同 payload 返回原单，不同 payload 拒绝 409 |
| 订单号格式 | `M + yyyyMMddHHmmss + 6 位随机`（作为 Credit ref_id 前缀的组成部分） |
| 错误模型 | `{"error":"<人话>","code":"MALL_XXXXX","retryable":bool}`（全量码表落 api-contract.md） |
| 购物车 | 上限 20 行、单行 qty ≤ 99 |
| 库存 | 下单事务内扣（MySQL 条件更新为准，Redis 前置校验）；closed/refunded 回补 |
| 履约重试 | 行级指数退避重试 5 次，之后标 failed 交对账 |

---

## 6. 核心业务流程

八张图全部在 [architecture.md](architecture.md)：充值闭环（含支付回调 + outbox 履约 + 推理扣费）、混合购物车订单、月卡生效、**退款**、秒杀、App 会话鉴权、全景架构、部署演进。含错误分支的详细时序在 spec/sequences.md 产出。

---

## 7. 数据模型（market 库——**实体级草案**，最终 DDL 在 spec/db-schema.sql 定稿）

> 定稿时补齐：字段类型、索引（tenant_id+created_at 分页等）、约束与默认值、时间戳列、utf8mb4、软删除策略。

```
categories(id, name, sort, status)
goods(id, type[general|recharge|subscription], category_id, name, subtitle,
      price_cent, face_value_cent, bonus_cent, orig_price_cent,
      image_url, stock, status, ref_plan_id, created_at)
cart_items(id, tenant_id, goods_id, qty, UNIQUE(tenant_id, goods_id), updated_at)
mall_orders(id, order_no UNIQUE /*M+时间戳+随机*/, tenant_id,
            status[created|paid|fulfilled|refunded|closed],
            amount_cent, idem_key /*UNIQUE(tenant_id,idem_key)*/,
            paid_at, fulfilled_at, closed_reason[timeout|user], created_at)
mall_order_items(id, order_id, line_no, goods_id, qty, price_snapshot_cent,
                 UNIQUE(order_id, line_no))
seckill_activities(id, goods_id, seckill_price_cent, total_stock, sold_stock,
                   start_at, end_at, status, redis_key)
seckill_orders(order_no UNIQUE, activity_id, tenant_id, UNIQUE(activity_id,tenant_id),
               status[pending|confirmed|timeout|cancelled], created_at)
payment_records(id, order_no, provider[mock|stripe_test|wechat],
                status[created|paid|failed|refunded],
                callback_payload, signature_verified, created_at)   /* 支付流水 */
fulfillment_tasks(id, order_no, line_no, type[ship|credit|subscribe],
                  status[pending|done|failed], retry_count,
                  UNIQUE(order_no, line_no), created_at)            /* 履约 outbox */
```

金额单位：分（与 ai-gateway 对齐）。

---

## 8. mall-bff 对外 API（概要，完整契约与全量错误码见 spec/api-contract.md）

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | /api/v1/auth/register · /auth/login · /auth/logout | 账密（经 gateway /auth/* 代理） |
| GET | /api/v1/categories · /goods?cursor= · /goods/{id} | 品类/商品分页/详情 |
| GET/POST/PATCH/DELETE | /api/v1/cart · /cart/items（加购，upsert）· /cart/items/{id}（改量/删除） | 购物车 |
| POST | /api/v1/orders（Idempotency-Key）· /orders/{no}/pay | 下单（混合/recharge 直购）/发起支付 |
| POST | /api/v1/payments/callback | 支付回调（Provider 驱动，验签幂等） |
| POST | /api/v1/orders/{no}/refund · /orders/{no}/cancel | 退款 / 主动取消 |
| GET | /api/v1/orders?cursor= | 我的订单 |
| GET | /api/v1/seckill/activities · /seckill/orders/{no} | 秒杀列表/结果 |
| POST | /api/v1/seckill/{id}/buy | 秒杀下单（排队语义，返回凭证=order_no） |
| GET | /api/v1/me/overview | 余额/最近流水（mall-bff 内部凭证调 billing GetBalance/topups） |
| * | /api/v1/admin/* | 管理端控制台（商品/订单/秒杀/统计——R6 前契约冻结，见 api-contract §9） |

鉴权：`Authorization: Bearer <会话token>`（登录凭证，非 API key）。
错误响应骨架：`{"error":"...","code":"MALL_XXXXX","retryable":false}`。

---

## 9. 非功能需求

| 维度 | 要求 |
|---|---|
| 性能 | 列表 P99<50ms；下单（不含回调）P99<200ms；秒杀热路径单副本 ≥1000 QPS、P99<100ms（R4 压测实测） |
| 可用性 | 无状态服务 ≥2 副本；滚动更新零停机；节点 drain 自愈；熔断保上游故障 |
| 可观测 | 全服务 Prometheus 指标 + Grafana 分域面板；mall→billing 链路 OTel trace 透传 |
| 一致性 | 余额/库存/订单最终一致 + 幂等可重放；outbox + 对账 job 兜底 |
| 安全 | 密钥/令牌不入库不入日志（X-Control-Token、AGENT_INTERNAL_TOKEN 等）；写操作幂等键强制；支付仅模拟并显著标注 |
| 资源 | dev（Mac 16G）compose 分 profile；K8s（Win VM 8G）全量+可观测 |

---

## 10. 部署架构

### 10.1 开发期（R0-R2.5，Mac，docker-compose）

`deploy/compose/docker-compose.yml` 分 profile：`infra`（MySQL/Redis/Kafka）+ `ai`（gateway 全家，**显式 GATEWAY_PORT=18080**；宿主进程接 Kafka 用 **29092**）+ `mall`（新增服务）。按需启动。

### 10.2 K8s 期（R3，七项验收即止，不加戏）

多副本+Pod 反亲和 · HPA 压测扩缩容 · 滚动更新/回滚 · Readiness/Liveness 探针 · Prometheus+Grafana+Jaeger 进集群 · Ingress 统一入口 · 节点 drain 自愈演示。
拓扑：Win 机（AMD64，VMware VM 8G Ubuntu，桥接+固定 IP）k3s server + registry:2；Mac（M1）远程 kubectl；双节点演示时 Mac 挂 arm64 agent。镜像自始 buildx 双架构。有状态服务 local-path/宿主，不做笔记本高可用。
服务发现：dev 用 compose DNS；K8s 用原生 Service DNS + headless + gRPC 客户端 round_robin。

---

## 11. 仓库布局与产物位置约定（工作纪律）

**所有产物各归其位，禁止随手散放。** 目标仓库结构：

```
my-market/
├── docs/
│   ├── PRD.md                 # 本文档
│   ├── architecture.md        # 架构与服务流转图（八图）
│   ├── spec/                  # 五份规格（SDD 依据）
│   │   ├── api-contract.md    # REST/gRPC 全契约 + 全量错误码
│   │   ├── db-schema.sql      # market 库 DDL
│   │   ├── sequences.md       # 核心时序（含错误分支）
│   │   ├── acceptance.md      # 各里程碑可执行验收清单
│   │   └── adr/               # 架构决策记录（每条一页）
│   └── plans/                 # 阶段任务拆解（superpowers 任务清单）
├── ai-gateway/                # 整仓迁入，模块路径不变，eval/ 随迁
├── services/                  # mall-bff / mall-goods / mall-order / seckill
├── web/                       # 商城前端工程（Vue3+Vite+TS 双入口 mall-user/mall-admin，R6；构建产物 go:embed 进 mall-bff）
├── proto/                     # 商城侧 .proto（含 Credit/ActivateSubscription 扩展）
├── pkg/                       # 商城侧公共库（mallerr/mallpage 等）
├── deploy/
│   ├── compose/               # 开发期编排（分 profile）
│   └── k8s/                   # R3 起的 manifests/Helm
├── scripts/                   # 构建/造数/压测脚本
└── go.work                    # 双模块工作区
```

根模块 `github.com/frostloom/my-market`（services/pkg/proto）+ 嵌套模块 `github.com/frostloom/ai-gateway`（import 路径零改动）。

**工作流程约定（SDD）**：PRD（本文档）→ 五份规格（docs/spec/）→ 阶段任务拆解（docs/plans/，superpowers 风格）→ **用户确认后才执行**。规格或计划变更先改文档再改代码。

---

## 12. ai-gateway 迁移方案（已定）

- **整仓迁入** `my-market/ai-gateway/`：全部源码、web/、docs/、scripts/、deploy/、**eval/ 全套（220M，含 langfuse 相关，评测在新仓继续）**；
- **不迁**：`.git`（2.6G 历史留在旧仓）、logs/、.gocache/、worktrees/ 等运行产物；
- Go 模块路径 `github.com/frostloom/ai-gateway` 不变——代码零改动即编译；
- 旧 GitHub 仓**归档只读**（历史凭证），README 顶部指向 my-market；此后 gateway 的开发都在新仓目录内进行；
- 两仓都公开，迁入前密钥/`.env` 脱敏复查。

---

## 13. 里程碑计划

| 阶段 | 内容 | 验收 |
|---|---|---|
| **R0 迁移** | ai-gateway 整仓迁入 + go.work + compose 分 profile 骨架 | `docker compose --profile ai up` 一键起 gateway 全家；全部 /healthz 绿；发一发 /v1 请求产生 reserve+settle 两行账（验收细化为可执行步骤，落 acceptance.md） |
| **R1 商城切片** | mall-goods/mall-order/mall-bff；品类/分页/购物车/general 下单支付（同步事务 + outbox 履约） | HTTP 全链路跑通；订单状态机与幂等验证（重复下单/支付/回调 no-op）；超时关闭+库存回补验证 |
| **R2 充值闭环** | billing 新增 **Credit gRPC（唯一程序化入账入口，不开旧 403 端点）**；recharge 直购；闭环主线 | 买充值包→入账→/v1 推理→扣费→流水可见；重试/断电入账不重不漏 |
| **R2.5 月卡** | **ActivateSubscription 全新开发**（幂等 ref + 模拟月费校验）；subscription 商品进购物车、混合结算 | 月卡可购可续；额度窗口行为与 billing 语义一致（验收口径落 acceptance.md） |
| **R3 K8s 化** | §10.2 七项 | 全体系上 k3s；HPA/滚动/自愈演示；Grafana/Jaeger 可查商城链路 |
| **R4 秒杀+压测** | seckill 全链路 + 对账 + 压测报告 | 1000 并发无超卖（DB 计数验证）；滞留对账收敛（保留待核查口径）；QPS/P99 报告；扩容曲线项待 R3 HPA 就绪后测 |
| **R6 双前端** | spec 增补冻结 admin 契约 → web/ 工程（mall-user + mall-admin 双入口）→ mall-bff 静态托管与 /admin API → 两端页面 | 用户端页面化全链路（登录→浏览→购物车→下单→支付→订单→充值包直购）；管理端可管商品/订单/秒杀活动；秒杀入口页待 R4 |

依赖：R1←R0；R2←R1；R2.5←R2；R3←R2.5；R4←R2（扩容曲线项依赖 R3）；R6←R2.5（秒杀入口页←R4），可与 R3 并行。
排期背景：原作业截止约束已随作业取消消失（v0.4）；v0.5 增补 R6 双前端为必达目标。

---

## 14. 已定决策清单（追溯用；v0.3 含审核修正）

| # | 决策 | 要点 |
|---|---|---|
| D1 | 整仓迁移、废弃旧仓 | eval/ 随迁；.git 不迁；旧仓归档；双开发=同仓两模块区 |
| D2 | 商品三类型与购物规则 | 无"token 包"；recharge 仅直购；月卡/普通商品可进购物车 |
| D3 | 余额口径 | 分，billing 唯一权威；Credit ref_id=order_no:line 幂等 |
| D4 | 订单按业务域隔离 | **（v0.3 修正机制归属）** billing 充值=同步 MySQL 事务（topups 权威）；推理 bills=Redis 实时权威+批量落账——两者均不动；mall_order=支付同步事务+outbox；分离理由为业务域与演进隔离 |
| D5 | App 账密登录 | 复用 /auth/register /auth/login；API key 仅外部程序用 |
| D6 | K8s 七项为限 | 多副本/HPA/滚动/探针/可观测/Ingress/自愈；不上 mesh/多集群/GitOps |
| D7 | 仓库公开 | 两仓公开，密钥脱敏 |
| D8 | 秒杀覆盖充值包+普通商品 | 履约按类型分派 |
| D9 | SDD 流程 | PRD→spec 五件→plans 任务→**用户确认后执行** |
| D10 | 支付抽象先行 | PaymentProvider + Mock 按真实语义设计；AI 侧真实支付为远期目标（§5.5 三档） |
| D11 | 落盘四路径 | 推理 bills 批量异步（已有不动）/ 充值 topups 同步事务（已有不动）/ mall_order 支付同步事务 + outbox 履约 / seckill Kafka 异步 |
| D12 | mall 侧稳定性 | 超时+重试+对账三板斧；不新增服务间熔断；限流三层（数据面 Lua/秒杀令牌桶/per-tenant 计数，落点 §5.2） |
| D13 | 入口开门策略 | Credit 与 ActivateSubscription 均为**新增 gRPC + 正式开门**，不打开旧 403 端点；两者是 billing 仅有的两处加法改造 |
| D14 | mall-bff 鉴权与数据路径 | /auth/* 会话验证经 gateway :18080 代理（令牌由 gateway 注入），不直连 9103；余额/流水由 mall-bff 以内部服务身份持 X-Control-Token 调 billing gRPC/admin |
| D15 | 退款语义 | paid→refunded 未入账直退（先作废 pending 履约行，竞态由幂等冲退兜底）；fulfilled→refunded：recharge 行 min(行额度,当前余额) 幂等冲退、general 行直退+回补库存、subscription 行不退费=退订语义（当期保留至窗口期满，与 billing 现有口径一致） |
| D16 | 对账口径 | 滞留默认保留待核查（沿用 reconciler 默认）；对账处置中唯一的自动回补场景是秒杀「超时未支付」——普通订单超时/取消/退款的库存回补属状态机常规动作，不在对账口径内 |
| D17 | 实现参数包 | §5.6 全表（超时 30/15 分钟、游标、幂等键作用域、订单号格式、错误模型、购物车上限、库存时机、重试 5 次） |
| D18 | 商城前端形态（v0.5） | 独立工程 my-market/web/（Vue3+Vite+TS 双入口），go:embed 由 mall-bff 托管；不改动 ai-gateway web/；admin 契约 R6 前 spec 增补冻结 |

---

## 15. 待决策清单（不挡主线）

| # | 事项 | 触发时机 |
|---|---|---|
| P1 | 熔断**状态机** Redis 共享化（失败计数已共享，仅状态机每实例内存；"20 连败"仅内存模式口径，Redis 模式为 60s 窗口 20 次） | R3 后评估，倾向 R4 末独立小任务 |
| P2 | portal 老收银台（现 403 关闭）是否收编为 mall_order | R2.5 后评估 |
| P3 | etcd 自研注册中心（学习件，不进主线） | R3 后任选 |
| P5 | **AI 充值侧真实支付接入**（档二 Stripe test 可先行，档三需个体工商户资质，见 §5.5） | R4 后评估 |
| P6 | **用户端 AI 客服导购**（agent 商城工具集：SearchGoods / AddToCart 走确认状态机；依赖 X-Agent-Internal，**部署须配置 AGENT_INTERNAL_TOKEN** 否则内部路径关闭） | R6 后有余力 |

---

## 16. 风险与已知限制

| 风险/限制 | 应对 |
|---|---|
| 双 16G 资源 | compose 分 profile；K8s 全量仅演示/压测时拉满 |
| 熔断状态机每实例内存态（失败计数已共享） | D6 下多副本可接受 + AutoBan 兜底；P1 待决策 |
| billing 历史吞吐基线 ~210 req/s（全链路延迟主导） | 不盲目优化；R4 记录为已知瓶颈与分片方向 |
| AI 客服依赖真实 key | demo 环境变量管理；离线走假 LLM 单测 |
| 充值退款口径（单池余额） | 沿用 min(订单额度, 当前余额)；mall 侧行级化 |
| eval 220M 使仓库变大 | 接受（评测资产价值 > 体积）；大文件不进根模块 |
| gateway 默认端口 8080 / Kafka 宿主 29092 | compose/K8s 显式配置（§10.1），脚本统一注入 |
