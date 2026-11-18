# my-market 架构与服务流转图

> 配套 [PRD.md](PRD.md) v0.3 使用。图为规格与实现的统一依据，变更先改图再改码。
> 以下 Mermaid 图可在 GitHub / VS Code / Typora 直接渲染。

---

## 1. 全景架构（分层）

```mermaid
flowchart TB
    subgraph Clients["客户端层"]
        APP["鸿蒙商城 App<br/>购买入口 R5"]
        WEB["Web 控制台<br/>portal / dashboard<br/>充值入口现关闭"]
        EXT["外部程序<br/>OpenAI 兼容调用"]
    end

    subgraph Edge["接入层"]
        BFF["mall-bff :18090<br/>商城 REST 入口<br/>会话适配·聚合"]
        GW["gateway :18080<br/>/v1 推理数据面 SSE<br/>鉴权·幂等·限流<br/>/auth 代理·Web 托管"]
    end

    subgraph Mall["商城域（新增）"]
        GOODS["mall-goods :9111<br/>商品·品类·分页"]
        ORDER["mall-order :9112<br/>购物车·订单状态机<br/>支付回调·outbox 履约"]
        SK["seckill :9113<br/>活动·库存预热<br/>Lua 原子扣减·回补"]
    end

    subgraph Core["计费与 AI 域（已有 + 两处加法）"]
        BILL["billing :9101 gRPC / :9103 admin<br/>余额·预扣·结算·充值 topups<br/>月卡 Plan<br/>新增：Credit · ActivateSubscription"]
        RT["router :9102<br/>加权路由·熔断·AutoBan"]
        AG["agent :9105<br/>AI 智能客服"]
    end

    subgraph Async["异步与对账层"]
        KF["Kafka<br/>容器 9092 · 宿主 29092<br/>计费明细·seckill.orders"]
        EC["event-consumer<br/>计费明细消费"]
        SKC["秒杀订单消费者<br/>落库·建 mall_order"]
        FW["履约 worker<br/>消费 fulfillment_tasks"]
        REC["reconciler<br/>滞留账单·默认保留待核查"]
        REC2["秒杀/订单对账 job<br/>余量 vs DB 计数"]
    end

    subgraph Data["数据层"]
        MY[("MySQL :3307<br/>ai 库 + market 库")]
        RD[("Redis :6381<br/>实时余额·秒杀库存<br/>普通商品库存计数<br/>限流·会话")]
    end

    PROV["上游 LLM<br/>mock-provider ×3 / 真实模型"]

    APP -->|"REST + Bearer 会话"| BFF
    WEB --> GW
    EXT -->|"Bearer API key"| GW

    BFF -->|"gRPC"| GOODS
    BFF -->|"gRPC"| ORDER
    BFF -->|"gRPC"| SK
    BFF -.->|"验证会话：经 gateway /auth 代理"| GW

    ORDER -->|"支付回调置 paid + outbox"| MY
    FW -->|"Credit / ActivateSubscription<br/>ref 幂等"| BILL
    SK -->|"削峰 produce"| KF
    KF --> SKC
    SKC --> MY

    GW -->|"Reserve / Settle / Reverse gRPC"| BILL
    GW -->|"HTTP 选路"| RT
    RT --> PROV

    BILL --> RD
    BILL --> MY
    BILL -->|"明细事件"| KF
    KF --> EC
    EC --> MY
    REC --> BILL
    REC2 --> SK
    SK --> RD
    GOODS --> RD
    GOODS --> MY
    GW -.->|"管理面转发"| AG
```

**读图要点**：
- 对外只有两个数据面入口：mall-bff（商城）与 gateway（推理 + Web + /auth 代理）；
- 一切「给用户加钱」的动作收敛到 billing 的新增 `Credit`（ref 幂等）；
- 写路径分层：推理 bills = Redis 实时权威 + 批量落账（已有不动）；充值 topups = 同步 MySQL 事务（已有不动）；mall_order = 支付同步事务 + outbox 履约（新）；seckill = Kafka 异步落库（新）。

---

## 2. 服务清单与端口（dev，已按代码核实）

| 服务 | 端口 | 协议 | 状态 |
|---|---|---|---|
| gateway | **18080（代码默认 8080，compose 须显式 GATEWAY_PORT）** | HTTP/SSE | 已有 |
| billing | 9101 gRPC · 9103 admin（全端口 X-Control-Token） | gRPC / HTTP | 已有（+Credit、+ActivateSubscription） |
| router | 9102 | HTTP | 已有 |
| agent | 9105 | HTTP | 已有 |
| mock-provider | 9201-9203 | HTTP/SSE | 已有 |
| mall-bff | 18090 | HTTP（REST） | 新增 |
| mall-goods | 9111 | gRPC | 新增 |
| mall-order | 9112 | gRPC | 新增 |
| seckill | 9113 | gRPC | 新增 |
| MySQL | 3307 | — | 已有（+market 库） |
| Redis | 6381 | — | 已有 |
| Kafka | **容器 9092 / 宿主 29092** | — | 已有（+新 topic，Producer 单 topic 需新实例） |
| registry | 5000 | — | R3 K8s 期 |

---

## 3. 服务流转一：充值闭环（商业主线：回调驱动 + outbox 履约）

```mermaid
sequenceDiagram
    autonumber
    participant A as App
    participant B as mall-bff :18090
    participant O as mall-order :9112
    participant P as PaymentProvider<br/>mock
    participant W as 履约 worker
    participant L as billing :9101
    participant R as Redis
    participant MM as MySQL market 库
    participant G as gateway :18080
    participant RT as router :9102
    participant UP as 上游 LLM
    participant MA as MySQL ai 库

    rect rgb(235,245,255)
    note over A,MM: ① 买：recharge 充值包直购（不进购物车）
    A->>B: POST /orders（recharge，Idempotency-Key）
    B->>O: CreateOrder（幂等键 (tenant,key)）
    O->>MM: 事务：INSERT mall_order=created
    A->>B: POST /orders/{no}/pay
    B->>O: PayOrder
    O->>P: CreatePayment（写 payment_records=created）
    P--)B: HandleCallback 异步回调（验签占位·幂等）
    B->>O: 回调确认
    O->>MM: 事务：payment_records=paid + status=paid<br/>+ INSERT fulfillment_tasks（outbox）
    W->>MM: 拉取 pending 履约任务
    W->>L: Credit（ref_type=mall，ref_id=order_no:line）
    L->>MA: 事务：topup 行 + ledger_balance（MySQL 提交点）
    L->>R: Redis 余额投影对齐
    L-->>W: ok（ref 已存在则 no-op）
    W->>MM: 行 done；全行 done → status=fulfilled
    W-->>A: 订单完成（App 轮询 /orders/{no} 可见）
    end

    rect rgb(235,255,240)
    note over A,MA: ② 用：推理按量扣费（billing 既有链路，不动）
    A->>G: POST /v1/chat/completions（Bearer API key，SSE）
    G->>L: Reserve（预扣 N）
    L->>R: Lua 原子「判余额≥N 再 DECRBY」（Redis 实时权威）
    G->>RT: 选路（熔断/failover）
    RT->>UP: HTTP/SSE 转发
    UP-->>G: 流式回复 + usage 帧
    G->>L: Settle（按实际用量结算）
    L->>MA: bills 定时批量落账（billing 进程执行，BILLING_LEDGER_BATCH_SECONDS）
    end
```

---

## 4. 服务流转二：混合购物车订单（general + 月卡，outbox 履约）

```mermaid
flowchart LR
    A["App 登录用户"] -->|"加购 general / 月卡<br/>上限20行·qty≤99"| CART["购物车 cart_items<br/>MySQL 持久化"]
    CART -->|"结算（行级价格快照）"| CREATE["mall-order 下单事务：<br/>mall_orders=created + 行快照<br/>+ 同库直扣 goods.stock"]
    CREATE -.->|"超时 30 分钟未支付<br/>或用户主动取消"| CLOSE["closed + general 行回补库存"]
    CREATE --> PAY["PaymentProvider<br/>CreatePayment"]
    PAY -->|"异步回调·验签·幂等"| TX["支付事务：status=paid<br/>+ payment_records<br/>+ fulfillment_tasks（outbox）"]
    TX --> W["履约 worker 按行执行<br/>行级幂等 ref=order_no:line"]
    W -->|"general 行"| F1["发货 mock → 行 done"]
    W -->|"subscription 行"| F2["billing ActivateSubscription<br/>（新增 RPC·幂等 ref·模拟月费校验）"]
    F1 --> DONE["全行 done → fulfilled"]
    F2 --> DONE
    style CREATE fill:#fff4e8
    style TX fill:#fff4e8
    style W fill:#e8f4ff
```

规则：recharge 充值包**不走此链路**（仅直购，见流转一）；秒杀商品也不走（见流转五）。

---

## 5. 服务流转三：月卡生效（额度窗口语义为 billing 既有；开通接口为新增）

```mermaid
sequenceDiagram
    autonumber
    participant A as App
    participant B as mall-bff
    participant O as mall-order
    participant W as 履约 worker
    participant L as billing

    A->>B: 月卡加购 → 混合结算 → 支付回调
    B->>O: 订单 paid + fulfillment_tasks（outbox）
    W->>L: ActivateSubscription（tenant, plan_id, ref=order_no:line）
    note over L: 新增 RPC：幂等 ref + 模拟月费校验<br/>（不接真钱）+ 落订阅记录<br/>手动续订 = 立即续一期
    L-->>W: 生效 → 行 done → fulfilled
    note over L: 额度语义（既有，不动）：<br/>每 5 小时滚动窗口各档 token 额度<br/>额度内调用 funding=subscription、pre=0<br/>超额 402 等刷新；不入账钱
    A->>B: GET /me/overview（余额+订阅额度余量）
```

---

## 6. 服务流转四：退款（D15 裁定）

```mermaid
sequenceDiagram
    autonumber
    participant A as App
    participant B as mall-bff
    participant O as mall-order
    participant L as billing
    participant M as MySQL market 库

    A->>B: POST /orders/{no}/refund
    B->>O: RefundOrder（幂等）
    alt 订单仍在 paid（钱未入账）
        O->>M: status=refunded（资金未动）
    else 已 fulfilled（钱已入账/已发货）
        O->>M: 逐行处置
        note over O,L: recharge 行：冲退 = min(行额度, 当前余额)<br/>走 billing 幂等冲退（ref 复用入账 ref）<br/>general 行：直接标记退回 + 回补库存<br/>subscription 行：不退费 = 退订语义（当期保留至窗口期满）
        O->>L: 冲退请求（幂等）
        L-->>O: ok
        O->>M: status=refunded
    end
```

---

## 7. 服务流转五：秒杀（充值包 + 普通商品）

```mermaid
flowchart TB
    REQ["抢购请求"] --> RL["mall-bff<br/>令牌桶限流 + 会话校验"]
    RL --> LUA["seckill：Redis Lua 原子<br/>判重（每人1件）+ 判库存 + 扣库存"]
    LUA -->|"售罄 / 重复"| FAIL["快速失败<br/>已抢完 / 不可重复抢购"]
    LUA -->|"成功"| KF["Kafka seckill.orders"]
    KF -->|"produce 后同步返回"| USER["用户收到：排队中 + 凭证=order_no"]
    KF --> CSM["秒杀订单消费者"]
    CSM -->|"落库（DB stock>0 条件更新兜底<br/>UNIQUE activity+tenant 防重）"| MY[("seckill_orders=pending<br/>+ mall_order=created")]
    USER -->|"15 分钟内支付"| PAY["正常支付回调 + outbox 履约链路<br/>recharge→Credit / general→发货"]
    USER -->|"超时 15 分钟未支付"| RB["延迟任务回补 Redis 库存<br/>+ 关闭订单（幂等）<br/>对账口径内唯一自动回补场景"]
    RECJ["对账 job<br/>Redis 余量 vs DB 计数"] -.->|"滞留默认保留待核查<br/>（沿用 reconciler 口径）"| MY
    style LUA fill:#ffe8e8
    style KF fill:#fff8e8
```

---

## 8. 服务流转六：App 会话与鉴权（billing 零改动，经 gateway 代理）

```mermaid
sequenceDiagram
    autonumber
    participant A as 鸿蒙 App
    participant B as mall-bff :18090
    participant G as gateway :18080
    participant L as billing :9103

    A->>B: POST /auth/register 或 /login（用户名+密码）
    B->>G: 转发 /auth/*
    G->>L: 代理（gateway 注入 X-Control-Token + 透传 cookie）
    L-->>G: 会话 token + tenant_id
    G-->>B: 同上
    B-->>A: 返回会话 token（App 本地保存）
    note over A,L: 之后每次商城请求：<br/>App 带 Authorization: Bearer 会话token<br/>mall-bff 经 gateway /auth/me 验证 → 派生 tenant
    note over A: API key 与登录无关：<br/>仅用于外部程序调 gateway /v1
```

---

## 9. 部署演进

```mermaid
flowchart LR
    subgraph DEV["R0-R2.5 开发期：Mac + docker-compose"]
        DC["deploy/compose<br/>profile: infra / ai / mall<br/>MySQL 3307 · Redis 6381 · Kafka 宿主 29092<br/>显式 GATEWAY_PORT=18080"]
    end
    subgraph K8S["R3 起：k3s 双机"]
        WIN["Windows 机 AMD64<br/>VMware VM 8G Ubuntu<br/>k3s server + 全量负载 + registry:2"]
        MAC["Mac M1<br/>开发 + 远程 kubectl<br/>演示时挂 arm64 agent"]
    end
    DEV -->|"buildx 双架构镜像<br/>compose 清单 → k8s manifests"| K8S
    style K8S fill:#f3fff3
```

K8s 验收七项（不再加戏）：多副本+反亲和 · HPA · 滚动更新 · 探针 · Prometheus/Grafana/Jaeger · Ingress · 节点 drain 自愈。

---

## 10. 图-文档映射

| 图 | 详细规格落点 |
|---|---|
| §1 全景架构 | PRD §3-§4 |
| §3 充值闭环（含回调+outbox） | spec/sequences.md#充值闭环（含错误分支） |
| §4 混合订单 | spec/sequences.md#混合订单 · spec/db-schema.sql |
| §5 月卡生效 | spec/sequences.md#月卡 |
| §6 退款 | spec/sequences.md#退款 |
| §7 秒杀 | spec/sequences.md#秒杀（含对账） |
| §8 会话鉴权 | spec/api-contract.md#鉴权 |
| §9 部署演进 | deploy/compose · deploy/k8s |
