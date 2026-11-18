# Task 4 报告：/me/overview + 闭环冒烟 + R2 验收（R2 收官）

> 分支 `feat/r2-recharge`，基线 72c47ba → **f24e912**（feat(compose)）→ **83e5681**（feat(mall-bff)）。日期 2026-11-19。环境 CGO_ENABLED=0，测试串行。

## 结论

- **TDD**：RED（新增 handler_me_test.go 13 个测试全编译失败）→ GREEN（handler_me.go 实现 + router/main 接线）→ 全绿。
- **compose 注入 + 重建**：mall-order / mall-bff 注入 BILLING_ADDR=billing:9101、BILLING_ADMIN_ADDR=http://billing:9103（仅 bff）、CONTROL_PLANE_TOKEN=${CONTROL_PLANE_TOKEN:?}；镜像重建并 recreate，四容器 healthy。
- **smoke-r2.sh 实跑两遍 PASS**（见台账），末行 `SMOKE R2: PASS`。
- **acceptance R2 全部 6 项实测通过**（逐项见下表）；未改 acceptance.md（回填由控制器）。
- 全仓测试绿：my-market 全模块 ok；ai-gateway billing 相关包全 ok 无回归（全量 `go test ./...` 仅剩一个 **R2 前既有**的 taskbench 失败，已用 pre-T1 基线 worktree 复现证明非本里程碑引入，见"测试证据"）。

## 交付物

| 文件 | 动作 | 内容 |
|---|---|---|
| `services/mall-bff/handler_me.go` | 新增 | GET /api/v1/me/overview：会话租户 → billingv1.GetBalance（薄拨号 DialBilling，BILLING_ADDR 缺省 127.0.0.1:9101，每请求 X-Control-Token metadata）+ adminTopupsClient（BILLING_ADMIN_ADDR 缺省 http://127.0.0.1:9103，X-Control-Token 头）→ `{balance_cent, recent_topups[≤10], subscription:null(注释 R2.5)}`。billing 两侧接口化（BillingClient / TopupsSource），空 token fail-closed。响应顶层恰三键；recent_topups 空数组非 null |
| `services/mall-bff/handler_me_test.go` | 新增 | 形状断言（三键/≤10 条/空数组非 null/subscription null/租户传播/未登录 401 MALL_10001/下游故障 503 MALL_50003）+ **凭据零泄漏断言**（真链路：DialBilling→127.0.0.1:0 桩 gRPC server + adminTopupsClient→httptest admin server，token 恰经 X-Control-Token 到达两侧下游，响应体与 slog 日志零泄漏）+ DialBilling/newAdminTopups 空 token fail-closed |
| `services/mall-bff/router.go` / `main.go` / `router_test.go` | 修改 | NewApp 增 billing/topups 两参数（测试桩注入）；authed 组注册 `/me/overview`；main 拨号 + Close 钩子 + env 注释 |
| `scripts/smoke-r2.sh` | 新增 | R2 闭环冒烟（详见台账），幂等可重跑（随机用户名/幂等键，SQL 仅 upsert/条件 UPDATE） |
| `deploy/compose/docker-compose.yml` | 修改 | mall-order/mall-bff 注入（裁定 2）；mall-order 加 BILLING_ADDR + 两服务加 CONTROL_PLANE_TOKEN，bff 另加 BILLING_ADMIN_ADDR |
| `go.mod` / `go.sum` | 修改 | **brief 外必要修正**（见"偏离"）：require ai-gateway + `replace => ./ai-gateway`（billingv1 是 mall 唯一 ai-gateway import 面）；tidy 后 go-sql-driver 1.8.1→1.10.0、edwards25519 1.1.0→1.2.0、miniredis/gin 转 direct（与 go.work workspace 解析结果一致，非行为回退） |
| `deploy/docker/mall-service.Dockerfile` | 修改 | **brief 外必要修正**（同上）：COPY ai-gateway/go.mod + ai-gateway/pkg/proto/billing（pruned graph 不需要 ai-gateway/go.sum；无新增下载依赖） |
| `.dockerignore` | 修改 | 从整目录排除 `ai-gateway/` 中精确放行 `!ai-gateway/go.mod`、`!ai-gateway/pkg/proto/billing`（KB 级） |

## 发现与裁定（重要）

1. **billing 容器也是旧镜像**（brief 只点名 mall 三容器）：billing 容器 9 小时前构建，proto 无 Credit/Refund → 首轮 smoke 步骤 4 卡 paid，worker 退避 5 次转 failed，`fulfillment_tasks.last_error = "unknown method Credit for service billing.v1.BillingService"`。处置：`docker compose --profile ai build billing && up -d billing`（R1 阶段构建镜像早于 T1 提交，属"代码未进容器"同款缺口）。smoke 前置健康随即加了 billing 容器镜像 = tag 校验 + 前置注释要求重建 billing。
2. **余额投影最终一致，账本才是权威**：billing `Credit/Refund` 同步写 MySQL（topups + tenants.ledger_balance），Redis 投影由 Kafka relay 异步追赶（实测滞后数十秒）→ /me/overview 的 GetBalance（投影口径）在入账后短时间内可能仍显示旧值。smoke 据此设计：**钱相关断言一律以 `ai_gateway.tenants.ledger_balance`（SQL 直读）为准，overview 只断言"投影与账本收敛"**（wait_converged 轮询 ≤60s）。这是 billing 既有架构（投影+重建兜底），非 mall 侧缺陷；前端语义上"充值后余额稍晚刷新"属可接受现状（R4 对账/重建兜底）。
3. **paid 直退竞态**：pay 后 mock 回调 ~200ms 才落 paid；"pay 后立即 refund"会撞 created → 409 MALL_30002。smoke 改为 pay → 轮询至 paid（≤8s）→ 立刻 refund，窗口内 worker（10s tick）不会抢跑；若极端抢跑 fulfilled，冲退 min 同样回满，脚本以"余额不动"端态不变量断言（两路径一致），实测走的是 paid 直退路径。
4. **admin topups 的 limit 参数不被 billing 支持**：端点固定回 50 条（admin.go 硬编码）。bff 客户端透传 limit=10（前向兼容）并在客户端截断 ≤10 + handler 出口二次截断兜底。

## smoke-r2.sh 台账（两遍 PASS）

关键断言序列（两轮一致）：

- 前置健康：bff/gateway/billing-admin healthz 200；billing + mall 三容器 healthy 且容器镜像 = 本地 tag（防"重建 tag 未重建容器"漂移）；mall-order/mall-bff 容器内 BILLING_ADDR / BILLING_ADMIN_ADDR / CONTROL_PLANE_TOKEN 注入在位（token 值不回显）。
- provider 重指 + mock-model SKU（smoke-r0 同款幂等 SQL）。
- 注册随机用户名 → overview `{balance_cent:0, recent_topups:[], subscription:null}`。
- goods#201（¥10→面值1000+赠0）直购 → pay → fulfilled → **overview 余额=1000 且与账本收敛**。
- 会话 cookie（agw_user=session_token）经 gateway `POST /portal/api-keys` 造 key → `POST :18080/v1/chat/completions`（Bearer key）200 chat.completion → **账本余额 1000→999，overview 收敛 999**（recent_topups 1 条 ≤10）。
- **断电演练**：第二笔 pay 后 kill 前 mall 流水=0 → `docker compose kill -s SIGKILL mall-order` + up -d → healthy → worker 补 Credit → fulfilled → **billing topups 该单 ref 恰 1 条**（uk_ref 不重不漏）→ 余额 = 消费后+1000。
- **退款示例**：
  - paid 直退（第四笔，paid 状态时退款）→ 200 refunded，**账本余额不动**（无 billing 流水）；
  - fulfilled 冲退（第三笔）→ **refund_cent = min(行额度 1000, 退款前余额 2999) = 1000**，账本 2999→1999，overview 收敛 1999，billing topups 冲退流水（ref_type=refund, ref_id=mall-refund:...）恰 1 条。
- 末行 `SMOKE R2: PASS`（两遍，exit 0）。

## acceptance R2 逐项核对（结果表；acceptance.md 未改动，回填由控制器执行）

| # | acceptance R2 项 | 结果 | 证据 |
|---|---|---|---|
| 1 | billing 新增 Credit gRPC（proto v1 扩展）单测覆盖「正常入账 / ref 重复 no-op / 金额为正校验」 | PASS（T1 交付，本轮复核） | `ai-gateway/internal/billing/server/credit_test.go`：TestCredit_HappyPath_TopupAndBalanceConsistent / TestCredit_DuplicateRef_NoOp / TestCredit_NonPositiveAmount_Rejected（另 TestCredit_CrossTenantRef_FailsConsistently、TestRefund_MinRuleAndIdempotent）本轮 -run 实跑 5/5 PASS；billing store 包全量 PASS |
| 2 | 旧 403 端点未被打开（`curl POST :18080/portal/recharge` 仍 403） | PASS（口径注记） | 无会话裸 curl → 401（gateway 会话门禁先拦）；**带有效会话 cookie → 403** `{"error":"充值入口暂时关闭，请使用余额兑换码"}`。403 语义在会话层之后保持关闭 |
| 3 | recharge 直购：下单→支付→worker Credit→fulfilled；/me/overview 余额 = 面值+赠送 | PASS | smoke 步骤 4（两遍）：goods#201 面值1000+赠0 → overview 余额=1000 且账本一致；账本口径与投影收敛均验 |
| 4 | 闭环主线：买包入账 → POST :18080/v1/chat/completions（同租户 key）→ 余额扣减 → admin topups 出现 ref_type=mall 流水 | PASS | smoke 步骤 4-5（两遍）：portal 造 key（agw_user cookie）→ chat 200 → 账本 1000→999 → admin topups 见 RefType=mall（ref_id=order_no:1） |
| 5 | 断电演练：支付后立刻 kill -9 mall-order → 重启后 worker 补 Credit，topups 恰好一条 | PASS | smoke 步骤 6（两遍）：pay 后 SIGKILL（kill 前流水 0 条）→ 容器重启 healthy → fulfilled → topups 该单 ref 恰 1 条 |
| 6 | 退款：paid 直退（无流水）；fulfilled 后退 min(行额度,余额)；消费部分余额后冲退金额 = min 验证 | PASS | smoke 步骤 7a：paid 状态退款 200 refunded、账本余额不动（无 billing 流水）；7b：消费后（账本 2999）冲退 → API refund_cent = min(1000, 2999) = 1000，账本 2999→1999，冲退流水恰 1 条。min 钳制分支（余额<行额度）由 billing TestRefund_MinRuleAndIdempotent / TestRefundCappedAtBalance 单测覆盖（mall-order refund_credit_test.go 同覆盖），smoke 环境余额始终 ≥ 行额度故实测值为满额 min |
| — | R2 完成定义：acceptance R2 全绿 + SMOKE R2: PASS + 全仓测试绿（含 ai-gateway 无回归） | 达成（注记 taskbench 既有失败） | 本报告 + 两遍 smoke PASS；my-market 全绿；ai-gateway billing 相关全绿无回归，唯一失败 taskbench TestActualV7CollectorClosedNative 经 pre-T1 基线 worktree 复现证明为 R2 前既有（见"测试证据"） |

## 测试证据

- mall-bff：`CGO_ENABLED=0 go test ./services/mall-bff/` ok（新增 13 个 overview/billing 客户端测试全绿；既有 8 组测试无回归）。
- my-market 全仓：`CGO_ENABLED=0 go test -count=1 ./...` 全 ok（mall-order 15.8s 内含 credit/refund 并发与断电重放测试）。
- ai-gateway：`CGO_ENABLED=0 go build ./...` 零错误；billing server/store/admin/outbox/consumer 全 ok（无回归）。
- **ai-gateway 全量 `go test ./...` 有一个非回归失败**：`internal/taskbench` 的 TestActualV7CollectorClosedNative（modelEOF/jevEOF/negative/fact 四子测试，"provider failure retried or released"）。已验证为 **R2 前既有问题**：在 pre-T1 基线 9d5370e（R2 动工前，billing Credit/Refund 落地之前）临时 worktree 中单跑该测试同样 FAIL（且 fae5f5b 同样 FAIL）；该失败与 billing proto 扩展/Credit 行为无因果（T1-T4 均未触碰 taskbench）。R4 前应由 ai-gateway 侧修复（不属本任务范围）。
- 容器实证：四容器 healthy，镜像 ID 与本地 tag 一致（b464c7033cd7 / 7b5a464bf763 / edb4b40036a0 / 171b113054aa）；live container overview 返回契约形状 `{"balance_cent":0,"recent_topups":[],"subscription":null}`。

## 偏离 brief 的两点（必要修正，已验证）

1. brief 称 "mall-service.Dockerfile 不变"——不成立：T2/T3 起 mall-order（本任务起 mall-bff）import `ai-gateway/pkg/proto/billing/v1`，而根 go.mod 无该依赖、镜像 GOWORK=off 单模块构建 → 容器内构建必然失败（`no required module provides package`）。最小修正：go.mod require+replace `./ai-gateway`；Dockerfile COPY ai-gateway/go.mod 与 pkg/proto/billing；.dockerignore 放行两路径。workspace 构建不受影响（go.work 优先）。
2. brief 称 compose 只注入 mall 两容器——实际 billing 容器同样需要重建（见"发现 1"），否则闭环卡 Credit Unimplemented。已并入 smoke 前置健康校验。

## Concerns（移交后续轮次）

1. **投影滞后感知**：/me/overview 充值后余额可能数十秒不刷新（billing Kafka relay 异步投影）。契约未冻结刷新时序；R4 对账/重建兜底落地前，前端可按"最终一致"提示。如需即时性，可后续在 billing Credit 返回后由调用方触发定向投影刷新（需 billing 侧加接口，超出本任务）。
2. **ai-gateway taskbench 既有失败**（非 R2 回归，见测试证据）：TestActualV7CollectorClosedNative 在 pre-T1 基线即 FAIL；ai-gateway 侧后续轮修复。
3. R2.5（subscription 字段）接线点已在 handler 注释标明（`"subscription": nil`）。
4. 首轮 smoke 产生的 failed 履约任务行（billing Unimplemented 时期，task id 429）留库：`fulfillment_tasks` 1 行 status=failed，属 R1 镜像期产物；R4 对账 job 落地时可清。不影响本任务断言（每轮随机新租户）。
