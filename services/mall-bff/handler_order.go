// handler_order.go：订单/支付编排端点（api-contract §5）。
//
// 支付编排（task-6 裁定 4，D13 同口径；2026-11-22 建单步骤改走 BeginPayment，
// PRD §5.2「CreatePayment 写 payment_records=created」落位）：bff 持 PayProvider
// （渠道回调侧）——POST /orders/{no}/pay → 会话校验 + GetOrder(tenant) →
// orders.BeginPayment（mall-order 订单行锁内建/复用在途单并分配 trade_no）
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
// 行 → CreateRechargeOrder RPC / kind=cart 结算购物车 → CreateCartOrder RPC）。
// 幂等语义（同 key 同 payload 回原单 / 不同 payload MALL_30003）在 mall-order
// 侧以 (tenant_id, key) 唯一索引承载；goods 类型校验（type=2 才可直购）也在
// order 服务端，bff 只透传 kind/goods_id。
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
		// 与其余下游调用同口径：下游调用统一吃 rpcTimeout（终审 M-3），
		// 不裸用请求 ctx——下游卡死时 5s 内以 MALL_50002 出错而非挂满连接。
		ctx, cancel := context.WithTimeout(c.Request.Context(), rpcTimeout)
		defer cancel()
		var reply *mallv1.OrderReply
		switch in.Kind {
		case "recharge":
			if in.GoodsID == 0 {
				abortWith(c, mallerr.E(mallerr.CodeInvalidParam, false, "recharge 下单须带 goods_id"))
				return
			}
			reply, err = orders.CreateRechargeOrder(ctx, &mallv1.CreateRechargeOrderReq{
				TenantId: tenantID(c), GoodsId: in.GoodsID, IdemKey: idem,
			})
		case "cart":
			if !in.FromCart {
				abortWith(c, mallerr.E(mallerr.CodeInvalidParam, false, "cart 下单须 from_cart=true"))
				return
			}
			var lines []*mallv1.Line
			lines, err = cartLines(ctx, orders, tenantID(c))
			if err != nil {
				abortWith(c, err)
				return
			}
			reply, err = orders.CreateCartOrder(ctx, &mallv1.CreateCartOrderReq{
				TenantId: tenantID(c), IdemKey: idem, Lines: lines,
			})
		default:
			abortWith(c, mallerr.E(mallerr.CodeInvalidParam, false, "kind 须为 recharge|cart"))
			return
		}
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

// PayProvider 支付渠道抽象（bff 侧，R1 仅 mock）。在途支付单（trade_no）
// 由 mall-order BeginPayment 落库分配（PRD §5.2）；渠道侧只负责按单号
// 回调——真实渠道接入时在此扩 CreatePayment（拿 BeginPayment 的单号去渠道建单）。
type PayProvider interface {
	ScheduleCallback(cb callbackPayload)
}

// MockProvider 进程内占位渠道：拿到 mall-order BeginPayment 分配的 trade_no
// 后起 goroutine，延迟 cfg.CallbackDelay（生产默认 ~200ms）向 PAY_CALLBACK_URL
// POST HMAC-SHA256 签名载荷，模拟真实渠道异步回调（真实渠道接入后按 ADR-007
// 替换实现）。
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

// providerMock 当前部署的支付渠道标识（ADR-007 档一；真实渠道接入时改为
// 配置项——三处使用点：BeginPayment 建单、回调载荷、payView 响应）。
const providerMock = "mock"

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

// handlePay POST /orders/{no}/pay：会话校验 + GetOrder(tenant) →
// created → BeginPayment（mall-order 在途单落库/复用，分配 trade_no）
// + 异步回调排程 → 200 created；
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
			// 在途支付单（payment_records=created）在 mall-order 建单/复用并
			// 分配 trade_no（PRD §5.2）；复用时返回同号，防止并发拉起多笔
			// 渠道支付（双扣款）。关单竞态由 BeginPayment 锁下复查兜住（30002）。
			begin, err := orders.BeginPayment(ctx, &mallv1.BeginPaymentReq{
				TenantId:   tenant,
				OrderNo:    ord.GetOrderNo(),
				Provider:   providerMock,
				AmountCent: ord.GetAmountCent(),
			})
			if err != nil {
				// TOCTOU：GetOrder 与 BeginPayment 之间订单可能已被回调翻成
				// paid（mock 快回调/并发回调）。契约口径「已支付返回当前状态
				//（30002 仅用于已关闭/已退）」——重查一次分流，不把瞬时竞窗
				// 暴露成 409；closed/refunded 维持 30002 原样透传。
				if code, _, ok := mallerr.FromGRPC(err); ok && code == mallerr.CodeOrderStateInvalid {
					if again, gerr := orders.GetOrder(ctx, &mallv1.GetOrderReq{TenantId: tenant, OrderNo: orderNo}); gerr == nil {
						switch again.GetStatus() {
						case "paid", "fulfilled":
							return payView("paid", again.GetOrderNo(), ""), nil
						}
					}
				}
				return nil, err
			}
			tradeNo := begin.GetTradeNo()
			// 异步回调载荷：tenant 取自当前会话（验签后的信任链起点）。
			// 复用在途单时同样排程（MarkPaidTx 幂等，重复回调 no-op——
			// 兼作上一轮回调丢失的恢复路径）。
			provider.ScheduleCallback(callbackPayload{
				OrderNo:    ord.GetOrderNo(),
				Provider:   providerMock,
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
	payment := gin.H{"order_no": orderNo, "provider": providerMock, "status": status}
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
