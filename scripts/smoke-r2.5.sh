#!/usr/bin/env bash
# my-market R2.5 月卡订阅闭环冒烟（scripts/smoke-r2.5.sh）：
# mall-bff ↔ mall-order ↔ billing ↔ gateway 全链路（加购→支付→开通→额度内免费
# 推理→超额 402→退款=退订不退费）。
#
# 前置：
#   deploy/compose 三 profile（infra + ai + mall）已起且 healthy——
#     docker compose -f deploy/compose/docker-compose.yml \
#       --profile infra --profile ai --profile mall up -d
#   且容器镜像为 R2.5 版（mall-order 含订阅 qty=1 限购 + 订阅行退订退款；
#   billing 含 ActivateSubscription/CancelSubscription RPC）——改动 mall 侧后先
#     docker compose --profile mall build mall-order && \
#     docker compose --profile mall up -d mall-order
#   market 库种子就绪（bash scripts/schema-apply.sh，先 scripts/seed-plans.sql
#   后 scripts/seed-goods.sql——301 上架且 ref_plan_id 关联在售定期套餐）。
#
# 步骤（acceptance R2.5 + brief Step 2 顺序）：
#   1) 前置健康：bff/gateway/billing-admin healthz + mall/billing 四容器
#      healthy 且镜像 = 本地同名 tag + BILLING_ADDR/CONTROL_PLANE_TOKEN 注入
#   2) 幂等造数（smoke-r0/r2 同款）：providers mock-a/mock-b 重指
#      http://mock-provider:9201 + 清熔断计数；目录补 mock-model SKU（status=0，
#      tags 轻量档）
#   3) 注册（随机用户名）→ /me/overview 余额=0、recent_topups=[]、subscription=null
#   4) 订阅限购守卫：POST /cart/items goods=301 qty=2 → 400 MALL_20005（车空）
#   5) 加购月卡 301×1 + general 113×1 → cart 两行 → 结算（kind=cart from_cart）
#      → pay → 轮询 fulfilled（worker 10s tick，≤40s）→ 订阅 active+auto_renew=1
#      （ai_gateway.subscriptions）+ mall-sub 0 元锚恰 1 条
#   6) 会话 cookie 造 API key → 窗口内 /v1/chat/completions 免费：
#      200 + 账本余额不动（0）+ bills 出现 funding='subscription' 流水
#   7) 超额 402：同 key 一次 max_tokens=9999999（> 套餐全档窗口额度）→
#      402（RESERVE_SUB_QUOTA；余额/流水零变更）
#   8) 退款（D15 退订不退费）：POST /orders/{no}/refund → 200 refunded 且
#      refund_cent = general 行金额（订阅行金额不进退款额）→ 账本余额仍 0
#      （不退费）→ 订阅行 status=cancelled、auto_renew=0（当期权益保留至期满）
#      → 退款后再来一次窗口内 /v1 调用仍 200 免费（当期保留的行为面证据）
#      → billing 零 refund 冲退流水（general 直退/订阅退订不走 Refund 通道）
#   9) overview 的 subscription 字段存在（bff 未透传订阅详情，恒 null——契约
#      字段点亮属 R6 前端面，本步只验字段在位）
#  末行输出 SMOKE R2.5: PASS / FAIL。
#
# 幂等可重跑：每轮随机用户名/幂等键（新租户全新余额与订阅）；SQL 仅 upsert/
# 条件 UPDATE；订阅行断言按 tenant_id 精确过滤。
# 凭据：CONTROL_PLANE_TOKEN / MYSQL_ROOT_PASSWORD 只读 deploy/compose/.env，
#       不回显、不落日志；billing admin 查询经 X-Control-Token 头。
set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ENV_FILE="$ROOT/deploy/compose/.env"
HOSTADDR="localhost" # 双栈回退口径，见 smoke-r0.sh 顶注
BASE="http://$HOSTADDR:18090/api/v1"
GATEWAY="http://$HOSTADDR:18080"
BILLING_ADMIN="http://$HOSTADDR:9103"
RUN_TAG="$(date +%s)-$RANDOM"

FAILS=0
DC=(docker compose -f "$ROOT/deploy/compose/docker-compose.yml")

pass() { printf '[PASS] %s\n' "$1"; }
fail() { printf '[FAIL] %s\n' "$1"; FAILS=$((FAILS + 1)); }
info() { printf '[....] %s\n' "$1"; }

# ---------- 依赖与凭据 ----------
if [ ! -f "$ENV_FILE" ]; then
  echo "缺少 $ENV_FILE（cp deploy/compose/.env.example deploy/compose/.env 并填写后重试）"
  echo "SMOKE R2.5: FAIL"
  exit 1
fi
get_env() { sed -n "s/^$1=//p" "$ENV_FILE" | head -1 | tr -d '\r'; }
MYSQL_ROOT_PASSWORD="$(get_env MYSQL_ROOT_PASSWORD)"
CONTROL_PLANE_TOKEN="$(get_env CONTROL_PLANE_TOKEN)"
if [ -z "$MYSQL_ROOT_PASSWORD" ] || [ -z "$CONTROL_PLANE_TOKEN" ]; then
  echo ".env 缺 MYSQL_ROOT_PASSWORD / CONTROL_PLANE_TOKEN"
  echo "SMOKE R2.5: FAIL"
  exit 1
fi
for tool in curl docker jq; do
  command -v "$tool" >/dev/null 2>&1 || { echo "需要 $tool"; echo "SMOKE R2.5: FAIL"; exit 1; }
done

sql() { # market 库直连（走 compose mysql 容器）
  local out rc
  out=$("${DC[@]}" exec -T -e MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql \
        mysql -uroot --default-character-set=utf8mb4 -N -B market -e "$1" 2>&1)
  rc=$?
  if [ $rc -ne 0 ]; then
    printf 'sql 失败: %s\n' "$out" >&2
    return 1
  fi
  printf '%s\n' "$out"
}

sql_ai() { # ai_gateway 库直连（billing subscriptions/topups/bills 断言）
  local out rc
  out=$("${DC[@]}" exec -T -e MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql \
        mysql -uroot --default-character-set=utf8mb4 -N -B ai_gateway -e "$1" 2>&1)
  rc=$?
  if [ $rc -ne 0 ]; then
    printf 'sql 失败: %s\n' "$out" >&2
    return 1
  fi
  printf '%s\n' "$out"
}

TOKEN="" # 会话 token（register/login 后填充；req 自动带 Bearer）
HTTP_CODE=""
HTTP_BODY=""

req() { # req METHOD PATH [JSON_BODY] [HEADER_NAME] [HEADER_VALUE]
  local method="$1" path="$2" body="${3:-}" hname="${4:-}" hval="${5:-}"
  local args=(-s --max-time 15 -w $'\n%{http_code}' -X "$method" "$BASE$path")
  [ -n "$TOKEN" ] && args+=(-H "Authorization: Bearer $TOKEN")
  [ -n "$body" ] && args+=(-H "Content-Type: application/json" -d "$body")
  [ -n "$hname" ] && args+=(-H "$hname: $hval")
  local out
  out=$(curl "${args[@]}")
  HTTP_CODE="${out##*$'\n'}"
  HTTP_BODY="${out%$'\n'*}"
}

# admin_topups 经 billing admin HTTP 查流水（X-Control-Token；键=Go 字段名）。
admin_topups() { # admin_topups TENANT_ID → JSON on stdout
  curl -s --max-time 10 -H "X-Control-Token: $CONTROL_PLANE_TOKEN" \
    "$BILLING_ADMIN/admin/topups?tenant_id=$1&limit=10"
}

# wait_healthy SERVICE TIMEOUT_S → 0/1（容器 healthy 轮询）
wait_healthy() {
  local svc="$1" timeout="$2"
  local deadline=$((SECONDS + timeout))
  while [ "$SECONDS" -lt "$deadline" ]; do
    if "${DC[@]}" ps "$svc" 2>/dev/null | grep -q '(healthy)'; then return 0; fi
    sleep 2
  done
  return 1
}

# overview 安全读取（带 3 次重试，防限流/抖动假阴性）
overview() {
  local i
  for i in 1 2 3; do
    req GET /me/overview
    if [ "$HTTP_CODE" = "200" ]; then
      OVERVIEW_BODY="$HTTP_BODY"
      return 0
    fi
    sleep 2
  done
  OVERVIEW_BODY="$HTTP_BODY"
  return 1
}
balance_of() { jq -r '.balance_cent // -1' <<<"$1" 2>/dev/null; }

# ledger_balance TENANT_ID → 权威账本余额（ai_gateway.tenants.ledger_balance；
# 本冒烟全程订阅资金面为 0，断言口径 = 账本列）。
ledger_balance() {
  sql_ai "SELECT ledger_balance FROM tenants WHERE id=$1" 2>/dev/null
}

# sub_row TENANT_ID → "status<TAB>auto_renew"（该租户订阅行最新态）。
sub_row() {
  sql_ai "SELECT CONCAT(status,'#',auto_renew) FROM subscriptions WHERE tenant_id=$1 ORDER BY id DESC LIMIT 1" 2>/dev/null
}

# wait_sub_status TENANT_ID WANT_STATUS TIMEOUT_S → 0/1（订阅行状态轮询）。
wait_sub_status() {
  local tenant="$1" want="$2" timeout="$3"
  local deadline=$((SECONDS + timeout))
  while [ "$SECONDS" -lt "$deadline" ]; do
    if grep -q "^$want#" <<<"$(sub_row "$tenant")"; then return 0; fi
    sleep 2
  done
  return 1
}

# wait_funding_bill TENANT_ID TIMEOUT_S → 0/1（bills 出现 funding=subscription 流水）。
wait_funding_bill() {
  local tenant="$1" timeout="$2"
  local deadline=$((SECONDS + timeout))
  while [ "$SECONDS" -lt "$deadline" ]; do
    local n
    n=$(sql_ai "SELECT COUNT(*) FROM bills WHERE tenant_id=$tenant AND funding='subscription'" 2>/dev/null)
    [ "${n:-0}" -ge 1 ] 2>/dev/null && return 0
    sleep 3
  done
  return 1
}

# chat_call KEY MAX_TOKENS → HTTP code（响应体落 /tmp 供 object 断言）。
chat_call() {
  local key="$1" maxtok="${2:-}"
  local body='{"model":"mock-model","messages":[{"role":"user","content":"R2.5 smoke ping"}],"stream":false'
  [ -n "$maxtok" ] && body="$body,\"max_tokens\":$maxtok"
  body="$body}"
  curl -s --max-time 30 -o /tmp/smoke-r25-chat-$$.json -w '%{http_code}' \
    -H "Authorization: Bearer $key" -H "Content-Type: application/json" \
    -d "$body" "$GATEWAY/v1/chat/completions"
}

# ---------- 1) 前置健康 ----------
info "== Step 1: 前置健康（bff / gateway / billing-admin / mall 三容器新镜像）=="
code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "http://$HOSTADDR:18090/healthz")
if [ "$code" = "200" ]; then pass "mall-bff /healthz :18090 = 200"; else fail "mall-bff /healthz :18090 = $code（期望 200）"; fi
code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$GATEWAY/")
if [ "$code" = "200" ]; then pass "gateway 首页 :18080 = 200（会话/portal /v1 链路上游）"; else fail "gateway :18080 = $code（期望 200）"; fi
code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 -H "X-Control-Token: $CONTROL_PLANE_TOKEN" "$BILLING_ADMIN/healthz")
if [ "$code" = "200" ]; then pass "billing admin /healthz :9103 = 200（X-Control-Token 门禁通过）"; else fail "billing admin :9103 = $code（期望 200）"; fi

for svc in billing mall-goods mall-order mall-bff; do
  if ! "${DC[@]}" ps "$svc" 2>/dev/null | grep -q '(healthy)'; then
    fail "$svc 容器非 healthy"
    continue
  fi
  tag_id=$(docker images --format '{{.ID}}' "my-market/$svc:local" 2>/dev/null | head -1)
  ctr_id=$(docker inspect --format '{{.Image}}' "my-market-$svc-1" 2>/dev/null | sed 's/^sha256://' | cut -c1-12)
  if [ -n "$tag_id" ] && [ "$tag_id" = "$ctr_id" ]; then
    pass "$svc 容器 healthy 且镜像 = my-market/$svc:local（$tag_id）"
  else
    fail "$svc 镜像漂移：容器=$ctr_id tag=$tag_id（先 build + up -d 重建）"
  fi
done

for svc in mall-order mall-bff; do
  envs=$(docker exec "my-market-$svc-1" env 2>/dev/null)
  addr_ok=$(grep -q '^BILLING_ADDR=billing:9101$' <<<"$envs" && echo 1 || echo 0)
  tok_ok=$(grep -q '^CONTROL_PLANE_TOKEN=.\+' <<<"$envs" && echo 1 || echo 0)
  if [ "$addr_ok" = "1" ] && [ "$tok_ok" = "1" ]; then
    pass "$svc 已注入 BILLING_ADDR=billing:9101 + CONTROL_PLANE_TOKEN（值不回显）"
  else
    fail "$svc 注入缺失（BILLING_ADDR=$addr_ok CONTROL_PLANE_TOKEN=$tok_ok）"
  fi
done

if sql "SELECT 1" >/dev/null 2>&1; then
  pass "MySQL market 库可达"
else
  fail "MySQL market 库不可达（先起 infra + schema-apply + seed-plans + seed-goods）"
fi
PLAN_OK=$(sql_ai "SELECT COUNT(*) FROM plans WHERE name='月度·标准' AND plan_type='recurring' AND status=0" 2>/dev/null)
if [ "$PLAN_OK" = "1" ]; then
  pass "billing 套餐种子在位（月度·标准 recurring 在售，goods 301 关联）"
else
  fail "billing 套餐缺失（先跑 scripts/seed-plans.sql + seed-goods.sql）"
fi

# ---------- 2) 幂等造数 ----------
info "== Step 2: provider 重指 + 目录 SKU（smoke-r0/r2 同款，幂等）=="
PROVIDER_URL="http://mock-provider:9201"
if sql_ai "INSERT INTO providers (name, base_url, models, weight) VALUES
          ('mock-a', '$PROVIDER_URL', '[\"*\"]', 5),
          ('mock-b', '$PROVIDER_URL', '[\"*\"]', 3)
        ON DUPLICATE KEY UPDATE base_url = VALUES(base_url);
        UPDATE providers SET base_url='$PROVIDER_URL', status=0,
               fail_count=0, consecutive_fail=0, cooldown_until=NULL
        WHERE name IN ('mock-a','mock-b');" >/dev/null 2>&1 \
   && [ "$(sql_ai "SELECT COUNT(*) FROM providers WHERE name IN ('mock-a','mock-b') AND base_url='$PROVIDER_URL' AND status=0" 2>/dev/null)" = "2" ]; then
  pass "providers mock-a/mock-b 已指向 $PROVIDER_URL 且 active（幂等）"
else
  fail "providers 重指失败（gateway /v1 链路将不通）"
fi
# 目录 SKU upsert 同时回写 tags（healing）：订阅额度路径按模型 tier 档取窗口
# 额度——存量库可能存有历史乱码 tags（tier 解析为空 → 恒走余额路径，订阅额度
# 永不生效），此处幂等重写 ["轻量"]。billing 侧 GetModelByModelID 有进程内
# 模型缓存（ledger batch），SQL 直改不失效——healing 实际改动了行则重启
# billing 容器清缓存（幂等可重跑：行已正确时零重启）。
MODELS_HEALED=0
BEFORE_TAGS=$(sql_ai "SELECT tags FROM models WHERE model_id='mock-model'" 2>/dev/null)
if sql_ai "INSERT INTO models (vendor, model_id, name, input_price_cent, output_price_cent, context_len, tags)
        VALUES ('mock', 'mock-model', 'Mock Model', 100, 200, 128000, '[\"轻量\"]')
        ON DUPLICATE KEY UPDATE status = 0, tags = VALUES(tags);" >/dev/null 2>&1 \
   && [ "$(sql_ai "SELECT COUNT(*) FROM models WHERE model_id='mock-model' AND status=0 AND tags LIKE '%轻量%'" 2>/dev/null)" = "1" ]; then
  pass "目录 SKU mock-model 已上架且轻量档 tags 就绪（幂等 + 乱码 healing）"
else
  fail "mock-model SKU 写入/校验失败（含轻量档 tags）"
fi
AFTER_TAGS=$(sql_ai "SELECT tags FROM models WHERE model_id='mock-model'" 2>/dev/null)
if [ "$BEFORE_TAGS" != "$AFTER_TAGS" ]; then
  MODELS_HEALED=1
  info "tags 被 healing 改写（before=${BEFORE_TAGS:0:40} → after=${AFTER_TAGS}），重启 billing 清模型缓存"
fi
if [ "$MODELS_HEALED" = "1" ]; then
  "${DC[@]}" --profile mall restart billing >/dev/null 2>&1
  if wait_healthy billing 90; then
    pass "billing 已重启且 healthy（模型缓存刷新，healed tags 生效）"
  else
    fail "billing 重启后 90s 未 healthy"
  fi
fi

# ---------- 3) 注册 → overview 余额 0 ----------
info "== Step 3: 注册（随机用户名）→ /me/overview 新租户余额 0 =="
USERNAME="smoker25${RUN_TAG}"    # a-z0-9_- 3-32 位
PASSWORD="R25-smoke-passw0rd!"   # 本地一次性测试账号
TOKEN=""
req POST /auth/register "{\"username\":\"$USERNAME\",\"password\":\"$PASSWORD\"}"
REG_TENANT=$(jq -r '.tenant_id // empty' <<<"$HTTP_BODY" 2>/dev/null)
TOKEN=$(jq -r '.session_token // empty' <<<"$HTTP_BODY" 2>/dev/null)
if [ "$HTTP_CODE" = "200" ] && [ -n "$REG_TENANT" ] && [ -n "$TOKEN" ]; then
  pass "注册 200（username=$USERNAME tenant_id=$REG_TENANT，session_token 就绪）"
else
  fail "注册失败（HTTP $HTTP_CODE）：$(printf '%s' "$HTTP_BODY" | head -c 160)"
  echo "SMOKE R2.5: FAIL"
  exit 1
fi
if overview && [ "$(balance_of "$OVERVIEW_BODY")" = "0" ] \
   && [ "$(jq '.recent_topups | length' <<<"$OVERVIEW_BODY" 2>/dev/null)" = "0" ] \
   && grep -q '"subscription":null' <<<"$OVERVIEW_BODY"; then
  pass "overview 新租户：balance_cent=0、recent_topups=[]、subscription=null"
else
  fail "overview 新租户形状/余额不符：$OVERVIEW_BODY"
fi

# ---------- 4) 订阅限购守卫（cart 层） ----------
info "== Step 4: 订阅商品 qty=1 限购（cart 层拒 qty=2，MALL_20005）=="
req POST /cart/items '{"goods_id":301,"qty":2}'
if [ "$HTTP_CODE" = "400" ] && grep -q 'MALL_20005' <<<"$HTTP_BODY"; then
  pass "加购月卡 qty=2 → 400 MALL_20005（订阅限购守卫在位）"
else
  fail "加购月卡 qty=2 未被拒（HTTP $HTTP_CODE）：$(printf '%s' "$HTTP_BODY" | head -c 160)"
fi
req GET /cart
if [ "$HTTP_CODE" = "200" ] && [ "$(jq '.items | length' <<<"$HTTP_BODY" 2>/dev/null)" = "0" ]; then
  pass "拒绝路径购物车为空（零落行）"
else
  fail "拒绝路径购物车异常：$HTTP_BODY"
fi

# ---------- 5) 混合单：月卡 301 + general 113 → 支付 → fulfilled ----------
info "== Step 5: 加购月卡+general → 结算 → 支付 → fulfilled（订阅开通）=="
req GET /goods/113
P113=$(jq -r '.price_cent // 0' <<<"$HTTP_BODY" 2>/dev/null)
req GET /goods/301
P301=$(jq -r '.price_cent // 0' <<<"$HTTP_BODY" 2>/dev/null)
if [ "${P113:-0}" -gt 0 ] 2>/dev/null && [ "${P301:-0}" -gt 0 ] 2>/dev/null; then
  pass "价目就绪（#113=$P113 general / #301=$P301 月卡）"
else
  fail "价目读取失败（#113=$P113 #301=$P301）"
fi
req POST /cart/items '{"goods_id":301,"qty":1}'
[ "$HTTP_CODE" = "200" ] && pass "加购月卡 301×1（正例放行）" || fail "加购月卡失败（HTTP $HTTP_CODE）"
req POST /cart/items "{\"goods_id\":113,\"qty\":1}"
[ "$HTTP_CODE" = "200" ] && pass "加购 general 113×1（混合单）" || fail "加购 general 失败（HTTP $HTTP_CODE）"
req GET /cart
CART_N=$(jq '.items | length' <<<"$HTTP_BODY" 2>/dev/null)
CART_TOTAL=$(jq -r '.total_cent // 0' <<<"$HTTP_BODY" 2>/dev/null)
EXPECT_TOTAL=$(( P113 + P301 ))
if [ "$CART_N" = "2" ] && [ "$CART_TOTAL" = "$EXPECT_TOTAL" ]; then
  pass "cart 两行且 total=$CART_TOTAL（=$P113+$P301）"
else
  fail "cart 断言失败（rows=$CART_N total=$CART_TOTAL want=$EXPECT_TOTAL）：$HTTP_BODY"
fi
IDEM1="r25o1-${RUN_TAG}"
req POST /orders '{"kind":"cart","from_cart":true}' "Idempotency-Key" "$IDEM1"
O1=$(jq -r '.order_no // empty' <<<"$HTTP_BODY" 2>/dev/null)
ORD_AMOUNT=$(jq -r '.amount_cent // 0' <<<"$HTTP_BODY" 2>/dev/null)
if [ "$HTTP_CODE" = "201" ] && [ -n "$O1" ] && [ "$ORD_AMOUNT" = "$EXPECT_TOTAL" ]; then
  pass "混合单创建 201：order_no=$O1 amount=$ORD_AMOUNT"
else
  fail "混合单创建失败（HTTP $HTTP_CODE amount=$ORD_AMOUNT）：$(printf '%s' "$HTTP_BODY" | head -c 160)"
  echo "SMOKE R2.5: FAIL"
  exit 1
fi
req POST "/orders/$O1/pay"
if [ "$HTTP_CODE" = "200" ]; then
  pass "pay 200（mock 渠道异步回调）"
else
  fail "pay 失败（HTTP $HTTP_CODE）：$(printf '%s' "$HTTP_BODY" | head -c 160)"
  echo "SMOKE R2.5: FAIL"
  exit 1
fi
STATUS1=""
deadline=$((SECONDS + 40))
while [ "$SECONDS" -lt "$deadline" ]; do
  req GET "/orders/$O1"
  STATUS1=$(jq -r '.status // empty' <<<"$HTTP_BODY" 2>/dev/null)
  [ "$STATUS1" = "fulfilled" ] && break
  sleep 2
done
if [ "$STATUS1" = "fulfilled" ]; then
  pass "订单 fulfilled（ship + subscribe 双行履约，worker tick 收口）"
else
  fail "轮询 40s 未 fulfilled（order_no=$O1 末态=$STATUS1）"
fi
if wait_sub_status "$REG_TENANT" "active" 15; then
  SR="$(sub_row "$REG_TENANT")"
  if [ "$SR" = "active#1" ]; then
    pass "billing 订阅行 active + auto_renew=1（月卡开通）"
  else
    fail "订阅行 auto_renew 异常：$SR（want active#1）"
  fi
else
  fail "billing 订阅行未出现 active（tenant=$REG_TENANT）"
fi
deadline=$((SECONDS + 15))
ANCHOR_N=0
while [ "$SECONDS" -lt "$deadline" ]; do
  ANCHOR_N=$(sql_ai "SELECT COUNT(*) FROM topups WHERE tenant_id=$REG_TENANT AND ref_type='mall-sub' AND ref_id LIKE '${O1}%'" 2>/dev/null)
  [ "${ANCHOR_N:-0}" -ge 1 ] 2>/dev/null && break
  sleep 2
done
if [ "${ANCHOR_N:-0}" = "1" ]; then
  pass "mall-sub 0 元激活锚恰 1 条（ref_type=mall-sub ref_id=$O1:*）"
else
  fail "mall-sub 激活锚条数 = ${ANCHOR_N:-?}，want 恰 1"
fi

# ---------- 6) 造 API key → 窗口内 /v1 免费调用 ----------
info "== Step 6: gateway /portal/api-keys → 窗口内 /v1 免费（余额不动 + funding=subscription）=="
KEY_NAME="r25smoke-${RUN_TAG}"
KEY_JSON=$(curl -s --max-time 15 -X POST "$GATEWAY/portal/api-keys" \
  -H "Content-Type: application/json" -b "agw_user=$TOKEN" \
  -d "{\"name\":\"$KEY_NAME\"}")
API_KEY=$(jq -r '.key // empty' <<<"$KEY_JSON" 2>/dev/null)
if [ -n "$API_KEY" ]; then
  pass "gateway /portal/api-keys 200：API key 就绪（会话 cookie 鉴权；明文不回显）"
else
  fail "造 key 失败：$(printf '%s' "$KEY_JSON" | head -c 160)"
fi
CHAT_OK=0
CHAT_CODE=""
for attempt in 1 2 3 4 5 6; do
  CHAT_CODE=$(chat_call "$API_KEY")
  if [ "$CHAT_CODE" = "200" ] && grep -q '"object":"chat.completion"' "/tmp/smoke-r25-chat-$$.json" 2>/dev/null; then
    CHAT_OK=1
    break
  fi
  info "窗口内 chat 第 $attempt 发未成功（HTTP $CHAT_CODE），8s 后重试（router 30s 路由缓存）"
  rm -f "/tmp/smoke-r25-chat-$$.json"
  sleep 8
done
rm -f "/tmp/smoke-r25-chat-$$.json"
if [ "$CHAT_OK" = "1" ]; then
  pass "窗口内 /v1/chat/completions 200（funding=subscription 额度内免费）"
else
  fail "窗口内 chat 重试 6 次仍失败（最后 HTTP $CHAT_CODE）"
fi
BAL_NOW="$(ledger_balance "$REG_TENANT")"
if [ "${BAL_NOW:-x}" = "0" ]; then
  pass "窗口内调用后账本余额不动：$BAL_NOW（订阅额度内免费，不走余额）"
else
  fail "窗口内调用动了余额：ledger=$BAL_NOW，want 0"
fi
if wait_funding_bill "$REG_TENANT" 45; then
  FUND_ROWS=$(sql_ai "SELECT COUNT(*) FROM bills WHERE tenant_id=$REG_TENANT AND funding='subscription'" 2>/dev/null)
  pass "bills 可查 funding=subscription 流水（${FUND_ROWS} 行，reserve/settle 0 元额度记账）"
else
  fail "45s 内 bills 未出现 funding=subscription 流水"
fi

# ---------- 7) 超额 402 ----------
info "== Step 7: 超额一次 402（max_tokens 超套餐窗口额度，RESERVE_SUB_QUOTA）=="
OVER_CODE=$(chat_call "$API_KEY" 9999999)
rm -f "/tmp/smoke-r25-chat-$$.json"
BAL_AFTER_402="$(ledger_balance "$REG_TENANT")"
if [ "$OVER_CODE" = "402" ] && [ "${BAL_AFTER_402:-x}" = "0" ]; then
  pass "超额请求 402（RESERVE_SUB_QUOTA 拒绝）且余额不动（$BAL_AFTER_402）"
else
  fail "超额断言失败（HTTP $OVER_CODE balance=$BAL_AFTER_402，want 402/0）"
fi

# ---------- 8) 退款 = 退订不退费 ----------
info "== Step 8: fulfilled 退款 → general 行退回 + 订阅行退订不退费（D15）=="
req POST "/orders/$O1/refund"
R_STATUS=$(jq -r '.status // empty' <<<"$HTTP_BODY" 2>/dev/null)
R_CENT=$(jq -r '.refund_cent // -1' <<<"$HTTP_BODY" 2>/dev/null)
if [ "$HTTP_CODE" = "200" ] && [ "$R_STATUS" = "refunded" ] && [ "$R_CENT" = "$P113" ]; then
  pass "退款 200 refunded：refund_cent=$R_CENT = general 行金额（订阅行 $P301 不进退款额）"
else
  fail "退款断言失败（HTTP $HTTP_CODE status=$R_STATUS refund_cent=$R_CENT want=$P113）：$(printf '%s' "$HTTP_BODY" | head -c 160)"
fi
BAL_AFTER_REFUND="$(ledger_balance "$REG_TENANT")"
if [ "${BAL_AFTER_REFUND:-x}" = "0" ]; then
  pass "退款后账本余额仍 0（订阅行不退费，账本零漂移）"
else
  fail "退款后余额异常：ledger=$BAL_AFTER_REFUND，want 0"
fi
if wait_sub_status "$REG_TENANT" "cancelled" 15; then
  SR="$(sub_row "$REG_TENANT")"
  if [ "$SR" = "cancelled#0" ]; then
    pass "订阅行 status=cancelled + auto_renew=0（退款=退订，当期权益保留至期满）"
  else
    fail "订阅行退订态异常：$SR（want cancelled#0）"
  fi
else
  fail "订阅行未退订（tenant=$REG_TENANT）"
fi
CYCLE_END_OK=$(sql_ai "SELECT COUNT(*) FROM subscriptions WHERE tenant_id=$REG_TENANT AND status='cancelled' AND auto_renew=0 AND cycle_end > NOW(3)" 2>/dev/null)
if [ "$CYCLE_END_OK" = "1" ]; then
  pass "当期权益保留：cancelled 订阅 cycle_end 仍在未来（退订不收回本期）"
else
  fail "当期权益断言失败（cycle_end 未到期 cancelled 行 = ${CYCLE_END_OK:-?}，want 1）"
fi
info "退款后再来一次窗口内调用（当期保留的行为面证据）"
POST_CODE=$(chat_call "$API_KEY")
rm -f "/tmp/smoke-r25-chat-$$.json"
BAL_POST_REFUND_CALL="$(ledger_balance "$REG_TENANT")"
if [ "$POST_CODE" = "200" ] && [ "${BAL_POST_REFUND_CALL:-x}" = "0" ]; then
  pass "退款后窗口内调用仍 200 免费（余额 $BAL_POST_REFUND_CALL，当期权益可用）"
else
  fail "退款后窗口内调用异常（HTTP $POST_CODE balance=$BAL_POST_REFUND_CALL）"
fi
REF_ROWS=$(jq --arg no "$O1" '[.items[] | select(.RefType=="refund")] | length' <<<"$(admin_topups "$REG_TENANT")" 2>/dev/null)
if [ "${REF_ROWS:-x}" = "0" ]; then
  pass "billing 零 refund 冲退流水（general 直退/订阅退订不走 Refund 通道，不退费）"
else
  fail "出现 refund 冲退流水 ${REF_ROWS:-?} 条，want 0"
fi
SUB_TOPUP_N=$(sql_ai "SELECT COUNT(*) FROM topups WHERE tenant_id=$REG_TENANT AND ref_type='mall-sub'" 2>/dev/null)
if [ "${SUB_TOPUP_N:-0}" = "1" ]; then
  pass "mall-sub 激活锚仍恰 1 条（退订零新增流水）"
else
  fail "mall-sub 锚条数 = ${SUB_TOPUP_N:-?}，want 1"
fi

# ---------- 9) overview subscription 字段 ----------
info "== Step 9: overview subscription 字段（bff 未透传订阅详情，恒 null——字段在位）=="
if overview && grep -q '"subscription":null' <<<"$OVERVIEW_BODY"; then
  pass "overview 含 subscription 字段（bff 未透传订阅详情，恒 null；透传点亮属后续前端面）"
else
  fail "overview 缺 subscription 字段：$OVERVIEW_BODY"
fi

# ---------- 汇总 ----------
echo "------------------------------------------------------------"
if [ "$FAILS" -eq 0 ]; then
  echo "SMOKE R2.5: PASS"
else
  echo "SMOKE R2.5: FAIL（$FAILS 项失败）"
  exit 1
fi
