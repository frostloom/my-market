// handler_me_test.go：GET /api/v1/me/overview（R2-T4，api-contract §7）。
//
// 三组断言：
//  1. 契约形状：{balance_cent, recent_topups[≤10], subscription}——recent_topups
//     空数组非 null（前端可直接 .map），subscription 恒 null（R2.5 月卡接入前）；
//  2. 凭据零泄漏：响应体与进程日志（slog 默认 handler）均不出现
//     CONTROL_PLANE_TOKEN；token 只以 X-Control-Token（gRPC metadata / HTTP 头）
//     流向 billing gRPC 与 billing admin HTTP，经"下游桩捕获值"证到达；
//  3. 故障映射：billing gRPC / admin HTTP 不可用 → 503 MALL_50003。
//
// 桩策略（与 router_test.go 同口径）：BillingClient / TopupsSource 接口函数桩
// 驱 gin 引擎；真客户端链路（tokenUnaryClient / adminTopupsClient）另起
// 127.0.0.1:0 桩 gRPC server + httptest admin server 做端到端零泄漏验证。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
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
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/frostloom/my-market/pkg/mallerr"
	billingv1 "github.com/frostloom/my-market/pkg/proto/billing/v1"
)

// ---------------------------------------------------------------------------
// 接口桩（handler 级）

type stubBilling struct {
	mu         sync.Mutex
	tenantRec  []int64
	getBalance func(ctx context.Context, req *billingv1.GetBalanceRequest, _ ...grpc.CallOption) (*billingv1.GetBalanceResponse, error)
}

func (s *stubBilling) GetBalance(ctx context.Context, req *billingv1.GetBalanceRequest, co ...grpc.CallOption) (*billingv1.GetBalanceResponse, error) {
	s.mu.Lock()
	s.tenantRec = append(s.tenantRec, req.GetTenantId())
	s.mu.Unlock()
	if s.getBalance == nil {
		panic("unexpected GetBalance")
	}
	return s.getBalance(ctx, req, co...)
}

func (s *stubBilling) balanceCalls() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int64(nil), s.tenantRec...)
}

type stubTopups struct {
	mu        sync.Mutex
	tenantRec []uint64
	limitRec  []int
	recent    func(ctx context.Context, tenantID uint64, limit int) ([]billingTopup, error)
}

func (s *stubTopups) RecentTopups(ctx context.Context, tenantID uint64, limit int) ([]billingTopup, error) {
	s.mu.Lock()
	s.tenantRec = append(s.tenantRec, tenantID)
	s.limitRec = append(s.limitRec, limit)
	s.mu.Unlock()
	if s.recent == nil {
		panic("unexpected RecentTopups")
	}
	return s.recent(ctx, tenantID, limit)
}

func (s *stubTopups) calls() (tenants []uint64, limits []int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]uint64(nil), s.tenantRec...), append([]int(nil), s.limitRec...)
}

// billingTopupRowAdmin 伪造 billing admin wire 行（键 = Go 字段名，无 json tag
// 的 writeJSON 默认序列化口径；CreatedAt 为 RFC3339 字符串）。
func billingTopupRowAdmin(id uint64, refType, refID string, amount int64) map[string]any {
	return map[string]any{
		"ID":            id,
		"TenantID":      uint64(7),
		"Source":        refType,
		"Amount":        amount,
		"RequestedCent": nil,
		"RefType":       refType,
		"RefID":         refID,
		"CreatedAt":     "2026-11-19T12:00:00+08:00",
	}
}

// captureLogs 把 slog 默认 handler 换成内存 buffer（NewApp 会捕获 slog.Default，
// 必须在 NewApp 之前调用；测试串行执行，退出还原）。
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

// ---------------------------------------------------------------------------
// 1. 契约形状

func TestMeOverview_Shape_BalanceTopupsSubscription(t *testing.T) {
	logs := captureLogs(t)
	fx := newTestApp(t, nil)
	fx.billing.getBalance = func(_ context.Context, _ *billingv1.GetBalanceRequest, _ ...grpc.CallOption) (*billingv1.GetBalanceResponse, error) {
		return &billingv1.GetBalanceResponse{Balance: 1030}, nil
	}
	fx.topups.recent = func(_ context.Context, _ uint64, _ int) ([]billingTopup, error) {
		return []billingTopup{
			{ID: 2, TenantID: 7, Source: "mall", Amount: 1000, RefType: "mall", RefID: "M1:1", CreatedAt: "2026-11-19T12:00:01+08:00"},
			{ID: 1, TenantID: 7, Source: "refund", Amount: -300, RefType: "refund", RefID: "mall-refund:M0:1", CreatedAt: "2026-11-19T12:00:00+08:00"},
		}, nil
	}

	tok := fx.gw.issue(t, 7)
	rec := doReq(t, fx.app.Handler(), http.MethodGet, "/api/v1/me/overview", tok, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("overview = %d，want 200；body=%s", rec.Code, rec.Body.String())
	}
	raw := rec.Body.String()
	var body struct {
		BalanceCent  int64               `json:"balance_cent"`
		RecentTopups []overviewTopupJSON `json:"recent_topups"`
		Subscription any                 `json:"subscription"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应体解析: %v\nbody=%s", err, raw)
	}
	if body.BalanceCent != 1030 {
		t.Fatalf("balance_cent = %d，want 1030", body.BalanceCent)
	}
	if len(body.RecentTopups) != 2 {
		t.Fatalf("recent_topups 条数 = %d，want 2\nbody=%s", len(body.RecentTopups), raw)
	}
	// 倒序第一条 = 最新入账。
	first := body.RecentTopups[0]
	if first.ID != 2 || first.Source != "mall" || first.Amount != 1000 ||
		first.RefType != "mall" || first.RefID != "M1:1" || first.CreatedAt == "" {
		t.Fatalf("recent_topups[0] 映射不符: %+v", first)
	}
	if body.RecentTopups[1].Amount != -300 {
		t.Fatalf("recent_topups[1].Amount = %d，want -300（退款负数）", body.RecentTopups[1].Amount)
	}
	// subscription 恒 null（R2.5 前不得出现对象/true）。
	if body.Subscription != nil {
		t.Fatalf("subscription = %v，want null（R2.5 月卡接入前恒 null）", body.Subscription)
	}
	if !strings.Contains(raw, `"subscription":null`) {
		t.Fatalf("响应体缺少 \"subscription\":null 字面形状: %s", raw)
	}
	// 顶层恰三键（契约 §7 形状冻结，多字段=契约外泄）。
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &keys); err != nil {
		t.Fatalf("顶层键解析: %v", err)
	}
	if len(keys) != 3 {
		t.Fatalf("顶层键数 = %d（%v），want 恰 {balance_cent, recent_topups, subscription}", len(keys), raw)
	}
	// 凭据零泄漏：token 不落响应体，也不落日志。
	const secret = "test-control-token-secret"
	if strings.Contains(raw, secret) {
		t.Fatalf("响应体泄漏控制面 token:\n%s", raw)
	}
	if s := logs.String(); strings.Contains(s, secret) {
		t.Fatalf("进程日志泄漏控制面 token:\n%s", s)
	}
}

// recent_topups 空数组非 null：前端可直接遍历（契约裁定）。
func TestMeOverview_EmptyTopups_ArrayNotNull(t *testing.T) {
	_ = captureLogs(t)
	fx := newTestApp(t, nil)
	fx.billing.getBalance = func(_ context.Context, _ *billingv1.GetBalanceRequest, _ ...grpc.CallOption) (*billingv1.GetBalanceResponse, error) {
		return &billingv1.GetBalanceResponse{Balance: 0}, nil
	}
	fx.topups.recent = func(_ context.Context, _ uint64, _ int) ([]billingTopup, error) {
		return []billingTopup{}, nil
	}
	tok := fx.gw.issue(t, 7)
	rec := doReq(t, fx.app.Handler(), http.MethodGet, "/api/v1/me/overview", tok, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("overview = %d，want 200；body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"recent_topups":[]`) {
		t.Fatalf("空流水应序列化为 [] 非 null: %s", rec.Body.String())
	}
	var body struct {
		BalanceCent  int64          `json:"balance_cent"`
		RecentTopups []any          `json:"recent_topups"`
		Subscription map[string]any `json:"subscription"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if body.RecentTopups == nil {
		t.Fatalf("recent_topups = null，want []")
	}
	if body.BalanceCent != 0 || body.Subscription != nil {
		t.Fatalf("新租户 overview 不符: %+v", body)
	}
}

// ≤10 条上限（契约 §7「最近 10 条」）：上游溢出由 handler 截断。
func TestMeOverview_ClampRecentTopupsToTen(t *testing.T) {
	_ = captureLogs(t)
	fx := newTestApp(t, nil)
	fx.billing.getBalance = func(_ context.Context, _ *billingv1.GetBalanceRequest, _ ...grpc.CallOption) (*billingv1.GetBalanceResponse, error) {
		return &billingv1.GetBalanceResponse{Balance: 1}, nil
	}
	fx.topups.recent = func(_ context.Context, _ uint64, _ int) ([]billingTopup, error) {
		rows := make([]billingTopup, 0, 12)
		for i := 0; i < 12; i++ {
			rows = append(rows, billingTopup{ID: uint64(i + 1), TenantID: 7, Source: "mall", Amount: 1, RefType: "mall", RefID: fmt.Sprintf("M%d:1", i), CreatedAt: "2026-11-19T12:00:00+08:00"})
		}
		return rows, nil
	}
	tok := fx.gw.issue(t, 7)
	rec := doReq(t, fx.app.Handler(), http.MethodGet, "/api/v1/me/overview", tok, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("overview = %d，want 200", rec.Code)
	}
	var body struct {
		RecentTopups []overviewTopupJSON `json:"recent_topups"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if len(body.RecentTopups) != overviewTopupsLimit {
		t.Fatalf("recent_topups = %d 条，want ≤%d（截断到最近 10 条）", len(body.RecentTopups), overviewTopupsLimit)
	}
}

// 会话租户传播：balance 与 topups 都以会话派生租户查询（不收前端参数）。
func TestMeOverview_TenantPropagationToDownstreams(t *testing.T) {
	_ = captureLogs(t)
	fx := newTestApp(t, nil)
	fx.billing.getBalance = func(_ context.Context, _ *billingv1.GetBalanceRequest, _ ...grpc.CallOption) (*billingv1.GetBalanceResponse, error) {
		return &billingv1.GetBalanceResponse{Balance: 5}, nil
	}
	fx.topups.recent = func(_ context.Context, _ uint64, _ int) ([]billingTopup, error) {
		return nil, nil
	}
	tok := fx.gw.issue(t, 4242)
	rec := doReq(t, fx.app.Handler(), http.MethodGet, "/api/v1/me/overview?tenant_id=999", tok, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("overview = %d，want 200（租户只认会话，查询串忽略）；body=%s", rec.Code, rec.Body.String())
	}
	if got := fx.billing.balanceCalls(); len(got) != 1 || got[0] != 4242 {
		t.Fatalf("GetBalance 租户 = %v，want [4242]", got)
	}
	tenants, limits := fx.topups.calls()
	if len(tenants) != 1 || tenants[0] != 4242 {
		t.Fatalf("RecentTopups 租户 = %v，want [4242]", tenants)
	}
	if len(limits) != 1 || limits[0] != overviewTopupsLimit {
		t.Fatalf("RecentTopups limit = %v，want [%d]", limits, overviewTopupsLimit)
	}
}

// ---------------------------------------------------------------------------
// 2. 鉴权与故障映射

func TestMeOverview_Unauthenticated_401_MALL10001(t *testing.T) {
	_ = captureLogs(t)
	fx := newTestApp(t, nil)
	rec := doReq(t, fx.app.Handler(), http.MethodGet, "/api/v1/me/overview", "", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无 token overview = %d，want 401；body=%s", rec.Code, rec.Body.String())
	}
	eb := decodeErr(t, rec)
	if eb.Code != string(mallerr.CodeUnauthenticated) {
		t.Fatalf("code = %s，want MALL_10001", eb.Code)
	}
}

func TestMeOverview_DownstreamFailure_MapsMALL50003(t *testing.T) {
	_ = captureLogs(t)
	fx := newTestApp(t, nil)
	tok := fx.gw.issue(t, 7)

	// billing gRPC Unavailable → 503 MALL_50003（无载体粗映射）。
	fx.billing.getBalance = func(_ context.Context, _ *billingv1.GetBalanceRequest, _ ...grpc.CallOption) (*billingv1.GetBalanceResponse, error) {
		return nil, status.Error(codes.Unavailable, "billing down")
	}
	fx.topups.recent = func(_ context.Context, _ uint64, _ int) ([]billingTopup, error) {
		return nil, nil
	}
	rec := doReq(t, fx.app.Handler(), http.MethodGet, "/api/v1/me/overview", tok, "", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("billing 不可用 overview = %d，want 503；body=%s", rec.Code, rec.Body.String())
	}
	if eb := decodeErr(t, rec); eb.Code != string(mallerr.CodeUpstreamUnavailable) {
		t.Fatalf("code = %s，want MALL_50003", eb.Code)
	}

	// admin topups 失败 → 同样 503 MALL_50003（balance 已成功也整体失败）。
	fx.billing.getBalance = func(_ context.Context, _ *billingv1.GetBalanceRequest, _ ...grpc.CallOption) (*billingv1.GetBalanceResponse, error) {
		return &billingv1.GetBalanceResponse{Balance: 9}, nil
	}
	fx.topups.recent = func(_ context.Context, _ uint64, _ int) ([]billingTopup, error) {
		return nil, mallerr.E(mallerr.CodeUpstreamUnavailable, true, "topups 不可用")
	}
	rec = doReq(t, fx.app.Handler(), http.MethodGet, "/api/v1/me/overview", tok, "", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("topups 不可用 overview = %d，want 503；body=%s", rec.Code, rec.Body.String())
	}
	if eb := decodeErr(t, rec); eb.Code != string(mallerr.CodeUpstreamUnavailable) || !eb.Retryable {
		t.Fatalf("code = %s retryable=%v，want MALL_50003 true", eb.Code, eb.Retryable)
	}
}

// ---------------------------------------------------------------------------
//  3. 端到端零泄漏（真客户端链路）：DialBilling → 127.0.0.1:0 桩 gRPC server；
//     adminTopupsClient → httptest admin server。断言 token 恰好经
//     X-Control-Token 到达两侧下游、不落响应体与日志。
func TestMeOverview_ZeroLeak_LiveDownstreams(t *testing.T) {
	const secret = "tok-r2-overview-live-secret"

	logs := captureLogs(t)

	// billing gRPC 桩：捕获入站 token metadata，回固定余额。
	var mu sync.Mutex
	var grpcTokens []string
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	gs := grpc.NewServer()
	billingv1.RegisterBillingServiceServer(gs, &stubBillingGRPC{
		onCall: func(ctx context.Context) {
			md, _ := metadata.FromIncomingContext(ctx)
			mu.Lock()
			grpcTokens = append(grpcTokens, strings.Join(md.Get(billingMetadataKey), ","))
			mu.Unlock()
		},
	})
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	// admin HTTP 桩：捕获 X-Control-Token 头与查询串，回一条流水（键 = Go 字段
	// 名，billing writeJSON 默认序列化口径）。
	var httpTokens []string
	var httpQueries []string
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		httpTokens = append(httpTokens, r.Header.Get("X-Control-Token"))
		httpQueries = append(httpQueries, r.URL.RawQuery)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[` + string(mustJSON(t, billingTopupRowAdmin(1, "mall", "M9:1", 1000))) + `]}`))
	}))
	t.Cleanup(admin.Close)

	billing, closeBilling, err := DialBilling(lis.Addr().String(), secret)
	if err != nil {
		t.Fatalf("DialBilling: %v", err)
	}
	t.Cleanup(closeBilling)
	topups, err := newAdminTopups(admin.URL, secret)
	if err != nil {
		t.Fatalf("newAdminTopups: %v", err)
	}

	fx := newTestAppRaw(t, billing, topups)
	tok := fx.gw.issue(t, 55)
	rec := doReq(t, fx.app.Handler(), http.MethodGet, "/api/v1/me/overview", tok, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("overview = %d，want 200；body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		BalanceCent  int64               `json:"balance_cent"`
		RecentTopups []overviewTopupJSON `json:"recent_topups"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析: %v\nbody=%s", err, rec.Body.String())
	}
	if body.BalanceCent != 1000 || len(body.RecentTopups) != 1 || body.RecentTopups[0].RefID != "M9:1" {
		t.Fatalf("live overview 不符: %+v", body)
	}

	// token 双侧到达。
	mu.Lock()
	defer mu.Unlock()
	if len(grpcTokens) != 1 || grpcTokens[0] != secret {
		t.Fatalf("billing gRPC 捕获 token = %v，want [%s]", grpcTokens, secret)
	}
	if len(httpTokens) != 1 || httpTokens[0] != secret {
		t.Fatalf("admin HTTP 捕获 token = %v，want [%s]", httpTokens, secret)
	}
	if !strings.Contains(httpQueries[0], "tenant_id=55") {
		t.Fatalf("admin HTTP 查询串缺 tenant_id=55: %s", httpQueries[0])
	}
	// 零泄漏：响应体 + 日志。
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("响应体泄漏 token:\n%s", rec.Body.String())
	}
	if s := logs.String(); strings.Contains(s, secret) {
		t.Fatalf("日志泄漏 token:\n%s", s)
	}
}

// stubBillingGRPC GetBalance 桩（回调捕获 token 后回 1000）。
type stubBillingGRPC struct {
	billingv1.UnimplementedBillingServiceServer
	onCall func(ctx context.Context)
}

func (s *stubBillingGRPC) GetBalance(ctx context.Context, _ *billingv1.GetBalanceRequest) (*billingv1.GetBalanceResponse, error) {
	s.onCall(ctx)
	return &billingv1.GetBalanceResponse{Balance: 1000}, nil
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// ---------------------------------------------------------------------------
// 4. adminTopupsClient（billing admin HTTP 客户端）行为

func TestAdminTopupsClient_TokenHeaderAndSlice(t *testing.T) {
	const secret = "tok-admin-topups-secret"
	var gotToken string
	var gotQuery string
	var gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("X-Control-Token")
		gotQuery = r.URL.RawQuery
		gotMethod = r.Method
		// 15 条溢出 + 一条非 mall 流水（退款），倒序 id 递减模拟真实。
		var rows []string
		for i := 15; i >= 1; i-- {
			rows = append(rows, string(mustJSON(t, billingTopupRowAdmin(uint64(i), "mall", fmt.Sprintf("M%d:1", i), 100))))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[` + strings.Join(rows, ",") + `]}`))
	}))
	t.Cleanup(srv.Close)

	client, err := newAdminTopups(srv.URL, secret)
	if err != nil {
		t.Fatalf("newAdminTopups: %v", err)
	}
	rows, err := client.RecentTopups(context.Background(), 33, 10)
	if err != nil {
		t.Fatalf("RecentTopups: %v", err)
	}
	if gotMethod != http.MethodGet {
		t.Fatalf("method = %s，want GET", gotMethod)
	}
	if gotToken != secret {
		t.Fatalf("X-Control-Token = %q，want %q", gotToken, secret)
	}
	if !strings.Contains(gotQuery, "tenant_id=33") {
		t.Fatalf("query 缺 tenant_id=33: %s", gotQuery)
	}
	if len(rows) != 10 {
		t.Fatalf("rows = %d，want 截断到 10（上游回 50 条口径）", len(rows))
	}
	// 倒序第一条 = id 15（最新）。
	if rows[0].ID != 15 || rows[0].RefType != "mall" || rows[0].RefID != "M15:1" || rows[0].Amount != 100 {
		t.Fatalf("rows[0] 映射不符: %+v", rows[0])
	}
}

func TestAdminTopupsClient_UpstreamError_MALL50003(t *testing.T) {
	const secret = "tok-admin-err-secret"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"db down"}`))
	}))
	t.Cleanup(srv.Close)
	client, err := newAdminTopups(srv.URL, secret)
	if err != nil {
		t.Fatalf("newAdminTopups: %v", err)
	}
	_, err = client.RecentTopups(context.Background(), 1, 10)
	if err == nil {
		t.Fatalf("500 应报错")
	}
	var me *mallerr.Error
	if !errors.As(err, &me) || me.Code != mallerr.CodeUpstreamUnavailable || !me.Retryable {
		t.Fatalf("err = %v，want MALL_50003 retryable", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("错误信息泄漏 token: %v", err)
	}
}

func TestAdminTopupsClient_EmptyTokenFailClosed(t *testing.T) {
	if _, err := newAdminTopups("http://127.0.0.1:9103", ""); err == nil {
		t.Fatalf("空 token 应 fail-closed 报错")
	}
}

// ---------------------------------------------------------------------------
// 5. bff DialBilling（与 mall-order 同款语义：每请求注 token / 空 token fail-closed）

func TestBillingClient_InjectsTokenPerRequest(t *testing.T) {
	const secret = "tok-bff-billing-secret"
	var mu sync.Mutex
	var toks []string
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	gs := grpc.NewServer()
	billingv1.RegisterBillingServiceServer(gs, &stubBillingGRPC{
		onCall: func(ctx context.Context) {
			md, _ := metadata.FromIncomingContext(ctx)
			mu.Lock()
			toks = append(toks, strings.Join(md.Get(billingMetadataKey), ","))
			mu.Unlock()
		},
	})
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	client, closeConn, err := DialBilling(lis.Addr().String(), secret)
	if err != nil {
		t.Fatalf("DialBilling: %v", err)
	}
	defer closeConn()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rep, err := client.GetBalance(ctx, &billingv1.GetBalanceRequest{TenantId: 8})
	if err != nil {
		t.Fatalf("GetBalance: %v", err)
	}
	if rep.GetBalance() != 1000 {
		t.Fatalf("balance = %d，want 1000（proto 往返）", rep.GetBalance())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(toks) != 1 || toks[0] != secret {
		t.Fatalf("桩捕获 token = %v，want [%s]", toks, secret)
	}
}

func TestBillingClient_EmptyTokenFailClosed(t *testing.T) {
	if _, closeFn, err := DialBilling("127.0.0.1:9101", ""); err == nil {
		closeFn()
		t.Fatalf("空 token 拨号应报错（fail-closed）")
	}
}

// newTestAppRaw 以真下游客户端装配 App（零泄漏端到端测试用）。
func newTestAppRaw(t *testing.T, billing BillingClient, topups TopupsSource) *appFixture {
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
		CallbackSelfURL:   "http://127.0.0.1:18090",
		PayCallbackSecret: []byte("test-pay-secret"),
		RateLimitPerMin:   120,
		AuthCacheTTL:      30 * time.Second,
		CallbackDelay:     200 * time.Millisecond,
	}
	fx.app = NewApp(cfg, fx.good, fx.ord, billing, topups, redis.NewClient(&redis.Options{Addr: fx.mr.Addr()}))
	t.Cleanup(func() { _ = fx.app.Close() })
	return fx
}
