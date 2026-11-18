# 里程碑验收清单（spec/acceptance.md）

> 每项可执行、可判定（命令级）；全绿才算里程碑完成。引用：[PRD.md](../PRD.md) §13。

---

## R0 迁移

- [ ] `ls my-market/ai-gateway/cmd` 与旧仓一致（7 个服务入口 + 工具齐全）；eval/、docs/、web/、scripts/ 已在
- [ ] 旧仓根 `.git`、logs/、.gocache/ **未**迁入（`du -sh my-market/ai-gateway` 无 GB 级目录）
- [ ] `go.work` 含两个模块：`github.com/frostloom/my-market` + `github.com/frostloom/ai-gateway`；`go build ./...` 两模块均零错误
- [ ] `docker compose -f deploy/compose/docker-compose.yml --profile infra up -d` 起 MySQL 3307 / Redis 6381 / Kafka（宿主 29092 可 `kcat -L -b 127.0.0.1:29092`）
- [ ] `docker compose --profile ai up -d` 起 gateway 全家（compose 显式 `GATEWAY_PORT=18080`）
- [ ] `for p in 18080 9101 9103 9102 9105; do curl -sf localhost:$p/healthz; done` 全部 200
- [ ] 冒烟：注册租户 → curl `POST :18080/v1/chat/completions`（Bearer key）→ billing 查账出现 reserve+settle 两行
- [ ] 旧 GitHub 仓已归档，README 顶部指向 my-market；新仓无 .env/密钥（`git log --all --diff-filter=A -- '*.env'` 为空）

## R1 商城切片（goods/cart/order/bff）

- [ ] `deploy/compose` 新增 `--profile mall`（bff 18090 / goods 9111 / order 9112）一键起
- [ ] market 库按 db-schema.sql 建表（6 品类种子存在）
- [ ] GET /categories、/goods 分页（游标翻页到底返回 next_cursor=null）、/goods/{id} 通过契约（api-contract §3）
- [ ] 购物车 upsert/改量/删除 + 20 行/qty≤99 上限生效（超限返回 MALL_20004/20005）
- [ ] general 下单：同库直扣库存；库存不足事务回滚返回 MALL_20003（`SELECT stock` 不变验证）
- [ ] 幂等：同 Idempotency-Key 同 payload 重发返回原单；异 payload 返回 MALL_30003
- [ ] 支付：mock Provider 异步回调 → paid + payment_records + fulfillment_tasks 同事务（`SELECT` 三表一致）；重复回调幂等
- [ ] 履约：general 行 worker 执行后 fulfilled；人为 kill worker 重启后任务续跑（outbox 生效）
- [ ] 超时：造 31 分钟前 created 订单 → 扫描后 closed(timeout) + 库存回补；主动取消同理 closed(user)
- [ ] 错误模型：全部非 2xx 返回 `{error, code, retryable}`，码表与 api-contract §1 一致
- [ ] mall-bff 鉴权：无 token 401（MALL_10001）；过期 token 401（MALL_10002）；per-tenant 120 req/min 触发 429（MALL_10006）

## R2 充值闭环

- [ ] billing 新增 Credit gRPC（billing.proto v1 扩展）：单测覆盖「正常入账 / ref 重复 no-op / 金额为正校验」
- [ ] 旧 403 端点**未被打开**（`curl POST :18080/portal/recharge` 仍 403）
- [ ] recharge 商品直购：下单→支付→worker Credit→fulfilled；/me/overview 余额 = 面值+赠送
- [ ] 闭环主线：买包入账 → `POST :18080/v1/chat/completions`（同租户 key）→ 余额扣减 → admin topups 出现 ref_type=mall 流水
- [ ] 断电演练：支付后立刻 kill -9 mall-order → 重启后 worker 补 Credit，topups 恰好一条（幂等验证）
- [ ] 退款：paid 直退（无流水）；fulfilled 后退 min(行额度,余额)；消费部分余额后冲退金额 = min 验证

## R2.5 月卡

- [ ] billing 新增 ActivateSubscription gRPC：幂等 ref、模拟月费校验、已有订阅=续一期（单测三分支）
- [ ] 混合单（general+月卡）支付后：general 行发货 done、月卡行订阅生效、订单 fulfilled
- [ ] 月卡生效后推理：窗口额度内 funding=subscription、余额不动（/me/overview 验证）；超额 402
- [ ] 退款：混合单 fulfilled 退款 → general 行回补、月卡行退订语义（当期保留、不退费）

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
- [ ] 正确性：1000 并发 × N 用户抢 M 件 → Redis 余量=0、DB 订单数=M、无重复（唯一索引零冲突确认无超卖）
- [ ] 失败分支：售罄 MALL_40002、重复 MALL_40003、令牌桶 MALL_40004、未开始/结束 MALL_40001
- [ ] 超时回补：15 分钟未支付 → Redis INCR 回补幂等（回补一次）
- [ ] 对账 job：人为造滞留（停消费者）→ 差异记录 + 保留待核查（不自动回补）
- [ ] 压测报告：秒杀热路径 QPS/P99、商品列表 P99、下单 P99、HPA 扩容曲线（R3 就绪后补测）落 docs/plans/ 或 docs/ 报告

## R5（已随鸿蒙作业取消移除，2026-11-18）

原"鸿蒙 App + 作业四件套"冲刺不再交付；商城以 API + 冒烟脚本形态演示。
