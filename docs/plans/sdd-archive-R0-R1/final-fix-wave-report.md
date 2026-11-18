# 终审修复波报告（final-fix-wave-report.md）

**Status: DONE** — 分支 `feat/r0-r1-foundation`（基点 37fa17b），六项全部落地，build/vet/test 全绿，smoke-r1.sh 复跑 PASS，1 笔提交。

日期：2026-11-18。执行者：终审修复波 implementer（后台会话）。

---

## 逐项明细

### 1.（I-1）MarkPaidTx 金额不匹配分支 — `services/mall-order/payment.go`

**改了什么**：`MarkPaidTx` 事务内（已 `FOR UPDATE` 锁定订单行）幂等判定之后、置 paid 路径之前，新增"2.5) 金额校验"分支：`cb.Paid && cb.AmountCent != row.AmountCent` 时——
- 落 `payment_records=failed`（`amount_cent` 记回调金额；审计载荷 `callback_payload` 内嵌回调原文 + 新增 `order_amount_cent` 键记订单金额，`mismatchPayload` 结构，两金额均在案）；
- 订单停留 `created`，函数返回 `nil`（回调被受理但拒付，HTTP 回调 200）；
- 同 trade_no 的错额回调重放：流水已 failed，幂等 no-op 不重复落流水；
- 金额匹配才走既有路径（新号插 paid 流水 / 同号 failed 升级 paid + 条件更新 created→paid + 投递履约）。

同步更新 payment.go 包注释（三分支语义补第三条）。

**证据**：新增测试 `TestMarkPaidTx_AmountMismatchFailsPayment`（payment_test.go 第 4 节）：
- 首回调金额错（订单金额+1 分=6901 vs 6900）→ 返回 nil；订单停留 created；流水恰 1 条 status=3(failed)、amount=6901（回调金额）；payload 含 `"AmountCent":6901` 与 `"order_amount_cent":6900`（MySQL JSON 列回读带空格规范化，测试先去空格再比对）；fulfillment_tasks=0；
- 随后同 trade_no 正确金额重发 → failed 升级 paid（流水仍 1 条、status=2、amount=6900），订单 paid，履约任务按行投递 pending——恢复路径不被污染。
- 与既有 3 个 MarkPaidTx 测试（happy/dup-amount/verify-fail）同跑全绿。

```
=== RUN   TestMarkPaidTx_AmountMismatchFailsPayment
--- PASS: TestMarkPaidTx_AmountMismatchFailsPayment (0.04s)
PASS    ok  github.com/frostloom/my-market/services/mall-order  0.534s
```

### 2.（I-2）api-contract.md §0.4 幂等表 — `docs/spec/api-contract.md`

**改了什么**：下单行"同 key 同 payload 返回原单（**200**）"→"（**201，与首次一致——重放响应与首次创建同形**）"。消除与 §5（201）及实现/smoke（重发 201 且 order_no 一致）的内部矛盾（控制器已裁定 201）。

### 3.（I-3）.dockerignore — 仓库根新建

**改了什么**：排除 `ai-gateway/`、`.git/`、`.superpowers/`、`docs/`、`*.pdf`、`logs/`、`node_modules/`、`deploy/compose/.env`（密钥拦在上下文外）、`scripts/`；保留 `services/ pkg/ proto/ go.mod go.sum Makefile`（mall-service.Dockerfile 构建面）与 `deploy/`（dockerfile 本身在上下文内）；3 行注释说明用途。

**证据**：构建上下文 259MB 级（ai-gateway 234M + .git 23M + .superpowers 1.3M + docs/scripts/pdf）→ 剩余上下文 **536K**；重建 mall-order/mall-bff 镜像一次成功（上下文裁剪未破坏 COPY 面）。

### 4.（M-4）acceptance.md R1 回填 — `docs/spec/acceptance.md`

**改了什么**：R1 节 11 项全部 `[ ]`→`[x]`，节首加回填注记（同 R0 样式）：
"> 2026-11-18 回填：R1 已完成（11/11 勾选；5 项全实测——smoke-r1.sh 两遍 PASS + SQL 三表实证，6 项部分实测——取消/上限/超时/过期 token 等分支引用 T3-T6 单测报告证据；逐项核对表见 .superpowers/sdd/2026-11-18-R1-商城垂直切片/task-7-report.md）。"
逐项勾选依据 = task-7-report.md 核对表：#1/2/3/6/10 实测通过，#4/5/7/8/9/11 部分实测（分支引用 cart_store_test/order_store_test/payment_test/worker_test/timeout_test/mw_auth_test 等单测证据）。

### 5.（M-2）api-contract.md §3 Goods 字段 — `docs/spec/api-contract.md`

**改了什么**：GET /goods 的 Goods 字段清单补 `"category_id":..,"limited":bool`（proto GoodsItem field 15/16，mall-goods 实现已填充，文档漏列）。

### 6.（M-1+M-3）cart goods 保真 + cartLines 超时

**① mall-order 全形状**（`services/mall-order/order_store.go` + `server.go`）：
- `goodsRow` 补 `Subtitle / OrigPriceCent / ImageURL / RefPlanID` 四列（字段全集对齐 mall-goods 同名行；detail 富文本本服务不消费故不载）；
- `toProtoGoods` 映射补 `Subtitle / OrigPriceCent / ImageUrl / RefPlanId`（与 mall-goods 的 toProtoItem 同口径，CategoryId/Limited 原本已带）。
- bff 侧确认无需改：`handler_goods.go` 的 `toGoodsJSON` 已透传全部 16 字段（cart JSON 直接复用同一 goodsJSON 视图）。

**② cartLines 补超时**（`services/mall-bff/handler_order.go:132`）：`cartLines(c.Request.Context(), …)` → `context.WithTimeout(c.Request.Context(), rpcTimeout)`（5s，与其余下游调用同模式；下游卡死 5s 内 MALL_50002 出错）。

**证据（live，重建镜像后）**：注册→加购 #113→GET /cart，goods 行带全字段：
```json
{"id":113,"name":"重磅纯棉圆领 T 恤","subtitle":"260g 面料 | 不透不变形",
 "orig_price_cent":9900,"image_url":"/img/goods/113.png","ref_plan_id":null,
 "category_id":3,"limited":null}
```
（ref_plan_id=0 / limited=false 经 omitempty 出 null，与列表/详情口径一致。）

---

## 验证命令与输出（全绿）

```
$ gofmt -w services/mall-order/{order_store,server,payment,payment_test}.go services/mall-bff/handler_order.go
  # 顺带修正 order_store.go 既有结构体对齐（gofmt 强制）；cart_store.go 为 HEAD 既有未格式化项，未动（不属本波范围）

$ CGO_ENABLED=0 go build ./... && echo BUILD-OK
BUILD-OK

$ CGO_ENABLED=0 go vet ./... && echo VET-OK
VET-OK

$ CGO_ENABLED=0 go test ./services/... ./pkg/...
ok   github.com/frostloom/my-market/services/mall-bff    (cached)
?    github.com/frostloom/my-market/services/mall-gen/mallv1  [no test files]
ok   github.com/frostloom/my-market/services/mall-goods  (cached)
ok   github.com/frostloom/my-market/services/mall-order  1.528s
ok   github.com/frostloom/my-market/pkg/mallerr          (cached)
ok   github.com/frostloom/my-market/pkg/mallpage         (cached)
# 首跑曾红一次：新测试 payload 断言按紧凑 JSON 写死，MySQL JSON 列回读带空格规范化——已改去空格后比对，复跑绿

$ docker compose -f deploy/compose/docker-compose.yml build mall-order mall-bff
 Image my-market/mall-order:local Built
 Image my-market/mall-bff:local Built

$ docker compose -f deploy/compose/docker-compose.yml --profile mall up -d mall-order mall-bff
  # 三容器 healthy；mall-goods 无代码改动未重建

$ bash scripts/smoke-r1.sh
[PASS] × 22（健康 5 / detail / 注册登录品类翻页 3 页 43 条 / 详情 / cart 2 行合并 total=40600 /
        下单 201+直扣 721→718+同 key 重发原单 / pay→fulfilled 轮询 / 订单列表 / 取消闭环 330→329→330 /
        幂等负例 409 MALL_30003 三字段错误体）
SMOKE R1: PASS
```

## 范围与遗留

- 未动 ai-gateway/ 既有代码；未动 mall-goods / seckill / proto（无契约形状变化，无需 make proto）。
- `services/mall-order/cart_store.go` 在 HEAD 即未过 gofmt（const 块），不属本波六项，未动。
- 履约 kill-worker 真实容器演练、过期 token 实测等待——task-7 已如实标注"单测覆盖"，本波不重复。

## Commits

- `fix(mall): final review wave (amount mismatch, cart goods fields, contract+dockerignore)`（本报告不入仓——.superpowers/ 在 .gitignore）
