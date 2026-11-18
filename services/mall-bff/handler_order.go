// handler_order.go：订单/支付编排端点（api-contract §5）。
//
// 支付编排（task-6 裁定 4，D13 同口径）：bff 持 PaymentProvider——
// POST /orders/{no}/pay → 会话校验 + GetOrder(tenant) → provider.CreatePayment
// → 200 {status, payment}；MockProvider 内部 goroutine ~200ms 后向
// PAY_CALLBACK_URL POST HMAC-SHA256 签名载荷（密钥 PAY_CALLBACK_SECRET），
// 模拟真实渠道异步回调。
//
// POST /payments/callback（公开端点，无会话，走验签）：信任边界 = HMAC 验签
// （恒时比较）。载荷携带 tenant_id（mock 签发时取自发起支付的会话租户），
// bff 验签后经 gRPC MarkPaid 上报——下游服务端再次重校验订单归属
// （不符 → MALL_30001），并按入口如实落 signature_verified=1（MarkPaidReq
// field 7，审计 Minor-4 口径：不虚报验签）。
package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/frostloom/my-market/pkg/mallerr"
	"github.com/frostloom/my-market/services/mall-gen/mallv1"
)

const orderBodyLimit = 16 << 10

// ---------------------------------------------------------------------------
// JSON 视图

// orderItemJSON 订单行快照。
type orderItemJSON struct {
	GoodsID   uint64 `json:"goods_id"`
	GoodsName string `json:"goods_name"`
	Type      string `json:"type"`
	PriceCent int64  `json:"price_cent"`
	Qty       int32  `json:"qty"`
}

// orderJSON 契约 §5 Order 视图（轮询履约结果用）。
type orderJSON struct {
	OrderNo      string          `json:"order_no"`
	Status       string          `json:"status"`
	AmountCent   int64           `json:"amount_cent"`
	Items        []orderItemJSON `json:"items"`
	CreatedAt    string          `json:"created_at"`
	PaidAt       string          `json:"paid_at"`
	FulfilledAt  string          `json:"fulfilled_at"`
	ClosedReason string          `json:"closed_reason"`
}

func toOrderJSON(o *mallv1.OrderReply) orderJSON {
	items := make([]orderItemJSON, 0, len(o.GetItems()))
	for _, it := range o.GetItems() {
		items = append(items, orderItemJSON{
			GoodsID:   it.GetGoodsId(),
			GoodsName: it.GetGoodsName(),
			Type:      goodsTypeStr(it.GetGoodsType()),
			PriceCent: it.GetPriceCent(),
			Qty:       it.GetQty(),
		})
	}
	return orderJSON{
		OrderNo:      o.GetOrderNo(),
		Status:       o.GetStatus(),
		AmountCent:   o.GetAmountCent(),
		Items:        items,
		CreatedAt:    o.GetCreatedAt(),
		PaidAt:       o.GetPaidAt(),
		FulfilledAt:  o.GetFulfilledAt(),
		ClosedReason: o.GetClosedReason(),
	}
}

// payParams Provider 创建支付单返回（契约 §5：对象；无 → null）。
type payParamsJSON struct {
	TradeNo  string `json:"trade_no"`
	Provider string `json:"provider"`
}

// ---------------------------------------------------------------------------
// 端点

// handleCreateOrder POST /orders（头 Idempotency-Key 必填；kind=recharge 直购
// 行 / kind=cart 结算购物车）。幂等语义（同 key 同 payload 回原单 / 不同 payload
// MALL_30003）在 mall-order 侧以 (tenant_id, key) 唯一索引承载。
// 契约 201 响应的 pay_params：R1 支付编排落在 /pay（裁定），建单时下游
// pay_params_json 恒空 → 出 null（形状稳定，前端先建单后拉起支付）。
func handleCreateOrder(orders OrderClient) gin.HandlerFunc {
	return func(c *gin.Context) {
		idem := c.GetHeader("Idempotency-Key")
		if strings.TrimSpace(idem) == "" {
			abortWith(c, mallerr.E(mallerr.CodeInvalidParam, false, "缺少 Idempotency-Key 请求头"))
			return
		}
		var in struct {
			Kind     string `json:"kind"`
			GoodsID  uint64 `json:"goods_id"`
			FromCart bool   `json:"from_cart"`
		}
		raw, err := io.ReadAll(io.LimitReader(c.Request.Body, orderBodyLimit+1))
		if err != nil || len(raw) > orderBodyLimit {
			abortWith(c, mallerr.E(mallerr.CodeInvalidParam, false, "请求体过大（上限 16KB）"))
			return
		}
		if err := json.Unmarshal(raw, &in); err != nil {
			abortWith(c, mallerr.E(mallerr.CodeInvalidParam, false, "请求体须为合法 JSON"))
			return
		}
		var lines []*mallv1.Line
		switch in.Kind {
		case "recharge":
			if in.GoodsID == 0 {
				abortWith(c, mallerr.E(mallerr.CodeInvalidParam, false, "recharge 下单须带 goods_id"))
				return
			}
			lines = []*mallv1.Line{{GoodsId: in.GoodsID, Qty: 1}}
		case "cart":
			if !in.FromCart {
				abortWith(c, mallerr.E(mallerr.CodeInvalidParam, false, "cart 下单须 from_cart=true"))
				return
			}
			lines, err = cartLines(c.Request.Context(), orders, tenantID(c))
			if err != nil {
				abortWith(c, err)
				return
			}
		default:
			abortWith(c, mallerr.E(mallerr.CodeInvalidParam, false, "kind 须为 recharge|cart"))
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), rpcTimeout)
		defer cancel()
		reply, err := orders.CreateCartOrder(ctx, &mallv1.CreateCartOrderReq{
			TenantId: tenantID(c), IdemKey: idem, Lines: lines,
		})
		if err != nil {
			abortWith(c, err)
			return
		}
		c.JSON(http.StatusCreated, gin.H{
			"order_no":    reply.GetOrderNo(),
			"status":      reply.GetStatus(),
			"amount_cent": reply.GetAmountCent(),
			"items":       toOrderJSON(reply).Items,
			"pay_params":  decodePayParams(reply.GetPayParamsJson()),
		})
	}
}

// cartLines 购物车结算行（cart → 行快照）。空车 → MALL_10005。
func cartLines(ctx context.Context, orders OrderClient, tenant uint64) ([]*mallv1.Line, error) {
	v, err := orders.GetCart(ctx, &mallv1.GetCartReq{TenantId: tenant})
	if err != nil {
		return nil, err
	}
	if len(v.GetItems()) == 0 {
		return nil, mallerr.E(mallerr.CodeInvalidParam, false, "购物车为空，无法结算")
	}
	lines := make([]*mallv1.Line, 0, len(v.GetItems()))
	for _, it := range v.GetItems() {
		lines = append(lines, &mallv1.Line{GoodsId: it.GetGoods().GetId(), Qty: it.GetQty()})
	}
	return lines, nil
}

// decodePayParams 下游 pay_params_json → 对象（空/非法 → nil，契约 null）。
func decodePayParams(s string) *payParamsJSON {
	if s == "" {
		return nil
	}
	var pp payParamsJSON
	if err := json.Unmarshal([]byte(s), &pp); err != nil {
		return nil
	}
	return &pp
}

// handleListOrders GET /orders（游标分页；next_cursor 空 → null 即到底）。
func handleListOrders(orders OrderClient) gin.HandlerFunc {
	return func(c *gin.Context) {
		page, err := parsePageReq(c)
		if err != nil {
			abortWith(c, err)
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), rpcTimeout)
		defer cancel()
		reply, err := orders.ListOrders(ctx, &mallv1.ListOrdersReq{TenantId: tenantID(c), Page: page})
		if err != nil {
			abortWith(c, err)
			return
		}
		items := make([]orderJSON, 0, len(reply.GetItems()))
		for _, o := range reply.GetItems() {
			items = append(items, toOrderJSON(o))
		}
		writePage(c, items, reply.GetNextCursor(), reply.GetHasMore())
	}
}

// handleGetOrder GET /orders/{no}（服务端租户重校验：他租户 → MALL_30001 载体还原）。
func handleGetOrder(orders OrderClient) gin.HandlerFunc {
	return orderScoped(orders, func(ctx context.Context, orders OrderClient, tenant uint64, orderNo string) (any, error) {
		reply, err := orders.GetOrder(ctx, &mallv1.GetOrderReq{TenantId: tenant, OrderNo: orderNo})
		if err != nil {
			return nil, err
		}
		return toOrderJSON(reply), nil
	})
}

// handleCancelOrder POST /orders/{no}/cancel：仅 created 可取消（幂等：
// 已 closed 200；其余 MALL_30002 载体还原）。
func handleCancelOrder(orders OrderClient) gin.HandlerFunc {
	return orderScoped(orders, func(ctx context.Context, orders OrderClient, tenant uint64, orderNo string) (any, error) {
		reply, err := orders.CancelOrder(ctx, &mallv1.CancelOrderReq{TenantId: tenant, OrderNo: orderNo})
		if err != nil {
			return nil, err
		}
		return toOrderJSON(reply), nil
	})
}

// handleRefundOrder POST /orders/{no}/refund（R1 仅 paid 分支；30002/30008 载体还原）。
func handleRefundOrder(orders OrderClient) gin.HandlerFunc {
	return orderScoped(orders, func(ctx context.Context, orders OrderClient, tenant uint64, orderNo string) (any, error) {
		reply, err := orders.RefundOrder(ctx, &mallv1.RefundOrderReq{TenantId: tenant, OrderNo: orderNo})
		if err != nil {
			return nil, err
		}
		return gin.H{"status": reply.GetStatus(), "refund_cent": reply.GetRefundCent()}, nil
	})
}

// orderScoped 订单号路径端点公共壳：解析单号 + 统一 ctx 超时 + 会话租户注入。
func orderScoped(orders OrderClient, fn func(ctx context.Context, orders OrderClient, tenant uint64, orderNo string) (any, error)) gin.HandlerFunc {
	return func(c *gin.Context) {
		orderNo := c.Param("no")
		if orderNo == "" {
			abortWith(c, mallerr.E(mallerr.CodeInvalidParam, false, "订单号不能为空"))
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), rpcTimeout)
		defer cancel()
		out, err := fn(ctx, orders, tenantID(c), orderNo)
		if err != nil {
			abortWith(c, err)
			return
		}
		c.JSON(http.StatusOK, out)
	}
}

// ---------------------------------------------------------------------------
// 支付编排（bff 持 Provider）

// PayProvider 支付渠道抽象（bff 侧；与 mall-order 的 PaymentProvider 语义一致，
// R1 仅 mock）。CreatePayment 返回渠道侧支付单号。
type PayProvider interface {
	CreatePayment(ctx context.Context, orderNo string, amountCent int64) (tradeNo string, err error)
	ScheduleCallback(cb callbackPayload)
}

// MockProvider 进程内占位渠道：签发 trade_no 后起 goroutine，延迟
// cfg.CallbackDelay（生产默认 ~200ms）向 PAY_CALLBACK_URL POST HMAC-SHA256
// 签名载荷，模拟真实渠道异步回调（真实渠道接入后按 ADR-007 替换实现）。
type MockProvider struct {
	callbackURL func() string // 动态解析（测试可先行注入 httptest 基址）
	secret      []byte
	delay       time.Duration
	hc          *http.Client
	log         *slog.Logger
}

func newMockProvider(selfURL func() string, secret []byte, delay time.Duration, log *slog.Logger) *MockProvider {
	if delay <= 0 {
		delay = 200 * time.Millisecond
	}
	return &MockProvider{
		callbackURL: func() string {
			base := selfURL()
			if base == "" {
				base = "http://127.0.0.1:18090"
			}
			return strings.TrimRight(base, "/") + "/api/v1/payments/callback"
		},
		secret: secret,
		delay:  delay,
		hc:     &http.Client{Timeout: 5 * time.Second},
		log:    log,
	}
}

// callbackPayload bff 回调载荷。tenant_id 由 mock 签发时取自发起支付的会话
// 租户——回调端点无会话语境，验签通过后据此调 MarkPaid（下游再重校验归属）。
type callbackPayload struct {
	OrderNo    string `json:"order_no"`
	Provider   string `json:"provider"`
	TradeNo    string `json:"provider_trade_no"`
	Paid       bool   `json:"paid"`
	AmountCent int64  `json:"amount_cent"`
	TenantID   uint64 `json:"tenant_id"`
}

// signatureHeader HMAC 签名请求头（hex(HMAC-SHA256(secret, rawBody))）。
const signatureHeader = "X-Signature"

func (p *MockProvider) CreatePayment(_ context.Context, orderNo string, amountCent int64) (string, error) {
	if orderNo == "" {
		return "", mallerr.E(mallerr.CodeInvalidParam, false, "创建支付单缺 order_no")
	}
	if amountCent <= 0 {
		return "", mallerr.E(mallerr.CodeInvalidParam, false, "创建支付单金额非法: %d", amountCent)
	}
	return genTradeNo(), nil
}

// ScheduleCallback 异步投递回调（独立 goroutine；失败仅记日志——渠道回调
// 天然重试语义由调用方幂等兜底）。
func (p *MockProvider) ScheduleCallback(cb callbackPayload) {
	go func() {
		time.Sleep(p.delay)
		raw, err := json.Marshal(cb)
		if err != nil {
			p.log.Error("mock provider marshal callback", "err", err)
			return
		}
		mac := hmac.New(hashSHA256, p.secret)
		mac.Write(raw)
		req, err := http.NewRequest(http.MethodPost, p.callbackURL(), bytes.NewReader(raw))
		if err != nil {
			p.log.Error("mock provider build callback", "err", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(signatureHeader, hex.EncodeToString(mac.Sum(nil)))
		resp, err := p.hc.Do(req)
		if err != nil {
			p.log.Warn("mock provider callback failed", "order_no", cb.OrderNo, "err", err)
			return
		}
		defer resp.Body.Close() //nolint:errcheck // 读侧丢弃
		if resp.StatusCode != http.StatusOK {
			p.log.Warn("mock provider callback non-200", "order_no", cb.OrderNo, "status", resp.StatusCode)
		}
	}()
}

// genTradeNo 渠道支付单号：MP + yyyyMMddHHmmss + 6 位随机（CHAR(128) 上限内）。
func genTradeNo() string {
	n, err := randInt()
	if err != nil {
		n = 0 // 熵源故障退化：唯一性由 payment_records 组合幂等键兜底
	}
	return "MP" + time.Now().Format("20060102150405") + leftPad6(n)
}

// handlePay POST /orders/{no}/pay：会话校验 + GetOrder(tenant) →
// created → CreatePayment + 异步回调排程 → 200 created；
// paid/fulfilled → 200 paid（幂等，不重复拉起支付）；
// closed/refunded → MALL_30002（契约：30002 仅用于已关闭/已退）。
func handlePay(orders OrderClient, provider PayProvider) gin.HandlerFunc {
	return orderScoped(orders, func(ctx context.Context, orders OrderClient, tenant uint64, orderNo string) (any, error) {
		ord, err := orders.GetOrder(ctx, &mallv1.GetOrderReq{TenantId: tenant, OrderNo: orderNo})
		if err != nil {
			return nil, err
		}
		switch ord.GetStatus() {
		case "paid", "fulfilled":
			return payView("paid", ord.GetOrderNo(), ""), nil
		case "created":
			tradeNo, err := provider.CreatePayment(ctx, ord.GetOrderNo(), ord.GetAmountCent())
			if err != nil {
				return nil, err
			}
			// 异步回调载荷：tenant 取自当前会话（验签后的信任链起点）。
			provider.ScheduleCallback(callbackPayload{
				OrderNo:    ord.GetOrderNo(),
				Provider:   "mock",
				TradeNo:    tradeNo,
				Paid:       true,
				AmountCent: ord.GetAmountCent(),
				TenantID:   tenant,
			})
			return payView("created", ord.GetOrderNo(), tradeNo), nil
		default: // closed / refunded / 其他终态
			return nil, mallerr.E(mallerr.CodeOrderStateInvalid, false, "订单状态不允许支付: %s", ord.GetStatus())
		}
	})
}

// payView 契约 §5 pay 响应：{"status":"paid|created","payment":{...}}。
func payView(status, orderNo, tradeNo string) gin.H {
	payment := gin.H{"order_no": orderNo, "provider": "mock", "status": status}
	if tradeNo != "" {
		payment["trade_no"] = tradeNo
	}
	return gin.H{"status": status, "payment": payment}
}

// ---------------------------------------------------------------------------
// 回调端点（公开；信任边界 = HMAC 验签）

// handlePaymentCallback POST /payments/callback：验签 → gRPC MarkPaid
// （tenant 自签名载荷；下游服务端重校验归属）→ 200 {order_no, status}。
// 幂等：重复回调由 payment_records 组合键 + 订单条件更新兜底，返回当前状态。
func handlePaymentCallback(orders OrderClient, secret []byte) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw, err := io.ReadAll(io.LimitReader(c.Request.Body, orderBodyLimit+1))
		if err != nil || len(raw) > orderBodyLimit {
			abortWith(c, mallerr.E(mallerr.CodeCallbackSignInvalid, false, "回调载荷非法"))
			return
		}
		cb, err := verifyCallback(secret, raw, c.GetHeader(signatureHeader))
		if err != nil {
			abortWith(c, err)
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), rpcTimeout)
		defer cancel()
		reply, err := orders.MarkPaid(ctx, &mallv1.MarkPaidReq{
			OrderNo:           cb.OrderNo,
			Provider:          cb.Provider,
			ProviderTradeNo:   cb.TradeNo,
			AmountCent:        cb.AmountCent,
			Paid:              cb.Paid,
			TenantId:          cb.TenantID,
			SignatureVerified: true, // 已过验签，按入口如实上报（落流水 1）
		})
		if err != nil {
			abortWith(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"order_no": reply.GetOrderNo(), "status": reply.GetStatus()})
	}
}

// verifyCallback 验签（hmac.Equal 恒时比较）→ 还原载荷。签名不符 / 载荷非法 /
// 缺字段 → MALL_30005。
func verifyCallback(secret, raw []byte, sig string) (*callbackPayload, error) {
	want := hmac.New(hashSHA256, secret)
	want.Write(raw)
	got, err := hex.DecodeString(sig)
	if err != nil || !hmac.Equal(want.Sum(nil), got) {
		return nil, mallerr.E(mallerr.CodeCallbackSignInvalid, false, "回调验签失败")
	}
	var cb callbackPayload
	if err := json.Unmarshal(raw, &cb); err != nil || cb.OrderNo == "" || cb.Provider == "" || cb.TradeNo == "" || cb.TenantID == 0 {
		return nil, mallerr.E(mallerr.CodeCallbackSignInvalid, false, "回调载荷非法")
	}
	return &cb, nil
}

// leftPad6 6 位数字补零。
func leftPad6(n int64) string {
	s := big.NewInt(n).String()
	for len(s) < 6 {
		s = "0" + s
	}
	return s
}
