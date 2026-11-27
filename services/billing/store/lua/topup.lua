-- 入账/退款：把一笔资金增量恰好在 Redis 投影上应用一次（幂等）。
--
-- 为什么需要它：批量落账模式下账本列落后于 Redis（billing 批次里还有未落库的
-- reserve/settle 净额，即 pending != 0）。此时「用账本列覆盖投影」是错的——列
-- 里缺了在途扣款，覆盖会把已扣的钱复活，并发请求能再花一次。唯一安全的做法
-- 是在 Redis 上做同向增量，让不变式「Redis = 账本列 + pending」继续成立。
--
-- 与 settle.lua 的关系：两者都做「delta 恰好应用一次」，但 settle 允许余额 key
-- 缺失时按 0 起算（结算路径已先 requireCashMatch 保证 key 存在）；入账/退款不能
-- 这样——key 缺失时按 0 起算会丢掉 initial_quota，必须由调用方先补种投影。
-- 因此这里在 key 缺失时返回 -1，让调用方走重建路径。
--
-- KEYS[1] = billing:balance:{tenant_id}   Int  当前余额
-- KEYS[2] = billing:topup:{ref_type}:{ref_id}   值 = v2:已应用金额（TTL 24h）
-- ARGV[1] = delta（入账为正、退款为负；语义同 settle：新余额 = 旧余额 - delta）
--
-- return {code, balance, applied}
--   code: 1=按请求额应用, 2=已应用过（幂等重放）, -1=投影 key 缺失, -2=扣减超出余额（只扣到 0）
--   applied: 本次（或首次）实际应用的 delta
-- Lua 5.1 numbers are doubles, and tostring only retains limited digits.
-- Restrict all arithmetic and persisted delta markers to exact 14-digit integers.
local function safe_int(raw, signed)
  if not raw or not string.match(raw, '^%-?%d+$') then return nil end
  local negative = string.sub(raw, 1, 1) == '-'
  if negative and not signed then return nil end
  local digits = negative and string.sub(raw, 2) or raw
  digits = string.gsub(digits, '^0+', '')
  if digits == '' then digits = '0' end
  if #digits > 14 then return nil end
  return tonumber(raw)
end
local raw_bal = redis.call('GET', KEYS[1])
if not raw_bal then
  -- 投影缺失：调用方必须先补种，不能按 0 起算（会丢掉 initial_quota）。
  return {-1, 0, 0}
end
local bal = safe_int(raw_bal, false)
local delta = safe_int(ARGV[1], true)
if not bal or not delta then return redis.error_reply('cash amount outside safe range') end
local existing = redis.call('GET', KEYS[2])
if existing then
  if string.sub(existing, 1, 3) == 'v2:' then
    local prior = safe_int(string.sub(existing, 4), true)
    if not prior then return redis.error_reply('cash guard outside safe range') end
    return {2, bal, prior}
  end
  local prior = safe_int(existing, true)
  if not prior then return redis.error_reply('cash guard outside safe range') end
  return {2, bal, prior}
end
local applied = delta
if delta > 0 and bal < delta then
  -- 扣减（退款）超过当前余额时只扣到 0，绝不把投影打成负数。
  applied = bal
  if applied < 0 then applied = 0 end
end
if bal - applied > 99999999999999 then return redis.error_reply('cash balance outside safe range') end
redis.call('SET', KEYS[2], 'v2:' .. tostring(applied), 'EX', 86400)
if applied > 0 then
  redis.call('DECRBY', KEYS[1], applied)
elseif applied < 0 then
  redis.call('INCRBY', KEYS[1], -applied)
end
local code = 1
if delta > 0 and applied < delta then
  code = -2
end
return {code, bal - applied, applied}
