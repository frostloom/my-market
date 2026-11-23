package middleware

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/gin-gonic/gin"

	"github.com/frostloom/my-market/pkg/xid"
)

// Idempotency 给这次调用生成 request_id。明细按这个值把预扣和结算记在一起。
//
// 客户端带 Idempotency-Key 时参与去重（OpenAI 语义：超时重试同一请求不得
// 双倍计费）：request_id = "idem-" + sha256(tenant|key)——同租户同 key 的
// 重试映射到同一 request_id，复用同一笔预扣，不再二次计费。若首笔已完成，
// 重试会收到 409（重复/已终态）——客户端应视为「请求已被受理」。
// 注意与 OpenAI 24h 窗口的差异：本实现无时间窗，key 一经使用即永久去重，
// 调用方应为每个新请求取新 key。换 body 重用同一 key 由 Reserve 的请求
// 指纹校验拒绝（同 request_id 参数不一致即 mismatch），既不会白嫖也不会
// 错账。不带 key 时仍用服务端 xid。
//
// 租户取值用 GetInt64：apikey/portal 中间件存的是 int64（protobuf 字段），
// GetUint64 断言失败恒返 0 会让租户盐失效（跨租户同 key 串台）。
func Idempotency() gin.HandlerFunc {
	return func(c *gin.Context) {
		requestID := ""
		if key := c.GetHeader("Idempotency-Key"); key != "" {
			tenant := c.GetInt64("tenant_id")
			sum := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s", tenant, key)))
			requestID = "idem-" + hex.EncodeToString(sum[:16])
		} else {
			requestID = xid.New()
		}
		c.Set("request_id", requestID)
		c.Next()
	}
}
