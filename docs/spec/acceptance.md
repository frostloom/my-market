# 里程碑验收清单（spec/acceptance.md）

> **ai-gateway 全量测试门禁口径（R2.6 固化）**：空闲机 + `CGO_ENABLED=0` 下，失败**名集合**与基线逐名恒等（taskbench 失败计数随机器负载漂移，空闲 ~80/满载可降至 ~17——只比集合不比数字；判据=「0 个基线之外的新增失败」）。

> 每项可执行、可判定（命令级）；全绿才算里程碑完成。引用：[PRD.md](../PRD.md) §13。

---

## R0 迁移

> 2026-11-18 回填：R0 已完成（7/8 勾选；第 8 项旧仓归档=待用户操作，密钥子项已验通过）。

- [x] `ls my-market/ai-gateway/cmd` 与旧仓一致（7 个服务入口 + 工具齐全）；eval/、docs/、web/、scripts/ 已在（该目录 R2.6 结构归位至 `my-market/services/ai-gateway/`，模块路径不变）
- [x] 旧仓根 `.git`、logs/、.gocache/ **未**迁入（`du -sh my-market/ai-gateway`——R2.6 后为 `du -sh my-market/services/ai-gateway`——无 GB 级目录）
- [x] `go.work` 含两个模块：`github.com/frostloom/my-market` + `github.com/frostloom/ai-gateway`；`go build ./...` 两模块均零错误
- [x] `docker compose -f deploy/compose/docker-compose.yml --profile infra up -d` 起 MySQL 3307 / Redis 6381 / Kafka（宿主 29092 可 `kcat -L -b 127.0.0.1:29092`）
- [x] `docker compose --profile ai up -d` 起 gateway 全家（compose 显式 `GATEWAY_PORT=18080`）
- [x] `for p in 18080 9101 9103 9102 9105; do curl -sf localhost:$p/healthz; done` 全部 200（注：实际契约修正为 9103/9102/9105 /healthz + 18080 GET / + 9101 nc，见 R0 台账裁定）
- [x] 冒烟：注册租户 → curl `POST :18080/v1/chat/completions`（Bearer key）→ billing 查账出现 reserve+settle 两行
- [ ] 旧 GitHub 仓已归档，README 顶部指向 my-market；新仓无 .env/密钥（密钥子项已验通过；**旧仓归档=待用户操作**）

## R1 商城切片（goods/cart/order/bff）

> 2026-11-18 回填：R1 已完成（11/11 勾选；5 项全实测——smoke-r1.sh 两遍 PASS + SQL 三表实证，6 项部分实测——取消/上限/超时/过期 token 等分支引用 T3-T6 单测报告证据；逐项核对表见 .superpowers/sdd/2026-11-18-R1-商城垂直切片/task-7-report.md）。

- [x] `deploy/compose` 新增 `--profile mall`（bff 18090 / goods 9111 / order 9112）一键起
- [x] market 库按 db-schema.sql 建表（6 品类种子存在）
- [x] GET /categories、/goods 分页（游标翻页到底返回 next_cursor=null）、/goods/{id} 通过契约（api-contract §3）
- [x] 购物车 upsert/改量/删除 + 20 行/qty≤99 上限生效（超限返回 MALL_20004/20005）
- [x] general 下单：同库直扣库存；库存不足事务回滚返回 MALL_20003（`SELECT stock` 不变验证）
- [x] 幂等：同 Idempotency-Key 同 payload 重发返回原单；异 payload 返回 MALL_30003
- [x] 支付：mock Provider 异步回调 → paid + payment_records + fulfillment_tasks 同事务（`SELECT` 三表一致）；重复回调幂等
- [x] 履约：general 行 worker 执行后 fulfilled；人为 kill worker 重启后任务续跑（outbox 生效）
- [x] 超时：造 31 分钟前 created 订单 → 扫描后 closed(timeout) + 库存回补；主动取消同理 closed(user)
- [x] 错误模型：全部非 2xx 返回 `{error, code, retryable}`，码表与 api-contract §1 一致
- [x] mall-bff 鉴权：无 token 401（MALL_10001）；过期 token 401（MALL_10002）；per-tenant 120 req/min 触发 429（MALL_10006）

## R2 充值闭环

> 2026-11-19 回填：R2 已完成（6/6 实测 PASS，证据表见 docs/plans/sdd-archive-R2/R2-ledger.md 与 task-4 报告；SMOKE R2 两遍 PASS）。

- [x] billing 新增 Credit gRPC（billing.proto v1 扩展）：单测覆盖「正常入账 / ref 重复 no-op / 金额为正校验」
- [x] 旧 403 端点**未被打开**（`curl POST :18080/portal/recharge` 仍 403）
- [x] recharge 商品直购：下单→支付→worker Credit→fulfilled；/me/overview 余额 = 面值+赠送
- [x] 闭环主线：买包入账 → `POST :18080/v1/chat/completions`（同租户 key）→ 余额扣减 → admin topups 出现 ref_type=mall 流水
- [x] 断电演练：支付后立刻 kill -9 mall-order → 重启后 worker 补 Credit，topups 恰好一条（幂等验证）
- [x] 退款：paid 直退（无流水）；fulfilled 后退 min(行额度,余额)；消费部分余额后冲退金额 = min 验证

## R2.5 月卡

> 2026-11-19 回填：R2.5 已完成（4/4 达成，证据见 docs/plans/sdd-archive-R2.5/R2.5-ledger.md 与 task 报告；SMOKE R2.5 三轮 PASS，含免费窗口/超额 402/退订不退费）。

- [x] billing 新增 ActivateSubscription gRPC：幂等 ref、模拟月费校验、已有订阅=续一期（单测三分支）
- [x] 混合单（general+月卡）支付后：general 行发货 done、月卡行订阅生效、订单 fulfilled
- [x] 月卡生效后推理：窗口额度内 funding=subscription、余额不动（/me/overview 验证）；超额 402
- [x] 退款：混合单 fulfilled 退款 → general 行回补、月卡行退订语义（当期保留、不退费）

## R2.6 仓库结构统一（计划外里程碑，2026-11-19）

- [x] ai-gateway 整体迁入 services/（17,075 文件 100% rename，零语义改动；门禁三连绿）
- [x] eval/ 提升为顶层统一资产（15,682 rename + 三类引用锚定 + Node 工具链修复 + 资产规约 README）
- [x] 测试门禁口径固化（空闲机+CGO=0+失败名集合恒等，见头部）+ 冻结证据 .gitattributes 上移根（审计修复）

## R3 K8s 化（七项逐一验收）

- [ ] Win 机 k3s server 就绪；Mac `kubectl get nodes` 双节点（演示时）
- [ ] 全部镜像 buildx 双架构（amd64+arm64）推送 registry:2；无 ImagePullBackOff
- [ ] 无状态服务 ≥2 副本 + Pod 反亲和（`kubectl get pods -o wide` 跨节点分布）
- [ ] HPA：vegeta 压 mall-bff → 副本自动扩容并回缩（`kubectl get hpa -w` 记录曲线）
- [ ] 滚动更新：新版本镜像替换零 5xx（压测中滚动，错误率=0）
- [ ] 探针：就绪未过不入流量；杀进程自动重启
- [ ] Prometheus+Grafana+Jaeger 进集群：Grafana 有商城分域面板；Jaeger 可查 mall-bff→order→billing 链路
- [ ] Ingress 统一入口；节点 drain 演录（Pod 漂移业务不中断）

## R4 秒杀+压测

- [ ] 活动预热脚本：Redis 库存键 = total - sold；DB 计数一致
- [ ] 正确性：**10000 并发 × N 用户抢 100 件** → Redis 余量=0、DB 订单数=100、无重复（唯一索引零冲突确认无超卖）；压测客户端须复用连接，服务端 `kern.ipc.somaxconn` 调至 ≥1024（见 R4 设计文档 §10.3）；若本机跑不动，可降级为「10000 请求总量、1000 并发」并在报告写明实际参数
- [ ] 失败分支：售罄 MALL_40002、重复 MALL_40003、令牌桶 MALL_40004、未开始/结束 MALL_40001
- [ ] 超时回补：15 分钟未支付 → Redis INCR 回补幂等（回补一次）
- [ ] 对账 job：人为造滞留（停消费者）→ 差异记录 + 保留待核查（不自动回补）
- [ ] 压测报告：秒杀热路径 QPS/P99、商品列表 P99、下单 P99、HPA 扩容曲线（R3 就绪后补测）落 docs/plans/ 或 docs/ 报告

## R5（已随鸿蒙作业取消移除，2026-11-18）

原"鸿蒙 App + 作业四件套"冲刺不再交付；客户端改为 Web 双前端（v0.5）。

## R6 双前端（v0.5 新增；详细验收随 admin 契约 spec 增补冻结后细化）

- [ ] spec 增补：api-contract.md §9 管理端契约冻结（含鉴权方案裁定）
- [ ] web/ 工程（Vue3+Vite+TS 双入口 mall-user/mall-admin）建立，设计系统 tokens 移植
- [ ] mall-bff go:embed 托管前端产物（同 gateway 模式）
- [ ] 用户端页面化全链路：登录→品类/列表（游标翻页+到底提示）→详情→购物车→下单→模拟支付→我的订单→充值包直购
- [ ] 管理端：商品增改/上下架、订单查询/退款处置、秒杀活动创建/启停
- [ ] 冒烟：smoke-r6.sh 覆盖两端关键路径
