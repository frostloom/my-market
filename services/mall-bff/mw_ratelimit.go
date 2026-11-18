// mw_ratelimit.go：mall-bff 基础限流（api-contract §8 第一层，D12/N11）。
//
// 机制（task-6 裁定 3）：per-tenant Redis 分钟桶计数器——INCR + 首次 EXPIRE 60s，
// key 含作用域与身份（mall:rl:<scope>:<id>），超 120/min → 429 MALL_10006
// （retryable=true）。
//
// 两个作用域（同一实现不同 keyFn/挂点）：
//   - tenant：鉴权后全接口（authed 路由组，auth 中间件之后——匿名请求先被 401，
//     不占租户配额）；
//   - ip：/auth/register、/auth/login（无租户语境；billing 4 并发槽位之外的
//     第二层，常数同为 120）。
//
// Redis 故障 fail-open（限流器故障不应打死商城主链路）：记日志放行。
package main

import (
	"context"
	"log/slog"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"

	"github.com/frostloom/my-market/pkg/mallerr"
)

// rateLimitKeyFn 由路由组决定限流键（tenant 或 ip 维度）。
type rateLimitKeyFn func(c *gin.Context) (string, bool)

// tenantRateLimitKey 鉴权路由组：per-tenant 键；拿不到租户（不应发生）
// 返回 false → 本请求不计数。
func tenantRateLimitKey(c *gin.Context) (string, bool) {
	id := tenantID(c)
	if id == 0 {
		return "", false
	}
	return strconv.FormatUint(id, 10), true
}

// ipRateLimitKey 匿名端点（register/login）：客户端 IP 维度。
func ipRateLimitKey(c *gin.Context) (string, bool) {
	ip := c.ClientIP()
	if ip == "" {
		return "", false
	}
	return ip, true
}

// rateLimitScript 原子分钟桶：INCR；首个计数才设 60s 过期。
// 单条 Lua 保证原子性——INCR/EXPIRE 两步分离时 EXPIRE 失败会让桶永不过期，
// 该租户/IP 被永久 429（失效模式是硬锁而非漏限，评审 Fix Round 1）。
// KEYS[1]=桶键；返回：计数（EXPIRE 失败时返回 -1，调用方 fail-open 放行）。
var rateLimitScript = redis.NewScript(`
local n = redis.call('INCR', KEYS[1])
if n == 1 then
  if redis.call('PEXPIRE', KEYS[1], 60000) == 0 then
    return -1
  end
end
return n
`)

// rateLimitMiddleware 分钟桶限流。keyFn 产出桶身份；超限 → 429 MALL_10006。
func rateLimitMiddleware(rdb *redis.Client, scope string, limit int, keyFn rateLimitKeyFn, log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		key, ok := keyFn(c)
		if !ok {
			c.Next()
			return
		}
		bucket := "mall:rl:" + scope + ":" + key
		n, err := rateLimitScript.Run(context.Background(), rdb, []string{bucket}).Int64()
		if err != nil || n < 0 {
			// Redis 故障 / 过期设置失败：fail-open（限流器故障不应打死主链路）。
			if err != nil {
				log.Warn("ratelimit redis error, fail-open", "err", err)
			}
			c.Next()
			return
		}
		if n > int64(limit) {
			abortWith(c, mallerr.E(mallerr.CodeRateLimited, false, "请求过于频繁，请稍后重试"))
			return
		}
		c.Next()
	}
}
