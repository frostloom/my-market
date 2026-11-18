// bff POST /orders kind 分派测试（R2-T2）：recharge 直购走 CreateRechargeOrder
// RPC（goods 类型校验在 order 服务端，bff 只透传 kind/goods_id）；cart 路径
// 不变走 CreateCartOrder；入参校验分支（缺 Idempotency-Key / 缺 goods_id /
// cart 缺 from_cart / 非法 kind）不触达下游。
//
// 接口桩（gclient.go OrderClient，stubOrder 于 router_test.go）：
// createRech/createCart 函数字段捕获收到的请求，断言分派与 payload。
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"google.golang.org/grpc"

	"github.com/frostloom/my-market/pkg/mallerr"
	"github.com/frostloom/my-market/services/mall-gen/mallv1"
)

// rpcRecorder 记录 createRech/createCart 收到的请求（并发安全）。
type rpcRecorder struct {
	mu      sync.Mutex
	recharge []*mallv1.CreateRechargeOrderReq
	cart    []*mallv1.CreateCartOrderReq
}

func (r *rpcRecorder) rechargeCalls() []*mallv1.CreateRechargeOrderReq {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*mallv1.CreateRechargeOrderReq(nil), r.recharge...)
}

func (r *rpcRecorder) cartCalls() []*mallv1.CreateCartOrderReq {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*mallv1.CreateCartOrderReq(nil), r.cart...)
}

// wireRecorder 把 recorder 接进 stubOrder（两分支各回一单形状的 reply）。
func (r *rpcRecorder) wire(fx *appFixture) {
	fx.ord.createRech = func(_ context.Context, req *mallv1.CreateRechargeOrderReq, _ ...grpc.CallOption) (*mallv1.OrderReply, error) {
		r.mu.Lock()
		r.recharge = append(r.recharge, req)
		r.mu.Unlock()
		return &mallv1.OrderReply{
			OrderNo: "M202611190000000000001", Status: "created", AmountCent: 5300,
			Items: []*mallv1.OrderItem{{
				GoodsId: 202, GoodsName: "余额充值 ¥50", GoodsType: 2, PriceCent: 5000, Qty: 1,
			}},
			CreatedAt: "2026-11-19T12:00:00+08:00",
		}, nil
	}
	fx.ord.createCart = func(_ context.Context, req *mallv1.CreateCartOrderReq, _ ...grpc.CallOption) (*mallv1.OrderReply, error) {
		r.mu.Lock()
		r.cart = append(r.cart, req)
		r.mu.Unlock()
		return &mallv1.OrderReply{OrderNo: "M202611190000000000002", Status: "created", AmountCent: 6900}, nil
	}
}

// ---------------------------------------------------------------------------
// TestOrdersKindRecharge_Dispatch：kind=recharge → CreateRechargeOrder（透传
// goods_id + Idempotency-Key + 会话租户）；kind=cart → CreateCartOrder（既有
// 路径不变）；校验分支 400（MALL_10005）不触达下游。
func TestOrdersKindRecharge_Dispatch(t *testing.T) {
	fx := newTestApp(t, nil)
	tok := fx.gw.issue(t, 42)
	rec := &rpcRecorder{}
	rec.wire(fx)
	h := fx.app.Handler()

	// 1) recharge 分支：CreateRechargeOrder 收到 (tenant=42, goods=202, key 透传)。
	rec1 := doReq(t, h, http.MethodPost, "/api/v1/orders", tok,
		`{"kind":"recharge","goods_id":202}`, map[string]string{"Idempotency-Key": "idem-rech-1"})
	if rec1.Code != http.StatusCreated {
		t.Fatalf("recharge 建单 = %d，want 201；body=%s", rec1.Code, rec1.Body.String())
	}
	rc := rec.rechargeCalls()
	if len(rc) != 1 {
		t.Fatalf("CreateRechargeOrder 调用数 = %d，want 1", len(rc))
	}
	if rc[0].GetTenantId() != 42 || rc[0].GetGoodsId() != 202 || rc[0].GetIdemKey() != "idem-rech-1" {
		t.Fatalf("CreateRechargeOrder payload = %+v，want (tenant=42, goods=202, key=idem-rech-1)", rc[0])
	}
	if len(rec.cartCalls()) != 0 {
		t.Fatalf("cart 路径被误触达: %+v", rec.cartCalls())
	}
	// 201 响应形状（契约 §5：order_no/status/amount_cent/items/pay_params）。
	var body struct {
		OrderNo    string          `json:"order_no"`
		Status     string          `json:"status"`
		AmountCent int64           `json:"amount_cent"`
		Items      []orderItemJSON `json:"items"`
		PayParams  any             `json:"pay_params"`
	}
	if err := json.Unmarshal(rec1.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应体解析: %v\nbody=%s", err, rec1.Body.String())
	}
	if body.OrderNo != "M202611190000000000001" || body.Status != "created" || body.AmountCent != 5300 {
		t.Fatalf("201 响应头不符: %+v", body)
	}
	if len(body.Items) != 1 || body.Items[0].GoodsID != 202 || body.Items[0].Type != "recharge" {
		t.Fatalf("201 响应行不符: %+v", body.Items)
	}
	if body.PayParams != nil {
		t.Fatalf("pay_params = %v，want null（R1 建单不带支付参数）", body.PayParams)
	}

	// 2) cart 分支不变：CreateCartOrder 收到购物车行，CreateRechargeOrder 不再被调。
	fx.ord.cartItems = []*mallv1.CartItem{{Id: 7, Qty: 1, Goods: &mallv1.GoodsItem{Id: 113}}}
	doReq(t, h, http.MethodPost, "/api/v1/orders", tok,
		`{"kind":"cart","from_cart":true}`, map[string]string{"Idempotency-Key": "idem-cart-1"})
	cc := rec.cartCalls()
	if len(cc) != 1 {
		t.Fatalf("CreateCartOrder 调用数 = %d，want 1（cart 路径回归）", len(cc))
	}
	if len(cc[0].GetLines()) != 1 || cc[0].GetLines()[0].GetGoodsId() != 113 || cc[0].GetIdemKey() != "idem-cart-1" {
		t.Fatalf("CreateCartOrder payload = %+v", cc[0])
	}
	if got := len(rec.rechargeCalls()); got != 1 {
		t.Fatalf("CreateRechargeOrder 调用数 = %d，want 仍 1（cart 不走 recharge）", got)
	}

	// 3) 校验分支：400（MALL_10005）且不触达任何建单 RPC。
	baseline := len(rec.rechargeCalls()) + len(rec.cartCalls())
	for _, tc := range []struct {
		name string
		body string
		idem string
	}{
		{"缺Idempotency-Key", `{"kind":"recharge","goods_id":202}`, ""},
		{"recharge缺goods_id", `{"kind":"recharge"}`, "idem-x"},
		{"cart缺from_cart", `{"kind":"cart"}`, "idem-x"},
		{"非法kind", `{"kind":"seckill"}`, "idem-x"},
		{"goods_id为0", `{"kind":"recharge","goods_id":0}`, "idem-x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hdr := map[string]string{}
			if tc.idem != "" {
				hdr["Idempotency-Key"] = tc.idem
			}
			r2 := doReq(t, h, http.MethodPost, "/api/v1/orders", tok, tc.body, hdr)
			if r2.Code != http.StatusBadRequest {
				t.Fatalf("%s: code = %d，want 400；body=%s", tc.name, r2.Code, r2.Body.String())
			}
			eb := decodeErr(t, r2)
			if eb.Code != string(mallerr.CodeInvalidParam) {
				t.Fatalf("%s: code 载体 = %s，want MALL_10005", tc.name, eb.Code)
			}
		})
	}
	if got := len(rec.rechargeCalls()) + len(rec.cartCalls()); got != baseline {
		t.Fatalf("校验分支触达下游 %d 次，want 0", got-baseline)
	}
}
