#!/usr/bin/env bash
# ci-gotest-baseline.sh — ai-gateway 失败名基线门禁（honest green，不是绕过）。
#
# 用法（在 services/ai-gateway 模块内，或任意目录）：
#   bash scripts/ci-gotest-baseline.sh scripts/ci-baseline-ai-gateway.txt
#
# 行为：
#   1. 以 CGO_ENABLED=0 / AGENT_LIVE_JEV=0（不覆盖外部已设值）跑 `go test ./... -count=1`，
#      完整输出原样透传到 stdout（CI 日志保留全部证据）。
#   2. 从输出提取顶层 `--- FAIL: TestXxx` 测试名（不含缩进的子测试行），排序去重。
#   3. 与基线文件（# 注释与空行忽略）比对：
#      - 出现基线之外的新增失败   → 打印新增名，exit 1（门禁拒绝）；
#      - 失败集 ⊆ 基线（全等或部分复现）→ exit 0：基线内失败仍在属预期
#        （go test 因此 exit≠0 是正常形态）；其中基线内消失的名字按
#        "BASELINE SHRUNK（好事）" 打印，应当从基线文件删除（只减不增）。
#      - 全部通过                  → 打印 "BASELINE SHRUNK（好事）"，exit 0。
#
# 保守失败口径（注释明示）：
#   - go test exit≠0 但一个顶层失败名都提不出来（典型：编译错误 `#[build failed]`、
#     超时、包级 setup 失败、vet 失败等）→ 无法证明"失败都在基线内"，
#     直接 exit 1，绝不放行。
#   - 输出中出现 `[build failed]` / `[setup failed]` 标记，或 `panic:` /
#     `fatal error:` 进程级崩溃标记时，即使同时存在基线内的普通测试失败
#     （可能掩盖编译破损/崩溃）也 exit 1。
set -uo pipefail

BASELINE="${1:?usage: ci-gotest-baseline.sh <baseline-file>}"
MODULE_DIR="$(cd "$(dirname "$0")/.." && pwd)"
cd "$MODULE_DIR"

if [ ! -f "$BASELINE" ]; then
    echo "ci-gotest-baseline: baseline file not found: $BASELINE" >&2
    exit 1
fi

export CGO_ENABLED="${CGO_ENABLED:-0}"
export AGENT_LIVE_JEV="${AGENT_LIVE_JEV:-0}"

OUT="$(mktemp)"
trap 'rm -f "$OUT" "$OUT.names"' EXIT

rc=0
go test ./... -count=1 | tee "$OUT"
rc=${PIPESTATUS[0]}

# 顶层失败名：`--- FAIL: TestXxx`（行首无缩进；子测试行有缩进，天然被 ^ 排除）。
grep -E '^--- FAIL: Test[A-Za-z0-9_]+' "$OUT" \
    | sed -E 's/^--- FAIL: (Test[A-Za-z0-9_]+).*$/\1/' \
    | LC_ALL=C sort -u > "$OUT.names" || true

# 编译/装配破损标记：出现即保守拒绝（它们不是普通测试失败，不在基线口径内）。
if grep -Eq '\[(build|setup) failed\]' "$OUT"; then
    echo "ci-gotest-baseline: build/setup failure detected; refusing baseline masking" >&2
    exit 1
fi

baseline="$(grep -vE '^(#|$)' "$BASELINE" | tr -d '\r' | LC_ALL=C sort -u)"
if [ -z "$baseline" ]; then
    echo "ci-gotest-baseline: baseline file has no entries; refusing (misconfigured gate)" >&2
    exit 1
fi

new_failures="$(LC_ALL=C comm -23 "$OUT.names" <(printf '%s\n' "$baseline"))"
healed="$(LC_ALL=C comm -13 "$OUT.names" <(printf '%s\n' "$baseline"))"

if [ -n "$new_failures" ]; then
    echo "ci-gotest-baseline: NEW FAILURES outside baseline (gate fails):" >&2
    printf '%s\n' "$new_failures"
    exit 1
fi

# 走到这里：当前失败集 ⊆ 基线。基线内的失败仍然失败是预期形态（go test 因此
# exit≠0 属正常，不得据此拒绝）。保守口径只拒绝两类非测试失败形态：
#   1) 一个顶层失败名都提不出来但 exit≠0 —— 超时/崩溃/包级失败；
#   2) panic/fatal 标记 —— 进程级崩溃（可能伴随部分失败名，掩盖真实状态）。
if grep -Eq '^(panic:|fatal error:)' "$OUT"; then
    echo "ci-gotest-baseline: panic/fatal detected; refusing baseline masking" >&2
    exit 1
fi
if [ ! -s "$OUT.names" ]; then
    if [ "$rc" -ne 0 ]; then
        echo "ci-gotest-baseline: go test exited $rc with no top-level FAIL names" \
             "(timeout/panic/package-level failure); refusing" >&2
        exit 1
    fi
    echo "BASELINE SHRUNK（好事）: all tests pass; 基线已全部修复 — 清空/裁剪基线文件"
    exit 0
fi

if [ -n "$healed" ]; then
    echo "BASELINE SHRUNK（好事）: 以下失败本地已不复现，请从 ci-baseline-ai-gateway.txt 删除对应行："
    printf '%s\n' "$healed"
else
    echo "BASELINE EQUAL（全等）: 失败集与基线完全一致，无新增、无收缩"
fi
exit 0
