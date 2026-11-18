// handler_auth.go：认证端点（api-contract §2）。
//
// register/login 代理 gateway :18080 /auth/*（D14）：上游响应体不含 token，
// token 只在 Set-Cookie: agw_user=... 头里——bff 提取后放进自己的 JSON
// 响应 session_token 字段；上游 Set-Cookie 一律不透传给客户端（前端以
// Authorization: Bearer 回传，会话 cookie 语义留在 gateway 链路内）。
// 上游非 2xx 按状态映射契约码（§2 错误列）：400→10005、401→10004、
// 403→10005、409→10003、429→10006、其余/5xx→50003。
// logout 转发当前会话 cookie 撤销上游会话，成功回 {}（幂等）。
package main

import (
	"bytes"
	"context"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/frostloom/my-market/pkg/mallerr"
)

const authBodyLimit = 4 << 10 // 与 gateway 代理同一上限（4096B）

// proxyRegister / proxyLogin：同一代理实现，仅上游动作不同。
func proxyRegister(gw *gatewayClient) gin.HandlerFunc { return proxyAuthAction(gw, "register") }
func proxyLogin(gw *gatewayClient) gin.HandlerFunc    { return proxyAuthAction(gw, "login") }

func proxyAuthAction(gw *gatewayClient, action string) gin.HandlerFunc {
	return func(c *gin.Context) {
		body, err := io.ReadAll(io.LimitReader(c.Request.Body, authBodyLimit+1))
		if err != nil || len(body) > authBodyLimit {
			abortWith(c, mallerr.E(mallerr.CodeInvalidParam, false, "请求体过大（上限 4KB）"))
			return
		}
		out, err := gw.postAuthAction(c.Request.Context(), action, body)
		if err != nil {
			abortWith(c, err)
			return
		}
		if out.sessionToken == "" {
			// 2xx 但无会话 cookie：上游行为异常（契约要求签到 token）。
			abortWith(c, mallerr.E(mallerr.CodeInternal, false, "登录响应缺会话凭证"))
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"id":            out.id,
			"username":      out.username,
			"tenant_id":     out.tenantID,
			"session_token": out.sessionToken,
		})
	}
}

// handleLogout：撤销当前会话（转发 cookie + 立即逐出本地会话缓存），幂等 200 {}。
func handleLogout(gw *gatewayClient, sc *sessionCache) gin.HandlerFunc {
	return func(c *gin.Context) {
		token, _ := c.Get(ctxKeyToken)
		tok, _ := token.(string)
		if err := gw.postLogout(c.Request.Context(), tok); err != nil {
			abortWith(c, err)
			return
		}
		sc.invalidate(tok)
		c.JSON(http.StatusOK, gin.H{})
	}
}

// authOutcome 上游 register/login 结果（token 自 Set-Cookie 提取）。
type authOutcome struct {
	id           uint64
	username     string
	tenantID     uint64
	sessionToken string
}

// postAuthAction 转发 register/login 到 gateway；映射上游错误为契约码。
func (g *gatewayClient) postAuthAction(ctx context.Context, action string, body []byte) (*authOutcome, error) {
	up, cookies, status, err := g.postJSON(ctx, "/auth/"+action, body)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, mapGatewayAuthErr(status, up.message)
	}
	out := &authOutcome{
		id:       up.id,
		username: up.username,
		tenantID: up.tenantID,
	}
	for _, ck := range cookies {
		if ck.Name == "agw_user" && ck.Value != "" {
			out.sessionToken = ck.Value
		}
	}
	return out, nil
}

// postLogout 撤销上游会话（幂等；上游恒 200）。
func (g *gatewayClient) postLogout(ctx context.Context, token string) error {
	_, _, status, err := g.postJSONWithCookie(ctx, "/auth/logout", nil, token)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return mapGatewayAuthErr(status, "")
	}
	return nil
}

// postJSON POST JSON 到 gateway /auth/*，解析上游错误/账户体。
func (g *gatewayClient) postJSON(ctx context.Context, path string, body []byte) (*gatewayBody, []*http.Cookie, int, error) {
	return g.postJSONWithCookie(ctx, path, body, "")
}

type gatewayBody struct {
	id       uint64
	username string
	tenantID uint64
	message  string
}

func (g *gatewayClient) postJSONWithCookie(ctx context.Context, path string, body []byte, token string) (*gatewayBody, []*http.Cookie, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, nil, 0, mallerr.E(mallerr.CodeInternal, false, "构造上游请求失败")
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.AddCookie(&http.Cookie{Name: "agw_user", Value: token})
	}
	resp, err := g.hc.Do(req)
	if err != nil {
		return nil, nil, 0, upstreamErr(err)
	}
	defer resp.Body.Close() //nolint:errcheck // 读侧丢弃
	var payload struct {
		ID       uint64 `json:"id"`
		Username string `json:"username"`
		TenantID uint64 `json:"tenant_id"`
		Error    string `json:"error"`
	}
	_ = jsonDecode(resp.Body, &payload)
	gb := &gatewayBody{id: payload.ID, username: payload.Username, tenantID: payload.TenantID, message: payload.Error}
	return gb, resp.Cookies(), resp.StatusCode, nil
}

// mapGatewayAuthErr gateway /auth/* 非 2xx → 契约码（api-contract §2 错误列）。
func mapGatewayAuthErr(status int, msg string) error {
	switch status {
	case http.StatusBadRequest:
		return mallerr.E(mallerr.CodeInvalidParam, false, "%s", friendly(msg, "参数校验失败"))
	case http.StatusUnauthorized:
		return mallerr.E(mallerr.CodeBadCredentials, false, "%s", friendly(msg, "用户名或密码错误"))
	case http.StatusForbidden:
		return mallerr.E(mallerr.CodeInvalidParam, false, "%s", friendly(msg, "参数校验失败"))
	case http.StatusConflict:
		return mallerr.E(mallerr.CodeUsernameTaken, false, "%s", friendly(msg, "用户名已被占用"))
	case http.StatusTooManyRequests:
		return mallerr.E(mallerr.CodeRateLimited, false, "%s", friendly(msg, "请求过于频繁，请稍后重试"))
	default:
		return mallerr.E(mallerr.CodeUpstreamUnavailable, false, "账户服务暂不可用，请稍后重试")
	}
}

// friendly 上游人话描述优先，缺省回落默认文案。
func friendly(msg, def string) string {
	if msg != "" {
		return msg
	}
	return def
}

// upstreamErr 网络层失败 → 5xxx（超时/不可达区分）。
func upstreamErr(err error) error {
	if err == context.DeadlineExceeded {
		return mallerr.E(mallerr.CodeUpstreamTimeout, false, "上游超时，请重试")
	}
	return mallerr.E(mallerr.CodeUpstreamUnavailable, false, "账户服务不可达，请稍后重试")
}
