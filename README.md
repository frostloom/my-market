# my-market

> 一个「卖算力的商城」：Go 微服务商城 + AI 模型网关计费平台，跑通「**购买 → 入账 → 推理 → 计量 → 扣费**」的完整商业闭环。

[![status](https://img.shields.io/badge/R0--R2.5-已完成-green)](docs/spec/acceptance.md) [![smoke](https://img.shields.io/badge/SMOKE-R0%2FR1%2FR2%2FR2.5%20PASS-brightgreen)](scripts/) [![lang](https://img.shields.io/badge/Go-1.26-00ADD8)](go.mod) [![k8s](https://img.shields.io/badge/K8s-R3%20规划中-orange)](docs/PRD.md)

## 这是什么

用户在商城购买两类商品：

| 模式 | 买什么 | 钱去哪 | 怎么消费 |
|---|---|---|---|
| **按量计费** | 充值包（面值+赠送，分口径） | billing `Credit` 入账（topups 账本） | `/v1` 推理按各模型单价从余额扣（Reserve→Settle） |
| **月卡订阅** | 月卡（billing Plan：每 5 小时滚动窗口免费额度） | `ActivateSubscription` 开通/续期 | 窗口内免费（`funding=subscription`），超额 402 |

另有普通商品（手机/服饰/穿搭/家居）走标准电商链路（购物车→下单→模拟支付→发货）。退款语义：充值包按 `min(行额度, 当前余额)` 冲退；月卡**退订不退费**（当期权益保留）。

## 系统全景

```
客户端（curl/脚本；Web 双前端规划中 R6）
   │ REST + 会话
   ▼
mall-bff :18090 ──商城唯一入口（鉴权/限流/错误模型/支付编排）
   │ gRPC
   ├── mall-goods :9111   商品·品类·游标分页
   ├── mall-order :9112   购物车·订单状态机·支付回调·outbox 履约 worker
   │      └─ worker ──billing 加法 RPC──▶ billing :9101
   └──（R4：seckill :9113 秒杀）

gateway :18080 ──AI 网关数据面（OpenAI 兼容 /v1，SSE 流式，鉴权/幂等/限流）
   ├── billing :9101/9103  计费权威：余额预扣/结算/冲正/充值/订阅/对账
   │      └── 5 个加法 RPC（ADR-005）：Credit/Refund/Activate/CancelSubscription
   ├── router :9102        上游加权路由 + 熔断 + AutoBan
   │      └── mock-provider :9201（假上游，故障注入）
   └── agent :9105         AI 智能客服（执行层不信任 LLM）

基础设施（docker compose，deploy/compose/）：MySQL :3307（ai 库+market 库）· Redis :6381 · Kafka 容器 9092/宿主 29092
```

完整分层图与八张服务流转图（充值闭环/混合订单/月卡/退款/秒杀/会话/部署）：[docs/architecture.md](docs/architecture.md)

## 已验证的能力（对应冒烟脚本）

| 冒烟 | 证明了什么 |
|---|---|
| `scripts/smoke-r0.sh` | ai-gateway 迁移后推理计费链路完好：/v1 请求 → 账本恰 reserve+settle 两行 |
| `scripts/smoke-r1.sh` | 商城基础链路：注册→浏览→购物车→下单→支付回调→outbox 履约→取消/幂等/错误体 |
| `scripts/smoke-r2.sh` | **商业主线闭环**：买充值包→入账→造 API key→真实推理扣费→流水可见→**容器断电演练（topups 恰好一条，不重不漏）**→退款 min 口径 |
| `scripts/smoke-r2.5.sh` | 月卡全语义：混合单→**窗口内免费推理（余额不动）**→超额 402→退款退订不退费→退款后当期仍可用 |

## 仓库布局

```
docs/
  PRD.md                    产品需求 v0.5（决策清单 D1-D18 / 待决策 P 系列）
  architecture.md           架构与服务流转图（八图）
  spec/                     SDD 规格：api-contract（27 错误码全表）/ db-schema / sequences / acceptance / adr×10
  plans/                    实现计划 + SDD 执行台账归档（每个里程碑的完整审查记录）
services/ai-gateway/         既有 AI 网关（整仓迁入，模块路径不变；仅 5 个加法 RPC，内核零改动）
services/{mall-bff,mall-goods,mall-order}   商城三服务（根模块）
pkg/{mallerr,mallpage,importguard}          错误码/游标分页/跨模块 import 守卫
proto/mall/v1/              商城侧 gRPC 契约（make proto 生成）
deploy/compose/             docker compose（profiles: infra / ai / mall）
scripts/                    建库/种子/冒烟脚本
```

## 跑起来

```bash
cd deploy/compose && cp .env.example .env   # 按注释填令牌（openssl rand 生成）
docker compose --profile infra --profile ai --profile mall up -d   # 全家 11 容器
bash scripts/smoke-r2.5.sh                  # 端到端验证（可重复跑）
```

开发：`CGO_ENABLED=0 go build ./... && go test ./services/... ./pkg/...`（本机 SDK 兼容性要求 CGO 关闭）。

## 开发过程

规格驱动（SDD）：PRD → 规格五件套 → 任务计划 → 子代理逐任务实现+独立审查+修复循环 → 整分支终审（opus）→ 合并。每个里程碑的完整裁定与审查记录归档在 `docs/plans/sdd-archive-*/`。关键架构决策见 [ADR 目录](docs/spec/adr/)——服务边界、订单分域、outbox 履约、billing 加法原则、支付抽象三档、秒杀三道防线等。

## 路线图

- ✅ R0 迁移与基建 · R1 商城垂直切片 · R2 充值闭环 · R2.5 月卡
- 🔜 R3 K8s 化（k3s 双机、多副本、HPA、可观测进集群）与 R6 Web 双前端（Vue3 双入口，mall-bff 托管）——可并行
- 🔜 R4 秒杀 + 压测（Redis Lua 扣减、Kafka 削峰、对账 job、带数字的压测报告）
- 远期 P5：真实支付接入（Stripe test → 个体工商户资质 → 微信/支付宝）

## 致谢

商城域构建于既有的 [ai-gateway](services/ai-gateway/)（OpenAI 兼容多租户模型网关 + 计费平台）之上——计费内核（Redis 原子预扣、幂等结算、对账收敛）一行未动，只做加法。
