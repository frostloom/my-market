#!/usr/bin/env bash
# my-market R2 充值闭环冒烟（scripts/smoke-r2.sh）：mall-bff ↔ mall-order ↔ billing 全链路。
#
# 前置：
#   deploy/compose 三 profile（infra + ai + mall）已起且 healthy——
#     docker compose -f deploy/compose/docker-compose.yml \
#       --profile infra --profile ai --profile mall up -d
#   且四容器镜像为 R2 版（billing 含 Credit/Refund RPC；mall-order/bff 含
#   billing 客户端）——先
#     docker compose --profile ai build billing && \
#     docker compose --profile ai up -d billing && \
#     docker compose --profile mall build mall-order mall-bff && \
#     docker compose --profile mall up -d mall-order mall-bff
#   market 库种子就绪（bash scripts/schema-apply.sh + seed-goods.sql）。
#   mall-order 只以容器形态跑（宿主 9112 无本地进程——断电演练语义要求）。
#
# 步骤（acceptance R2 顺序）：
#   1) 前置健康：bff/gateway/billing-admin healthz + billing 与 mall 三容器
#      healthy 且容器镜像 = 本地同名 tag（防"重建了 tag 但没重建容器"漂移；
#      billing 也须 R2 版含 Credit/Refund RPC）+ mall-order/mall-bff 容器带
#      BILLING_ADDR/CONTROL_PLANE_TOKEN 注入
#   2) 幂等造数（复用 smoke-r0 口径）：providers mock-a/mock-b 重指
#      http://mock-provider:9201 + 清熔断计数；目录补 mock-model SKU（status=0）
#   3) 注册（随机用户名）→ /me/overview 余额=0、recent_topups=[]、subscription=null
#   4) 买 ¥10 充值包（goods#201，kind=recharge + Idempotency-Key）→ pay →
#      轮询 fulfilled（worker 10s tick，≤30s）→ overview 余额 = 面值+赠送
#      （种子 201 档：1000 + 0；余额断言以 ai_gateway.tenants.ledger_balance
#      账本为权威、overview 投影经 wait_converged 收敛判定——Credit/Refund
#      同步写 MySQL 账本，Redis 投影由 Kafka relay 异步追赶，滞后可数十秒）
#      + billing admin topups（:9103 + X-Control-Token）出现该单 ref_type=mall 流水
#   5) 会话 cookie（agw_user=session_token）经 gateway :18080/portal/api-keys
#      造 API key → POST :18080/v1/chat/completions（Bearer key，mock 上游，
#      router 30s 路由缓存故带重试）→ 账本余额下降 + overview 投影收敛
#   6) 断电演练：第二笔充值单 pay 后立刻 SIGKILL mall-order 容器 → up -d →
#      等容器 healthy → worker 补 Credit → 轮询 fulfilled → topups 该单 ref
#      恰好 1 条（billing uk_ref 幂等，不重不漏）→ 余额 = 消费后 + 面值
#   7) 退款两分支：
#      a) paid 直退：第四笔 pay → 轮询 paid → 立刻 refund → refunded、
#         账本余额不动（两条路径余额端态都应不变：直退无 billing 流水；若
#         撞上已 fulfilled 则冲退 min 也回全额——不变量一致）
#      b) fulfilled 冲退 min：第三笔 fulfilled 后退款 → refund_cent =
#         min(行额度 face+bonus, 退款前账本余额)，账本余额恰好回退实退额、
#         overview 投影收敛至退款后余额、冲退流水恰 1 条
#   末行输出 SMOKE R2: PASS / FAIL。
#
# 幂等可重跑：每轮随机用户名/幂等键（新租户全新余额）；SQL 仅 upsert/条件 UPDATE。
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
  echo "SMOKE R2: FAIL"
  exit 1
fi
get_env() { sed -n "s/^$1=//p" "$ENV_FILE" | head -1 | tr -d '\r'; }
MYSQL_ROOT_PASSWORD="$(get_env MYSQL_ROOT_PASSWORD)"
CONTROL_PLANE_TOKEN="$(get_env CONTROL_PLANE_TOKEN)"
if [ -z "$MYSQL_ROOT_PASSWORD" ] || [ -z "$CONTROL_PLANE_TOKEN" ]; then
  echo ".env 缺 MYSQL_ROOT_PASSWORD / CONTROL_PLANE_TOKEN"
  echo "SMOKE R2: FAIL"
  exit 1
fi
for tool in curl docker jq; do
  command -v "$tool" >/dev/null 2>&1 || { echo "需要 $tool"; echo "SMOKE R2: FAIL"; exit 1; }
done

sql() { # market 库直连（走 compose mysql 容器；造数兜底用）
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

sql_ai() { # ai_gateway 库直连（providers/models 造数 + billing topups 兜底核对）
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
  local i out
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

# ledger_balance TENANT_ID → 权威账本余额（ai_gateway.tenants.ledger_balance，
# Credit/Refund 同事务写入，即时可见）。GetBalance 走 Redis 投影（Kafka relay
# 异步追赶，滞后可数十秒）→ 涉及钱的断言一律以账本为准，投影只验"最终收敛"。
ledger_balance() {
  sql_ai "SELECT ledger_balance FROM tenants WHERE id=$1" 2>/dev/null
}

# wait_converged TENANT_ID TIMEOUT_S：轮询 overview 直至投影 == 账本（收敛）。
# 成功时 OVERVIEW_BODY / BAL_PROJ（投影余额）/ BAL_LEDGER（账本余额）就绪。
wait_converged() {
  local tenant="$1" timeout="$2"
  local deadline=$((SECONDS + timeout))
  while [ "$SECONDS" -lt "$deadline" ]; do
    overview || { sleep 2; continue; }
    BAL_PROJ="$(balance_of "$OVERVIEW_BODY")"
    BAL_LEDGER="$(ledger_balance "$tenant")"
    if [ -n "$BAL_LEDGER" ] && [ "$BAL_PROJ" = "$BAL_LEDGER" ]; then return 0; fi
    sleep 3
  done
  overview >/dev/null 2>&1 || true
  BAL_PROJ="$(balance_of "$OVERVIEW_BODY")"
  BAL_LEDGER="$(ledger_balance "$tenant")"
  return 1
}

# buy_and_fulfill GOODS_ID IDEM_KEY ORDER_VAR：充值包直购→pay→轮询 fulfilled
# （≤30s）。失败置 FAILS 并返回 1。
buy_and_fulfill() {
  local goods="$1" idem="$2" __var="$3"
  req POST /orders "{\"kind\":\"recharge\",\"goods_id\":$goods}" "Idempotency-Key" "$idem"
  local no; no=$(jq -r '.order_no // empty' <<<"$HTTP_BODY" 2>/dev/null)
  if [ "$HTTP_CODE" = "201" ] && [ -n "$no" ]; then
    pass "充值单创建 201：goods#$goods order_no=$no（idem=$idem）"
  else
    fail "充值单创建失败（HTTP $HTTP_CODE）：$(printf '%s' "$HTTP_BODY" | head -c 160)"
    return 1
  fi
  req POST "/orders/$no/pay"
  if [ "$HTTP_CODE" = "200" ]; then
    pass "pay 200（order_no=$no，mock 渠道异步回调）"
  else
    fail "pay 失败（HTTP $HTTP_CODE）：$(printf '%s' "$HTTP_BODY" | head -c 160)"
    return 1
  fi
  local status="" deadline=$((SECONDS + 30))
  while [ "$SECONDS" -lt "$deadline" ]; do
    req GET "/orders/$no"
    status=$(jq -r '.status // empty' <<<"$HTTP_BODY" 2>/dev/null)
    [ "$status" = "fulfilled" ] && break
    sleep 2
  done
  if [ "$status" = "fulfilled" ]; then
    pass "订单 fulfilled（order_no=$no，credit 行入账）"
  else
    fail "轮询 30s 未 fulfilled（order_no=$no 末态=$status）"
    return 1
  fi
  printf -v "$__var" '%s' "$no"
  return 0
}

# ---------- 1) 前置健康 ----------
info "== Step 1: 前置健康（bff / gateway / billing-admin / mall 三容器新镜像）=="
code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "http://$HOSTADDR:18090/healthz")
if [ "$code" = "200" ]; then pass "mall-bff /healthz :18090 = 200"; else fail "mall-bff /healthz :18090 = $code（期望 200）"; fi
code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$GATEWAY/")
if [ "$code" = "200" ]; then pass "gateway 首页 :18080 = 200（会话/portal 链路上游）"; else fail "gateway :18080 = $code（期望 200）"; fi
code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 -H "X-Control-Token: $CONTROL_PLANE_TOKEN" "$BILLING_ADMIN/healthz")
if [ "$code" = "200" ]; then pass "billing admin /healthz :9103 = 200（X-Control-Token 门禁通过）"; else fail "billing admin :9103 = $code（期望 200）"; fi

# 闭环四容器（billing 也须为含 Credit/Refund RPC 的 R2 版镜像）
for svc in billing mall-goods mall-order mall-bff; do
  if ! "${DC[@]}" ps "$svc" 2>/dev/null | grep -q '(healthy)'; then
    fail "$svc 容器非 healthy"
    continue
  fi
  # 容器镜像 = 本地同名 tag（重建 tag 未重建容器的漂移在此拦截；tag id 12 位截断对齐）
  tag_id=$(docker images --format '{{.ID}}' "my-market/$svc:local" 2>/dev/null | head -1)
  ctr_id=$(docker inspect --format '{{.Image}}' "my-market-$svc-1" 2>/dev/null | sed 's/^sha256://' | cut -c1-12)
  if [ -n "$tag_id" ] && [ "$tag_id" = "$ctr_id" ]; then
    pass "$svc 容器 healthy 且镜像 = my-market/$svc:local（$tag_id）"
  else
    fail "$svc 镜像漂移：容器=$ctr_id tag=$tag_id（先 build + up -d 重建）"
  fi
done

# compose 注入生效（只检查变量存在与地址形状，不回显 token 值）
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
admin_ok=$(docker exec my-market-mall-bff-1 env 2>/dev/null | grep -q '^BILLING_ADMIN_ADDR=http://billing:9103$' && echo 1 || echo 0)
if [ "$admin_ok" = "1" ]; then pass "mall-bff 已注入 BILLING_ADMIN_ADDR=http://billing:9103"; else fail "mall-bff 缺 BILLING_ADMIN_ADDR 注入"; fi

if sql "SELECT 1" >/dev/null 2>&1; then
  pass "MySQL market 库可达"
else
  fail "MySQL market 库不可达（先起 infra + schema-apply + seed）"
fi

# ---------- 2) 幂等造数（复用 smoke-r0 口径） ----------
info "== Step 2: provider 重指 + 目录 SKU（smoke-r0 同款，幂等）=="
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
if sql_ai "INSERT INTO models (vendor, model_id, name, input_price_cent, output_price_cent, context_len, tags)
        VALUES ('mock', 'mock-model', 'Mock Model', 100, 200, 128000, '[\"轻量\"]')
        ON DUPLICATE KEY UPDATE status = 0;" >/dev/null 2>&1 \
   && [ "$(sql_ai "SELECT COUNT(*) FROM models WHERE model_id='mock-model' AND status=0" 2>/dev/null)" = "1" ]; then
  pass "目录 SKU mock-model 已上架（100/200 分/百万 token，幂等）"
else
  fail "mock-model SKU 写入/校验失败"
fi

# ---------- 3) 注册 → overview 余额 0 ----------
info "== Step 3: 注册（随机用户名）→ /me/overview 新租户余额 0 =="
USERNAME="smoker2${RUN_TAG}"     # a-z0-9_- 3-32 位
PASSWORD="R2-smoke-passw0rd!"    # 本地一次性测试账号
TOKEN=""
req POST /auth/register "{\"username\":\"$USERNAME\",\"password\":\"$PASSWORD\"}"
REG_TENANT=$(jq -r '.tenant_id // empty' <<<"$HTTP_BODY" 2>/dev/null)
TOKEN=$(jq -r '.session_token // empty' <<<"$HTTP_BODY" 2>/dev/null)
if [ "$HTTP_CODE" = "200" ] && [ -n "$REG_TENANT" ] && [ -n "$TOKEN" ]; then
  pass "注册 200（username=$USERNAME tenant_id=$REG_TENANT，session_token 就绪）"
else
  fail "注册失败（HTTP $HTTP_CODE）：$(printf '%s' "$HTTP_BODY" | head -c 160)"
  echo "SMOKE R2: FAIL"
  exit 1
fi
if overview && [ "$(balance_of "$OVERVIEW_BODY")" = "0" ] \
   && [ "$(jq '.recent_topups | length' <<<"$OVERVIEW_BODY" 2>/dev/null)" = "0" ] \
   && [ "$(jq 'has("recent_topups") and (.recent_topups != null)' <<<"$OVERVIEW_BODY" 2>/dev/null)" = "true" ] \
   && grep -q '"subscription":null' <<<"$OVERVIEW_BODY"; then
  pass "overview 新租户：balance_cent=0、recent_topups=[]（空数组非 null）、subscription=null"
else
  fail "overview 新租户形状/余额不符：$OVERVIEW_BODY"
fi

# ---------- 4) 买 ¥10 充值包 → fulfilled → 余额 = 面值+赠送 ----------
info "== Step 4: goods#201（¥10 → 面值 1000 + 赠送 0）直购闭环 =="
O1=""
buy_and_fulfill 201 "r2o1-${RUN_TAG}" O1 || { echo "SMOKE R2: FAIL"; exit 1; }
if wait_converged "$REG_TENANT" 60 && [ "$BAL_LEDGER" = "1000" ] && [ "$BAL_PROJ" = "1000" ]; then
  pass "overview 余额 = 1000 且与账本收敛（种子 201 档：面值 1000 + 赠送 0；投影经 relay 追赶）"
else
  fail "余额断言失败：投影=$BAL_PROJ 账本=$BAL_LEDGER want=1000（body=$OVERVIEW_BODY）"
fi
TOPUPS_JSON="$(admin_topups "$REG_TENANT")"
if jq -e --arg no "$O1" '.items[] | select(.RefType=="mall" and (.RefID | startswith($no)))' >/dev/null 2>&1 <<<"$TOPUPS_JSON"; then
  pass "overview recent_topups 同口径：billing admin topups 已见 ref_type=mall 流水（ref_id=$O1:*）"
else
  fail "billing admin topups 未见 $O1:* 流水"
fi

# ---------- 5) 造 API key → chat 消费 → 余额下降 ----------
info "== Step 5: gateway /portal/api-keys（agw_user cookie）→ chat 消费 =="
KEY_NAME="r2smoke-${RUN_TAG}"
KEY_JSON=$(curl -s --max-time 15 -X POST "$GATEWAY/portal/api-keys" \
  -H "Content-Type: application/json" -b "agw_user=$TOKEN" \
  -d "{\"name\":\"$KEY_NAME\"}")
API_KEY=$(jq -r '.key // empty' <<<"$KEY_JSON" 2>/dev/null)
if [ -n "$API_KEY" ]; then
  pass "gateway /portal/api-keys 200：API key 就绪（会话 cookie 鉴权；明文不回显）"
else
  fail "造 key 失败：$(printf '%s' "$KEY_JSON" | head -c 160)"
fi
BAL_BEFORE_CHAT="$BAL_LEDGER" # =1000（Step 4 已收敛）
chat_ok=0
CHAT_BODY=""
for attempt in 1 2 3 4 5 6; do
  CHAT_BODY=$(curl -s --max-time 30 -o /tmp/smoke-r2-chat-$$.json -w '%{http_code}' \
    -H "Authorization: Bearer $API_KEY" -H "Content-Type: application/json" \
    -d '{"model":"mock-model","messages":[{"role":"user","content":"R2 smoke ping"}],"stream":false}' \
    "$GATEWAY/v1/chat/completions")
  if [ "$CHAT_BODY" = "200" ] && grep -q '"object":"chat.completion"' "/tmp/smoke-r2-chat-$$.json" 2>/dev/null; then
    chat_ok=1
    break
  fi
  info "chat 第 $attempt 发未成功（HTTP $CHAT_BODY），8s 后重试（router 30s 路由缓存）"
  rm -f "/tmp/smoke-r2-chat-$$.json"
  sleep 8
done
rm -f "/tmp/smoke-r2-chat-$$.json"
if [ "$chat_ok" = "1" ]; then
  pass "chat completions 200 且返回 chat.completion（同租户 key，余额计费口径）"
else
  fail "chat completions 重试 6 次仍失败（最后 HTTP $CHAT_BODY）"
fi
# 消费落账（settle 经 billing gRPC 同步写账本）+ 投影追赶
BAL_AFTER_CHAT="$BAL_BEFORE_CHAT"
deadline=$((SECONDS + 45))
while [ "$SECONDS" -lt "$deadline" ]; do
  L="$(ledger_balance "$REG_TENANT")"
  if [ -n "$L" ] && [ "$L" -lt "$BAL_BEFORE_CHAT" ] 2>/dev/null && [ "$L" -ge 0 ] 2>/dev/null; then
    BAL_AFTER_CHAT="$L"
    break
  fi
  sleep 3
done
if [ "$BAL_AFTER_CHAT" != "$BAL_BEFORE_CHAT" ] && [ "$BAL_AFTER_CHAT" -lt "$BAL_BEFORE_CHAT" ] && [ "$BAL_AFTER_CHAT" -ge 0 ] 2>/dev/null; then
  pass "消费后账本余额下降：$BAL_BEFORE_CHAT → $BAL_AFTER_CHAT 分"
else
  fail "余额未下降（before=$BAL_BEFORE_CHAT after=$BAL_AFTER_CHAT）"
fi
if wait_converged "$REG_TENANT" 60; then
  pass "overview 投影与账本收敛（balance=$BAL_PROJ；recent_topups=$(jq '.recent_topups | length' <<<"$OVERVIEW_BODY") 条 ≤10）"
else
  fail "overview 投影未收敛（proj=$BAL_PROJ ledger=$BAL_LEDGER）：$OVERVIEW_BODY"
fi
if [ "$(jq '.recent_topups | length' <<<"$OVERVIEW_BODY" 2>/dev/null)" -ge 1 ] 2>/dev/null; then
  pass "overview recent_topups 最近流水可见（≥1 条）"
else
  fail "overview recent_topups 异常：$OVERVIEW_BODY"
fi

# ---------- 6) 断电演练 ----------
info "== Step 6: 断电演练（pay 后立刻 SIGKILL mall-order 容器 → 重启补 Credit）=="
req POST /orders '{"kind":"recharge","goods_id":201}' "Idempotency-Key" "r2o2-${RUN_TAG}"
O2=$(jq -r '.order_no // empty' <<<"$HTTP_BODY" 2>/dev/null)
if [ "$HTTP_CODE" = "201" ] && [ -n "$O2" ]; then
  pass "断电单创建 201：order_no=$O2"
else
  fail "断电单创建失败（HTTP $HTTP_CODE）：$(printf '%s' "$HTTP_BODY" | head -c 160)"
  echo "SMOKE R2: FAIL"
  exit 1
fi
req POST "/orders/$O2/pay"
if [ "$HTTP_CODE" = "200" ]; then
  pass "断电单 pay 200（立刻断电，不等 worker tick）"
else
  fail "断电单 pay 失败（HTTP $HTTP_CODE）"
  echo "SMOKE R2: FAIL"
  exit 1
fi
PRE_ROWS=$(jq --arg no "$O2" '[.items[] | select(.RefType=="mall" and (.RefID | startswith($no)))] | length' <<<"$(admin_topups "$REG_TENANT")" 2>/dev/null)
info "kill 前该单 mall 流水条数 = ${PRE_ROWS:-?}（0=worker 未跑，1=已抢跑；两者皆合法）"
"${DC[@]}" kill -s SIGKILL mall-order >/dev/null 2>&1
"${DC[@]}" up -d mall-order >/dev/null 2>&1
if wait_healthy mall-order 90; then
  pass "mall-order 容器已重启并 healthy（SIGKILL 断电 → 自动拉起）"
else
  fail "mall-order 重启后 90s 未 healthy"
fi
deadline=$((SECONDS + 30))
STATUS2=""
while [ "$SECONDS" -lt "$deadline" ]; do
  req GET "/orders/$O2"
  STATUS2=$(jq -r '.status // empty' <<<"$HTTP_BODY" 2>/dev/null)
  [ "$STATUS2" = "fulfilled" ] && break
  sleep 2
done
if [ "$STATUS2" = "fulfilled" ]; then
  pass "断电单重启后 fulfilled（worker 补 Credit）"
else
  fail "断电单 30s 未 fulfilled（末态=$STATUS2）"
fi
POST_ROWS=$(jq --arg no "$O2" '[.items[] | select(.RefType=="mall" and (.RefID | startswith($no)))] | length' <<<"$(admin_topups "$REG_TENANT")" 2>/dev/null)
if [ "$POST_ROWS" = "1" ]; then
  pass "billing topups 该单 ref 恰 1 条（kill 前=${PRE_ROWS:-?} → 重启后=1，uk_ref 幂等不重不漏）"
else
  fail "断电单流水条数 = ${POST_ROWS:-?}，want 恰 1"
fi
if wait_converged "$REG_TENANT" 60 && [ "$BAL_LEDGER" = "$((BAL_AFTER_CHAT + 1000))" ]; then
  pass "断电单入账后余额 = $BAL_LEDGER（消费后余额 + 1000，投影已收敛）"
else
  fail "断电单余额断言失败：投影=$BAL_PROJ 账本=$BAL_LEDGER want=$((BAL_AFTER_CHAT + 1000))"
fi

# ---------- 7) 退款两分支 ----------
info "== Step 7a: paid 直退（第四笔 pay → 轮询 paid → 立刻 refund，余额不动）=="
O4=""
req POST /orders '{"kind":"recharge","goods_id":201}' "Idempotency-Key" "r2o4-${RUN_TAG}"
O4=$(jq -r '.order_no // empty' <<<"$HTTP_BODY" 2>/dev/null)
[ "$HTTP_CODE" = "201" ] && [ -n "$O4" ] && pass "第四单创建 201：order_no=$O4" || fail "第四单创建失败（HTTP $HTTP_CODE）"
req POST "/orders/$O4/pay"
[ "$HTTP_CODE" = "200" ] && pass "第四单 pay 200（mock 回调 ~200ms）" || fail "第四单 pay 失败（HTTP $HTTP_CODE）"
# 等回调落 paid（不 fulfilled——worker 10s tick 有充足窗口），再立刻直退
deadline=$((SECONDS + 8))
ST4=""
while [ "$SECONDS" -lt "$deadline" ]; do
  req GET "/orders/$O4"
  ST4=$(jq -r '.status // empty' <<<"$HTTP_BODY" 2>/dev/null)
  [ "$ST4" = "paid" ] && break
  [ "$ST4" = "fulfilled" ] && break # 抢跑兜底：冲退路径端态同样余额不动
  sleep 1
done
if [ "$ST4" = "paid" ]; then
  info "第四单已 paid（未 fulfilled，直退路径）"
elif [ "$ST4" = "fulfilled" ]; then
  info "第四单已 fulfilled（worker 抢跑，改走冲退路径——余额端态不变量相同）"
else
  fail "第四单未到可退状态（status=$ST4）"
fi
BAL_BEFORE_REFUND4="$BAL_LEDGER" # Step 6 收敛后的账本余额
req POST "/orders/$O4/refund"
R4_STATUS=$(jq -r '.status // empty' <<<"$HTTP_BODY" 2>/dev/null)
if [ "$HTTP_CODE" = "200" ] && [ "$R4_STATUS" = "refunded" ]; then
  pass "直退 200 refunded（order_no=$O4，status=$ST4 时退款）"
else
  fail "直退异常（HTTP $HTTP_CODE status=$R4_STATUS）：$(printf '%s' "$HTTP_BODY" | head -c 160)"
fi
if wait_converged "$REG_TENANT" 60 && [ "$BAL_LEDGER" = "$BAL_BEFORE_REFUND4" ]; then
  pass "直退后账本余额不动：$BAL_LEDGER（paid 直退无 billing 流水；撞上 fulfilled 则冲退 min 同样回满——端态一致）"
else
  fail "直退余额不变断言失败：before=$BAL_BEFORE_REFUND4 after(ledger)=$BAL_LEDGER proj=$BAL_PROJ"
fi

info "== Step 7b: fulfilled 冲退 min（第三笔 fulfilled 后退款）=="
O3=""
buy_and_fulfill 201 "r2o3-${RUN_TAG}" O3 || { echo "SMOKE R2: FAIL"; exit 1; }
if ! wait_converged "$REG_TENANT" 60; then
  fail "第三单入账后投影未收敛（proj=$BAL_PROJ ledger=$BAL_LEDGER）"
fi
BAL_BEFORE_REFUND3="$BAL_LEDGER"
if [ "$BAL_BEFORE_REFUND3" -ge 0 ] 2>/dev/null; then
  info "退款前余额（账本）= $BAL_BEFORE_REFUND3 分 → min(行额度 1000, $BAL_BEFORE_REFUND3)"
else
  fail "退款前余额读取失败：ledger=$BAL_LEDGER body=$OVERVIEW_BODY"
fi
req POST "/orders/$O3/refund"
R3_CENT=$(jq -r '.refund_cent // -1' <<<"$HTTP_BODY" 2>/dev/null)
R3_STATUS=$(jq -r '.status // empty' <<<"$HTTP_BODY" 2>/dev/null)
EXPECT_MIN=1000 # min(面值1000+赠送0, 退款前余额)
[ "$BAL_BEFORE_REFUND3" -lt 1000 ] 2>/dev/null && EXPECT_MIN="$BAL_BEFORE_REFUND3"
if [ "$HTTP_CODE" = "200" ] && [ "$R3_STATUS" = "refunded" ] && [ "$R3_CENT" = "$EXPECT_MIN" ]; then
  pass "冲退实退 = min(行额度, 余额) = $R3_CENT 分（API 返回）"
else
  fail "冲退 min 断言失败（HTTP $HTTP_CODE status=$R3_STATUS refund_cent=$R3_CENT want=$EXPECT_MIN）"
fi
deadline=$((SECONDS + 45))
BAL_AFTER_REFUND3="$BAL_BEFORE_REFUND3"
while [ "$SECONDS" -lt "$deadline" ]; do
  L="$(ledger_balance "$REG_TENANT")"
  if [ -n "$L" ] && [ "$L" = "$((BAL_BEFORE_REFUND3 - EXPECT_MIN))" ]; then
    BAL_AFTER_REFUND3="$L"
    break
  fi
  sleep 3
done
if [ "$BAL_AFTER_REFUND3" = "$((BAL_BEFORE_REFUND3 - EXPECT_MIN))" ]; then
  pass "账本余额恰好回退实退额：$BAL_BEFORE_REFUND3 → $BAL_AFTER_REFUND3（-$EXPECT_MIN）"
else
  fail "余额回退断言失败：before=$BAL_BEFORE_REFUND3 after=$BAL_AFTER_REFUND3 want=$((BAL_BEFORE_REFUND3 - EXPECT_MIN))"
fi
if wait_converged "$REG_TENANT" 60 && [ "$BAL_PROJ" = "$BAL_AFTER_REFUND3" ]; then
  pass "overview 投影收敛至退款后余额（balance_cent=$BAL_PROJ）"
else
  fail "退款后投影未收敛（proj=$BAL_PROJ ledger=$BAL_LEDGER want=$BAL_AFTER_REFUND3）"
fi
REF_ROWS=$(jq --arg no "$O3" '[.items[] | select(.RefType=="refund" and (.RefID | startswith("mall-refund:" + $no)))] | length' <<<"$(admin_topups "$REG_TENANT")" 2>/dev/null)
if [ "$REF_ROWS" = "1" ]; then
  pass "billing topups 冲退流水恰 1 条（ref_type=refund，ref_id=mall-refund:$O3:*）"
else
  fail "冲退流水条数 = ${REF_ROWS:-?}，want 恰 1"
fi

# ---------- 汇总 ----------
echo "------------------------------------------------------------"
if [ "$FAILS" -eq 0 ]; then
  echo "SMOKE R2: PASS"
else
  echo "SMOKE R2: FAIL（$FAILS 项失败）"
  exit 1
fi
