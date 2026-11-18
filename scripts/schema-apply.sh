#!/usr/bin/env bash
# R1-T1: market 库 schema 应用（scripts/schema-apply.sh）。
#
# 把 docs/spec/db-schema.sql 逐字执行进 deploy/compose 的 MySQL（my-market-mysql-1，宿主 3307）。
# db-schema.sql 不做任何修改（裁定：逐字执行）——它自带 CREATE DATABASE IF NOT EXISTS market
# 与六品类种子（精选/手机/服饰/穿搭/家居/福利）。
#
# 幂等：db-schema.sql 里的 CREATE TABLE 是普通 CREATE TABLE（无 IF NOT EXISTS），
# 直接重跑会报表已存在。故本脚本自守卫：
#   - market 库已存在 9 张表 → 跳过 schema 应用（第二次运行 = no-op，成功退出）；
#   - 否则（0 张表或建到一半）→ 逐字执行 db-schema.sql 后复核。
#
# 凭据：MYSQL_ROOT_PASSWORD 只从 deploy/compose/.env 读，经 docker exec -e MYSQL_PWD
# 注入容器环境，不硬编码、不进 mysql 客户端 argv（与 scripts/smoke-r0.sh 同姿势）。
#
# 用法：bash scripts/schema-apply.sh        # 应用 schema（幂等）
set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ENV_FILE="$ROOT/deploy/compose/.env"
SCHEMA_FILE="$ROOT/docs/spec/db-schema.sql"
EXPECTED_TABLES=9

FAILS=0
DC=(docker compose -f "$ROOT/deploy/compose/docker-compose.yml")

pass() { printf '[PASS] %s\n' "$1"; }
fail() { printf '[FAIL] %s\n' "$1"; FAILS=$((FAILS + 1)); }
info() { printf '[....] %s\n' "$1"; }

# ---------- 凭据（只读，不回显） ----------
if [ ! -f "$ENV_FILE" ]; then
  echo "缺少 $ENV_FILE（cp deploy/compose/.env.example deploy/compose/.env 并填写后重试）"
  exit 1
fi
get_env() { sed -n "s/^$1=//p" "$ENV_FILE" | head -1 | tr -d '\r'; }
MYSQL_ROOT_PASSWORD="$(get_env MYSQL_ROOT_PASSWORD)"
if [ -z "$MYSQL_ROOT_PASSWORD" ]; then
  echo ".env 缺 MYSQL_ROOT_PASSWORD"
  exit 1
fi
if [ ! -f "$SCHEMA_FILE" ]; then
  echo "缺少 $SCHEMA_FILE"
  exit 1
fi

command -v docker >/dev/null 2>&1 || { echo "需要 docker"; exit 1; }

# mysql 客户端执行（容器内；--default-character-set 保证中文品类名不乱码）
mcysql() {
  "${DC[@]}" exec -T -e MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql \
    mysql -uroot --default-character-set=utf8mb4 "$@"
}

# SQL 查询（-N -B 裸输出）
sql() {
  mcysql -N -B -e "$1"
}

# ---------- 前置：MySQL 可达 ----------
if ! sql "SELECT 1" >/dev/null 2>&1; then
  echo "MySQL（compose mysql 容器）不可达或凭据不符——请先起 infra profile"
  exit 1
fi
pass "MySQL 可达（compose 服务 mysql / 宿主 3307）"

# ---------- 应用 schema（自守卫，逐字执行文件本体） ----------
tables_now="$(sql "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema='market'" 2>/dev/null)"
if [ "$tables_now" = "$EXPECTED_TABLES" ]; then
  info "market 库已有 $EXPECTED_TABLES 张表，跳过 schema 应用（幂等 no-op）"
else
  info "应用 docs/spec/db-schema.sql（逐字，现 market 表数=$tables_now）"
  if mcysql < "$SCHEMA_FILE" 2>&1 | tee /tmp/schema-apply-last.log | grep -iE 'error' ; then
    fail "db-schema.sql 执行报错（详见上方 ERROR 行 / /tmp/schema-apply-last.log）"
  else
    pass "db-schema.sql 执行完成"
  fi
fi

# ---------- 复核 ----------
info "== 复核 =="
tables="$(sql "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema='market'" 2>/dev/null)"
if [ "$tables" = "$EXPECTED_TABLES" ]; then
  pass "market 表数 = $tables（期望 $EXPECTED_TABLES）"
else
  fail "market 表数 = $tables（期望 $EXPECTED_TABLES）"
fi
cats="$(sql "SELECT COUNT(*) FROM market.categories" 2>/dev/null)"
if [ "$cats" = "6" ]; then
  pass "categories = $cats 行（期望 6：精选/手机/服饰/穿搭/家居/福利）"
else
  fail "categories = $cats 行（期望 6）"
fi
names="$(sql "SELECT GROUP_CONCAT(name ORDER BY sort) FROM market.categories" 2>/dev/null)"
info "品类：$names"

# ---------- 汇总 ----------
if [ "$FAILS" -eq 0 ]; then
  echo "SCHEMA APPLY: PASS"
else
  echo "SCHEMA APPLY: FAIL（$FAILS 项失败）"
  exit 1
fi
