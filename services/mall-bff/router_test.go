// mall-bff 路由与端点行为测试（TDD，brief 6 测试 + 补充 2 + D14 会话适配）。
// gRPC 下游用接口桩（gclient.go 的 GoodsClient/OrderClient 接口，
// 桩体为函数字段——选择"generated client 接口化"而非 generated server 桩，
// httptest 直接驱动 gin 引擎，无需 bufconn）。
//
// 会话链路（D14）：fake gateway 模拟 :18080 /auth/*——token 只在
// Set-Cookie: agw_user 头里签发，/auth/me 按 Cookie 校验。
package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/frostloom/my-market/pkg/mallerr"
	"github.com/frostloom/my-market/services/mall-gen/mallv1"
)

// ---------------------------------------------------------------------------
// fake gateway（D14 会话链路：/auth/register、/auth/login 响应体不含 token，
// token 只在 Set-Cookie: agw_user=...；/auth/me 验 Cookie: agw_user=<token>）

type fakeGateway struct {
	srv      *httptest.Server
	mu       sync.Mutex
	tokens   map[string]*gatewayUser // token → user
	seq      int
	meHits   int                                        // /auth/me 调用计数（缓存断言用）
	register func(username, password string) (int, any) // 注入错误分支用，nil 走默认
}

type gatewayUser struct {
	ID       uint64
	Username string
	TenantID uint64
}

func newFakeGateway(t *testing.T) *fakeGateway {
	t.Helper()
	fg := &fakeGateway{tokens: map[string]*gatewayUser{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/me", func(w http.ResponseWriter, r *http.Request) {
		fg.mu.Lock()
		fg.meHits++
		var u *gatewayUser
		ck, err := r.Cookie("agw_user")
		if err == nil {
			u, _ = fg.tokens[ck.Value]
		}
		fg.mu.Unlock()
		if u == nil {
			writeFakeJSON(w, 401, map[string]string{"error": "登录已过期，请重新登录"})
			return
		}
		writeFakeJSON(w, 200, map[string]any{"id": u.ID, "username": u.Username, "role": "user", "tenant_id": u.TenantID, "api_key_id": 1})
	})
	mux.HandleFunc("/auth/logout", func(w http.ResponseWriter, r *http.Request) {
		// 与真实 gateway 一致：登出即撤销会话（PortalSession 失效）。
		if ck, err := r.Cookie("agw_user"); err == nil {
			fg.mu.Lock()
			delete(fg.tokens, ck.Value)
			fg.mu.Unlock()
		}
		writeFakeJSON(w, 200, map[string]any{"ok": true})
	})
	for _, action := range []string{"register", "login"} {
		act := action
		mux.HandleFunc("/auth/"+act, func(w http.ResponseWriter, r *http.Request) {
			var in struct {
				Username string `json:"username"`
				Password string `json:"password"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			if fg.register != nil {
				code, body := fg.register(in.Username, in.Password)
				writeFakeJSON(w, code, body)
				return
			}
			fg.mu.Lock()
			fg.seq++
			tok := fmt.Sprintf("tok-%s-%d", act, fg.seq)
			u := &gatewayUser{ID: uint64(fg.seq), Username: in.Username, TenantID: uint64(1000 + fg.seq)}
			fg.tokens[tok] = u
			fg.mu.Unlock()
			// 关键：token 只在 Set-Cookie 里，响应体不含 token（D14）。
			http.SetCookie(w, &http.Cookie{Name: "agw_user", Value: tok, Path: "/", HttpOnly: true})
			writeFakeJSON(w, 200, map[string]any{"id": u.ID, "username": u.Username, "role": "user", "tenant_id": u.TenantID})
		})
	}
	fg.srv = httptest.NewServer(mux)
	t.Cleanup(fg.srv.Close)
	return fg
}

func (fg *fakeGateway) issue(t *testing.T, tenantID uint64) string {
	t.Helper()
	fg.mu.Lock()
	fg.seq++
	tok := fmt.Sprintf("tok-fixed-%d", fg.seq)
	fg.tokens[tok] = &gatewayUser{ID: uint64(fg.seq), Username: "u", TenantID: tenantID}
	fg.mu.Unlock()
	return tok
}

func writeFakeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

// ---------------------------------------------------------------------------
// gRPC 下游接口桩（签名与 generated client 一致——真实 client 可直接赋给接口）

type stubGoods struct {
	listCategories func(context.Context, *emptypb.Empty, ...grpc.CallOption) (*mallv1.ListCategoriesReply, error)
	listGoods      func(context.Context, *mallv1.ListGoodsReq, ...grpc.CallOption) (*mallv1.ListGoodsReply, error)
	getGoods       func(context.Context, *mallv1.GetGoodsReq, ...grpc.CallOption) (*mallv1.GoodsItem, error)
}

func (s *stubGoods) ListCategories(ctx context.Context, e *emptypb.Empty, co ...grpc.CallOption) (*mallv1.ListCategoriesReply, error) {
	if s.listCategories == nil {
		panic("unexpected ListCategories")
	}
	return s.listCategories(ctx, e, co...)
}

func (s *stubGoods) ListGoods(ctx context.Context, req *mallv1.ListGoodsReq, co ...grpc.CallOption) (*mallv1.ListGoodsReply, error) {
	if s.listGoods == nil {
		panic("unexpected ListGoods")
	}
	return s.listGoods(ctx, req, co...)
}

func (s *stubGoods) GetGoods(ctx context.Context, req *mallv1.GetGoodsReq, co ...grpc.CallOption) (*mallv1.GoodsItem, error) {
	if s.getGoods == nil {
		panic("unexpected GetGoods")
	}
	return s.getGoods(ctx, req, co...)
}

type stubOrder struct {
	mu          sync.Mutex
	markPaidRec []*mallv1.MarkPaidReq // MarkPaid 收到的请求（回调链路断言用）
	getOrder    func(context.Context, *mallv1.GetOrderReq, ...grpc.CallOption) (*mallv1.OrderReply, error)
	markPaid    func(*mallv1.MarkPaidReq) (*mallv1.OrderReply, error)
}

func (s *stubOrder) CreateCartOrder(ctx context.Context, req *mallv1.CreateCartOrderReq, co ...grpc.CallOption) (*mallv1.OrderReply, error) {
	panic("unexpected CreateCartOrder")
}
func (s *stubOrder) GetOrder(ctx context.Context, req *mallv1.GetOrderReq, co ...grpc.CallOption) (*mallv1.OrderReply, error) {
	if s.getOrder == nil {
		panic("unexpected GetOrder")
	}
	return s.getOrder(ctx, req, co...)
}
func (s *stubOrder) ListOrders(ctx context.Context, req *mallv1.ListOrdersReq, co ...grpc.CallOption) (*mallv1.ListOrdersReply, error) {
	panic("unexpected ListOrders")
}
func (s *stubOrder) MarkPaid(ctx context.Context, req *mallv1.MarkPaidReq, co ...grpc.CallOption) (*mallv1.OrderReply, error) {
	s.mu.Lock()
	s.markPaidRec = append(s.markPaidRec, req)
	s.mu.Unlock()
	if s.markPaid == nil {
		panic("unexpected MarkPaid")
	}
	return s.markPaid(req)
}
func (s *stubOrder) CancelOrder(ctx context.Context, req *mallv1.CancelOrderReq, co ...grpc.CallOption) (*mallv1.OrderReply, error) {
	panic("unexpected CancelOrder")
}
func (s *stubOrder) RefundOrder(ctx context.Context, req *mallv1.RefundOrderReq, co ...grpc.CallOption) (*mallv1.RefundReply, error) {
	panic("unexpected RefundOrder")
}
func (s *stubOrder) GetCart(ctx context.Context, req *mallv1.GetCartReq, co ...grpc.CallOption) (*mallv1.CartReply, error) {
	return &mallv1.CartReply{}, nil
}
func (s *stubOrder) AddCart(ctx context.Context, req *mallv1.AddCartReq, co ...grpc.CallOption) (*mallv1.CartReply, error) {
	panic("unexpected AddCart")
}
func (s *stubOrder) PatchCartQty(ctx context.Context, req *mallv1.PatchCartQtyReq, co ...grpc.CallOption) (*mallv1.CartReply, error) {
	panic("unexpected PatchCartQty")
}
func (s *stubOrder) RemoveCart(ctx context.Context, req *mallv1.RemoveCartReq, co ...grpc.CallOption) (*mallv1.CartReply, error) {
	panic("unexpected RemoveCart")
}

func (s *stubOrder) markPaidCalls() []*mallv1.MarkPaidReq {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*mallv1.MarkPaidReq(nil), s.markPaidRec...)
}

// ---------------------------------------------------------------------------
// App 测试装配（miniredis 撑限流；cfg 项测试可覆写）

type appFixture struct {
	app  *App
	gw   *fakeGateway
	good *stubGoods
	ord  *stubOrder
	mr   *miniredis.Miniredis
}

func newTestApp(t *testing.T, mutate func(*Config)) *appFixture {
	t.Helper()
	fx := &appFixture{
		gw:   newFakeGateway(t),
		good: &stubGoods{},
		ord:  &stubOrder{},
	}
	fx.mr = miniredis.RunT(t)
	cfg := Config{
		Addr:              "127.0.0.1:18090",
		GatewayBase:       fx.gw.srv.URL,
		CallbackSelfURL:   "http://127.0.0.1:18090", // 测试里起真服时覆写
		PayCallbackSecret: []byte("test-pay-secret"),
		RateLimitPerMin:   120,
		AuthCacheTTL:      30 * time.Second,
		CallbackDelay:     200 * time.Millisecond,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	fx.app = NewApp(cfg, fx.good, fx.ord, redis.NewClient(&redis.Options{Addr: fx.mr.Addr()}))
	t.Cleanup(func() { _ = fx.app.Close() })
	return fx
}

func doReq(t *testing.T, h http.Handler, method, path, token string, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var rd *strings.Reader
	if body == "" {
		rd = strings.NewReader("")
	} else {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

type errBody struct {
	Error     string `json:"error"`
	Code      string `json:"code"`
	Retryable bool   `json:"retryable"`
}

func decodeErr(t *testing.T, rec *httptest.ResponseRecorder) errBody {
	t.Helper()
	var eb errBody
	if err := json.Unmarshal(rec.Body.Bytes(), &eb); err != nil {
		t.Fatalf("响应体不是合法 JSON 错误形状: %v\nbody=%s", err, rec.Body.String())
	}
	if eb.Error == "" || eb.Code == "" {
		t.Fatalf("错误体缺字段: %+v\nbody=%s", eb, rec.Body.String())
	}
	return eb
}

// grpcNotFoundMALL30001 模拟 mall-order 的跨租户拒绝路径：
// mallerr 载体经 ToGRPC 发出（ErrorInfo.Reason=MALL_30001），无 HTTP 痕迹。
func grpcNotFoundMALL30001(orderNo string) error {
	return mallerr.ToGRPC(mallerr.E(mallerr.CodeOrderNotFound, false, "订单不存在: %s", orderNo))
}

func sampleOrder(status string) *mallv1.OrderReply {
	return &mallv1.OrderReply{
		OrderNo:    "M20261118120000aaaaaa",
		Status:     status,
		AmountCent: 6900,
		Items: []*mallv1.OrderItem{{
			GoodsId: 113, GoodsName: "通用商品", GoodsType: 1, PriceCent: 6900, Qty: 1,
		}},
		CreatedAt: "2026-11-18T12:00:00+08:00",
	}
}

// ---------------------------------------------------------------------------
// 1. TestErrorShape_AllNon2xx：所有非 2xx 一律 {"error","code","retryable"}

func TestErrorShape_AllNon2xx(t *testing.T) {
	fx := newTestApp(t, nil)
	tok := fx.gw.issue(t, 1)
	fx.good.getGoods = func(_ context.Context, req *mallv1.GetGoodsReq, _ ...grpc.CallOption) (*mallv1.GoodsItem, error) {
		if req.GetId() == 404 {
			return nil, mallerr.ToGRPC(mallerr.E(mallerr.CodeGoodsNotFound, false, "商品不存在: %d", req.GetId()))
		}
		if req.GetId() == 500 {
			// 无载体 gRPC 错误：粗映射 404 + MALL_50001 兜底
			return nil, status.Error(codes.NotFound, "db row missing")
		}
		if req.GetId() == 999 {
			panic("boom") // panic recovery 走统一形状
		}
		return &mallv1.GoodsItem{Id: req.GetId(), Name: "ok"}, nil
	}

	cases := []struct {
		name       string
		methodPath string
		token      string
		wantHTTP   int
		wantCode   mallerr.Code
		wantRetry  bool
	}{
		{"401 未登录", "GET /api/v1/cart", "", 401, mallerr.CodeUnauthenticated, false},
		{"404 载体还原", "GET /api/v1/goods/404", tok, 404, mallerr.CodeGoodsNotFound, false},
		// 无载体兜底：契约码落 MALL_50001 族，retryable 随码表（50001=true）。
		{"404 无载体兜底", "GET /api/v1/goods/500", tok, 404, mallerr.CodeInternal, true},
		{"500 panic 统一形状", "GET /api/v1/goods/999", tok, 500, mallerr.CodeInternal, true},
	}
	for _, tc := range cases {
		parts := strings.SplitN(tc.methodPath, " ", 2)
		rec := doReq(t, fx.app.Handler(), parts[0], parts[1], tc.token, "", nil)
		if rec.Code != tc.wantHTTP {
			t.Fatalf("%s: HTTP = %d, want %d (body=%s)", tc.name, rec.Code, tc.wantHTTP, rec.Body.String())
		}
		eb := decodeErr(t, rec)
		if eb.Code != string(tc.wantCode) {
			t.Fatalf("%s: code = %s, want %s", tc.name, eb.Code, tc.wantCode)
		}
		if eb.Retryable != tc.wantRetry {
			t.Fatalf("%s: retryable = %v, want %v", tc.name, eb.Retryable, tc.wantRetry)
		}
	}
}

// ---------------------------------------------------------------------------
// 2. TestRateLimit_120PerMinute_Then429_MALL10006（per-tenant 分钟桶 +
//    注册/登录 IP 维度第二层）

func TestRateLimit_120PerMinute_Then429_MALL10006(t *testing.T) {
	fx := newTestApp(t, nil)
	tok := fx.gw.issue(t, 1)
	// 第 1..120 次 200；第 121 次 429 MALL_10006 retryable=true
	for i := 1; i <= 120; i++ {
		rec := doReq(t, fx.app.Handler(), "GET", "/api/v1/cart", tok, "", nil)
		if rec.Code != 200 {
			t.Fatalf("第 %d 次: HTTP = %d, want 200 (body=%s)", i, rec.Code, rec.Body.String())
		}
	}
	rec := doReq(t, fx.app.Handler(), "GET", "/api/v1/cart", tok, "", nil)
	if rec.Code != 429 {
		t.Fatalf("第 121 次: HTTP = %d, want 429 (body=%s)", rec.Code, rec.Body.String())
	}
	eb := decodeErr(t, rec)
	if eb.Code != "MALL_10006" || !eb.Retryable {
		t.Fatalf("第 121 次: got {code=%s retryable=%v}, want MALL_10006 retryable=true", eb.Code, eb.Retryable)
	}
	// 租户隔离：另一租户不受该租户计数影响
	tok2 := fx.gw.issue(t, 2)
	if rec := doReq(t, fx.app.Handler(), "GET", "/api/v1/cart", tok2, "", nil); rec.Code != 200 {
		t.Fatalf("租户 2 首请求: HTTP = %d, want 200", rec.Code)
	}

	// 注册/登录 IP 维度第二层：同 IP 121 次后 429；换 IP 不受影响
	fx2 := newTestApp(t, nil)
	for i := 1; i <= 120; i++ {
		req := httptest.NewRequest("POST", "/api/v1/auth/register", strings.NewReader(`{"username":"u1","password":"password123"}`))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "9.9.9.9:5555"
		rec := httptest.NewRecorder()
		fx2.app.Handler().ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("IP 限流 第 %d 次: HTTP = %d, want 200 (body=%s)", i, rec.Code, rec.Body.String())
		}
	}
	req := httptest.NewRequest("POST", "/api/v1/auth/register", strings.NewReader(`{"username":"u1","password":"password123"}`))
	req.RemoteAddr = "9.9.9.9:5555"
	rec = httptest.NewRecorder()
	fx2.app.Handler().ServeHTTP(rec, req)
	if rec.Code != 429 {
		t.Fatalf("IP 限流 第 121 次: HTTP = %d, want 429", rec.Code)
	}
	if eb := decodeErr(t, rec); eb.Code != "MALL_10006" {
		t.Fatalf("IP 限流 code = %s, want MALL_10006", eb.Code)
	}
	req.RemoteAddr = "8.8.8.8:5555"
	rec = httptest.NewRecorder()
	fx2.app.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("换 IP: HTTP = %d, want 200", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// 3. TestOrderAccess_TenantIsolation（Review Focus #5：他租户订单 → 404 MALL_30001
//    经 FromGRPC 载体还原；同租户 → 200）

func TestOrderAccess_TenantIsolation(t *testing.T) {
	fx := newTestApp(t, nil)
	tokA := fx.gw.issue(t, 1) // 租户 1
	tokB := fx.gw.issue(t, 2) // 租户 2
	fx.ord.getOrder = func(_ context.Context, req *mallv1.GetOrderReq, _ ...grpc.CallOption) (*mallv1.OrderReply, error) {
		if req.GetTenantId() != 2 { // 订单归属租户 2：模拟 mall-order 服务端重校验
			return nil, grpcNotFoundMALL30001(req.GetOrderNo())
		}
		return sampleOrder("paid"), nil
	}

	rec := doReq(t, fx.app.Handler(), "GET", "/api/v1/orders/M1", tokA, "", nil)
	if rec.Code != 404 {
		t.Fatalf("跨租户: HTTP = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}
	eb := decodeErr(t, rec)
	if eb.Code != "MALL_30001" || eb.Retryable {
		t.Fatalf("跨租户: got {code=%s retryable=%v}, want MALL_30001 false（载体还原）", eb.Code, eb.Retryable)
	}
	if rec := doReq(t, fx.app.Handler(), "GET", "/api/v1/orders/M1", tokB, "", nil); rec.Code != 200 {
		t.Fatalf("本租户: HTTP = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// 4. TestGoodsCursor_Garbage_400_MALL10005（Review Focus #2）

func TestGoodsCursor_Garbage_400_MALL10005(t *testing.T) {
	fx := newTestApp(t, nil)
	tok := fx.gw.issue(t, 1)
	called := false
	fx.good.listGoods = func(_ context.Context, _ *mallv1.ListGoodsReq, _ ...grpc.CallOption) (*mallv1.ListGoodsReply, error) {
		called = true
		return &mallv1.ListGoodsReply{}, nil
	}
	// 垃圾游标（查询串中 URL 编码传输：%%% / garbage!! / 非 base64 字符集 /
	// 可解码但字段格式错）。注：httptest 直发原始 URL 时 % 会被查询解析吞掉，
	// 线上客户端同样会编码后再发——这里按编码形态测试。
	for _, cur := range []string{"%25%25%25", "garbage%21%21", "aGVsbG8", "aGVsbG8%3D"} /*末者解码为 hello,1 字段格式错*/ {
		rec := doReq(t, fx.app.Handler(), "GET", "/api/v1/goods?cursor="+cur, tok, "", nil)
		if rec.Code != 400 {
			t.Fatalf("cursor=%q: HTTP = %d, want 400 (body=%s)", cur, rec.Code, rec.Body.String())
		}
		if eb := decodeErr(t, rec); eb.Code != "MALL_10005" {
			t.Fatalf("cursor=%q: code = %s, want MALL_10005", cur, eb.Code)
		}
	}
	if called {
		t.Fatalf("垃圾游标不应打到下游 ListGoods")
	}
	// 合法游标透传下游
	rec := doReq(t, fx.app.Handler(), "GET", "/api/v1/goods", tok, "", nil)
	if rec.Code != 200 {
		t.Fatalf("无游标: HTTP = %d, want 200", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// 5. TestPayFlow_AsyncCallbackToPaid：pay → 200 created → MockProvider 异步回调
//    （验签）→ MarkPaid → 订单 paid（计划修订：202 已改 200）

func TestPayFlow_AsyncCallbackToPaid(t *testing.T) {
	var mu sync.Mutex
	orderStatus := "created"
	fx := newTestApp(t, func(c *Config) {
		c.CallbackDelay = 20 * time.Millisecond // 加速异步回调
	})
	ts := httptest.NewServer(fx.app.Handler())
	t.Cleanup(ts.Close)
	fx.app.cfg.CallbackSelfURL = ts.URL // 回调打回测试服自身

	tok := fx.gw.issue(t, 1)
	fx.ord.getOrder = func(_ context.Context, req *mallv1.GetOrderReq, _ ...grpc.CallOption) (*mallv1.OrderReply, error) {
		mu.Lock()
		defer mu.Unlock()
		o := sampleOrder(orderStatus)
		o.OrderNo = req.GetOrderNo()
		return o, nil
	}
	var paidReqSnapshot *mallv1.MarkPaidReq
	fx.ord.markPaid = func(req *mallv1.MarkPaidReq) (*mallv1.OrderReply, error) {
		mu.Lock()
		orderStatus = "paid" // 模拟 MarkPaidTx 落账
		paidReqSnapshot = req
		mu.Unlock()
		o := sampleOrder("paid")
		o.OrderNo = req.GetOrderNo()
		return o, nil
	}

	// 1) pay：立即返回 200 created（异步回调尚未落地）
	rec := doReq(t, fx.app.Handler(), "POST", "/api/v1/orders/M1/pay", tok, "", nil)
	if rec.Code != 200 {
		t.Fatalf("pay: HTTP = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var payResp struct {
		Status  string `json:"status"`
		Payment struct {
			TradeNo  string `json:"trade_no"`
			Provider string `json:"provider"`
		} `json:"payment"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payResp); err != nil {
		t.Fatalf("pay 响应非法 JSON: %v body=%s", err, rec.Body.String())
	}
	if payResp.Status != "created" || payResp.Payment.TradeNo == "" || payResp.Payment.Provider != "mock" {
		t.Fatalf("pay 响应不符: %+v", payResp)
	}

	// 2) 等异步回调落地（≤2s），断言 MarkPaid 收到验签后载荷。
	//    断言在锁内读取后立即释放——后续请求的桩体也要用这把锁，不能持锁请求。
	deadline := time.Now().Add(2 * time.Second)
	var paidReq *mallv1.MarkPaidReq
	for {
		mu.Lock()
		paidReq = paidReqSnapshot
		mu.Unlock()
		if paidReq != nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if paidReq == nil {
		t.Fatalf("2s 内未收到异步回调触发的 MarkPaid")
	}
	if !paidReq.GetSignatureVerified() {
		t.Fatalf("MarkPaidReq.signature_verified = false, want true（验签入口如实上报）")
	}
	if paidReq.GetTenantId() != 1 || paidReq.GetOrderNo() != "M1" || !paidReq.GetPaid() ||
		paidReq.GetAmountCent() != 6900 || paidReq.GetProvider() != "mock" || paidReq.GetProviderTradeNo() == "" {
		t.Fatalf("MarkPaidReq 字段不符: %+v", paidReq)
	}

	// 3) 回调后订单已 paid：GET /orders/{no} 可见
	rec = doReq(t, fx.app.Handler(), "GET", "/api/v1/orders/M1", tok, "", nil)
	if rec.Code != 200 {
		t.Fatalf("GET order: HTTP = %d", rec.Code)
	}
	var ord struct {
		OrderNo string `json:"order_no"`
		Status  string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ord); err != nil || ord.Status != "paid" {
		t.Fatalf("订单状态 = %s (err=%v), want paid", ord.Status, err)
	}

	// 4) 幂等：已支付再 pay → 200 paid（不报错）
	rec = doReq(t, fx.app.Handler(), "POST", "/api/v1/orders/M1/pay", tok, "", nil)
	if rec.Code != 200 {
		t.Fatalf("重复 pay: HTTP = %d, want 200", rec.Code)
	}
	var again struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &again)
	if again.Status != "paid" {
		t.Fatalf("重复 pay status = %s, want paid", again.Status)
	}
}

// ---------------------------------------------------------------------------
// 6. TestCallback_BadSignature_400_MALL30005（补充：验签失败拒绝、零下游调用）

func TestCallback_BadSignature_400_MALL30005(t *testing.T) {
	fx := newTestApp(t, nil)
	called := false
	fx.ord.markPaid = func(*mallv1.MarkPaidReq) (*mallv1.OrderReply, error) {
		called = true
		return sampleOrder("paid"), nil
	}
	payload := `{"order_no":"M1","provider":"mock","provider_trade_no":"T1","paid":true,"amount_cent":6900,"tenant_id":1}`
	cases := []struct {
		name string
		sig  string
	}{
		{"签名错误", hexEncode([]byte("deadbeef"))},
		{"缺失签名头", ""},
		{"非 hex 签名", "zz-not-hex"},
	}
	for _, tc := range cases {
		hdr := map[string]string{}
		if tc.sig != "" {
			hdr["X-Signature"] = tc.sig
		}
		rec := doReq(t, fx.app.Handler(), "POST", "/api/v1/payments/callback", "", payload, hdr)
		if rec.Code != 400 {
			t.Fatalf("%s: HTTP = %d, want 400 (body=%s)", tc.name, rec.Code, rec.Body.String())
		}
		if eb := decodeErr(t, rec); eb.Code != "MALL_30005" {
			t.Fatalf("%s: code = %s, want MALL_30005", tc.name, eb.Code)
		}
	}
	if called {
		t.Fatalf("验签失败不得触达下游 MarkPaid")
	}
}

// ---------------------------------------------------------------------------
// 7. TestCallback_FailedPayment_StaysCreated（补充：paid=false 失败回调，
//    闭环 T5 concern ③——订单停留 created）

func TestCallback_FailedPayment_StaysCreated(t *testing.T) {
	fx := newTestApp(t, nil)
	var got *mallv1.MarkPaidReq
	fx.ord.markPaid = func(req *mallv1.MarkPaidReq) (*mallv1.OrderReply, error) {
		got = req
		o := sampleOrder("created") // 模拟 MarkPaidTx：失败回调订单停留 created
		o.OrderNo = req.GetOrderNo()
		return o, nil
	}
	payload := `{"order_no":"M1","provider":"mock","provider_trade_no":"T9","paid":false,"amount_cent":6900,"tenant_id":1}`
	sig := signPayload(t, []byte("test-pay-secret"), []byte(payload))
	rec := doReq(t, fx.app.Handler(), "POST", "/api/v1/payments/callback", "", payload,
		map[string]string{"X-Signature": sig})
	if rec.Code != 200 {
		t.Fatalf("失败回调: HTTP = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		OrderNo string `json:"order_no"`
		Status  string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Status != "created" || resp.OrderNo != "M1" {
		t.Fatalf("失败回调响应 = %s (err=%v), want {M1 created}", rec.Body.String(), err)
	}
	if got == nil || got.GetPaid() || !got.GetSignatureVerified() || got.GetTenantId() != 1 {
		t.Fatalf("MarkPaidReq = %+v, want {paid=false, signature_verified=true, tenant=1}", got)
	}
}

// ---------------------------------------------------------------------------
// 测试辅助

func hexEncode(b []byte) string { return hex.EncodeToString(b) }

func hmacSHA256(secret, payload []byte) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write(payload)
	return mac.Sum(nil)
}

func signPayload(t *testing.T, secret, payload []byte) string {
	t.Helper()
	return hex.EncodeToString(hmacSHA256(secret, payload))
}
