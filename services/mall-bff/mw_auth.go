// mw_auth.go：会话中间件（D14 裁定 / ADR-006）。
//
// 链路：前端 Authorization: Bearer <会话token> → bff 验证 = 以
// Cookie: agw_user=<token> 调 gateway :18080 GET /auth/me（gateway 注入
// X-Control-Token 转发 billing），200 取 {id, username, tenant_id}；
// token 不直连 billing，gateway 也不回 Set-Cookie（bff 不代管 cookie）。
//
// 30s 进程内缓存按 token 键（task-6 裁定 1）：命中不打 gateway。
// 缓存语义：只缓存"验证通过"的租户身份；撤销/过期 token 走 gateway 实时拒绝。
//
// 10001 vs 10002 口径（task-6 实现决定，见报告）：billing /auth/me 对
// 未知/撤销/过期 token 统一 401"登录已过期，请重新登录"，无法区分；
// bff 映射——无有效凭证可验（缺头/非 Bearer/空 token）→ MALL_10001
// （未登录/会话无效）；凭证提交但网关拒绝 → MALL_10002（会话过期，重新登录），
// 与 gateway 文案及契约语义一致。
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/frostloom/my-market/pkg/mallerr"
)

// sessionCache 会话验证结果缓存（按 token 键，TTL 30s）。
type sessionCache struct {
	ttl time.Duration
	mu  sync.RWMutex
	m   map[string]sessionEntry
}

type sessionEntry struct {
	ident     gatewayIdentity
	expiresAt time.Time
}

type gatewayIdentity struct {
	UserID   uint64
	Username string
	TenantID uint64
}

func newSessionCache(ttl time.Duration) *sessionCache {
	return &sessionCache{ttl: ttl, m: map[string]sessionEntry{}}
}

func (sc *sessionCache) get(token string) (gatewayIdentity, bool) {
	sc.mu.RLock()
	e, ok := sc.m[token]
	sc.mu.RUnlock()
	if !ok || time.Now().After(e.expiresAt) {
		return gatewayIdentity{}, false
	}
	return e.ident, true
}

func (sc *sessionCache) set(token string, id gatewayIdentity) {
	now := time.Now()
	sc.mu.Lock()
	// 顺带清扫过期项（低频；避免长尾 token 常驻）。
	if len(sc.m) > 4096 {
		for k, v := range sc.m {
			if now.After(v.expiresAt) {
				delete(sc.m, k)
			}
		}
	}
	sc.m[token] = sessionEntry{ident: id, expiresAt: now.Add(sc.ttl)}
	sc.mu.Unlock()
}

// invalidate 主动逐出（logout）：撤销会话后立即失效本地缓存——当前会话的
// 注销必须即时生效，否则前端登出后仍能继续调用。其他副本（同 token 多处使用）
// 最迟随 TTL（30s）失效； gateway 侧撤销后任何缓存未命中的请求都会被拒。
func (sc *sessionCache) invalidate(token string) {
	sc.mu.Lock()
	delete(sc.m, token)
	sc.mu.Unlock()
}

// authMiddleware 会话验证：Bearer token →（缓存或 gateway /auth/me）→ 租户身份
// 注入 gin context（ctxKeyTenant/ctxKeyUser/ctxKeyToken）。
func authMiddleware(log *slog.Logger, gw *gatewayClient, sc *sessionCache) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := bearerToken(c.Request.Header.Get("Authorization"))
		if token == "" {
			abortWith(c, mallerr.E(mallerr.CodeUnauthenticated, false, "未登录或会话无效"))
			return
		}
		if id, ok := sc.get(token); ok {
			setIdentity(c, id, token)
			c.Next()
			return
		}
		id, err := gw.me(c.Request.Context(), token)
		if err != nil {
			// 网关拒绝（401）→ 会话过期/无效（重新登录）；网关不可达/超时 → 5xxx。
			abortWith(c, err)
			return
		}
		sc.set(token, id)
		setIdentity(c, id, token)
		c.Next()
	}
}

func setIdentity(c *gin.Context, id gatewayIdentity, token string) {
	c.Set(ctxKeyTenant, id.TenantID)
	c.Set(ctxKeyUser, id)
	c.Set(ctxKeyToken, token)
}

// bearerToken 解析 Authorization 头；非法/缺失返回空串（→ MALL_10001）。
func bearerToken(h string) string {
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return ""
	}
	t := strings.TrimSpace(strings.TrimPrefix(h, prefix))
	if t == "" {
		return ""
	}
	return t
}

// tenantID 取中间件注入的租户（authed 路由组内必有）。
func tenantID(c *gin.Context) uint64 {
	if v, ok := c.Get(ctxKeyTenant); ok {
		if id, ok := v.(uint64); ok {
			return id
		}
	}
	return 0
}

// gatewayClient gateway :18080 /auth/* HTTP 客户端（会话链路代理）。
type gatewayClient struct {
	base string
	hc   *http.Client
}

func newGatewayClient(base string) *gatewayClient {
	return &gatewayClient{base: strings.TrimRight(base, "/"), hc: &http.Client{Timeout: 5 * time.Second}}
}

// me 会话验证：Cookie: agw_user=<token> → {id, username, tenant_id}。
// 401 → MALL_10002；网络错误 → MALL_50003；超时 → MALL_50002。
func (g *gatewayClient) me(ctx context.Context, token string) (gatewayIdentity, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.base+"/auth/me", nil)
	if err != nil {
		return gatewayIdentity{}, mallerr.E(mallerr.CodeInternal, false, "构造会话验证请求失败")
	}
	req.AddCookie(&http.Cookie{Name: "agw_user", Value: token})
	resp, err := g.hc.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil || strings.Contains(err.Error(), "context deadline exceeded") {
			return gatewayIdentity{}, mallerr.E(mallerr.CodeUpstreamTimeout, false, "会话验证超时，请重试")
		}
		return gatewayIdentity{}, mallerr.E(mallerr.CodeUpstreamUnavailable, false, "会话服务不可达，请稍后重试")
	}
	defer resp.Body.Close() //nolint:errcheck // 读侧丢弃
	if resp.StatusCode != http.StatusOK {
		// billing 对未知/撤销/过期统一 401（"登录已过期，请重新登录"）→ 10002。
		if resp.StatusCode == http.StatusUnauthorized {
			return gatewayIdentity{}, mallerr.E(mallerr.CodeSessionExpired, false, "会话已过期，请重新登录")
		}
		return gatewayIdentity{}, mallerr.E(mallerr.CodeUpstreamUnavailable, false, "会话服务异常(%d)，请稍后重试", resp.StatusCode)
	}
	var body struct {
		ID       uint64 `json:"id"`
		Username string `json:"username"`
		TenantID uint64 `json:"tenant_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body.TenantID == 0 {
		return gatewayIdentity{}, mallerr.E(mallerr.CodeInternal, false, "会话验证响应异常")
	}
	return gatewayIdentity{UserID: body.ID, Username: body.Username, TenantID: body.TenantID}, nil
}
