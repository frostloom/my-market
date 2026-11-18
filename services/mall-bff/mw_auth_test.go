// mall-bff 鉴权中间件与会话适配测试（D14 裁定：Bearer ↔ gateway agw_user cookie、
// /auth/register /auth/login 从 Set-Cookie 提取 token 进 session_token 字段、
// 30s 进程内会话缓存按 token 键）。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/frostloom/my-market/pkg/mallerr"
)

// TestAuth_InvalidToken_401_MALL10001：未携带 / 格式非法凭证 → 401 MALL_10001。
// （gateway /auth/me 对"未知/撤销/过期"统一 401"登录已过期"无法区分，
//
//	bff 口径：无有效凭证可验 → 10001；凭证提交但网关拒绝 → 10002，见下一测试。）
func TestAuth_InvalidToken_401_MALL10001(t *testing.T) {
	fx := newTestApp(t, nil)
	cases := []struct {
		name string
		hdr  map[string]string
	}{
		{"缺 Authorization 头", nil},
		{"Bearer 空token", map[string]string{"Authorization": "Bearer "}},
		{"非 Bearer 方案", map[string]string{"Authorization": "Basic Zm9v"}},
		{"纯 token 无方案", map[string]string{"Authorization": "rawtoken"}},
	}
	for _, tc := range cases {
		req := httptest.NewRequest("GET", "/api/v1/cart", nil)
		for k, v := range tc.hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		fx.app.Handler().ServeHTTP(rec, req)
		if rec.Code != 401 {
			t.Fatalf("%s: HTTP = %d, want 401 (body=%s)", tc.name, rec.Code, rec.Body.String())
		}
		eb := decodeErr(t, rec)
		if eb.Code != "MALL_10001" || eb.Retryable {
			t.Fatalf("%s: got {code=%s retryable=%v}, want MALL_10001 false", tc.name, eb.Code, eb.Retryable)
		}
	}
}

// TestAuth_Expired_MALL10002：凭证提交但 gateway /auth/me 拒绝（未知/撤销/过期
// 在 billing 侧坍缩为同一 401）→ 401 MALL_10002（重新登录）。
func TestAuth_Expired_MALL10002(t *testing.T) {
	fx := newTestApp(t, nil)
	rec := doReq(t, fx.app.Handler(), "GET", "/api/v1/cart", "tok-revoked-or-expired", "", nil)
	if rec.Code != 401 {
		t.Fatalf("HTTP = %d, want 401 (body=%s)", rec.Code, rec.Body.String())
	}
	eb := decodeErr(t, rec)
	if eb.Code != "MALL_10002" || eb.Retryable {
		t.Fatalf("got {code=%s retryable=%v}, want MALL_10002 false", eb.Code, eb.Retryable)
	}
	if fx.gw.meHits != 1 {
		t.Fatalf("gateway /auth/me hits = %d, want 1", fx.gw.meHits)
	}
}

// TestAuthCache_SecondRequestSkipsGateway：验证 30s 进程内缓存按 token 键——
// 同 token 第二次请求不再打 gateway（D14：每请求或短缓存，这里落缓存）。
func TestAuthCache_SecondRequestSkipsGateway(t *testing.T) {
	fx := newTestApp(t, func(c *Config) { c.AuthCacheTTL = 30 * time.Second })
	tok := fx.gw.issue(t, 7)
	for i := 0; i < 3; i++ {
		if rec := doReq(t, fx.app.Handler(), "GET", "/api/v1/cart", tok, "", nil); rec.Code != 200 {
			t.Fatalf("第 %d 次: HTTP = %d, want 200 (body=%s)", i+1, rec.Code, rec.Body.String())
		}
	}
	if fx.gw.meHits != 1 {
		t.Fatalf("gateway /auth/me hits = %d, want 1（同 token 应走 30s 缓存）", fx.gw.meHits)
	}
	// 不同 token 独立缓存键：仍会打 gateway
	tok2 := fx.gw.issue(t, 8)
	if rec := doReq(t, fx.app.Handler(), "GET", "/api/v1/cart", tok2, "", nil); rec.Code != 200 {
		t.Fatalf("另一 token: HTTP = %d, want 200", rec.Code)
	}
	if fx.gw.meHits != 2 {
		t.Fatalf("gateway /auth/me hits = %d, want 2（不同 token 独立键）", fx.gw.meHits)
	}
}

// TestAuthCache_TTLExpiryRefetches：TTL 过期后重新打 gateway（缓存有界）。
func TestAuthCache_TTLExpiryRefetches(t *testing.T) {
	fx := newTestApp(t, func(c *Config) { c.AuthCacheTTL = 10 * time.Millisecond })
	tok := fx.gw.issue(t, 7)
	_ = doReq(t, fx.app.Handler(), "GET", "/api/v1/cart", tok, "", nil)
	time.Sleep(30 * time.Millisecond)
	_ = doReq(t, fx.app.Handler(), "GET", "/api/v1/cart", tok, "", nil)
	if fx.gw.meHits != 2 {
		t.Fatalf("gateway /auth/me hits = %d, want 2（TTL 过期应重取）", fx.gw.meHits)
	}
}

// TestAuthProxy_RegisterLoginExtractsToken（D14 核心）：register/login 代理——
// 响应体含 session_token（自上游 Set-Cookie 提取），且上游 Set-Cookie 不透传给客户端。
func TestAuthProxy_RegisterLoginExtractsToken(t *testing.T) {
	fx := newTestApp(t, nil)
	for _, ep := range []string{"/api/v1/auth/register", "/api/v1/auth/login"} {
		rec := doReq(t, fx.app.Handler(), "POST", ep, "",
			`{"username":"alice","password":"password123"}`, nil)
		if rec.Code != 200 {
			t.Fatalf("%s: HTTP = %d, want 200 (body=%s)", ep, rec.Code, rec.Body.String())
		}
		if ck := rec.Header().Get("Set-Cookie"); ck != "" {
			t.Fatalf("%s: 上游 Set-Cookie 不应透传: %s", ep, ck)
		}
		var resp struct {
			ID           uint64 `json:"id"`
			Username     string `json:"username"`
			TenantID     uint64 `json:"tenant_id"`
			SessionToken string `json:"session_token"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("%s: 非法 JSON: %v", ep, err)
		}
		if resp.SessionToken == "" || resp.TenantID == 0 || resp.Username != "alice" {
			t.Fatalf("%s: 响应缺字段: %+v (body=%s)", ep, resp, rec.Body.String())
		}
		if !strings.HasPrefix(resp.SessionToken, "tok-") {
			t.Fatalf("%s: session_token 应来自上游 Set-Cookie: %q", ep, resp.SessionToken)
		}
	}
}

// TestAuthProxy_GatewayErrorsShaped：gateway 侧错误映射契约码（400→10005、
// 409→10003、401→10004、429→10006），保持统一错误形状。
func TestAuthProxy_GatewayErrorsShaped(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		gwBody   any
		wantCode string
		wantHTTP int
	}{
		{"格式错→10005", 400, map[string]string{"error": "用户名须为 3-32 位"}, "MALL_10005", 400},
		{"占用→10003", 409, map[string]string{"error": "用户名已被使用"}, "MALL_10003", 409},
		{"密码错→10004", 401, map[string]string{"error": "用户名或密码错误"}, "MALL_10004", 401},
		{"槽满→10006", 429, map[string]string{"error": "请求过于频繁"}, "MALL_10006", 429},
		{"上游500→50003", 500, map[string]string{"error": "boom"}, "MALL_50003", 503},
	}
	for _, tc := range cases {
		fx := newTestApp(t, nil)
		fx.gw.register = func(string, string) (int, any) { return tc.status, tc.gwBody }
		rec := doReq(t, fx.app.Handler(), "POST", "/api/v1/auth/login", "",
			`{"username":"alice","password":"password123"}`, nil)
		if rec.Code != tc.wantHTTP {
			t.Fatalf("%s: HTTP = %d, want %d (body=%s)", tc.name, rec.Code, tc.wantHTTP, rec.Body.String())
		}
		eb := decodeErr(t, rec)
		if eb.Code != tc.wantCode {
			t.Fatalf("%s: code = %s, want %s", tc.name, eb.Code, tc.wantCode)
		}
	}
}

// TestLogout_ForwardsCookieAndShapes：logout 撤销当前会话，响应 {}。
func TestLogout_ForwardsCookieAndShapes(t *testing.T) {
	fx := newTestApp(t, nil)
	tok := fx.gw.issue(t, 3)
	rec := doReq(t, fx.app.Handler(), "POST", "/api/v1/auth/logout", tok, "", nil)
	if rec.Code != 200 {
		t.Fatalf("HTTP = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if body := strings.TrimSpace(rec.Body.String()); body != "{}" {
		t.Fatalf("logout 响应 = %s, want {}", body)
	}
}

// TestUpstreamErr_WrappedDeadlineIsTimeout（Fix Round 1 Minor-1）：http.Client
// 超时经 url.Error 包装——errors.Is 须穿透识别为超时（MALL_50002），而非 50003。
func TestUpstreamErr_WrappedDeadlineIsTimeout(t *testing.T) {
	wrapped := fmt.Errorf(`Post "http://127.0.0.1:1/auth/register": %w`, context.DeadlineExceeded)
	var me *mallerr.Error
	if !errors.As(upstreamErr(wrapped), &me) || me.Code != mallerr.CodeUpstreamTimeout {
		t.Fatalf("包装的超时应映射 MALL_50002, got %+v", me)
	}
}

// TestLogout_EvictsSessionCacheImmediately：logout 后同一 token 立即失效
// （缓存逐出，下一次请求打 gateway 被拒 → MALL_10002）——登出必须即时生效。
func TestLogout_EvictsSessionCacheImmediately(t *testing.T) {
	fx := newTestApp(t, nil)
	tok := fx.gw.issue(t, 3)
	// 先触发一次验证（进缓存）
	if rec := doReq(t, fx.app.Handler(), "GET", "/api/v1/cart", tok, "", nil); rec.Code != 200 {
		t.Fatalf("预热: HTTP = %d, want 200", rec.Code)
	}
	// 登出
	if rec := doReq(t, fx.app.Handler(), "POST", "/api/v1/auth/logout", tok, "", nil); rec.Code != 200 {
		t.Fatalf("logout: HTTP = %d, want 200", rec.Code)
	}
	// 同 token 再用：缓存已逐出 → gateway 拒绝 → MALL_10002（不再 200）
	rec := doReq(t, fx.app.Handler(), "GET", "/api/v1/cart", tok, "", nil)
	if rec.Code != 401 {
		t.Fatalf("logout 后复用: HTTP = %d, want 401 (body=%s)", rec.Code, rec.Body.String())
	}
	if eb := decodeErr(t, rec); eb.Code != "MALL_10002" {
		t.Fatalf("logout 后复用 code = %s, want MALL_10002", eb.Code)
	}
}
