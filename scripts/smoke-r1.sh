#!/usr/bin/env bash
# my-market R1 冒烟（scripts/smoke-r1.sh）：mall-bff (:18090) 全链路走查。
#
# 前置：
#   deploy/compose 三 profile（infra + ai + mall）已起且 healthy——
#     docker compose -f deploy/compose/docker-compose.yml \
#       --profile infra --profile ai --profile mall up -d
#   market 库种子已就绪（bash scripts/schema-apply.sh + seed-goods.sql，
#   44 商品：general 101-140 / recharge 201-203 / subscription 301 下架不进列表）。
#
# 步骤（api-contract §2-§5 顺序，目标 http://localhost:18090/api/v1）：
#   1) 前置健康：bff /healthz + goods/order 容器 healthy + gateway(:18080) 可达
#      + MySQL market 库可达
#   2) 幂等造数：goods#113 详情富文本回填（种子 detail 为 NULL，R1 详情断言需要）
#   3) 注册（随机用户名）→ 登录 → categories=6 → /goods 游标翻页至 next_cursor=null
#      （断言 ≥2 页；on_sale=1 共 43 = 40 general + 3 recharge，subscription 下架不进列表）
#   4) 详情 #113：type=general（proto 编码 1）+ detail 非空
#   5) 购物车：#113 qty1 + #113 qty2（同商品合并=3）+ #121 qty1 → cart total=Σ价×qty
#   6) 下单（Idempotency-Key）→ 201 + 行快照 + 同 key 重发返回原单；断言 general
#      下单事务内同库直扣库存（contract §5 / ADR-009）
#   7) pay（mock provider）→ 轮询 GET /orders/{no} 至 fulfilled（worker 10s tick，
#      超时 30s）→ 断言 paid_at/fulfilled_at 落值
#   8) 我的订单列表包含该单
#   9) 取消路径：再造一单（加 #114）→ cancel → closed(user) + 库存回补
#  10) 幂等负例：用第 6 步的 key 携不同 payload（购物车内容已变）重发 → 409 MALL_30003；
#      并断言错误体三字段形状 {error, code, retryable}（api-contract §0.2）
#   末行输出 SMOKE R1: PASS / FAIL。
#
# 幂等可重跑：每轮随机用户名/幂等键（新租户全新购物车）；SQL 仅条件 UPDATE。
# 凭据：只读 deploy/compose/.env 的 MYSQL_ROOT_PASSWORD（SQL 造数用）；
#       PAY_CALLBACK_SECRET 冒烟不需要——mock provider 在 bff 进程内自签名回调，
#       脚本只做 HTTP（本脚本不读、不回显任何密钥）。
set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ENV_FILE="$ROOT/deploy/compose/.env"
HOSTADDR="localhost" # 双栈回退口径，见 smoke-r0.sh 顶注
BASE="http://$HOSTADDR:18090/api/v1"
RUN_TAG="$(date +%s)-$RANDOM"

FAILS=0
DC=(docker compose -f "$ROOT/deploy/compose/docker-compose.yml")

pass() { printf '[PASS] %s\n' "$1"; }
fail() { printf '[FAIL] %s\n' "$1"; FAILS=$((FAILS + 1)); }
info() { printf '[....] %s\n' "$1"; }

# ---------- 依赖与凭据 ----------
if [ ! -f "$ENV_FILE" ]; then
  echo "缺少 $ENV_FILE（cp deploy/compose/.env.example deploy/compose/.env 并填写后重试）"
  echo "SMOKE R1: FAIL"
  exit 1
fi
get_env() { sed -n "s/^$1=//p" "$ENV_FILE" | head -1 | tr -d '\r'; }
MYSQL_ROOT_PASSWORD="$(get_env MYSQL_ROOT_PASSWORD)"
if [ -z "$MYSQL_ROOT_PASSWORD" ]; then
  echo ".env 缺 MYSQL_ROOT_PASSWORD"
  echo "SMOKE R1: FAIL"
  exit 1
fi
for tool in curl docker jq; do
  command -v "$tool" >/dev/null 2>&1 || { echo "需要 $tool"; echo "SMOKE R1: FAIL"; exit 1; }
done

sql() { # market 库直连（走 compose mysql 容器，不依赖宿主客户端；utf8mb4 供中文富文本）
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

# ---------- 1) 前置健康 ----------
info "== Step 1: 前置健康（bff / goods / order / gateway / mysql）=="
code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "http://$HOSTADDR:18090/healthz")
if [ "$code" = "200" ]; then pass "mall-bff /healthz :18090 = 200"; else fail "mall-bff /healthz :18090 = $code（期望 200）"; fi
for svc in mall-goods mall-order; do
  if "${DC[@]}" ps "$svc" 2>/dev/null | grep -q '(healthy)'; then
    pass "$svc 容器 healthy（健康口容器内探活，不发布宿主）"
  else
    fail "$svc 容器非 healthy"
  fi
done
code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "http://$HOSTADDR:18080/")
if [ "$code" = "200" ]; then pass "gateway 首页 :18080 = 200（会话链路上游）"; else fail "gateway :18080 = $code（会话链路依赖，期望 200）"; fi
if sql "SELECT COUNT(*) FROM goods" >/dev/null 2>&1; then
  pass "MySQL market 库可达"
else
  fail "MySQL market 库不可达（先起 infra + schema-apply + seed）"
fi

# ---------- 2) 幂等造数：详情富文本 ----------
info "== Step 2: goods#113 detail 回填（种子 detail=NULL）=="
if sql "UPDATE goods SET detail='<p>重磅纯棉圆领 T 恤——R1 冒烟详情富文本（smoke-r1.sh 条件回填，幂等）。</p>' WHERE id=113 AND (detail IS NULL OR detail='');" >/dev/null 2>&1 \
   && [ "$(sql "SELECT detail IS NOT NULL AND detail<>'' FROM goods WHERE id=113" 2>/dev/null)" = "1" ]; then
  pass "goods#113 detail 非空就绪"
else
  fail "goods#113 detail 回填失败"
fi

# ---------- 3) 注册 → 登录 → 品类 → 翻页 ----------
info "== Step 3: 注册 / 登录 / categories / goods 游标翻页 =="
USERNAME="smoker1${RUN_TAG}"    # a-z0-9_- 3-32 位
PASSWORD="R1-smoke-passw0rd!"   # 8-72 字节（本地一次性测试账号，随机用户名每轮新建）
TOKEN=""
req POST /auth/register "{\"username\":\"$USERNAME\",\"password\":\"$PASSWORD\"}"
REG_TENANT=$(jq -r '.tenant_id // empty' <<<"$HTTP_BODY" 2>/dev/null)
REG_USER=$(jq -r '.username // empty' <<<"$HTTP_BODY" 2>/dev/null)
if [ "$HTTP_CODE" = "200" ] && [ -n "$REG_TENANT" ] && [ "$REG_USER" = "$USERNAME" ]; then
  pass "注册 200（username=$USERNAME tenant_id=$REG_TENANT）"
else
  fail "注册失败（HTTP $HTTP_CODE）：$(printf '%s' "$HTTP_BODY" | head -c 160)"
fi

TOKEN=""
req POST /auth/login "{\"username\":\"$USERNAME\",\"password\":\"$PASSWORD\"}"
TOKEN=$(jq -r '.session_token // empty' <<<"$HTTP_BODY" 2>/dev/null)
if [ "$HTTP_CODE" = "200" ] && [ -n "$TOKEN" ]; then
  pass "登录 200，session_token 就绪（后续全部走 Bearer）"
else
  fail "登录失败（HTTP $HTTP_CODE）：$(printf '%s' "$HTTP_BODY" | head -c 160)"
fi

req GET /categories
CAT_N=$(jq '.items | length' <<<"$HTTP_BODY" 2>/dev/null)
if [ "$HTTP_CODE" = "200" ] && [ "$CAT_N" = "6" ]; then
  pass "GET /categories 200，六品类"
else
  fail "categories 异常（HTTP $HTTP_CODE items=${CAT_N:-?}，期望 6）"
fi

# 游标翻页至 next_cursor=null（page_size=20；on_sale=1 共 43）
PAGES=0; TOTAL=0; CURSOR=""; PAGE_OK=1
while :; do
  if [ -n "$CURSOR" ]; then req GET "/goods?cursor=$(jq -rn --arg c "$CURSOR" '$c|@uri')" ; else req GET "/goods?page_size=20"; fi
  [ "$HTTP_CODE" = "200" ] || { PAGE_OK=0; break; }
  n=$(jq '.items | length' <<<"$HTTP_BODY" 2>/dev/null)
  TOTAL=$((TOTAL + ${n:-0}))
  PAGES=$((PAGES + 1))
  CURSOR=$(jq -r '.next_cursor // empty' <<<"$HTTP_BODY" 2>/dev/null)
  [ -z "$CURSOR" ] && break
  [ "$PAGES" -ge 20 ] && break # 防御：异常不收敛时跳出
done
if [ "$PAGE_OK" = "1" ] && [ "$PAGES" -ge 2 ] && [ "$TOTAL" = "43" ]; then
  pass "goods 翻页到底：$PAGES 页 / 43 条（next_cursor=null 到底；40 general + 3 recharge）"
else
  fail "goods 翻页异常（pages=$PAGES total=$TOTAL page_ok=$PAGE_OK，期望 ≥2 页且 43 条）"
fi

# ---------- 4) 详情 ----------
info "== Step 4: GET /goods/113 详情 =="
req GET /goods/113
D_TYPE=$(jq -r '.type // empty' <<<"$HTTP_BODY" 2>/dev/null)
D_DETAIL=$(jq -r '.detail // empty' <<<"$HTTP_BODY" 2>/dev/null)
P113=$(jq -r '.price_cent // 0' <<<"$HTTP_BODY" 2>/dev/null)
S113_0=$(jq -r '.stock // 0' <<<"$HTTP_BODY" 2>/dev/null)
if [ "$HTTP_CODE" = "200" ] && [ "$D_TYPE" = "general" ] && [ -n "$D_DETAIL" ]; then
  pass "详情 200：type=general（proto 编码 1）、detail 非空、price=${P113} 分、stock=$S113_0"
else
  fail "详情断言失败（HTTP $HTTP_CODE type=$D_TYPE detail=${#D_DETAIL} 字节）"
fi
req GET /goods/121
P121=$(jq -r '.price_cent // 0' <<<"$HTTP_BODY" 2>/dev/null)
req GET /goods/114
P114=$(jq -r '.price_cent // 0' <<<"$HTTP_BODY" 2>/dev/null)
S114_0=$(jq -r '.stock // 0' <<<"$HTTP_BODY" 2>/dev/null)
if [ "${P113:-0}" -gt 0 ] 2>/dev/null && [ "${P121:-0}" -gt 0 ] 2>/dev/null && [ "${P114:-0}" -gt 0 ] 2>/dev/null; then
  pass "三件商品价目就绪（#113=$P113 #121=$P121 #114=$P114 分）"
else
  fail "价目读取失败（#113=$P113 #121=$P121 #114=$P114）"
fi

# ---------- 5) 购物车 ----------
info "== Step 5: 加购（含同商品 qty 合并）+ cart 总价 =="
req POST /cart/items "{\"goods_id\":113,\"qty\":1}"; [ "$HTTP_CODE" = "200" ] || fail "加购 #113×1 失败（HTTP $HTTP_CODE）"
req POST /cart/items "{\"goods_id\":113,\"qty\":2}"; [ "$HTTP_CODE" = "200" ] || fail "加购 #113×2 失败（HTTP $HTTP_CODE）"
req POST /cart/items "{\"goods_id\":121,\"qty\":1}"; [ "$HTTP_CODE" = "200" ] || fail "加购 #121×1 失败（HTTP $HTTP_CODE）"
req GET /cart
CART_N=$(jq '.items | length' <<<"$HTTP_BODY" 2>/dev/null)
Q113=$(jq -r '[.items[] | select(.goods.id==113) | .qty] | first // 0' <<<"$HTTP_BODY" 2>/dev/null)
Q121=$(jq -r '[.items[] | select(.goods.id==121) | .qty] | first // 0' <<<"$HTTP_BODY" 2>/dev/null)
TOTAL_CENT=$(jq -r '.total_cent // 0' <<<"$HTTP_BODY" 2>/dev/null)
EXPECT_TOTAL=$(( P113 * 3 + P121 ))
if [ "$HTTP_CODE" = "200" ] && [ "$CART_N" = "2" ] && [ "$Q113" = "3" ] && [ "$Q121" = "1" ] \
   && [ "$TOTAL_CENT" = "$EXPECT_TOTAL" ]; then
  pass "cart 就绪：2 行、#113 合并 qty=3、#121 qty=1、total=$TOTAL_CENT 分（=3×$P113+$P121）"
else
  fail "cart 断言失败（HTTP $HTTP_CODE rows=$CART_N q113=$Q113 q121=$Q121 total=$TOTAL_CENT 期望 $EXPECT_TOTAL）"
fi

# ---------- 6) 下单 + 幂等重发 + 直扣库存 ----------
info "== Step 6: POST /orders（Idempotency-Key）=="
IDEM1="r1o1-${RUN_TAG}"
req POST /orders '{"kind":"cart","from_cart":true}' "Idempotency-Key" "$IDEM1"
O1=$(jq -r '.order_no // empty' <<<"$HTTP_BODY" 2>/dev/null)
A1=$(jq -r '.amount_cent // 0' <<<"$HTTP_BODY" 2>/dev/null)
ITEMS1=$(jq '.items | length' <<<"$HTTP_BODY" 2>/dev/null)
PAYPARAMS1=$(jq -c '.pay_params' <<<"$HTTP_BODY" 2>/dev/null)
if [ "$HTTP_CODE" = "201" ] && [ -n "$O1" ] && [ "$A1" = "$EXPECT_TOTAL" ] && [ "$ITEMS1" = "2" ] \
   && [ "$PAYPARAMS1" = "null" ]; then
  pass "下单 201：order_no=$O1 amount=$A1 分、2 行快照、pay_params=null（契约裁定：建单不拉起支付）"
else
  fail "下单断言失败（HTTP $HTTP_CODE no=$O1 amount=$A1 items=$ITEMS1 pay_params=$PAYPARAMS1）"
fi

req GET "/goods/113"
S113_1=$(jq -r '.stock // 0' <<<"$HTTP_BODY" 2>/dev/null)
if [ "$((S113_0 - 3))" = "$S113_1" ]; then
  pass "下单事务内同库直扣库存：#113 $S113_0 → $S113_1（-3）"
else
  fail "库存直扣断言失败（#113 $S113_0 → $S113_1，期望 -3）"
fi

req POST /orders '{"kind":"cart","from_cart":true}' "Idempotency-Key" "$IDEM1"
O1R=$(jq -r '.order_no // empty' <<<"$HTTP_BODY" 2>/dev/null)
if [ "$HTTP_CODE" = "201" ] && [ "$O1R" = "$O1" ]; then
  pass "同 key 同 payload 重发返回原单（order_no 一致=$O1，幂等命中）"
else
  fail "幂等重发异常（HTTP $HTTP_CODE no=$O1R 期望 $O1）"
fi

# ---------- 7) 支付 + 轮询履约 ----------
info "== Step 7: pay → 轮询至 fulfilled（worker 10s tick，超时 30s）=="
req POST "/orders/$O1/pay"
if [ "$HTTP_CODE" = "200" ] && [ "$(jq -r '.status' <<<"$HTTP_BODY" 2>/dev/null)" = "created" ] \
   && [ -n "$(jq -r '.payment.trade_no // empty' <<<"$HTTP_BODY" 2>/dev/null)" ]; then
  pass "pay 200 created（mock 渠道受理，trade_no 就绪；异步回调 ~200ms 后到达）"
else
  fail "pay 异常（HTTP $HTTP_CODE）：$(printf '%s' "$HTTP_BODY" | head -c 160)"
fi

STATUS=""; PAID_AT=""; FULFILLED_AT=""
deadline=$((SECONDS + 30))
while [ "$SECONDS" -lt "$deadline" ]; do
  req GET "/orders/$O1"
  STATUS=$(jq -r '.status // empty' <<<"$HTTP_BODY" 2>/dev/null)
  [ "$STATUS" = "fulfilled" ] && break
  sleep 2
done
PAID_AT=$(jq -r '.paid_at // empty' <<<"$HTTP_BODY" 2>/dev/null)
FULFILLED_AT=$(jq -r '.fulfilled_at // empty' <<<"$HTTP_BODY" 2>/dev/null)
if [ "$STATUS" = "fulfilled" ] && [ -n "$PAID_AT" ] && [ -n "$FULFILLED_AT" ]; then
  pass "订单已 fulfilled（mock 回调→paid→outbox worker 履约；paid_at/fulfilled_at 落值）"
else
  fail "轮询 30s 超时未 fulfilled（末态=$STATUS paid_at=$PAID_AT fulfilled_at=$FULFILLED_AT）"
fi

# ---------- 8) 我的订单列表 ----------
info "== Step 8: GET /orders 列表断言 =="
req GET "/orders"
if [ "$HTTP_CODE" = "200" ] && jq -e --arg no "$O1" '.items[] | select(.order_no==$no)' >/dev/null 2>&1 <<<"$HTTP_BODY"; then
  pass "订单列表包含 $O1"
else
  fail "订单列表未见 $O1（HTTP $HTTP_CODE）"
fi

# ---------- 9) 取消路径 ----------
info "== Step 9: 再造一单 → cancel → closed(user) + 库存回补 =="
req POST /cart/items "{\"goods_id\":114,\"qty\":1}"; [ "$HTTP_CODE" = "200" ] || fail "加购 #114×1 失败（HTTP $HTTP_CODE）"
IDEM2="r1o2-${RUN_TAG}"
req POST /orders '{"kind":"cart","from_cart":true}' "Idempotency-Key" "$IDEM2"
O2=$(jq -r '.order_no // empty' <<<"$HTTP_BODY" 2>/dev/null)
A2=$(jq -r '.amount_cent // 0' <<<"$HTTP_BODY" 2>/dev/null)
EXPECT2=$(( EXPECT_TOTAL + P114 ))
if [ "$HTTP_CODE" = "201" ] && [ -n "$O2" ] && [ "$A2" = "$EXPECT2" ]; then
  pass "第二单 201：order_no=$O2 amount=$A2 分（含 #114×1）"
else
  fail "第二单断言失败（HTTP $HTTP_CODE no=$O2 amount=$A2 期望 $EXPECT2）"
fi
req GET "/goods/114"
S114_1=$(jq -r '.stock // 0' <<<"$HTTP_BODY" 2>/dev/null)
req POST "/orders/$O2/cancel"
req GET "/orders/$O2"
C_STATUS=$(jq -r '.status // empty' <<<"$HTTP_BODY" 2>/dev/null)
C_REASON=$(jq -r '.closed_reason // empty' <<<"$HTTP_BODY" 2>/dev/null)
req GET "/goods/114"
S114_2=$(jq -r '.stock // 0' <<<"$HTTP_BODY" 2>/dev/null)
if [ "$C_STATUS" = "closed" ] && [ "$C_REASON" = "user" ] \
   && [ "$((S114_0 - 1))" = "$S114_1" ] && [ "$S114_2" = "$S114_0" ]; then
  pass "取消闭环：closed(user)，#114 库存 $S114_0 → $S114_1（-1 下单）→ $S114_2（回补）"
else
  fail "取消断言失败（status=$C_STATUS reason=$C_REASON stock $S114_0→$S114_1→$S114_2）"
fi

# ---------- 10) 幂等负例 + 错误体形状 ----------
info "== Step 10: 同 key 异 payload → 409 MALL_30003 + 错误体三字段 =="
req POST /orders '{"kind":"cart","from_cart":true}' "Idempotency-Key" "$IDEM1"
E_CODE=$(jq -r '.code // empty' <<<"$HTTP_BODY" 2>/dev/null)
E_RETRY=$(jq -r '.retryable' <<<"$HTTP_BODY" 2>/dev/null)
E_KEYS=$(jq -c 'keys | sort' <<<"$HTTP_BODY" 2>/dev/null)
if [ "$HTTP_CODE" = "409" ] && [ "$E_CODE" = "MALL_30003" ] && [ "$E_RETRY" = "false" ] \
   && [ "$E_KEYS" = '["code","error","retryable"]' ]; then
  pass "幂等冲突 409 MALL_30003 retryable=false；错误体恰为 {error, code, retryable} 三字段"
else
  fail "幂等负例断言失败（HTTP $HTTP_CODE code=$E_CODE retryable=$E_RETRY keys=$E_KEYS）"
fi

# ---------- 汇总 ----------
echo "------------------------------------------------------------"
if [ "$FAILS" -eq 0 ]; then
  echo "SMOKE R1: PASS"
else
  echo "SMOKE R1: FAIL（$FAILS 项失败）"
  exit 1
fi
