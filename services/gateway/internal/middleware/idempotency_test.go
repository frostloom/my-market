package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// Idempotency-Key 参与去重（审查 M9 修复）：同租户同 key → 同 request_id
// （重试映射到同一预扣/结算）；不同 key / 不带 key → 不同 id；不同租户同 key 也不串。
// tenant 用 int64 注入——镜像生产接线（apikey/portal 存 protobuf int64），
// 防止 GetInt64/GetUint64 类型失配回归。
func TestIdempotencyKeyDerivesStableRequestID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	run := func(tenant int64, key string) string {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		if key != "" {
			c.Request.Header.Set("Idempotency-Key", key)
		}
		if tenant != 0 {
			c.Set("tenant_id", tenant)
		}
		Idempotency()(c)
		return c.GetString("request_id")
	}

	a1 := run(7, "retry-1")
	a2 := run(7, "retry-1")
	if a1 == "" || a1 != a2 {
		t.Fatalf("same tenant+key must derive same request_id: %q vs %q", a1, a2)
	}
	if b := run(7, "retry-2"); b == a1 {
		t.Fatalf("different key derived same request_id: %q", b)
	}
	if other := run(8, "retry-1"); other == a1 {
		t.Fatalf("different tenant derived same request_id: %q", other)
	}
	if none := run(7, ""); none == "" || none == a1 || len(none) < 16 {
		t.Fatalf("no key must fall back to server xid, got %q", none)
	}
}
