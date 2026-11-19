#!/usr/bin/env bash
# my-market R0 冒烟与验收（scripts/smoke-r0.sh）。
#
# 前置：deploy/compose 的 infra + ai 两 profile 已起且 healthy（8/8）。
# 步骤：
#   1) 全 healthz（billing:9103 / router:9102 / agent:9105）+ gateway:18080 首页 200
#      + billing gRPC :9101 / gateway :18080 TCP 可达
#   2) 幂等造数（等价 SQL，复用 services/ai-gateway/scripts/schema.sql 的 seed 口径）：
#      - providers mock-a/mock-b 重指到容器服务名 http://mock-provider:9201
#        （schema.sql 种子写的是宿主回环 127.0.0.1:9201/9202，容器网络内不通）
#        并清零熔断计数（fail_count/consecutive_fail/cooldown_until）
#      - 目录补 mock-model SKU（billing 计费权威：目录无此模型 reserve 直接拒绝）
#      - demo 租户 + demo key（明文 sk-demo-8f3a2b1c9d4e5f60，与 schema.sql 种子一致）
#      - Redis 余额缺失/过低时按权威公式重算（initial + Σtopup - Σsettle - Σpending）
#   3) POST :18080/v1/chat/completions（Bearer demo key，mock 上游，router 30s 路由
#      缓存故带重试）
#   4) 查账：MySQL 直连断言该 request 恰好 reserve+settle 两行且均已 settled；
#      并经 billing admin（:9103 + X-Control-Token，凭据取自 .env）交叉验证 settle 行可见
#   5) 末行输出 SMOKE R0: PASS / FAIL
#
# 幂等：所有 SQL 都是 INSERT IGNORE / ON DUPLICATE KEY UPDATE / 条件 UPDATE，可重复执行。
# 凭据：CONTROL_PLANE_TOKEN / MYSQL_ROOT_PASSWORD / REDIS_PASSWORD 一律读 deploy/compose/.env，
#       脚本内不硬编码、不回显。
set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ENV_FILE="$ROOT/deploy/compose/.env"
# 统一走 localhost（双栈回退）：本机如残留旧仓原生进程占住 IPv4 127.0.0.1 回环，
# compose 的端口发布（Docker Desktop *:PORT IPv6 通配）仍可经 ::1 命中新栈；
# 老进程清掉后 localhost 也回落到 compose 的 IPv4 映射。两种形态都指向 compose 栈。
HOSTADDR="localhost"
GATEWAY="http://$HOSTADDR:18080"
BILLING_ADMIN="http://$HOSTADDR:9103"
DEMO_KEY="sk-demo-8f3a2b1c9d4e5f60" # schema.sql 种子明文（只存 SHA-256，key_hash 见下方 SQL）
DEMO_KEY_HASH="d0cc753715f6e0aa383534c467f56d49ad19474c2c5adf6c7e6ef02eb842c5b2"
PROVIDER_URL="http://mock-provider:9201"

FAILS=0
DC=(docker compose -f "$ROOT/deploy/compose/docker-compose.yml")

pass() { printf '[PASS] %s\n' "$1"; }
fail() { printf '[FAIL] %s\n' "$1"; FAILS=$((FAILS + 1)); }
info() { printf '[....] %s\n' "$1"; }

# ---------- 凭据（只读，不回显） ----------
if [ ! -f "$ENV_FILE" ]; then
  echo "缺少 $ENV_FILE（cp deploy/compose/.env.example deploy/compose/.env 并填写后重试）"
  echo "SMOKE R0: FAIL"
  exit 1
fi
get_env() { sed -n "s/^$1=//p" "$ENV_FILE" | head -1 | tr -d '\r'; }
CONTROL_PLANE_TOKEN="$(get_env CONTROL_PLANE_TOKEN)"
MYSQL_ROOT_PASSWORD="$(get_env MYSQL_ROOT_PASSWORD)"
REDIS_PASSWORD="$(get_env REDIS_PASSWORD)"
if [ -z "$CONTROL_PLANE_TOKEN" ] || [ -z "$MYSQL_ROOT_PASSWORD" ]; then
  echo ".env 缺 CONTROL_PLANE_TOKEN / MYSQL_ROOT_PASSWORD"
  echo "SMOKE R0: FAIL"
  exit 1
fi

command -v curl >/dev/null 2>&1 || { echo "需要 curl"; echo "SMOKE R0: FAIL"; exit 1; }
command -v docker >/dev/null 2>&1 || { echo "需要 docker"; echo "SMOKE R0: FAIL"; exit 1; }

# ---------- helpers ----------
http_code() { curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$1"; }

tcp_ok() { # host port；无 nc 时退回 bash /dev/tcp
  if command -v nc >/dev/null 2>&1; then
    nc -z -w 3 "$1" "$2" >/dev/null 2>&1
  else
    (exec 3<>"/dev/tcp/$1/$2") 2>/dev/null
  fi
}

# MySQL 直连（宿主 3307 无客户端依赖，走 compose 网络内 mysql 容器）
sql() {
  local out rc
  out=$("${DC[@]}" exec -T -e MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql \
        mysql -uroot -N -B ai_gateway -e "$1" 2>&1)
  rc=$?
  if [ $rc -ne 0 ]; then
    printf 'sql 失败: %s\n' "$out" >&2
    return 1
  fi
  printf '%s\n' "$out"
}

redis_cli() {
  if [ -n "$REDIS_PASSWORD" ]; then
    "${DC[@]}" exec -T -e REDISCLI_AUTH="$REDIS_PASSWORD" redis redis-cli "$@"
  else
    "${DC[@]}" exec -T redis redis-cli "$@"
  fi
}

# ---------- 1) 健康检查 ----------
info "== Step 1: healthz / TCP 探活 =="
hc=0
for spec in "billing-admin 9103" "router 9102" "agent 9105"; do
  set -- $spec
  code=$(http_code "http://$HOSTADDR:$2/healthz")
  if [ "$code" = "200" ]; then pass "$1 healthz :$2 = 200"; else fail "$1 healthz :$2 = $code（期望 200）"; hc=1; fi
done
code=$(http_code "$GATEWAY/")
if [ "$code" = "200" ]; then pass "gateway 首页 :18080 = 200"; else fail "gateway 首页 :18080 = $code（期望 200）"; hc=1; fi
if tcp_ok "$HOSTADDR" 9101; then pass "billing gRPC :9101 TCP 可达"; else fail "billing gRPC :9101 TCP 不可达"; hc=1; fi
if tcp_ok "$HOSTADDR" 18080; then pass "gateway :18080 TCP 可达"; else fail "gateway :18080 TCP 不可达"; hc=1; fi
if ! sql "SELECT 1" >/dev/null 2>&1; then fail "MySQL（compose mysql 容器）不可达或凭据不符"; hc=1; fi

# ---------- 2) 幂等造数 ----------
info "== Step 2: provider 重指 / 目录 SKU / 租户与 key / 余额 =="
if sql "INSERT INTO providers (name, base_url, models, weight) VALUES
          ('mock-a', '$PROVIDER_URL', '[\"*\"]', 5),
          ('mock-b', '$PROVIDER_URL', '[\"*\"]', 3)
        ON DUPLICATE KEY UPDATE base_url = VALUES(base_url);
        UPDATE providers SET base_url='$PROVIDER_URL', status=0,
               fail_count=0, consecutive_fail=0, cooldown_until=NULL
        WHERE name IN ('mock-a','mock-b');" >/dev/null; then
  n=$(sql "SELECT COUNT(*) FROM providers WHERE name IN ('mock-a','mock-b') AND base_url='$PROVIDER_URL' AND status=0" 2>/dev/null)
  if [ "$n" = "2" ]; then pass "providers mock-a/mock-b 已指向 $PROVIDER_URL 且 active"; else fail "providers 重指后校验不符（count=$n，期望 2）"; fi
else
  fail "providers 重指 SQL 执行失败"
fi

if sql "INSERT INTO models (vendor, model_id, name, input_price_cent, output_price_cent, context_len, tags)
        VALUES ('mock', 'mock-model', 'Mock Model', 100, 200, 128000, '[\"轻量\"]')
        ON DUPLICATE KEY UPDATE status = 0;" >/dev/null \
   && [ "$(sql "SELECT COUNT(*) FROM models WHERE model_id='mock-model' AND status=0" 2>/dev/null)" = "1" ]; then
  pass "目录 SKU mock-model 已上架（100/200 分/百万 token）"
else
  fail "mock-model SKU 写入/校验失败"
fi

if sql "INSERT IGNORE INTO tenants (name, initial_quota) VALUES ('demo', 5000);
        INSERT IGNORE INTO api_keys (tenant_id, key_hash, key_prefix, name)
          SELECT id, '$DEMO_KEY_HASH', 'sk-demo-8', 'demo-key' FROM tenants WHERE name='demo';
        UPDATE api_keys k JOIN tenants t ON k.tenant_id = t.id
          SET k.status = 0 WHERE t.name='demo' AND k.key_hash='$DEMO_KEY_HASH';" >/dev/null; then
  TID=$(sql "SELECT id FROM tenants WHERE name='demo'" 2>/dev/null)
  keycnt=$(sql "SELECT COUNT(*) FROM api_keys WHERE key_hash='$DEMO_KEY_HASH' AND status=0 AND tenant_id=$TID" 2>/dev/null)
  if [ -n "$TID" ] && [ "$keycnt" = "1" ]; then
    pass "demo 租户（id=$TID）+ active demo key 就绪"
  else
    fail "demo 租户/key 校验不符（tenant=$TID key=$keycnt）"
  fi
else
  fail "租户/key 造数 SQL 执行失败"
fi

# 余额投影兜底：缺失或 <100 分时按权威公式重算（与 seed 脚本同口径）
bal="$(redis_cli GET "billing:balance:$TID" 2>/dev/null | tr -d '\r\n')"
printf '%s' "$bal" | grep -qE '^-?[0-9]+$' || bal=""
if [ -z "$bal" ] || [ "$bal" -lt 100 ]; then
  initial=$(sql "SELECT initial_quota FROM tenants WHERE id=$TID" 2>/dev/null)
  settled=$(sql "SELECT COALESCE(SUM(actual_quota),0) FROM bills WHERE tenant_id=$TID AND phase='settle' AND status='settled'" 2>/dev/null)
  pending=$(sql "SELECT COALESCE(SUM(pre_quota),0) FROM bills WHERE tenant_id=$TID AND phase='reserve' AND status='pending'" 2>/dev/null)
  topups=$(sql "SELECT COALESCE(SUM(amount),0) FROM topups WHERE tenant_id=$TID" 2>/dev/null)
  computed=$(( ${initial:-0} + ${topups:-0} - ${settled:-0} - ${pending:-0} ))
  if [ "$computed" -lt 100 ]; then
    fail "demo 余额不足（Redis=$bal，账本重算=$computed 分）——请先充值/调额"
  else
    redis_cli SET "billing:balance:$TID" "$computed" >/dev/null 2>&1
    pass "Redis 余额缺失/过低，已按账本重算并回写（$computed 分）"
  fi
else
  pass "Redis 余额投影正常（$bal 分）"
fi

# ---------- 3) 全链路对话 ----------
info "== Step 3: POST :18080/v1/chat/completions（mock 上游）=="
PRE_MAX=$(sql "SELECT COALESCE(MAX(id),0) FROM bills" 2>/dev/null)
chat_body=""
chat_ok=0
# router 路由缓存 TTL 30s：provider 刚重指，前 1-2 发可能命中旧地址，带重试
for attempt in 1 2 3 4 5; do
  chat_body=$(curl -s --max-time 30 -w '
%{http_code}' \
    -H "Authorization: Bearer $DEMO_KEY" -H "Content-Type: application/json" \
    -d '{"model":"mock-model","messages":[{"role":"user","content":"R0 smoke ping"}],"stream":false}' \
    "$GATEWAY/v1/chat/completions")
  chat_code="$(printf '%s' "$chat_body" | tail -n 1)"
  chat_body="$(printf '%s' "$chat_body" | sed '$d')"
  if [ "$chat_code" = "200" ] && printf '%s' "$chat_body" | grep -q '"object":"chat.completion"'; then
    chat_ok=1
    break
  fi
  info "第 $attempt 发未成功（HTTP $chat_code）：$(printf '%s' "$chat_body" | head -c 160)，8s 后重试"
  sleep 8
done
if [ "$chat_ok" = "1" ]; then
  pass "chat completions 200 且返回 chat.completion（$(printf '%s' "$chat_body" | head -c 80)...）"
else
  fail "chat completions 重试 5 次仍失败（最后 HTTP $chat_code）"
fi

# ---------- 4) 查账：reserve+settle 两行 ----------
info "== Step 4: 账本断言（MySQL 直连 + billing admin 交叉验证）=="
rid=""
ledger_ok=0
for i in 1 2 3 4 5 6 7 8; do
  rid=$(sql "SELECT request_id FROM bills WHERE id > $PRE_MAX ORDER BY id DESC LIMIT 1" 2>/dev/null)
  if [ -n "$rid" ]; then
    cnt=$(sql "SELECT COUNT(*) FROM bills WHERE request_id='$rid'" 2>/dev/null)
    phases=$(sql "SELECT GROUP_CONCAT(phase ORDER BY phase) FROM bills WHERE request_id='$rid'" 2>/dev/null)
    rstat=$(sql "SELECT status FROM bills WHERE request_id='$rid' AND phase='reserve'" 2>/dev/null)
    sstat=$(sql "SELECT status FROM bills WHERE request_id='$rid' AND phase='settle'" 2>/dev/null)
    actual=$(sql "SELECT COALESCE(actual_quota,-1) FROM bills WHERE request_id='$rid' AND phase='settle'" 2>/dev/null)
    if [ "$cnt" = "2" ] && [ "$phases" = "reserve,settle" ] \
       && [ "$rstat" = "settled" ] && [ "$sstat" = "settled" ] && [ "$actual" -ge 0 ] 2>/dev/null; then
      ledger_ok=1
      break
    fi
  fi
  sleep 2
done
if [ "$ledger_ok" = "1" ]; then
  pass "该 request 账本恰为 reserve+settle 两行且均 settled（request_id=$rid，settle actual=${actual} 分）"
else
  fail "账本断言失败（request_id=${rid:-无}，count=${cnt:-0}，phases=${phases:-无}，reserve=$rstat settle=$sstat）"
fi

# billing admin（:9103 + X-Control-Token）交叉验证 settle 行可查
admin_hits=$(curl -s --max-time 5 -H "X-Control-Token: $CONTROL_PLANE_TOKEN" \
  "$BILLING_ADMIN/admin/bills?tenant_id=$TID&phase=settle&limit=5" \
  | grep -c "\"RequestID\":\"$rid\"" 2>/dev/null)
if [ "${rid:-x}" != "x" ] && [ "${admin_hits:-0}" -ge 1 ] 2>/dev/null; then
  pass "billing admin /admin/bills 可查到该 settle 行（:9103 + X-Control-Token）"
else
  fail "billing admin /admin/bills 未查到该 settle 行"
fi

# ---------- 汇总 ----------
echo "------------------------------------------------------------"
if [ "$FAILS" -eq 0 ]; then
  echo "SMOKE R0: PASS"
else
  echo "SMOKE R0: FAIL（$FAILS 项失败）"
  exit 1
fi
