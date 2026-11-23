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
// 重试映射到同一 request_id，billing 侧按既有幂等重放路径返回原结果。
// 换 body 重用同一 key 由 Reserve 的请求指纹校验拒绝（同 request_id 参数
// 不一致即 mismatch），既不会白嫖也不会错账。不带 key 时仍用服务端 xid。
func Idempotency() gin.HandlerFunc {
	return func(c *gin.Context) {
		requestID := ""
		if key := c.GetHeader("Idempotency-Key"); key != "" {
			tenant := c.GetUint64("tenant_id")
			sum := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s", tenant, key)))
			requestID = "idem-" + hex.EncodeToString(sum[:16])
		} else {
			requestID = xid.New()
		}
		c.Set("request_id", requestID)
		c.Next()
	}
}
